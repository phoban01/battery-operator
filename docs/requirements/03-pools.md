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
- **PO-041** When battery's `Events` stream reports a `VM_HOOK_FAILED`
  event for a Pool, the Pool Controller SHALL record on the Pool a
  Kubernetes Event of type `Warning` with the reason `HookFailed`, whose
  message names the MicroVM and, where the event's payload gives them, the
  hook and its error.

The Events stream makes the status prompt; the resync of PO-024 makes it
eventually right when the stream drops.

PO-041 shows on the Pool the one provisioning failure battery v0.3.3
reports. battery v0.3.3 records `VM_HOOK_FAILED` with no payload, so its
Event names the MicroVM only, and the cause is in the log of the battery
container; the fake battery's payload also gives the hook and the error.
battery replays its outbox on each subscription (BA-050), so the Pool
Controller remembers the last event id it recorded for each Pool and
records no event twice. A restart of the Operator forgets the ids, and may
record the replayed events again. Like PO-039, PO-041 is to be refactored
once battery reports why a provision fails.

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

## Refilling {#refill}

- **PO-035** The Pool Controller SHALL treat a Pool as stalled while its
  replenishment strategy is `ImmediateOnLease` or `ReplaceOnDelete`,
  battery holds the Pool and has accepted its current spec, its selector
  matches a Host, battery reports no provisioning MicroVM in it, and its
  shortfall is greater than zero.
- **PO-036** When a Pool has been stalled for its reseed wait, counted
  from the later of the time the Pool Controller first saw it stalled and
  the Pool Controller's last `UpdatePool` for it, the Pool Controller SHALL
  send battery the Pool's unchanged spec with `UpdatePool`.
- **PO-037** The Pool Controller SHALL make a Pool's reseed wait one
  minute, double it after each `UpdatePool` of PO-036 up to 30 minutes,
  and set it back to one minute when the Pool's shortfall changes or
  reaches zero.
- **PO-038** While a Pool's shortfall is greater than zero and its
  replenishment strategy is `ImmediateOnLease` or `ReplaceOnDelete`, the
  Pool Controller SHALL reconcile the Pool again no later than the end of
  its reseed wait.
- **PO-039** While a Pool's shortfall is unchanged since the Pool
  Controller saw the Pool stalled after an `UpdatePool` of PO-036, and the
  sum of its available, leased and provisioning MicroVMs is less than its
  size, the Pool Controller SHALL set the Pool's condition `Ready` false
  with the reason `NotFilling`.
- **PO-040** The Pool Controller SHALL give the condition `Ready` with the
  reason `NotFilling` a message that gives the Pool's shortfall, the number
  of `UpdatePool` calls of PO-036 since the shortfall last changed, the time
  of the next one while the Pool is stalled, and the log of the battery
  container as the place to find the cause.

A Pool's shortfall is its size less its available MicroVMs for
`ImmediateOnLease`, and its size less its available and leased MicroVMs
for `ReplaceOnDelete`. While nothing is provisioning in the Pool, that is
what battery would provision for it if its reconciler started now
(BA-075). The shortfall leaves the provisioning MicroVMs out, so that a
reseed's own provisioning does not count as progress: if it fails, the
shortfall is as it was, and the wait goes on growing.

This section works around battery v0.3.3, and is to be removed once
battery retries a failed seed. battery seeds a Pool of these two
strategies once, when the Pool's reconciler starts, and marks it seeded
whether or not the provisions succeed (BA-075). After that it provisions
for the Pool only on a claim or a deletion (BA-076). So a Pool whose seed
fails has nothing to claim and nothing to delete, and stays short for
good. The same holds for a Pool whose replacement after a claim or a
deletion fails. `UpdatePool` starts a new reconciler, which seeds again
(BA-074), and the workaround relies on that: battery v0.3.3 restarts the
reconciler on every `UpdatePool`, even with an unchanged spec, but battery
does not promise it. A `MinSizeThreshold` Pool needs none of this, since
battery tops it up on its tick.

The wait and its backoff keep the Pool Controller from calling battery
often. A MicroVM that battery is provisioning counts as provisioning, and
a Pool is stalled only when nothing is provisioning, so the wait does not
race a slow boot. Outside a failure, a Pool is stalled only for the moment
between a claim or a deletion and the start of its replacement, which is
far less than a minute. Each `UpdatePool` makes battery try to provision
the whole shortfall again; when those attempts keep failing, as when
`flintlockd` refuses the Pool's template, the doubling brings the calls
down to one every 30 minutes. A change in the shortfall is progress, or a
new failure, and starts the wait again from one minute.

The Pool Controller keeps the waits in memory. A restart of the Operator
loses them, which only starts each wait again. battery sends no event for
a stalled Pool, so the Pool Controller asks for its own reconcile when the
wait ends (PO-038).

The reason `NotFilling` (PO-039, PO-040) also works around battery
v0.3.3, and is to be refactored once battery reports why a provision
fails. battery v0.3.3 reports a failed provision nowhere the Operator can
read: `PoolStatus` holds only counts, and a `CreateMicroVM` that
`flintlockd` refuses records no event. So the Pool Controller can say only
that the Pool is not filling, and where to look. It says so once a reseed
has gone by and the Pool is stalled again with the same shortfall: one
failed reseed is evidence, where the first stall may be the short moment
after a claim. The reason stays while battery provisions for the next
reseed, so that `Ready` does not move between `NotFilling` and
`BelowSize` with each attempt. It goes, back to the reason of PO-022, when
the shortfall changes or reaches zero, which also sets the reseed wait
back (PO-037). While the provisioning MicroVMs make up the shortfall,
PO-022 sets `Ready` true, as for any Pool.
