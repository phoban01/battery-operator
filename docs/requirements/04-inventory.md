# The Inventory Controller

The Inventory Controller decides which Nodes are Hosts and keeps battery's
Hosts in step with them (ADR 0001, decisions 4 and 10). It does so through
battery's `HostAdmin` API: `AddHost`, `UpdateHost`, `RemoveHost`,
`CordonHost`, `UncordonHost`, `GetHost` and `ListHosts` (BA-080 to BA-089;
[ADR 0008](../adr/0008-hosts-through-battery-hostadmin-api.md)). battery
keeps its Hosts in its database, and changes them while it runs, so no
change to the Hosts restarts battery.

Up to battery v0.4.0, battery read its Hosts from its configuration file,
and the Inventory Controller rewrote that file and restarted battery for
every change (IN-010, IN-012 to IN-014, DP-006 to DP-008, all withdrawn).

## Admission {#admission}

- **IN-001** The Inventory Controller SHALL treat a Node as a Host only while
  the Node's Node report says the Host is ready and the Node is schedulable.
- **IN-002** (withdrawn): replaced by IN-015 and IN-016.
- **IN-003** The Inventory Controller SHALL give battery each Host under the
  Node's name, at the `flintlockd` address the Host's Node report gives.
- **IN-004** The Inventory Controller SHALL configure battery to reach every
  Host's `flintlockd` over TLS, verifying the serving certificate against
  the configured certificate authority and presenting the Operator's client
  certificate for `flintlockd`.
- **IN-015** When a Node that is a Host is cordoned, or its Node report
  says the Host is not ready or gives no `flintlockd` address, the
  Inventory Controller SHALL cordon the Host in battery with `CordonHost`,
  and SHALL keep it in battery's Hosts.
- **IN-016** When battery holds a Host whose Node does not exist or is being
  deleted, the Inventory Controller SHALL cordon the Host and remove it with
  `RemoveHost`, and SHALL try again while battery refuses because a Pool
  names the Host or a MicroVM is still on it.

A Host joins only after its own Exec Agent has checked it (decision 10), so
the Inventory Controller never admits a Node that could not run MicroVMs.

A Host that stops being one is cordoned, not removed, while its Node
exists (IN-015). battery then places no new MicroVM there (BA-084), and
every MicroVM already there stays: a claim Bound on it keeps its Lease and
runs to its end, and the Exec Agent holds the Node's drain open until it
does (EA-040). The two cases that cordon are these:

- A cordoned Node is most often one being drained for maintenance, and it
  comes back. Uncordoning the Host is then one call, and battery keeps its
  record of the Host and of every MicroVM on it.
- A Host whose report says not ready may have a `flintlockd` that is down.
  battery refuses to remove a Host while any MicroVM is on it (BA-083), so a
  removal would fail anyway. Kept, the Host lets battery delete its
  MicroVMs once `flintlockd` answers again. A claim on such a Host shows it
  at once, in its condition `HostReady` (CL-051).

A deleted Node is gone for good, so its Host is removed (IN-016). battery
allows that only once no Pool names the Host and no MicroVM is counted
against it. The Pool Controller drops the Host from every Pool once the
Inventory Controller takes it away (IN-018), and the MicroVMs on it go as
their Leases end. Until then the Host stays in battery, cordoned, and the
Inventory Controller tries again every `--inventory-remove-retry`, 30
seconds by default. If the Host's machine is gone too, battery cannot
delete the MicroVMs on it, and the Host stays; this is battery's rule, so
that it never loses track of a MicroVM.

IN-016 also removes a Host that battery holds and no Node names at all:
one left behind by a Node deleted while the Operator was down, and the
placeholder Host an older Operator gave battery (see
[Upgrading](#upgrade)).

## Applying the changes {#applying}

- **IN-010** (withdrawn): replaced by IN-017.
- **IN-011** The Inventory Controller SHALL act on a change in whether a Node
  is a Host only after the change has held for the configured settle time.
- **IN-012** (withdrawn): replaced by IN-017, which applies each change as
  it settles, without restarting battery.
- **IN-013** (withdrawn): replaced by IN-018.
- **IN-017** While a Node is a Host, the Inventory Controller SHALL keep a
  Host of the Node's name in battery, uncordoned, at the Node's `flintlockd`
  address and with the TLS settings of IN-004, adding it with `AddHost`,
  and changing its address or TLS settings with `UpdateHost` and uncordoning
  it with `UncordonHost` wherever battery's Host differs.
- **IN-018** The Inventory Controller SHALL give the Pool Controller a Host
  only once battery holds it uncordoned at the Node's address, and SHALL
  take a Host from the Pool Controller before it cordons or removes the Host.
- **IN-019** If `AddHost` answers that battery already holds a Host of that
  name, then the Inventory Controller SHALL read the Host with `GetHost` and
  bring it in step as IN-017 says, without failing the reconcile.

The settle time keeps a Host whose report flaps between ready and not
ready from cordoning and uncordoning it over and over, and from making the
Pool Controller update every Pool that selects it each time. A change
reaches battery once it has held for the settle time; a change that flaps
back before then reaches battery not at all. The settle time is the
Operator's flag `--inventory-settle-time`, 30 seconds by default.

battery refuses a Pool that names a Host it does not hold (BA-082), and
refuses to remove a Host that a Pool names (BA-083). IN-018 orders the
two sides so that neither refusal comes up in normal running. The Pool
Controller places Pools only on the Hosts the Inventory Controller gives
it (PO-010), so a joining Host reaches the Pools only once battery holds
it. A leaving Host is taken from the Pool Controller first; the Pool
Controller drops it from every Pool with `UpdatePool`, and the Inventory
Controller removes it from battery once battery agrees. A cordoned Host is
taken from the Pool Controller too, so the Pools stop naming a Host that
battery would not place on.

`AddHost` checks that battery can reach the Host and that its `flintlockd`
is recent enough, and stores nothing if not (BA-080). The Inventory
Controller asks for that check when it adds a Host, or changes the address
of a Host that its Node report says is ready: a Host that fails it stays
out of battery, and the Inventory Controller tries again. It skips the
check when it only reloads a Host's certificate (DP-035), so that a Host
that is down does not hold the reload back for the others.

## How battery reaches `flintlockd` {#flintlockd-reachability}

battery creates and deletes MicroVMs by calling `flintlockd` on each Host.
Each Host's `flintlockd` serves on the Host's internal address with mutual
TLS, and battery reaches it directly with the Operator's client certificate
([ADR 0002](../adr/0002-battery-reaches-flintlockd-over-mtls.md)). IN-004
is what keeps that connection authenticated both ways: battery can also be
given a Host without TLS, and the Inventory Controller never does so. The
TLS settings name files in battery's container, where the Manifests mount
the client certificate, its key and the serving CA (DP-005); battery reads
them each time it connects to the Host (BA-081).

## Keeping battery's configuration {#keeping}

- **IN-014** (withdrawn): replaced by IN-017; battery keeps its Hosts in
  its database, which applying the Manifests does not touch.

## Upgrading from battery v0.4 {#upgrade}

- **IN-020** When battery holds a Host whose TLS settings are not those of
  IN-004, the Inventory Controller SHALL update the Host with `UpdateHost` to
  those settings, keeping its address.

battery v0.4.0 kept its Hosts in its database too, as a copy of its
configuration file without the TLS settings. battery v0.5.0 migrates that
database and keeps those Hosts, but reads none from the file, and cannot
reach a Host stored without TLS settings until `UpdateHost` gives it some
(BA-086). On its first reconcile after the upgrade, the Inventory
Controller finds those Hosts with `ListHosts`. It updates the Host of each
Node that is a Host as IN-017 says, and every other Host as IN-020 says, so
that battery can reach a cordoned Host too and delete the MicroVMs on it as
their Leases end. A Host that `ListHosts` missed and `AddHost` then finds
is updated the same way (IN-019). Until then, battery fails to provision
on those Hosts, and claims already Bound on them keep running. Nothing in
this runs once for the upgrade alone: the same steps keep battery's Hosts
in step at every reconcile.

An Operator before v0.5 gave battery a placeholder Host, `_no-hosts`, when
no Node was a Host, because battery v0.4.0 refused to start with none. No
Node has that name, so IN-016 removes it.
