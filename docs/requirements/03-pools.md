# The Pool Controller

The Pool Controller declares each `Pool` to battery and mirrors battery's
view of it into the Pool's status (ADR 0001, decision 4).

## Declaration {#declaration}

- **PO-001** When a Pool exists that battery does not hold, the Pool
  Controller SHALL add the finalizer `battery.liquidmetal-x.dev/pool` to the
  Pool before it creates it in battery with `CreatePool`, under the Pool's
  namespace and name.
- **PO-002** When a Pool's `metadata.generation` differs from its
  `status.observedGeneration`, the Pool Controller SHALL send the Pool's spec
  to battery with `UpdatePool` and then set `status.observedGeneration` to
  that generation.
- **PO-003** The Pool Controller SHALL add a finalizer to each Pool, and when
  the Pool is deleted SHALL call `DeletePool`, drain the Pool while battery
  refuses it (PO-030, PO-031), and remove the finalizer only once battery
  has deleted the Pool or reported it unknown.
- **PO-004** If battery refuses a Pool's spec, then the Pool Controller SHALL
  set the Pool's condition `Ready` false with the reason `Rejected` and
  battery's message.

Before `CreatePool` means stored: the Pool Controller calls `CreatePool`
only for a Pool it has read back from the API server with the finalizer on
it. A Pool deleted before then goes at once and leaves nothing in battery,
and a Pool battery holds leaves the cluster only through the finalizer of
PO-003, which deletes it from battery first.

Most of what battery would refuse is refused at admission by RS-012; PO-004
covers the rest.

## Placement {#placement}

- **PO-010** The Pool Controller SHALL set a Pool's `flintlock_hosts` in
  battery to the names of the Hosts whose Nodes match the Pool's
  `spec.placement.nodeSelector`.
- **PO-011** When the set of Hosts a Pool's selector matches changes, the
  Pool Controller SHALL update the Pool in battery with `UpdatePool`.
- **PO-012** While a Pool's selector matches no Host, the Pool Controller
  SHALL set the Pool's condition `Ready` false with the reason
  `NoEligibleHost`.

battery v0.3.3 has each Pool list its Hosts by name. The Pool Controller
resolves the selector against the Hosts the Inventory Controller has given
battery and is not about to remove (IN-013), so adding a Host to the
cluster reaches every Pool that selects it with no change to any Pool
(ADR 0001, consequence 1). It watches those Hosts and the Nodes' labels,
so a Host joining or leaving, or a Node relabelled into or out of a
selector, updates the Pools at once.

While battery refuses a Pool's spec (PO-004), the reason `Rejected` stands
on `Ready` even when the Pool's selector also matches no Host: the
refusal is what the Pool's owner has to fix first, and `NoEligibleHost`
(PO-012) shows once battery accepts a spec. Nor can `UpdatePool` carry a
change in the Hosts the selector matches while battery refuses the spec it
comes with. battery's Pool keeps naming the Hosts of the last spec battery
accepted, or is not there at all if battery never accepted one, and the
Pool's counts stay battery's for that Pool. A Host that leaves the cluster
meanwhile is removed from battery once the drain timeout of IN-013 passes.

## Status {#pool-status}

- **PO-020** The Pool Controller SHALL take a Pool's counts of available,
  leased, provisioning and quarantined MicroVMs from battery's `PoolStatus`
  for the Pool and from nothing else.
- **PO-021** The Pool Controller SHALL set a Pool's condition `Exhausted`
  true while battery reports no available MicroVM in a Pool whose size is
  greater than zero, and false otherwise.
- **PO-022** The Pool Controller SHALL set a Pool's condition `Ready` true
  while battery holds the Pool, its selector matches a Host, and the sum of
  its available, leased and provisioning MicroVMs is at least its size.
- **PO-023** When battery's `Events` stream reports an event for a Pool, the
  Pool Controller SHALL refresh that Pool's status.
- **PO-024** While the Pool Controller's subscription to battery's `Events`
  stream is not connected, the Pool Controller SHALL refresh every Pool's
  status at the configured resync interval.

The Events stream makes the status prompt; the resync of PO-024 makes it
eventually right when the stream drops.

## Deletion {#deletion}

- **PO-030** When battery refuses `DeletePool` for a deleted Pool because
  the Pool still has MicroVMs, the Pool's spec in battery is not its
  drained spec, and battery reports no provisioning MicroVM in the Pool,
  the Pool Controller SHALL send battery the Pool's drained spec with
  `UpdatePool`.
- **PO-031** When battery refuses `DeletePool` for a deleted Pool whose
  spec in battery is its drained spec, the Pool Controller SHALL claim each
  available MicroVM of the Pool with `ClaimVM`, release it at once with
  `ReleaseVM`, and then call `DeletePool` again.
- **PO-032** The Pool Controller SHALL NOT release a Lease of a deleted
  Pool other than one it claimed itself under PO-031.
- **PO-033** While battery refuses `DeletePool` for a deleted Pool that has
  leased or quarantined MicroVMs, the Pool Controller SHALL set the Pool's
  condition `Ready` false with the reason `DeletionBlocked` and a message
  that gives those counts.
- **PO-034** While battery refuses `DeletePool` for a deleted Pool that has
  no leased or quarantined MicroVM, the Pool Controller SHALL set the Pool's
  condition `Ready` false with the reason `Draining`.

battery v0.3.3 refuses `DeletePool` while the Pool owns any MicroVM
(BA-070), and has no call that deletes a Pool with its MicroVMs. So the
Pool Controller empties the Pool first, with the calls battery has. The
drained spec (glossary) makes battery stop creating MicroVMs for the Pool:
a `MIN_SIZE_THRESHOLD` Pool of size 0 creates none (BA-072), where the
other strategies would replace each MicroVM the drain takes. It also makes
the drain's own claims run no pre-lease hook, and delete rather than
quarantine a MicroVM whose claim fails. The Pool Controller then claims
each available MicroVM and releases it, which deletes it (BA-030), and
asks battery to delete the Pool again.

PO-030 waits for the Pool's MicroVMs to finish provisioning, because
`UpdatePool` cancels a provisioning MicroVM under the Pool's old hook
failure policy (BA-074), which may quarantine it and so block the deletion
for good. A Pool that stays provisioning is one battery would refuse to
delete anyway.

The drain does not take a MicroVM a claim holds. A Pool deleted while
claims hold its MicroVMs stays, with `Ready` false and the reason
`DeletionBlocked` (PO-033), until each of those claims ends: it is
released (CL-020) or its Lease expires (BA-023). Its Bound claims keep
working meanwhile, and a claim can still bind to an available MicroVM
the drain has not yet taken, which then holds the deletion up the same
way; nothing replaces it. The Operator's Events stream reports each
deletion (PO-023), so the Pool is deleted soon after its last MicroVM.

A quarantined MicroVM also blocks the deletion, and battery v0.3.3 never
deletes one (BA-073). A Pool that has one keeps the reason
`DeletionBlocked` until an operator removes that MicroVM from battery's
database by hand; upstream would have to add a call that does it (#23).
While battery is only deleting MicroVMs, or finishing their provisioning,
the reason is `Draining` (PO-034).

If the Operator stops between the drain's `ClaimVM` and its `ReleaseVM`,
or the `ReleaseVM` fails in transit, the Lease is left with nothing to
renew it. battery deletes it and its MicroVM once the Pool's
`heartbeat_expiry_threshold` has passed (BA-023), and the deletion then
goes on. PO-032 keeps the drain from releasing a Lease it cannot tell
from a claim's (CL-031).
