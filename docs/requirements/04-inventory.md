# The Inventory Controller

The Inventory Controller decides which Nodes are Hosts and gives battery the
list (ADR 0001, decisions 4 and 10). battery v0.3.3 reads its Hosts once, from
its configuration file, and has no call to change them, so giving battery
the list means rewriting its configuration and restarting it
(consequence 1).

## Admission {#admission}

- **IN-001** The Inventory Controller SHALL treat a Node as a Host only while
  the Node's Node report says the Host is ready and the Node is schedulable.
- **IN-002** When a Node that is a Host is cordoned or deleted, or its Node
  report says the Host is not ready, the Inventory Controller SHALL remove
  the Host from battery's Hosts.
- **IN-003** The Inventory Controller SHALL give battery each Host under the
  Node's name, at the `flintlockd` address the Host's Node report gives.
- **IN-004** The Inventory Controller SHALL configure battery to reach every
  Host's `flintlockd` over TLS, verifying the serving certificate against
  the configured certificate authority and presenting the Operator's client
  certificate for `flintlockd`.

A Host joins only after its own Exec Agent has checked it (decision 10), so
the Inventory Controller never admits a Node that could not run MicroVMs.
A cordoned Host leaves battery's list so that battery places nothing new
there, once the change has settled and its restart window has closed
(IN-011, IN-012); until then battery can still place MicroVMs on it. Claims
already Bound on it keep running, and the Exec Agent holds the Node's drain
open until they end (EA-040).

## Applying the list {#applying}

- **IN-010** When the set of Hosts changes, the Inventory Controller SHALL
  write the new list to battery's configuration and restart battery.
- **IN-011** The Inventory Controller SHALL act on a change in whether a Node
  is a Host only after the change has held for the configured settle time.
- **IN-012** When a change has settled and no restart window is open, the
  Inventory Controller SHALL open a restart window of the configured
  length, and SHALL apply every change that has settled by the time the
  window closes in a single restart of battery.

Every change restarts battery, so IN-011 and IN-012 keep a Host whose report
flaps between ready and not ready, or a burst of Nodes joining, from
restarting it over and over. They also keep short the back-off with which
the kubelet restarts battery, which grows while battery keeps exiting soon
after starting.

The restart window opens when the first settled change is waiting, not at
a restart. A change still settling when the window closes waits for a
window of its own, and a window whose changes all flapped back closes
without a restart. Restarts are therefore at least one restart window
apart, and a change reaches battery at least the settle time and at most
the settle time plus one restart window after it happens. The settle time
and the restart window are the Operator's flags `--inventory-settle-time`
and `--inventory-restart-window`, 30 seconds and one minute by default.

A Host registration call or a configuration reload in battery would remove
the restarts; it is one of the upstream asks.

How the Operator restarts battery is decided with the Manifests
(`06-deployment.md#battery-sidecar`).

## How battery reaches `flintlockd` {#flintlockd-reachability}

battery creates and deletes MicroVMs by calling `flintlockd` on each Host.
Each Host's `flintlockd` serves on the Host's internal address with mutual
TLS, and battery reaches it directly with the Operator's client certificate
([ADR 0002](../adr/0002-battery-reaches-flintlockd-over-mtls.md)). IN-004
is what keeps that connection authenticated both ways: battery v0.3.3 can
also be configured without TLS, and the Inventory Controller never does so.
