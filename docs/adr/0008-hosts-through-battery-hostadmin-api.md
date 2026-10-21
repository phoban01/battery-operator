# 0008. Hosts through battery's HostAdmin API

- **Status:** Accepted
- **Date:** 2026-10-21
- **Discussion:** [#198](https://github.com/phoban01/battery-operator/issues/198);
  [liquidmetal-dev/battery#138](https://github.com/liquidmetal-dev/battery/pull/138)

## Context

ADR 0001 has the Operator drive an unchanged battery over its gRPC API. Its
consequence 1 records the largest cost of that choice: battery read its
Hosts once, from its configuration file, and had no call to change them.
So the Inventory Controller rendered battery's configuration into a
ConfigMap and restarted battery for every change. That took a shared
process namespace and a signal (DP-006), a restart window to batch changes
and keep the kubelet's back-off short (IN-012), a drain before a restart
that removed a Host (IN-013), and a write-back of the Hosts that an apply
of the Manifests reset (IN-014). battery also read its client certificate
only when it started, so a renewed certificate needed a restart too
(DP-007, DP-008).

ADR 0001 named a Host registration call as the upstream ask that would end
the restarts. battery's own ADR, `2026-09-24-host-api.md`, answers it, and
battery#138 builds it on battery's `main` branch, for release in v0.5.0:

- `HostAdmin` gains `AddHost`, `UpdateHost`, `RemoveHost` and `GetHost`,
  beside the `CordonHost`, `UncordonHost` and `ListHosts` of v0.4.0.
- battery keeps each Host's address, TLS settings and cordon state in its
  database, and changes them while it runs.
- battery reads no Hosts from its configuration file, and refuses to start
  with a `hosts` key in it.
- A Pool may name only Hosts battery holds, and battery removes a Host only
  once no Pool names it and no MicroVM is on it.
- battery reads a Host's certificate each time it connects to the Host, and
  `UpdateHost` makes it connect again.

battery is still unchanged by this project. The question is how the
Operator uses the new API, and what of the old mechanism goes.

## Decision

1. **The Inventory Controller keeps battery's Hosts through `HostAdmin`.**
   A Node that is a Host is added with `AddHost`, with the TLS settings of
   IN-004, and kept at its Node's address and those settings with
   `UpdateHost` (IN-017). Nothing writes battery's configuration, and
   nothing restarts battery for its Hosts.
2. **A Host that stops being one is cordoned while its Node exists, and
   removed once its Node is gone.** A cordoned Node, or a Host whose Node
   report says not ready, is cordoned in battery (IN-015): battery places
   nothing new there and claims Bound on it run to their end. A Host whose
   Node is deleted is cordoned and then removed with `RemoveHost`, retried
   until battery agrees (IN-016). The reasons are in
   [04-inventory.md](../requirements/04-inventory.md#admission).
3. **The Pool Controller sees a Host only while battery holds it
   uncordoned** (IN-018). A joining Host reaches the Pools after `AddHost`;
   a leaving Host leaves the Pools before it is cordoned or removed. This
   replaces the drain before a restart (IN-013).
4. **A renewed client certificate reaches battery through `UpdateHost`**
   for every Host, with the settings unchanged, once the Operator's mount of
   the Secret holds it, and once after the Operator starts (DP-035).
5. **The settle time stays; the restart window, the drain timeout and the
   write-back go.** The settle time (IN-011) still keeps a flapping Host
   from churning battery and the Pools. IN-010, IN-012 to IN-014 and DP-006
   to DP-008 are withdrawn; DP-004 is replaced by DP-036, a configuration
   that names no Hosts and that the Operator does not write.
6. **The upgrade is the steady state.** battery v0.5.0 keeps the Hosts a
   v0.4 database holds, without TLS settings. The Inventory Controller
   compares each Host battery holds with what IN-017 wants, and gives every
   other Host the TLS settings of IN-004 (IN-020), so its first reconcile
   updates them all. An `AddHost` that finds a Host already there updates
   it instead of failing (IN-019). The placeholder Host the old
   Operator gave battery has no Node, so it is removed (IN-016). No step
   runs once for the upgrade alone.

## Consequences

- battery no longer restarts for a change to its Hosts or a renewal. No
  `Events` stream drops, no claim in its pre-lease hook is abandoned, and
  the controllers no longer wait behind a restart gate.
- The Operator's pod no longer shares its process namespace, and the
  Operator's container no longer mounts battery's configuration. The
  Operator needs no access to battery's ConfigMap.
- A Host change reaches battery after the settle time, not after the
  settle time, a restart window and a drain.
- A cordoned or not-ready Host stays in battery. Its MicroVMs stay known to
  battery, which deletes them as their Leases end.
- A Host whose Node is deleted while MicroVMs remain on it, and whose
  machine is gone, cannot be removed: battery cannot delete those MicroVMs,
  and refuses the removal while they are counted. It stays cordoned, and
  the Inventory Controller keeps trying.
- Every Host gets a new connection each time the Operator starts (DP-035),
  because the Operator does not record which certificate battery last read.
- The Operator needs battery v0.5.0 and does not work with v0.4.0, which has
  no `AddHost`. Until v0.5.0 is released, the Operator builds against
  battery's `main` branch, and its e2e suite cannot pass.

ADR 0001 stands: the Operator is still a client of an unchanged battery.
This record replaces its consequence 1, and the restart in its decision 7.

## Open questions

1. **A Host whose machine is gone.** battery refuses to remove a Host with
   MicroVMs on it, so that it never loses track of one (battery's ADR,
   "Let RemoveHost force-remove a host"). A Node deleted with its machine
   leaves such a Host for good. Whether the Operator should ask battery
   for a forced removal, if battery ever offers one, is open.
