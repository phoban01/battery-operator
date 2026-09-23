# The Pool Controller

The Pool Controller declares each `Pool` to battery and mirrors battery's
view of it into the Pool's status (ADR 0001, decision 4).

## Declaration {#declaration}

- **PO-001** When a Pool exists that battery does not hold, the Pool
  Controller SHALL create it in battery with `CreatePool`, under the Pool's
  namespace and name.
- **PO-002** When a Pool's `metadata.generation` differs from its
  `status.observedGeneration`, the Pool Controller SHALL send the Pool's spec
  to battery with `UpdatePool` and then set `status.observedGeneration` to
  that generation.
- **PO-003** The Pool Controller SHALL add a finalizer to each Pool, and when
  the Pool is deleted SHALL call `DeletePool` and remove the finalizer only
  once battery has deleted the Pool or reported it unknown.
- **PO-004** If battery refuses a Pool's spec, then the Pool Controller SHALL
  set the Pool's condition `Ready` false with the reason `Rejected` and
  battery's message.

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

battery v0.1.0 has each Pool list its Hosts by name. The Pool Controller
resolves the selector against the Hosts the Inventory Controller has given
battery, so adding a Host to the cluster reaches every Pool that selects it
with no change to any Pool (ADR 0001, consequence 1).

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
