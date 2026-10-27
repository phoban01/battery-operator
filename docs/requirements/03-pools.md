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
  the Pool is deleted SHALL call `DeletePool` until battery accepts it
  (PO-042, PO-043), and remove the finalizer only once battery has
  deleted the Pool or reported it unknown.
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

battery v0.4.0 has each Pool list its Hosts by name. The Pool Controller
resolves the selector against the Hosts that battery holds uncordoned and
the Inventory Controller is not about to cordon or remove (IN-018), so
adding a Host to the cluster reaches every Pool that selects it with no
change to any Pool. battery refuses a Pool that names a Host it does not
hold (BA-082), and IN-018 keeps the Pool Controller from naming one. It watches those Hosts and the Nodes' labels,
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
meanwhile is cordoned in battery, and battery refuses to remove it while
that Pool names it (BA-083); the Inventory Controller tries again until
the Pool's spec is fixed or the Pool is deleted (IN-016).

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

PO-041 shows on the Pool the one provisioning failure battery v0.4.0
reports. battery v0.4.0 records `VM_HOOK_FAILED` with no payload, so its
Event names the MicroVM only, and the cause is in the log of the battery
container; the fake battery's payload also gives the hook and the error.
battery replays its outbox on each subscription (BA-050), so the Pool
Controller remembers the last event id it recorded for each Pool and
records no event twice. A restart of the Operator forgets the ids, and may
record the replayed events again. Like PO-039, PO-041 is to be refactored
once battery reports why a provision fails.

## Deletion {#deletion}

- **PO-030** (withdrawn): replaced by PO-042.
- **PO-031** (withdrawn): replaced by battery's own draining (BA-070)
  and PO-044.
- **PO-032** (withdrawn): replaced by PO-044.
- **PO-033** (withdrawn): replaced by PO-043.
- **PO-034** (withdrawn): replaced by PO-043.
- **PO-042** When battery refuses `DeletePool` for a deleted Pool and the
  Pool's spec in battery is not its drained spec, the Pool Controller
  SHALL send battery the Pool's drained spec with `UpdatePool`.
- **PO-043** While battery refuses `DeletePool` for a deleted Pool, the
  Pool Controller SHALL set the Pool's condition `Ready` false with the
  reason `DeletionBlocked` and a message that gives the Pool's leased
  MicroVMs.
- **PO-044** The Pool Controller SHALL NOT claim or release a MicroVM of a
  deleted Pool, and SHALL leave the deletion of the Pool's MicroVMs to
  battery's `DeletePool`.

battery v0.4.0's `DeletePool` drains the Pool it deletes
(liquidmetal-dev/battery#83). It refuses while the Pool has a leased or
claiming MicroVM, and otherwise deletes the Pool with every MicroVM it
owns: available, provisioning and quarantined ones alike (BA-070,
BA-073). So the Pool Controller calls `DeletePool` and does nothing to
the Pool's MicroVMs itself (PO-044). A Pool that no claim holds goes in
one call.

A Pool deleted while claims hold its MicroVMs stays, with `Ready` false
and the reason `DeletionBlocked` (PO-043), until each of those claims
ends: it is released (CL-020) or its Lease expires (BA-023). Its Bound
claims keep working meanwhile. The Operator's Events stream reports each
deletion (PO-023), so the Pool Controller asks again soon after the last
claim ends, and battery then deletes the Pool with its warm MicroVMs.

While battery refuses, it keeps the Pool and its reconciler, and would
replace each MicroVM a new claim takes. The drained spec (glossary) stops
that: a `MIN_SIZE_THRESHOLD` Pool of size 0 creates no MicroVM (BA-072).
So the deletion waits at most for the claims there are; nothing replaces
the MicroVMs they hold. The Claim Controller binds no new claim to a
deleted Pool (CL-063), but a claim whose `ClaimVM` raced the deletion can
still take a warm MicroVM. The drained spec keeps the Pool's hooks and
hook failure policy, since such a claim is a real one. `UpdatePool` cancels
any provisioning MicroVM under the Pool's hook failure policy (BA-074),
which may quarantine it; battery deletes a quarantined MicroVM with its
Pool (BA-073), so that does not hold the deletion up.

battery v0.3.3 refused `DeletePool` while the Pool owned a MicroVM in
any phase, and deleted none. The Pool Controller then emptied the Pool
itself: it gave the Pool its drained spec, claimed each available
MicroVM and released it at once, and waited for any MicroVM still
provisioning (PO-030 to PO-034, #137). battery v0.4.0 does that work in
`DeletePool`, so #199 withdrew those requirements. The drained spec stays,
for the reason above.

## Refilling {#refill}

- **PO-035** The Pool Controller SHALL treat a Pool as stalled while its
  replenishment strategy is `ImmediateOnLease` or `ReplaceOnDelete`,
  battery holds the Pool and has accepted its current spec, its selector
  matches a Host, battery reports no provisioning MicroVM in it, and its
  shortfall is greater than zero.
- **PO-036** (withdrawn): battery v0.4.0 provisions a Pool's shortfall on
  every tick (BA-075), so the Pool Controller sends no reseed.
- **PO-037** (withdrawn): with PO-036.
- **PO-038** (withdrawn): replaced by PO-047.
- **PO-039** (withdrawn): replaced by PO-045.
- **PO-040** (withdrawn): replaced by PO-046.
- **PO-045** While the Pool Controller has seen a Pool stalled with its
  current shortfall at two reconciles at least one minute apart, and the
  sum of the Pool's available, leased and provisioning MicroVMs is less
  than its size, the Pool Controller SHALL set the Pool's condition `Ready`
  false with the reason `NotFilling`.
- **PO-046** The Pool Controller SHALL give the condition `Ready` with the
  reason `NotFilling` a message that gives the Pool's shortfall, the time
  the Pool Controller first saw the Pool stalled with it, and the log of
  the battery container as the place to find the cause.
- **PO-047** While a Pool is stalled and PO-045 does not yet hold for its
  shortfall, the Pool Controller SHALL reconcile the Pool again no later
  than one minute after it first saw the Pool stalled with that shortfall.

A Pool's shortfall is its size less its available MicroVMs for
`ImmediateOnLease`, and its size less its available and leased MicroVMs
for `ReplaceOnDelete`. That is what battery provisions for the Pool on
each of its ticks, less what it is provisioning already (BA-075). The
shortfall leaves the provisioning MicroVMs out on purpose: an attempt that
fails changes nothing, so the Pool stays stalled with the same shortfall,
and only a MicroVM that becomes available, or a claim or a deletion,
changes it.

battery v0.4.0 provisions the shortfall of every Pool when its reconciler
starts and on every tick after that, ten seconds apart, so a failed
provision is tried again without the Operator's help (BA-075). The Pool
Controller therefore sends battery nothing for a stalled Pool. Until
battery v0.4.0 it did: battery v0.3.3 seeded such a Pool once and never
again, and PO-036 to PO-038 made the Pool Controller send the Pool's
unchanged spec with `UpdatePool`, which starts a new reconciler (BA-074),
after a wait that doubled from one minute to thirty. #242 removed that.
An `UpdatePool` is not free: it stops the Pool's reconciler, and a
`CreateMicroVM` the stop cancels can leave a MicroVM that battery does
not know of (liquidmetal-dev/battery#117).

What the Pool Controller still does is say when a Pool is not filling
(PO-045, PO-046). battery v0.4.0 reports a failed provision nowhere the
Operator can read: `PoolStatus` holds only counts, and a `CreateMicroVM`
that `flintlockd` refuses records no event. So the Pool Controller can
say only that the Pool is not filling, and where to look. One stalled
sighting is not evidence: a Pool is stalled for a moment after every
claim or deletion, before its replacement starts. Two sightings a minute
apart with the same shortfall are, since battery has had about six ticks
to fill it in between. A sighting in between that finds battery
provisioning does not start the minute again: an attempt that fails
quickly leaves the Pool stalled most of the time, and one that fails
slowly leaves it provisioning most of the time, and either way the
shortfall is unchanged. The reason stays while battery provisions for
the next attempt, so that `Ready` does not move between `NotFilling` and
`BelowSize` with each one, and goes, back to the reason of PO-022, when
the shortfall changes or reaches zero. While the provisioning MicroVMs
make up the size, PO-022 sets `Ready` true, as for any Pool.

The Pool Controller keeps the sightings in memory. A restart of the
Operator loses them, which only starts the minute again. battery sends no
event for a stalled Pool, so the Pool Controller asks for its own
reconcile when the minute ends (PO-047); after that, battery's events and
the resync of PO-024 bring the Pool back.
