// -*- mode: Bluespec; -*-
//
// Pools, placement and inventory: the Pool Controller and the Inventory
// Controller against battery.
//
// What it models (docs/requirements/03-pools.md, 04-inventory.md,
// 06-deployment.md#battery-sidecar, 10-battery.md#hosts, ADR 0008):
//
// - the Inventory Controller: decide which Nodes are Hosts (IN-001), let a
//   change settle (IN-011), then keep battery's Hosts in step through
//   battery's HostAdmin API: add or update and uncordon a Node that is a
//   Host (IN-017, IN-019), cordon one that stops being a Host while its
//   Node exists (IN-015), cordon and remove one whose Node is gone, again
//   and again until battery agrees (IN-016), give the Pool Controller a
//   Host only once battery holds it uncordoned and take it away before
//   cordoning or removing it (IN-018), and call UpdateHost on every Host
//   for a renewed client certificate, and once after it starts (DP-035);
// - the Pool Controller: the finalizer, CreatePool once it is stored,
//   UpdatePool on a new generation or a new set of matching Hosts, the
//   Rejected condition for a spec battery refuses and the NoEligibleHost
//   condition, and DeletePool under the finalizer, with the drained spec
//   for a Pool battery refuses to delete, and no claim or release of its
//   own (PO-001 to PO-004, PO-010 to PO-012, PO-042, PO-044);
// - battery: it holds its Hosts in its database with a cordon flag and
//   changes them while it runs, and never restarts for them. It reads a
//   Host's client certificate when it connects to it, in AddHost and
//   UpdateHost (BA-080, BA-081). It refuses a Pool that names a Host it
//   does not hold (BA-082), refuses to remove a Host a Pool names or a
//   MicroVM is on (BA-083), and places nothing on a cordoned Host (BA-084).
//   It replenishes each Pool on the least loaded eligible Host in the
//   Pool's flintlock_hosts (internal/reconciler/hostpick.go). It refuses
//   DeletePool while the Pool owns a leased MicroVM, and otherwise deletes
//   the Pool with its MicroVMs (BA-070). It refuses a spec whose heartbeat
//   expiry threshold is not positive, which stands here for every other
//   spec it refuses (PO-004). It may start on a database battery v0.4.0
//   wrote, whose Hosts have no TLS settings, with the placeholder Host an
//   older Operator gave it among them (BA-086);
// - the environment: Nodes are labelled, cordoned, uncordoned, deleted and
//   created again, their Node reports flip between ready and not ready,
//   Pools are created, changed (to a spec battery refuses, too) and
//   deleted, claims lease available MicroVMs of a Pool that is not being
//   deleted (CL-063) and release them, cert-manager renews battery's
//   client certificate and the kubelet later updates its mount, the
//   Operator restarts, and time passes.
//
// The Pool Controller resolves a selector against the Hosts the Inventory
// Controller has published (the HostSet) whose Nodes exist and match.
//
// Time: the controllers act promptly. Time does not pass while the
// Inventory Controller has a step to take, other than a RemoveHost that
// battery refuses; that is what makes the lag bounded.
//
// Model citations use //@= and //@# (docs/requirements/README.md, "Model
// citations"). They record what is modelled; they never count as an
// implementation or a test.
module pools {
  import types.* from "./types"

  // ---------------------------------------------------------------------
  // Parameters

  // The Nodes that can be Hosts.
  pure val HOSTS: Set[HostName] = Set("h1", "h2", "h3")
  // The placeholder Host an Operator before v0.5 gave battery when no Node
  // was a Host. No Node has its name (04-inventory.md, Upgrading).
  pure val PLACEHOLDER: HostName = "_no-hosts"
  // Every name battery can hold.
  pure val NAMES: Set[HostName] = HOSTS.union(Set(PLACEHOLDER))
  pure val POOLS: Set[PoolName] = Set("p1", "p2")

  // The configured settle time (IN-011), in ticks.
  pure val SETTLE: Time = 2

  pure val ZONE_A: Label = ("zone", "a")
  pure val ZONE_B: Label = ("zone", "b")
  pure val GPU: Label = ("gpu", "yes")
  pure val LABEL_SETS: Set[Set[Label]] = Set(Set(ZONE_A), Set(ZONE_B), Set(ZONE_A, GPU))
  // An empty matchLabels matches every Node.
  pure val SELECTORS: Set[Set[Label]] = Set(Set(), Set(ZONE_A), Set(ZONE_B), Set(GPU))
  pure val SIZES: Set[int] = Set(0, 1, 2)
  pure val EXPIRY: Time = 3
  // Heartbeat expiry thresholds a Pool's spec may have: battery refuses 0.
  pure val EXPIRIES: Set[Time] = Set(EXPIRY, 0)

  // The Inventory Controller's record of the certificate it last gave
  // every Host, before it has given one since it started.
  pure val NO_CERT: int = -1

  // ---------------------------------------------------------------------
  // State

  // A MicroVM battery has created, reduced to where it runs.
  // leased: a claim holds it; otherwise it is available.
  type PlacedVM = { id: int, pool: PoolName, host: HostName, leased: bool }

  // A Host battery holds (BA-086): whether it is cordoned, whether it has
  // the TLS settings of IN-004 (a Host from a battery v0.4.0 database has
  // none, and battery cannot connect to it), and the client certificate
  // battery read when it last connected to it (BA-081).
  type BatteryHost = { cordoned: bool, tls: bool, cert: int }

  type BatteryState = {
    hosts: HostName -> BatteryHost,
    pools: PoolName -> BatteryPool,
    vms: Set[PlacedVM],
    nextVM: int,
  }

  type InventoryState = {
    // the Hosts published to the Pool Controller (the HostSet, IN-018)
    published: Set[HostName],
    // the client certificate the Inventory Controller last gave every
    // Host with UpdateHost (DP-035); NO_CERT since the Operator started
    cert: int,
    // when battery last refused a RemoveHost: the Inventory Controller
    // tries again only after time has passed (a requeue)
    refusedAt: Time,
  }

  // battery's client certificate's Secret: its certificate, numbered from
  // 0 by renewal, and the one the kubelet has put in the Secret's volume,
  // which battery's container and the Operator's mount.
  type SecretState = { cert: int, mounted: int }

  // A MicroVM's creation, and whether its Host's Node was a Host then.
  type Birth = { host: HostName, at: Time, nodeIsHost: bool, age: Time }

  // History, for the invariants and witnesses only; nothing reads it to
  // decide a step.
  type History = {
    births: Set[Birth],
    // changes that flipped back before they settled
    absorbedFlaps: int,
    // Pools whose finalizer the Pool Controller removed, since the Pool of
    // that name was last created
    finalized: Set[PoolName],
    // UpdatePool calls that changed a Pool's flintlock_hosts
    placementUpdates: int,
    // provisioning attempts battery made on a Host it cannot connect to
    unreachablePicks: int,
    // available MicroVMs battery deleted with their Pool in DeletePool
    // (BA-070)
    drainedVMs: int,
    // MicroVMs claims leased, and leased MicroVMs released or expired
    leases: int,
    releases: int,
    // the Inventory Controller's HostAdmin calls
    adds: int,
    tlsUpdates: int,
    uncordons: int,
    cordons: int,
    removals: int,
    // RemoveHost calls battery refused (BA-083)
    removeRefusals: int,
    // the placeholder Host removed
    placeholderRemoved: bool,
    // UpdateHost on every Host for a certificate (DP-035), and those for a
    // renewal rather than a start
    refreshes: int,
    renewalRefreshes: int,
  }

  var now: Time
  var nodes: HostName -> Node
  // the Nodes that exist
  var present: Set[HostName]
  // when each Node last changed whether it is a Host. The Inventory
  // Controller sees every change (a watch), so it knows how long the
  // current state has held.
  var changedAt: HostName -> Time
  var pools: PoolName -> PoolSlot
  var battery: BatteryState
  var inv: InventoryState
  var hist: History
  var secret: SecretState

  // ---------------------------------------------------------------------
  // Views

  //@= docs/requirements/04-inventory.md#admission
  //@# The Inventory Controller SHALL treat a Node as a Host only while
  //@# the Node's Node report says the Host is ready and the Node is schedulable.
  pure def nodeIsHost(ns: HostName -> Node, pr: Set[HostName], h: HostName): bool =
    pr.contains(h) and ns.get(h).schedulable and ns.get(h).report.ready

  def isHost(h: HostName): bool = nodeIsHost(nodes, present, h)

  // How long the Node's current state has held.
  def age(h: HostName): Time = now - changedAt.get(h)

  //@= docs/requirements/04-inventory.md#applying
  //@# The Inventory Controller SHALL act on a change in whether a Node
  //@# is a Host only after the change has held for the configured settle time.
  def settled(h: HostName): bool = age(h) >= SETTLE

  def held(h: HostName): bool = battery.hosts.keys().contains(h)
  def heldHosts: Set[HostName] = battery.hosts.keys()
  def bhost(h: HostName): BatteryHost = battery.hosts.get(h)
  // battery holds the Host uncordoned: it may place on it.
  def active(h: HostName): bool = held(h) and not(bhost(h).cordoned)

  // Whether the Inventory Controller wants battery to place on the Host:
  // the Node's state once it has settled, and battery's until then.
  def desired(h: HostName): bool = if (settled(h)) isHost(h) else active(h)

  def slot(p: PoolName): PoolSlot = pools.get(p)
  def obj(p: PoolName): Pool = poolIn(pools.get(p))
  def live(p: PoolName): bool = isLivePool(pools.get(p))
  def inBattery(p: PoolName): bool = battery.pools.keys().contains(p)

  pure def matches(selector: Set[Label], labels: Set[Label]): bool = selector.subseteq(labels)

  // The Hosts a Pool's selector matches: the Hosts published to the Pool
  // Controller (IN-018) whose Nodes exist and match.
  def resolve(p: PoolName): Set[HostName] =
    inv.published.filter(h => present.contains(h) and matches(obj(p).spec.nodeSelector, nodes.get(h).labels))

  //@= docs/requirements/03-pools.md#placement
  //@# While a Pool's selector matches no Host, the Pool Controller
  //@# SHALL set the Pool's condition `Ready` false with the reason
  //@# `NoEligibleHost`.
  //
  // Otherwise Ready depends on battery's counts (PO-022), which this model
  // does not have; it leaves the condition Unknown.
  pure def readyFor(hosts: Set[HostName]): Condition =
    if (hosts == Set()) { status: CondFalse, reason: NoEligibleHost } else unknownCondition

  // Ready for a spec battery refuses (PO-004).
  pure val rejectedCondition: Condition = { status: CondFalse, reason: Rejected }

  // The Pool as the Pool Controller declares it to battery.
  def declared(p: PoolName): BatteryPool = {
    name: p,
    size: obj(p).spec.size,
    heartbeatExpiryThreshold: obj(p).spec.heartbeatExpiryThreshold,
    flintlockHosts: resolve(p),
  }

  //@= docs/requirements/10-battery.md#hosts
  //@# If `CreatePool` or `UpdatePool` names in `flintlock_hosts` a
  //@# Host battery does not hold, then battery SHALL refuse the call with
  //@# `INVALID_ARGUMENT`.
  //
  // battery refuses the Pool's spec: CreatePool and UpdatePool answer
  // INVALID_ARGUMENT and change nothing in battery. IN-018 keeps the
  // second cause from arising (poolsNameHeldHosts).
  def refused(p: PoolName): bool = or {
    obj(p).spec.heartbeatExpiryThreshold <= 0,
    not(resolve(p).subseteq(heldHosts)),
  }

  def vmCount(p: PoolName, h: HostName): int =
    battery.vms.filter(v => v.pool == p and v.host == h).size()

  //@= docs/requirements/10-battery.md#hosts
  //@# battery SHALL place no new MicroVM on a cordoned Host, and
  //@# SHALL leave the MicroVMs already on it as they are.
  //
  // battery's ineligibleHosts: a Host in flintlock_hosts that battery does
  // not hold, or holds cordoned, is skipped.
  def eligible(p: PoolName): Set[HostName] =
    battery.pools.get(p).flintlockHosts.filter(h => active(h))

  // battery's PickHost: the eligible Host with the fewest of the Pool's
  // MicroVMs. Ties go by the order of flintlock_hosts, which the model
  // does not keep, so any of the least loaded.
  def leastLoaded(p: PoolName): Set[HostName] =
    val el = eligible(p)
    el.filter(h => el.forall(h2 => vmCount(p, h) <= vmCount(p, h2)))

  //@= docs/requirements/10-battery.md#delete-pool
  //@# While a Pool's replenishment strategy is `MIN_SIZE_THRESHOLD`
  //@# and its size is 0, battery SHALL provision no MicroVM for it.
  //
  // battery tops a Pool up to its size; the model has no strategies, and a
  // Pool of size 0, drained or not, gets no MicroVM.
  def canProvision(p: PoolName): bool = and {
    inBattery(p),
    battery.vms.filter(v => v.pool == p).size() < battery.pools.get(p).size,
    eligible(p) != Set(),
  }

  // ---------------------------------------------------------------------
  // Init

  pure val initialLabels: HostName -> Set[Label] =
    Map("h1" -> Set(ZONE_A), "h2" -> Set(ZONE_A, GPU), "h3" -> Set(ZONE_B), PLACEHOLDER -> Set())

  //@= docs/requirements/10-battery.md#hosts
  //@# When battery starts on a database that battery v0.4.0 wrote,
  //@# battery SHALL keep the Hosts in it, with no TLS settings, and SHALL read no
  //@# Host from its configuration file.
  //
  // The Operator has just started. battery holds every Host, either as the
  // Operator gave it them (upgraded false) or as battery v0.4.0 left them
  // in its database: no TLS settings, and the placeholder Host too
  // (upgraded true). The HostSet holds the Hosts battery places on.
  action initWith(upgraded: bool): bool = all {
    now' = 0,
    secret' = { cert: 0, mounted: 0 },
    nodes' = NAMES.mapBy(h => {
      name: h,
      labels: initialLabels.get(h),
      schedulable: true,
      report: { ready: true, agentAddress: Some(h), flintlockdAddress: Some(h) },
    }),
    present' = HOSTS,
    changedAt' = NAMES.mapBy(_ => 0),
    pools' = POOLS.mapBy(_ => PoolUnborn),
    battery' = {
      hosts: (if (upgraded) NAMES else HOSTS).mapBy(_ => { cordoned: false, tls: not(upgraded), cert: 0 }),
      pools: Map(),
      vms: Set(),
      nextVM: 1,
    },
    inv' = { published: HOSTS, cert: NO_CERT, refusedAt: -1 },
    hist' = {
      births: Set(),
      absorbedFlaps: 0,
      finalized: Set(),
      placementUpdates: 0,
      unreachablePicks: 0,
      drainedVMs: 0,
      leases: 0,
      releases: 0,
      adds: 0,
      tlsUpdates: 0,
      uncordons: 0,
      cordons: 0,
      removals: 0,
      removeRefusals: 0,
      placeholderRemoved: false,
      refreshes: 0,
      renewalRefreshes: 0,
    },
  }

  action init = {
    nondet upgraded = Set(true, false).oneOf()
    initWith(upgraded)
  }

  // ---------------------------------------------------------------------
  // Environment: Nodes

  // A Node changes. If that changes whether it is a Host, the change
  // starts to settle now; a change that returns the Node to what battery
  // already has before it settled is a flap absorbed.
  action changeNode(h: HostName, n: Node, isPresent: bool): bool = {
    val pr = if (isPresent) present.union(Set(h)) else present.exclude(Set(h))
    val ns = nodes.set(h, n)
    val was = isHost(h)
    val becomes = nodeIsHost(ns, pr, h)
    all {
      nodes' = ns,
      present' = pr,
      changedAt' = if (was != becomes) changedAt.set(h, now) else changedAt,
      hist' = if (was != becomes and becomes == active(h) and not(settled(h)))
        { ...hist, absorbedFlaps: hist.absorbedFlaps + 1 } else hist,
      now' = now, secret' = secret, pools' = pools, battery' = battery, inv' = inv,
    }
  }

  action cordon(h: HostName): bool = all {
    present.contains(h), nodes.get(h).schedulable,
    changeNode(h, { ...nodes.get(h), schedulable: false }, true),
  }

  action uncordon(h: HostName): bool = all {
    present.contains(h), not(nodes.get(h).schedulable),
    changeNode(h, { ...nodes.get(h), schedulable: true }, true),
  }

  // The Exec Agent's Node report flips between ready and not ready.
  action flipReport(h: HostName): bool = all {
    present.contains(h),
    val r = nodes.get(h).report
    changeNode(h, { ...nodes.get(h), report: { ...r, ready: not(r.ready) } }, true),
  }

  action relabel(h: HostName, labels: Set[Label]): bool = all {
    present.contains(h), nodes.get(h).labels != labels,
    changeNode(h, { ...nodes.get(h), labels: labels }, true),
  }

  action deleteNode(h: HostName): bool = all {
    present.contains(h),
    changeNode(h, nodes.get(h), false),
  }

  // A Node joins, schedulable, with labels; its Exec Agent may not have
  // reported ready yet.
  action createNode(h: HostName, labels: Set[Label], ready: bool): bool = all {
    not(present.contains(h)),
    changeNode(h, {
      name: h,
      labels: labels,
      schedulable: true,
      report: { ready: ready, agentAddress: Some(h), flintlockdAddress: Some(h) },
    }, true),
  }

  // ---------------------------------------------------------------------
  // Environment: Pools, certificates, the Operator and time

  action createPool(p: PoolName, sel: Set[Label], n: int): bool = all {
    not(live(p)),
    pools' = pools.set(p, PoolLive(newPool(p, { size: n, heartbeatExpiryThreshold: EXPIRY, nodeSelector: sel }))),
    hist' = { ...hist, finalized: hist.finalized.exclude(Set(p)) },
    now' = now, secret' = secret, nodes' = nodes, present' = present, changedAt' = changedAt, battery' = battery, inv' = inv,
  }

  // A change to the spec bumps metadata.generation.
  action changePool(p: PoolName, sel: Set[Label], n: int): bool = all {
    live(p), not(obj(p).deleting),
    obj(p).spec.nodeSelector != sel or obj(p).spec.size != n,
    pools' = pools.set(p, PoolLive({
      ...obj(p),
      generation: obj(p).generation + 1,
      spec: { ...obj(p).spec, size: n, nodeSelector: sel },
    })),
    now' = now, secret' = secret, nodes' = nodes, present' = present, changedAt' = changedAt, battery' = battery, inv' = inv, hist' = hist,
  }

  // A change to the heartbeat expiry threshold: to 0, a spec that passed
  // admission and that battery refuses (PO-004), or back.
  action changePoolExpiry(p: PoolName, t: Time): bool = all {
    live(p), not(obj(p).deleting),
    obj(p).spec.heartbeatExpiryThreshold != t,
    pools' = pools.set(p, PoolLive({
      ...obj(p),
      generation: obj(p).generation + 1,
      spec: { ...obj(p).spec, heartbeatExpiryThreshold: t },
    })),
    now' = now, secret' = secret, nodes' = nodes, present' = present, changedAt' = changedAt, battery' = battery, inv' = inv, hist' = hist,
  }

  // With a finalizer the API server only marks the Pool; without one it is
  // gone at once.
  action deletePool(p: PoolName): bool = all {
    live(p), not(obj(p).deleting),
    pools' = pools.set(p,
      if (obj(p).hasFinalizer) PoolLive({ ...obj(p), deleting: true }) else PoolGone(obj(p))),
    now' = now, secret' = secret, nodes' = nodes, present' = present, changedAt' = changedAt, battery' = battery, inv' = inv, hist' = hist,
  }

  // cert-manager renews battery's client certificate.
  action renewCertificate: bool = all {
    secret' = { ...secret, cert: secret.cert + 1 },
    now' = now, nodes' = nodes, present' = present, changedAt' = changedAt, pools' = pools,
    battery' = battery, inv' = inv, hist' = hist,
  }

  // Some time later, the kubelet puts it in the Secret's volume.
  action mountCertificate: bool = all {
    secret.mounted != secret.cert,
    secret' = { ...secret, mounted: secret.cert },
    now' = now, nodes' = nodes, present' = present, changedAt' = changedAt, pools' = pools,
    battery' = battery, inv' = inv, hist' = hist,
  }

  // The Operator's container restarts: the Inventory Controller no longer
  // knows which certificate battery last read. The model keeps the
  // HostSet, which the first reconcile publishes again.
  action restartOperator: bool = all {
    inv.cert != NO_CERT,
    inv' = { ...inv, cert: NO_CERT },
    now' = now, secret' = secret, nodes' = nodes, present' = present, changedAt' = changedAt, pools' = pools,
    battery' = battery, hist' = hist,
  }

  // Time passes, once the Inventory Controller has caught up.
  action tick: bool = all {
    not(inventoryPending),
    now' = now + 1, secret' = secret,
    nodes' = nodes, present' = present, changedAt' = changedAt, pools' = pools,
    battery' = battery, inv' = inv, hist' = hist,
  }

  // ---------------------------------------------------------------------
  // battery

  // battery replenishes a Pool on the least loaded eligible Host in its
  // flintlock_hosts, which it can connect to.
  action batteryProvision(p: PoolName, h: HostName): bool = all {
    canProvision(p),
    leastLoaded(p).contains(h),
    bhost(h).tls,
    battery' = {
      ...battery,
      vms: battery.vms.union(Set({ id: battery.nextVM, pool: p, host: h, leased: false })),
      nextVM: battery.nextVM + 1,
    },
    hist' = { ...hist, births: hist.births.union(Set({ host: h, at: now, nodeIsHost: isHost(h), age: age(h) })) },
    now' = now, secret' = secret, nodes' = nodes, present' = present, changedAt' = changedAt, pools' = pools, inv' = inv,
  }

  // PickHost chose a Host battery holds without TLS settings, as a battery
  // v0.4.0 database left it: battery skipped it when it started, and
  // Provision fails with nothing created (BA-086).
  action batteryProvisionUnreachable(p: PoolName, h: HostName): bool = all {
    canProvision(p),
    leastLoaded(p).contains(h),
    not(bhost(h).tls),
    hist' = { ...hist, unreachablePicks: hist.unreachablePicks + 1 },
    now' = now, secret' = secret, nodes' = nodes, present' = present, changedAt' = changedAt, pools' = pools,
    battery' = battery, inv' = inv,
  }

  //@= docs/requirements/02-claims.md#binding
  //@# If the Pool of a claim that has no lease id in its status has
  //@# a deletion timestamp, then the Claim Controller SHALL NOT call battery's
  //@# `ClaimVM` for the claim, SHALL keep the claim in the phase `Pending` with
  //@# the condition `Bound` false and the reason `PoolDeleting`, and SHALL
  //@# retry with backoff.
  //
  // A claim leases an available MicroVM of the Pool on the Host, and only
  // of a Pool that is not being deleted. The model takes the Claim
  // Controller's read of the Pool and its ClaimVM as one step, so it
  // leaves out a deletion that races the ClaimVM.
  action leaseVM(p: PoolName, h: HostName): bool = all {
    inBattery(p),
    not(live(p) and obj(p).deleting),
    val free = battery.vms.filter(v => v.pool == p and v.host == h and not(v.leased))
    all {
      free != Set(),
      val oldest = free.fold(battery.nextVM, (m, v) => if (v.id < m) v.id else m)
      battery' = { ...battery, vms: battery.vms.map(v => if (v.id == oldest) { ...v, leased: true } else v) },
    },
    now' = now, secret' = secret, nodes' = nodes, present' = present, changedAt' = changedAt, pools' = pools, inv' = inv,
    hist' = { ...hist, leases: hist.leases + 1 },
  }

  // A leased MicroVM of the Pool on the Host is released, or its Lease
  // expires, and battery deletes it.
  action releaseVM(p: PoolName, h: HostName): bool = all {
    val heldVMs = battery.vms.filter(v => v.pool == p and v.host == h and v.leased)
    all {
      heldVMs != Set(),
      val newest = heldVMs.fold(0, (m, v) => if (v.id > m) v.id else m)
      battery' = { ...battery, vms: battery.vms.filter(v => v.id != newest) },
    },
    now' = now, secret' = secret, nodes' = nodes, present' = present, changedAt' = changedAt, pools' = pools, inv' = inv,
    hist' = { ...hist, releases: hist.releases + 1 },
  }

  // ---------------------------------------------------------------------
  // The Inventory Controller

  // The Host is not as IN-017 wants it: battery does not hold it, holds it
  // cordoned or without TLS settings, or the Pool Controller has not been
  // given it.
  def canRegister(h: HostName): bool = and {
    isHost(h),
    desired(h),
    not(active(h) and bhost(h).tls and inv.published.contains(h)),
  }

  //@= docs/requirements/04-inventory.md#applying
  //@# While a Node is a Host, the Inventory Controller SHALL keep a
  //@# Host of the Node's name in battery, uncordoned, at the Node's `flintlockd`
  //@# address and with the TLS settings of IN-004, adding it with `AddHost`,
  //@# and changing its address or TLS settings with `UpdateHost` and uncordoning
  //@# it with `UncordonHost` wherever battery's Host differs.
  //
  //@= docs/requirements/04-inventory.md#applying
  //@# If `AddHost` answers that battery already holds a Host of that
  //@# name, then the Inventory Controller SHALL read the Host with `GetHost` and
  //@# bring it in step as IN-017 says, without failing the reconcile.
  //
  //@= docs/requirements/10-battery.md#hosts
  //@# When `AddHost` names a Host battery does not hold, battery
  //@# SHALL store the Host uncordoned, with the address and TLS settings the
  //@# request gives
  //
  //@= docs/requirements/10-battery.md#hosts
  //@# battery SHALL read a Host's client certificate, key and
  //@# certificate authority each time it connects to the Host, which it does
  //@# when it starts and in `AddHost` and `UpdateHost`
  //
  // One reconcile: AddHost for a Host battery does not hold; for one it
  // holds (listed, or found by AddHost's ALREADY_EXISTS, which the model
  // does not tell apart), UpdateHost where it has no TLS settings and
  // UncordonHost where it is cordoned. battery reads the certificate the
  // Secret's volume holds when it connects. Then the Host is published.
  action invRegister(h: HostName): bool = all {
    canRegister(h),
    val added = not(held(h))
    val updated = held(h) and not(bhost(h).tls)
    val uncordoned = held(h) and bhost(h).cordoned
    val connected = added or updated
    all {
      battery' = { ...battery, hosts: battery.hosts.put(h, {
        cordoned: false,
        tls: true,
        cert: if (connected) secret.mounted else bhost(h).cert,
      }) },
      inv' = { ...inv, published: inv.published.union(Set(h)) },
      hist' = { ...hist,
        adds: if (added) hist.adds + 1 else hist.adds,
        tlsUpdates: if (updated) hist.tlsUpdates + 1 else hist.tlsUpdates,
        uncordons: if (uncordoned) hist.uncordons + 1 else hist.uncordons,
      },
    },
    now' = now, secret' = secret, nodes' = nodes, present' = present, changedAt' = changedAt, pools' = pools,
  }

  def canUnpublish(h: HostName): bool = inv.published.contains(h) and not(desired(h))

  //@= docs/requirements/04-inventory.md#applying
  //@# The Inventory Controller SHALL give the Pool Controller a Host
  //@# only once battery holds it uncordoned at the Node's address, and SHALL
  //@# take a Host from the Pool Controller before it cordons or removes the Host.
  //
  // A Host the Inventory Controller is about to cordon or remove leaves
  // the HostSet first, so the Pool Controller drops it from the Pools.
  action invUnpublish(h: HostName): bool = all {
    canUnpublish(h),
    inv' = { ...inv, published: inv.published.exclude(Set(h)) },
    now' = now, secret' = secret, nodes' = nodes, present' = present, changedAt' = changedAt, pools' = pools,
    battery' = battery, hist' = hist,
  }

  def canCordon(h: HostName): bool = and {
    active(h), not(desired(h)), not(inv.published.contains(h)),
  }

  //@= docs/requirements/04-inventory.md#admission
  //@# When a Node that is a Host is cordoned, or its Node report
  //@# says the Host is not ready or gives no `flintlockd` address, the
  //@# Inventory Controller SHALL cordon the Host in battery with `CordonHost`,
  //@# and SHALL keep it in battery's Hosts.
  //
  // The change has settled: battery places nothing new on the Host, and
  // keeps it and every MicroVM on it. A Host whose Node is gone is
  // cordoned too, before RemoveHost (IN-016).
  action invCordon(h: HostName): bool = all {
    canCordon(h),
    battery' = { ...battery, hosts: battery.hosts.set(h, { ...bhost(h), cordoned: true }) },
    hist' = { ...hist, cordons: hist.cordons + 1 },
    now' = now, secret' = secret, nodes' = nodes, present' = present, changedAt' = changedAt, pools' = pools, inv' = inv,
  }

  // The Inventory Controller would remove the Host: battery holds it
  // cordoned, its Node is gone, and the Pool Controller no longer has it.
  def wantsRemoved(h: HostName): bool = and {
    held(h), bhost(h).cordoned, not(present.contains(h)), not(desired(h)), not(inv.published.contains(h)),
  }

  //@= docs/requirements/10-battery.md#hosts
  //@# If `RemoveHost` names a Host that a Pool names in its
  //@# `flintlock_hosts`, or that has a MicroVM in any phase or a placement in
  //@# flight, then battery SHALL refuse the call with `FAILED_PRECONDITION`,
  //@# and SHALL keep the Host.
  def removable(h: HostName): bool = and {
    battery.pools.keys().forall(p => not(battery.pools.get(p).flintlockHosts.contains(h))),
    battery.vms.forall(v => v.host != h),
  }

  //@= docs/requirements/04-inventory.md#admission
  //@# When battery holds a Host whose Node does not exist or is being
  //@# deleted, the Inventory Controller SHALL cordon the Host and remove it with
  //@# `RemoveHost`, and SHALL try again while battery refuses because a Pool
  //@# names the Host or a MicroVM is still on it.
  action invRemove(h: HostName): bool = all {
    wantsRemoved(h),
    removable(h),
    battery' = { ...battery, hosts: battery.hosts.keys().exclude(Set(h)).mapBy(k => bhost(k)) },
    hist' = { ...hist,
      removals: hist.removals + 1,
      placeholderRemoved: hist.placeholderRemoved or h == PLACEHOLDER,
    },
    now' = now, secret' = secret, nodes' = nodes, present' = present, changedAt' = changedAt, pools' = pools, inv' = inv,
  }

  // battery refuses RemoveHost (BA-083), and keeps the Host; the
  // Inventory Controller tries again after a requeue, once time has
  // passed. This is the one step of the Inventory Controller that time
  // may pass.
  action invRemoveRefused(h: HostName): bool = all {
    wantsRemoved(h),
    not(removable(h)),
    inv.refusedAt < now,
    hist' = { ...hist, removeRefusals: hist.removeRefusals + 1 },
    inv' = { ...inv, refusedAt: now },
    now' = now, secret' = secret, nodes' = nodes, present' = present, changedAt' = changedAt, pools' = pools,
    battery' = battery,
  }

  def canSecure(h: HostName): bool = held(h) and not(bhost(h).tls)

  //@= docs/requirements/04-inventory.md#upgrade
  //@# When battery holds a Host whose TLS settings are not those of
  //@# IN-004, the Inventory Controller SHALL update the Host with `UpdateHost` to
  //@# those settings, keeping its address.
  //
  // A Host battery v0.4.0 left without TLS settings (BA-086), whatever its
  // Node: battery connects to it again, reading the certificate, and keeps
  // its cordon state (BA-089).
  action invSecure(h: HostName): bool = all {
    canSecure(h),
    battery' = { ...battery, hosts: battery.hosts.set(h, { ...bhost(h), tls: true, cert: secret.mounted }) },
    hist' = { ...hist, tlsUpdates: hist.tlsUpdates + 1 },
    now' = now, secret' = secret, nodes' = nodes, present' = present, changedAt' = changedAt, pools' = pools, inv' = inv,
  }

  def canRefresh: bool = and {
    inv.cert != secret.cert,
    secret.mounted == secret.cert,
  }

  //@= docs/requirements/06-deployment.md#battery-sidecar
  //@# When the client certificate in the Secret of DP-005 changes,
  //@# and once after the Operator starts, the Operator SHALL call `UpdateHost`
  //@# for every Host battery holds, with the Host's address and TLS settings
  //@# unchanged, once the Operator's own mount of that Secret holds the
  //@# certificate the Secret holds.
  //
  //@= docs/requirements/10-battery.md#hosts
  //@# battery SHALL keep a Host's cordon state across `UpdateHost`
  //
  // battery connects to each Host again and reads the certificate the
  // volume now holds. A Host without TLS settings (BA-086) gains none from
  // an UpdateHost that leaves them unchanged; IN-017 and IN-020 give them.
  action invRefresh: bool = all {
    canRefresh,
    battery' = { ...battery, hosts: heldHosts.mapBy(h =>
      if (bhost(h).tls) { ...bhost(h), cert: secret.mounted } else bhost(h)) },
    inv' = { ...inv, cert: secret.cert },
    hist' = { ...hist,
      refreshes: hist.refreshes + 1,
      renewalRefreshes: if (inv.cert != NO_CERT) hist.renewalRefreshes + 1 else hist.renewalRefreshes,
    },
    now' = now, secret' = secret, nodes' = nodes, present' = present, changedAt' = changedAt, pools' = pools,
  }

  // The Inventory Controller has a step to take that battery will not
  // refuse: time does not pass until it has taken it.
  def inventoryPending: bool = or {
    canRefresh,
    NAMES.exists(h => or {
      canRegister(h),
      canUnpublish(h),
      canCordon(h),
      canSecure(h),
      wantsRemoved(h) and removable(h),
    }),
  }

  // ---------------------------------------------------------------------
  // The Pool Controller

  def setPool(p: PoolName, o: Pool): PoolName -> PoolSlot = pools.set(p, PoolLive(o))

  //@= docs/requirements/03-pools.md#declaration
  //@# The Pool Controller SHALL add a finalizer to each Pool, and when
  //@# the Pool is deleted SHALL call `DeletePool` until battery accepts it
  //@# (PO-042, PO-043), and remove the finalizer only once battery has
  //@# deleted the Pool or reported it unknown.
  def canAddFinalizer(p: PoolName): bool = and {
    live(p), not(obj(p).deleting), not(obj(p).hasFinalizer),
  }

  action ctlAddFinalizer(p: PoolName): bool = all {
    canAddFinalizer(p),
    pools' = setPool(p, { ...obj(p), hasFinalizer: true }),
    now' = now, secret' = secret, nodes' = nodes, present' = present, changedAt' = changedAt, battery' = battery, inv' = inv, hist' = hist,
  }

  //@= docs/requirements/03-pools.md#declaration
  //@# When a Pool exists that battery does not hold, the Pool
  //@# Controller SHALL add the finalizer `battery.liquidmetal-x.dev/pool` to the
  //@# Pool before it creates it in battery with `CreatePool`, under the Pool's
  //@# namespace and name.
  //
  // The finalizer is stored: ctlAddFinalizer is the API server's write.
  def canCreate(p: PoolName): bool = and {
    live(p), not(obj(p).deleting), obj(p).hasFinalizer, not(inBattery(p)), not(refused(p)),
  }

  action ctlCreatePool(p: PoolName): bool = all {
    canCreate(p),
    battery' = { ...battery, pools: battery.pools.put(p, declared(p)) },
    pools' = setPool(p, { ...obj(p), status: { ...obj(p).status, observedGeneration: obj(p).generation, ready: readyFor(resolve(p)) } }),
    now' = now, secret' = secret, nodes' = nodes, present' = present, changedAt' = changedAt, inv' = inv, hist' = hist,
  }

  //@= docs/requirements/03-pools.md#declaration
  //@# When a Pool's `metadata.generation` differs from its
  //@# `status.observedGeneration`, the Pool Controller SHALL send the Pool's spec
  //@# to battery with `UpdatePool` and then set `status.observedGeneration` to
  //@# that generation.
  def canUpdate(p: PoolName): bool = and {
    live(p), not(obj(p).deleting), inBattery(p), not(refused(p)),
    or {
      obj(p).generation != obj(p).status.observedGeneration,
      hostsChanged(p),
    },
  }

  //@= docs/requirements/03-pools.md#placement
  //@# When the set of Hosts a Pool's selector matches changes, the
  //@# Pool Controller SHALL update the Pool in battery with `UpdatePool`.
  def hostsChanged(p: PoolName): bool = battery.pools.get(p).flintlockHosts != resolve(p)

  //@= docs/requirements/03-pools.md#placement
  //@# The Pool Controller SHALL set a Pool's `flintlock_hosts` in
  //@# battery to the names of the Hosts whose Nodes match the Pool's
  //@# `spec.placement.nodeSelector`.
  action ctlUpdatePool(p: PoolName): bool = all {
    canUpdate(p),
    battery' = { ...battery, pools: battery.pools.set(p, declared(p)) },
    pools' = setPool(p, { ...obj(p), status: { ...obj(p).status, observedGeneration: obj(p).generation, ready: readyFor(resolve(p)) } }),
    hist' = { ...hist,
      placementUpdates: if (hostsChanged(p)) hist.placementUpdates + 1 else hist.placementUpdates,
    },
    now' = now, secret' = secret, nodes' = nodes, present' = present, changedAt' = changedAt, inv' = inv,
  }

  //@= docs/requirements/03-pools.md#declaration
  //@# If battery refuses a Pool's spec, then the Pool Controller SHALL
  //@# set the Pool's condition `Ready` false with the reason `Rejected` and
  //@# battery's message.
  //
  // CreatePool or UpdatePool, refused, changes nothing in battery: a Pool
  // battery holds keeps the flintlock_hosts of the last spec it accepted,
  // and its observedGeneration stays. Rejected stands over NoEligibleHost
  // (03-pools.md, Placement). The Pool Controller calls again on each
  // reconcile, to the same answer; the model has no step for that.
  def canReject(p: PoolName): bool = and {
    live(p), not(obj(p).deleting), obj(p).hasFinalizer, refused(p),
    obj(p).status.ready != rejectedCondition,
  }

  action ctlRejectPool(p: PoolName): bool = all {
    canReject(p),
    pools' = setPool(p, { ...obj(p), status: { ...obj(p).status, ready: rejectedCondition } }),
    now' = now, secret' = secret, nodes' = nodes, present' = present, changedAt' = changedAt, battery' = battery, inv' = inv, hist' = hist,
  }

  def vmsOf(p: PoolName): Set[PlacedVM] = battery.vms.filter(v => v.pool == p)

  //@= docs/requirements/10-battery.md#delete-pool
  //@# If a Pool owns a MicroVM in the phase `LEASED` or
  //@# `PRE_LEASE_HOOK_RUNNING`, then battery SHALL answer a `DeletePool`
  //@# without `force` for it with `FAILED_PRECONDITION` and keep the Pool and
  //@# its MicroVMs.
  //
  // DeletePool succeeds for a Pool battery does not hold (NOT_FOUND, which
  // lets the finalizer go) or one that owns no leased MicroVM, which
  // battery deletes with its available ones; otherwise battery refuses it.
  def canDelete(p: PoolName): bool = and {
    live(p), obj(p).deleting, obj(p).hasFinalizer,
    not(inBattery(p)) or vmsOf(p).forall(v => not(v.leased)),
  }

  //@= docs/requirements/03-pools.md#deletion
  //@# The Pool Controller SHALL NOT claim or release a MicroVM of a
  //@# deleted Pool, and SHALL leave the deletion of the Pool's MicroVMs to
  //@# battery's `DeletePool`.
  //
  // DeletePool, then the finalizer. battery deletes the Pool's MicroVMs
  // with it; the Pool Controller has no step that takes one.
  action ctlDeletePool(p: PoolName): bool = all {
    canDelete(p),
    battery' = {
      ...battery,
      pools: battery.pools.keys().exclude(Set(p)).mapBy(q => battery.pools.get(q)),
      vms: battery.vms.filter(v => v.pool != p),
    },
    pools' = pools.set(p, PoolGone({ ...obj(p), hasFinalizer: false })),
    hist' = { ...hist,
      finalized: hist.finalized.union(Set(p)),
      drainedVMs: hist.drainedVMs + vmsOf(p).size(),
    },
    now' = now, secret' = secret, nodes' = nodes, present' = present, changedAt' = changedAt, inv' = inv,
  }

  // battery refused DeletePool: a claim holds a MicroVM of the deleted
  // Pool.
  def deletionRefused(p: PoolName): bool = and {
    live(p), obj(p).deleting, obj(p).hasFinalizer,
    inBattery(p), vmsOf(p).exists(v => v.leased),
  }

  //@= docs/requirements/03-pools.md#deletion
  //@# When battery refuses `DeletePool` for a deleted Pool and the
  //@# Pool's spec in battery is not its drained spec, the Pool Controller
  //@# SHALL send battery the Pool's drained spec with `UpdatePool`.
  //
  // The drained spec is a size of 0 here; the model has no replenishment
  // strategies, hooks or provisioning phase.
  def canDrainSpec(p: PoolName): bool = and {
    deletionRefused(p), battery.pools.get(p).size != 0,
  }

  action ctlDrainSpec(p: PoolName): bool = all {
    canDrainSpec(p),
    battery' = { ...battery, pools: battery.pools.set(p, { ...battery.pools.get(p), size: 0 }) },
    now' = now, secret' = secret, nodes' = nodes, present' = present, changedAt' = changedAt, pools' = pools, inv' = inv, hist' = hist,
  }

  // Whether the Pool Controller has any step it can take.
  def poolControllerIdle: bool =
    POOLS.forall(p => and {
      not(canAddFinalizer(p)),
      not(canCreate(p)),
      not(canUpdate(p)),
      not(canReject(p)),
      not(canDelete(p)),
      not(canDrainSpec(p)),
    })

  // ---------------------------------------------------------------------
  // Step

  // The step relation, in groups. step takes any of them; hack/quint.sh
  // also simulates stepPools and stepInventory, which leave out the
  // environment steps one side does not need, so that the rarer
  // interleavings of each side come up often enough to rely on (a
  // witness reached in a handful of 20000 traces fails an unseeded run
  // now and then).

  action nodeSteps = {
    nondet h = HOSTS.oneOf()
    nondet labels = LABEL_SETS.oneOf()
    nondet ready = Set(true, false).oneOf()
    any {
      cordon(h),
      uncordon(h),
      flipReport(h),
      relabel(h, labels),
      deleteNode(h),
      createNode(h, labels, ready),
    }
  }

  // Pools created and deleted, and claims; no spec changes.
  action poolLifeSteps = {
    nondet b = NAMES.oneOf()
    nondet p = POOLS.oneOf()
    nondet sel = SELECTORS.oneOf()
    nondet n = SIZES.oneOf()
    any {
      createPool(p, sel, n),
      deletePool(p),
      batteryProvision(p, b),
      batteryProvisionUnreachable(p, b),
      leaseVM(p, b),
      releaseVM(p, b),
    }
  }

  action poolSpecSteps = {
    nondet p = POOLS.oneOf()
    nondet sel = SELECTORS.oneOf()
    nondet n = SIZES.oneOf()
    nondet expiry = EXPIRIES.oneOf()
    any {
      changePool(p, sel, n),
      changePoolExpiry(p, expiry),
    }
  }

  // cert-manager and the kubelet. restartOperator is left to the scenario
  // tests: every trace starts with the Operator just started, and a step
  // that is nearly always enabled would crowd out the rarer
  // interleavings.
  action certificateSteps = any {
    renewCertificate,
    mountCertificate,
  }

  action inventorySteps = {
    nondet b = NAMES.oneOf()
    any {
      invRegister(b),
      invUnpublish(b),
      invCordon(b),
      invRemove(b),
      invRemoveRefused(b),
      invSecure(b),
      invRefresh,
    }
  }

  action poolControllerSteps = {
    nondet p = POOLS.oneOf()
    any {
      ctlAddFinalizer(p),
      ctlCreatePool(p),
      ctlUpdatePool(p),
      ctlRejectPool(p),
      ctlDeletePool(p),
      ctlDrainSpec(p),
    }
  }

  action step = any {
    nodeSteps,
    poolLifeSteps,
    poolSpecSteps,
    certificateSteps,
    tick,
    inventorySteps,
    poolControllerSteps,
  }

  // The deletion of Pools that claims hold: a fresh install whose Nodes
  // and Pool specs do not change, and no renewals.
  action initPools = initWith(false)
  action stepPoolDeletion = any {
    poolLifeSteps,
    tick,
    inventorySteps,
    poolControllerSteps,
  }


  // ---------------------------------------------------------------------
  // Invariants. Each names the requirement or ADR consequence it checks.

  // PO-010, PO-011, PO-002: once the Pool Controller has nothing left to
  // do, battery holds every live Pool whose spec it accepts with its
  // current spec, and its flintlock_hosts are exactly the Hosts published
  // to it that the Pool's selector matches.
  val poolHostsMatchSelector: bool =
    poolControllerIdle implies POOLS.forall(p => (live(p) and not(obj(p).deleting) and not(refused(p))) implies and {
      inBattery(p),
      battery.pools.get(p).flintlockHosts == inv.published.filter(h => and {
        present.contains(h),
        obj(p).spec.nodeSelector.subseteq(nodes.get(h).labels),
      }),
      battery.pools.get(p).size == obj(p).spec.size,
      obj(p).status.observedGeneration == obj(p).generation,
    })

  // PO-012: once the Pool Controller is idle, a live Pool whose spec
  // battery accepts says NoEligibleHost exactly when its selector matches
  // no Host.
  val noEligibleHostWhenNoneMatch: bool =
    poolControllerIdle implies POOLS.forall(p => (live(p) and not(obj(p).deleting) and not(refused(p))) implies
      ((obj(p).status.ready.reason == NoEligibleHost) iff (resolve(p) == Set())))

  // PO-004 over PO-012 (03-pools.md, Placement): once the Pool Controller
  // is idle, a live Pool whose spec battery refuses says Rejected, whether
  // or not its selector matches a Host.
  val rejectedStandsOverNoEligibleHost: bool =
    poolControllerIdle implies POOLS.forall(p => (live(p) and not(obj(p).deleting) and refused(p)) implies
      obj(p).status.ready == rejectedCondition)

  // PO-003: a Pool whose finalizer the Pool Controller removed is not in
  // battery.
  val finalizerRemovedOnlyAfterDelete: bool =
    POOLS.forall(p => (isGonePool(slot(p)) and hist.finalized.contains(p)) implies not(inBattery(p)))

  // IN-001, IN-011, IN-015, IN-016, IN-017: the Hosts battery places on
  // are the Nodes that are Hosts, except for a change younger than the
  // settle time.
  val hostsFollowNodes: bool =
    NAMES.forall(h => (active(h) != isHost(h)) implies age(h) <= SETTLE)

  // IN-015, BA-084: a cordoned, deleted or not-ready Host gets no new
  // MicroVM once its change has settled.
  val noNewVMOnFormerHost: bool =
    hist.births.forall(b => not(b.nodeIsHost) implies b.age <= SETTLE)

  // IN-018: the Pool Controller has only Hosts battery holds uncordoned.
  val publishedAreActive: bool =
    inv.published.forall(h => active(h))

  // IN-018, BA-082, BA-083 (#73): every Pool in battery names only Hosts
  // battery holds, so battery never refuses a Pool for a Host, and its
  // PickHost never picks a Host it does not hold. Unlike under battery
  // v0.4.0, with no exception.
  val poolsNameHeldHosts: bool =
    battery.pools.keys().forall(p => battery.pools.get(p).flintlockHosts.subseteq(heldHosts))

  // IN-015, IN-016, BA-083: battery holds the Host of every MicroVM, so a
  // Host leaves battery only once nothing is on it, and a Host whose Node
  // exists stays.
  val vmsOnHeldHosts: bool =
    battery.vms.forall(v => held(v.host))

  // IN-017, IN-020, BA-086: a Host battery holds without the TLS settings
  // of IN-004, as an upgrade leaves it, gets them before time passes, and
  // no step takes them away.
  val heldHostsGetTLS: bool =
    now > 0 implies heldHosts.forall(h => bhost(h).tls)

  // DP-035, BA-081: every Host battery can reach presents the certificate
  // the Secret holds, except between a renewal, or the Operator's start,
  // and the UpdateHost that follows once the volume holds it.
  val certificateFollowsSecret: bool =
    inv.cert == secret.cert implies heldHosts.forall(h => bhost(h).tls implies bhost(h).cert == secret.cert)

  //@= docs/requirements/03-pools.md#declaration
  //@# When a Pool exists that battery does not hold, the Pool
  //@# Controller SHALL add the finalizer `battery.liquidmetal-x.dev/pool` to the
  //@# Pool before it creates it in battery with `CreatePool`, under the Pool's
  //@# namespace and name.
  //
  // PO-001 with PO-003: a Pool gone from the API server is not in battery.
  // A Pool reaches battery only with its finalizer stored, so it leaves the
  // API server only through DeletePool (#72).
  val goneLeavesNothing: bool =
    POOLS.forall(p => isGonePool(slot(p)) implies not(inBattery(p)))

  // BA-070, PO-003 (#81): every MicroVM battery holds belongs to a Pool
  // battery holds. battery deletes a Pool's MicroVMs with it, so deleting
  // a Pool never orphans them.
  val vmsBelongToPools: bool =
    battery.vms.forall(v => inBattery(v.pool))

  // BA-070, PO-044: the deletion of a Pool never takes a MicroVM a claim
  // holds. battery refuses DeletePool while there is one, and the Pool
  // Controller takes none itself. A leased MicroVM leaves battery only
  // when it is released or its Lease expires, so battery holds exactly the
  // leased MicroVMs that were leased and not yet released.
  val drainLeavesLeases: bool =
    battery.vms.filter(v => v.leased).size() == hist.leases - hist.releases

  // All the invariants CI checks.
  val safety: bool = and {
    poolHostsMatchSelector,
    noEligibleHostWhenNoneMatch,
    rejectedStandsOverNoEligibleHost,
    finalizerRemovedOnlyAfterDelete,
    goneLeavesNothing,
    vmsBelongToPools,
    drainLeavesLeases,
    hostsFollowNodes,
    noNewVMOnFormerHost,
    publishedAreActive,
    poolsNameHeldHosts,
    vmsOnHeldHosts,
    heldHostsGetTLS,
    certificateFollowsSecret,
  }

  // ---------------------------------------------------------------------
  // Witnesses: states quint run reports reaching, to show the simulation
  // gets to the interesting interleavings.

  // a Node's change flipped back before it settled, and reached battery
  // not at all
  val witnessFlapAbsorbed: bool = hist.absorbedFlaps > 0
  // a Host was cordoned in battery while its Node exists
  val witnessCordoned: bool = NAMES.exists(h => present.contains(h) and held(h) and bhost(h).cordoned)
  // a cordoned Host was uncordoned
  val witnessUncordoned: bool = hist.uncordons > 0
  // a Host whose Node is gone was removed
  val witnessRemoved: bool = hist.removals > 0 and NAMES.exists(h => h != PLACEHOLDER and not(held(h)))
  // battery refused a RemoveHost while a Pool named the Host or a MicroVM
  // was on it (joiningHostTest and removalWaitsForThePoolsTest walk an
  // AddHost after a removal, and a removal after a refusal, which random
  // runs reach too seldom to require)
  val witnessRemovalRefused: bool = hist.removeRefusals > 0
  // a Host from a battery v0.4.0 database got its TLS settings
  val witnessUpgradedHost: bool = hist.tlsUpdates > 0
  // the placeholder Host was removed
  val witnessPlaceholderRemoved: bool = hist.placeholderRemoved
  // every Host got a renewed certificate
  val witnessRenewalRefreshed: bool = hist.renewalRefreshes > 0
  // a MicroVM was created on a Host whose Node had stopped being a Host,
  // before the change settled
  val witnessVMOnFormerHost: bool = hist.births.exists(b => not(b.nodeIsHost))
  // UpdatePool moved a Pool's flintlock_hosts
  val witnessPlacementUpdated: bool = hist.placementUpdates > 0
  // a live Pool's selector matches no Host, and it says so
  val witnessNoEligibleHost: bool =
    POOLS.exists(p => live(p) and obj(p).status.ready.reason == NoEligibleHost)
  // a Pool was deleted in battery under its finalizer
  val witnessPoolDeleted: bool = hist.finalized != Set()
  // battery deleted a Pool with an available MicroVM (BA-070)
  val witnessPoolDrained: bool = hist.drainedVMs > 0 and hist.finalized != Set()
  // a deleted Pool, with its drained spec, waits for a MicroVM a claim
  // holds while it keeps an available one (PO-042, BA-070)
  val witnessDeletionWaitsForClaim: bool =
    POOLS.exists(p => deletionRefused(p) and battery.pools.get(p).size == 0 and vmsOf(p).exists(v => not(v.leased)))
  // a live Pool whose selector matches no Host says Rejected, because
  // battery refuses its spec
  val witnessRejectedWithoutHost: bool =
    POOLS.exists(p => live(p) and obj(p).status.ready == rejectedCondition and resolve(p) == Set())
}
