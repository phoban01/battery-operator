// -*- mode: Bluespec; -*-
//
// Pools, placement and inventory: the Pool Controller and the Inventory
// Controller against battery.
//
// What it models (docs/requirements/03-pools.md, 04-inventory.md, ADR 0001
// consequence 1):
//
// - the Inventory Controller: decide which Nodes are Hosts (IN-001,
//   IN-002), let a change settle (IN-011), and apply the settled changes in
//   one restart of battery per restart window (IN-010, IN-012);
// - the Pool Controller: the finalizer, CreatePool, UpdatePool on a new
//   generation or a new set of matching Hosts, the NoEligibleHost
//   condition, and DeletePool under the finalizer (PO-001 to PO-003,
//   PO-010 to PO-012);
// - battery v0.3.3: it reads its Hosts from its configuration only when it
//   starts, and replenishes each Pool on the least loaded Host in the
//   Pool's flintlock_hosts (internal/reconciler/hostpick.go). It does not
//   check flintlock_hosts against its Hosts: CreatePool and UpdatePool take
//   any name, and provisioning on a Host it does not know fails
//   (ErrUnknownHost);
// - the environment: Nodes are labelled, cordoned, uncordoned, deleted and
//   created again, their Node reports flip between ready and not ready,
//   Pools are created, changed and deleted, leased MicroVMs are released,
//   and time passes.
//
// The Pool Controller resolves a selector against the Hosts battery is
// running with, which are the Hosts "the Inventory Controller has given
// battery" (03-pools.md, Placement), and against the Nodes that exist.
//
// Time: the controllers act promptly. Time does not pass while the
// Inventory Controller has a restart window to open or close, or while
// battery is restarting; that is what makes the lag bounded.
//
// Model citations use //@= and //@# (docs/requirements/README.md, "Model
// citations"). They record what is modelled; they never count as an
// implementation or a test.
module pools {
  import types.* from "./types"

  // ---------------------------------------------------------------------
  // Parameters

  pure val HOSTS: Set[HostName] = Set("h1", "h2", "h3")
  pure val POOLS: Set[PoolName] = Set("p1", "p2")

  // The configured settle time (IN-011) and restart window (IN-012), in
  // ticks. Different values, so that a model that confuses them fails.
  // The requirements do not define the restart window (#74); the model
  // opens one when the first settled change is waiting, and applies every
  // change settled by the time it closes.
  pure val SETTLE: Time = 2
  pure val WINDOW: Time = 3

  // The longest a Host can stay in battery after its Node stops being a
  // Host, or stay out after it becomes one: the change settles, then waits
  // at most one restart window.
  pure val MAX_LAG: Time = SETTLE + WINDOW

  pure val ZONE_A: Label = ("zone", "a")
  pure val ZONE_B: Label = ("zone", "b")
  pure val GPU: Label = ("gpu", "yes")
  pure val LABEL_SETS: Set[Set[Label]] = Set(Set(ZONE_A), Set(ZONE_B), Set(ZONE_A, GPU))
  // An empty matchLabels matches every Node.
  pure val SELECTORS: Set[Set[Label]] = Set(Set(), Set(ZONE_A), Set(ZONE_B), Set(GPU))
  pure val SIZES: Set[int] = Set(0, 1, 2)
  pure val EXPIRY: Time = 3

  // ---------------------------------------------------------------------
  // State

  // A MicroVM battery has created, reduced to where it runs.
  type PlacedVM = { id: int, pool: PoolName, host: HostName }

  type BatteryState = {
    up: bool,
    // the Hosts battery read from its configuration when it last started
    hosts: Set[HostName],
    pools: PoolName -> BatteryPool,
    vms: Set[PlacedVM],
    nextVM: int,
  }

  type InventoryState = {
    // battery's configuration file, as the Inventory Controller last wrote
    // it (IN-010). battery reads it on start.
    config: Set[HostName],
    // when the open restart window opened (IN-012)
    window: Option[Time],
  }

  // A restart of battery, and the age of each change it applied.
  type Restart = { at: Time, changed: Set[HostName], ages: Set[Time] }

  // A MicroVM's creation, and whether its Host's Node was a Host then.
  type Birth = { host: HostName, at: Time, nodeIsHost: bool, age: Time }

  // History, for the invariants and witnesses only; nothing reads it to
  // decide a step.
  type History = {
    restarts: List[Restart],
    births: Set[Birth],
    // changes that flipped back before they settled
    absorbedFlaps: int,
    // Pools whose finalizer the Pool Controller removed, since the Pool of
    // that name was last created
    finalized: Set[PoolName],
    // UpdatePool calls that changed a Pool's flintlock_hosts
    placementUpdates: int,
    // provisioning attempts battery made on a Host it does not know
    unknownHostPicks: int,
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

  // ---------------------------------------------------------------------
  // Views

  //@= docs/requirements/04-inventory.md#admission
  //@# The Inventory Controller SHALL treat a Node as a Host only while
  //@# the Node's Node report says the Host is ready and the Node is schedulable.
  pure def nodeIsHost(ns: HostName -> Node, pr: Set[HostName], h: HostName): bool = and {
    pr.contains(h),
    ns.get(h).schedulable,
    ns.get(h).report.ready,
  }

  def isHost(h: HostName): bool = nodeIsHost(nodes, present, h)

  // How long the Node's current state has held.
  def age(h: HostName): Time = now - changedAt.get(h)

  //@= docs/requirements/04-inventory.md#applying
  //@# The Inventory Controller SHALL act on a change in whether a Node
  //@# is a Host only after the change has held for the configured settle time.
  def settled(h: HostName): bool = age(h) >= SETTLE

  // The Hosts the Inventory Controller wants battery to have: each Node's
  // state once it has settled, and what the configuration says until then.
  def desired: Set[HostName] =
    HOSTS.filter(h => if (settled(h)) isHost(h) else inv.config.contains(h))

  def slot(p: PoolName): PoolSlot = pools.get(p)
  def obj(p: PoolName): Pool = poolIn(pools.get(p))
  def live(p: PoolName): bool = isLivePool(pools.get(p))
  def inBattery(p: PoolName): bool = battery.pools.keys().contains(p)

  pure def matches(selector: Set[Label], labels: Set[Label]): bool = selector.subseteq(labels)

  // The Hosts a Pool's selector matches: the Hosts battery runs with whose
  // Nodes exist and match.
  def resolve(p: PoolName): Set[HostName] =
    battery.hosts.filter(h => present.contains(h) and matches(obj(p).spec.nodeSelector, nodes.get(h).labels))

  //@= docs/requirements/03-pools.md#placement
  //@# While a Pool's selector matches no Host, the Pool Controller
  //@# SHALL set the Pool's condition `Ready` false with the reason
  //@# `NoEligibleHost`.
  //
  // Otherwise Ready depends on battery's counts (PO-022), which this model
  // does not have; it leaves the condition Unknown.
  pure def readyFor(hosts: Set[HostName]): Condition =
    if (hosts == Set()) { status: CondFalse, reason: NoEligibleHost } else unknownCondition

  // The Pool as the Pool Controller declares it to battery.
  def declared(p: PoolName): BatteryPool = {
    name: p,
    size: obj(p).spec.size,
    heartbeatExpiryThreshold: obj(p).spec.heartbeatExpiryThreshold,
    flintlockHosts: resolve(p),
  }

  def vmCount(p: PoolName, h: HostName): int =
    battery.vms.filter(v => v.pool == p and v.host == h).size()

  // battery v0.3.3's PickHost: the Host in flintlock_hosts with the fewest
  // of the Pool's MicroVMs. Ties go by the order of flintlock_hosts, which
  // the model does not keep, so any of the least loaded.
  def leastLoaded(p: PoolName): Set[HostName] =
    val fh = battery.pools.get(p).flintlockHosts
    fh.filter(h => fh.forall(h2 => vmCount(p, h) <= vmCount(p, h2)))

  def canProvision(p: PoolName): bool = and {
    battery.up,
    inBattery(p),
    battery.vms.filter(v => v.pool == p).size() < battery.pools.get(p).size,
    battery.pools.get(p).flintlockHosts != Set(),
  }

  // ---------------------------------------------------------------------
  // Init

  pure val initialLabels: HostName -> Set[Label] =
    Map("h1" -> Set(ZONE_A), "h2" -> Set(ZONE_A, GPU), "h3" -> Set(ZONE_B))

  action init = all {
    now' = 0,
    nodes' = HOSTS.mapBy(h => {
      name: h,
      labels: initialLabels.get(h),
      schedulable: true,
      report: { ready: true, agentAddress: Some(h), flintlockdAddress: Some(h) },
    }),
    present' = HOSTS,
    changedAt' = HOSTS.mapBy(_ => 0),
    pools' = POOLS.mapBy(_ => PoolUnborn),
    battery' = { up: true, hosts: HOSTS, pools: Map(), vms: Set(), nextVM: 1 },
    inv' = { config: HOSTS, window: None },
    hist' = {
      restarts: List(),
      births: Set(),
      absorbedFlaps: 0,
      finalized: Set(),
      placementUpdates: 0,
      unknownHostPicks: 0,
    },
  }

  // ---------------------------------------------------------------------
  // Environment: Nodes

  // A Node changes. If that changes whether it is a Host, the change
  // starts to settle now; a change that returns the Node to what battery's
  // configuration already says before it settled is a flap absorbed.
  action changeNode(h: HostName, n: Node, isPresent: bool): bool = {
    val pr = if (isPresent) present.union(Set(h)) else present.exclude(Set(h))
    val ns = nodes.set(h, n)
    val was = isHost(h)
    val becomes = nodeIsHost(ns, pr, h)
    all {
      nodes' = ns,
      present' = pr,
      changedAt' = if (was != becomes) changedAt.set(h, now) else changedAt,
      hist' = if (was != becomes and becomes == inv.config.contains(h) and not(settled(h)))
        { ...hist, absorbedFlaps: hist.absorbedFlaps + 1 } else hist,
      now' = now, pools' = pools, battery' = battery, inv' = inv,
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
  // Environment: Pools and time

  action createPool(p: PoolName, sel: Set[Label], n: int): bool = all {
    not(live(p)),
    pools' = pools.set(p, PoolLive(newPool(p, { size: n, heartbeatExpiryThreshold: EXPIRY, nodeSelector: sel }))),
    hist' = { ...hist, finalized: hist.finalized.exclude(Set(p)) },
    now' = now, nodes' = nodes, present' = present, changedAt' = changedAt, battery' = battery, inv' = inv,
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
    now' = now, nodes' = nodes, present' = present, changedAt' = changedAt, battery' = battery, inv' = inv, hist' = hist,
  }

  // With a finalizer the API server only marks the Pool; without one it is
  // gone at once.
  action deletePool(p: PoolName): bool = all {
    live(p), not(obj(p).deleting),
    pools' = pools.set(p,
      if (obj(p).hasFinalizer) PoolLive({ ...obj(p), deleting: true }) else PoolGone(obj(p))),
    now' = now, nodes' = nodes, present' = present, changedAt' = changedAt, battery' = battery, inv' = inv, hist' = hist,
  }

  // Time passes, once the Inventory Controller and battery have caught up.
  action tick: bool = all {
    battery.up,
    not(canOpenWindow),
    not(canCloseWindow),
    now' = now + 1,
    nodes' = nodes, present' = present, changedAt' = changedAt, pools' = pools,
    battery' = battery, inv' = inv, hist' = hist,
  }

  // ---------------------------------------------------------------------
  // battery

  // battery replenishes a Pool on the least loaded Host in its
  // flintlock_hosts, which it knows.
  action batteryProvision(p: PoolName, h: HostName): bool = all {
    canProvision(p),
    leastLoaded(p).contains(h),
    battery.hosts.contains(h),
    battery' = {
      ...battery,
      vms: battery.vms.union(Set({ id: battery.nextVM, pool: p, host: h })),
      nextVM: battery.nextVM + 1,
    },
    hist' = { ...hist, births: hist.births.union(Set({ host: h, at: now, nodeIsHost: isHost(h), age: age(h) })) },
    now' = now, nodes' = nodes, present' = present, changedAt' = changedAt, pools' = pools, inv' = inv,
  }

  // PickHost chose a Host battery was not configured with; Provision fails
  // with ErrUnknownHost and nothing is created.
  action batteryProvisionUnknownHost(p: PoolName, h: HostName): bool = all {
    canProvision(p),
    leastLoaded(p).contains(h),
    not(battery.hosts.contains(h)),
    hist' = { ...hist, unknownHostPicks: hist.unknownHostPicks + 1 },
    now' = now, nodes' = nodes, present' = present, changedAt' = changedAt, pools' = pools,
    battery' = battery, inv' = inv,
  }

  // A leased MicroVM of the Pool on the Host is released, and battery
  // deletes it.
  action releaseVM(p: PoolName, h: HostName): bool = all {
    battery.up,
    vmCount(p, h) > 0,
    val vs = battery.vms.filter(v => v.pool == p and v.host == h)
    val newest = vs.fold(0, (m, v) => if (v.id > m) v.id else m)
    battery' = { ...battery, vms: battery.vms.filter(v => v.id != newest) },
    now' = now, nodes' = nodes, present' = present, changedAt' = changedAt, pools' = pools, inv' = inv, hist' = hist,
  }

  // battery starts, and reads its Hosts from its configuration (ADR 0001,
  // consequence 1). Its Pools and MicroVMs survive in its database.
  action batteryStart: bool = all {
    not(battery.up),
    battery' = { ...battery, up: true, hosts: inv.config },
    now' = now, nodes' = nodes, present' = present, changedAt' = changedAt, pools' = pools, inv' = inv, hist' = hist,
  }

  // ---------------------------------------------------------------------
  // The Inventory Controller

  // A settled change is waiting: open a restart window.
  def canOpenWindow: bool = and {
    battery.up,
    inv.window == None,
    desired != inv.config,
  }

  action invOpenWindow: bool = all {
    canOpenWindow,
    inv' = { ...inv, window: Some(now) },
    now' = now, nodes' = nodes, present' = present, changedAt' = changedAt, pools' = pools, battery' = battery, hist' = hist,
  }

  def canCloseWindow: bool = match inv.window {
    | Some(t) => now >= t + WINDOW
    | None => false
  }

  //@= docs/requirements/04-inventory.md#applying
  //@# The Inventory Controller SHALL apply every change that has
  //@# settled within one restart window in a single restart of battery.
  //
  // The window closes: every change that has settled by now goes into one
  // restart. If the changes flapped back meanwhile, there is nothing to
  // apply and battery is left alone.
  action invCloseWindow: bool = all {
    canCloseWindow,
    desired == inv.config,
    inv' = { ...inv, window: None },
    now' = now, nodes' = nodes, present' = present, changedAt' = changedAt, pools' = pools, battery' = battery, hist' = hist,
  }

  //@= docs/requirements/04-inventory.md#applying
  //@# When the set of Hosts changes, the Inventory Controller SHALL
  //@# write the new list to battery's configuration and restart battery.
  action invRestart: bool = all {
    canCloseWindow,
    desired != inv.config,
    val changed = desired.exclude(inv.config).union(inv.config.exclude(desired))
    all {
      inv' = { config: desired, window: None },
      battery' = { ...battery, up: false },
      hist' = { ...hist, restarts: hist.restarts.append({ at: now, changed: changed, ages: changed.map(h => age(h)) }) },
    },
    now' = now, nodes' = nodes, present' = present, changedAt' = changedAt, pools' = pools,
  }

  // ---------------------------------------------------------------------
  // The Pool Controller

  def setPool(p: PoolName, o: Pool): PoolName -> PoolSlot = pools.set(p, PoolLive(o))

  //@= docs/requirements/03-pools.md#declaration
  //@# The Pool Controller SHALL add a finalizer to each Pool, and when
  //@# the Pool is deleted SHALL call `DeletePool` and remove the finalizer only
  //@# once battery has deleted the Pool or reported it unknown.
  def canAddFinalizer(p: PoolName): bool = and {
    live(p), not(obj(p).deleting), not(obj(p).hasFinalizer),
  }

  action ctlAddFinalizer(p: PoolName): bool = all {
    canAddFinalizer(p),
    pools' = setPool(p, { ...obj(p), hasFinalizer: true }),
    now' = now, nodes' = nodes, present' = present, changedAt' = changedAt, battery' = battery, inv' = inv, hist' = hist,
  }

  //@= docs/requirements/03-pools.md#declaration
  //@# When a Pool exists that battery does not hold, the Pool
  //@# Controller SHALL create it in battery with `CreatePool`, under the Pool's
  //@# namespace and name.
  //
  // Nothing in 03-pools.md orders CreatePool after the finalizer, as CL-001
  // does for ClaimVM, so this does not wait for it (#72).
  def canCreate(p: PoolName): bool = and {
    live(p), not(obj(p).deleting), battery.up, not(inBattery(p)),
  }

  action ctlCreatePool(p: PoolName): bool = all {
    canCreate(p),
    battery' = { ...battery, pools: battery.pools.put(p, declared(p)) },
    pools' = setPool(p, { ...obj(p), status: { ...obj(p).status, observedGeneration: obj(p).generation, ready: readyFor(resolve(p)) } }),
    now' = now, nodes' = nodes, present' = present, changedAt' = changedAt, inv' = inv, hist' = hist,
  }

  //@= docs/requirements/03-pools.md#declaration
  //@# When a Pool's `metadata.generation` differs from its
  //@# `status.observedGeneration`, the Pool Controller SHALL send the Pool's spec
  //@# to battery with `UpdatePool` and then set `status.observedGeneration` to
  //@# that generation.
  def canUpdate(p: PoolName): bool = and {
    live(p), not(obj(p).deleting), battery.up, inBattery(p),
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
    hist' = if (hostsChanged(p)) { ...hist, placementUpdates: hist.placementUpdates + 1 } else hist,
    now' = now, nodes' = nodes, present' = present, changedAt' = changedAt, inv' = inv,
  }

  def canDelete(p: PoolName): bool = and {
    live(p), obj(p).deleting, obj(p).hasFinalizer, battery.up,
  }

  // DeletePool, then the finalizer. battery deletes the Pool and its
  // MicroVMs; a Pool it does not hold is "unknown", which also lets the
  // finalizer go.
  action ctlDeletePool(p: PoolName): bool = all {
    canDelete(p),
    battery' = {
      ...battery,
      pools: battery.pools.keys().exclude(Set(p)).mapBy(q => battery.pools.get(q)),
      vms: battery.vms.filter(v => v.pool != p),
    },
    pools' = pools.set(p, PoolGone({ ...obj(p), hasFinalizer: false })),
    hist' = { ...hist, finalized: hist.finalized.union(Set(p)) },
    now' = now, nodes' = nodes, present' = present, changedAt' = changedAt, inv' = inv,
  }

  // Whether the Pool Controller has any step it can take.
  def poolControllerIdle: bool = and {
    battery.up,
    POOLS.forall(p => and {
      not(canAddFinalizer(p)),
      not(canCreate(p)),
      not(canUpdate(p)),
      not(canDelete(p)),
    }),
  }

  // ---------------------------------------------------------------------
  // Step

  action step = {
    nondet h = HOSTS.oneOf()
    nondet p = POOLS.oneOf()
    nondet labels = LABEL_SETS.oneOf()
    nondet sel = SELECTORS.oneOf()
    nondet n = SIZES.oneOf()
    nondet ready = Set(true, false).oneOf()
    any {
      // environment
      cordon(h),
      uncordon(h),
      flipReport(h),
      relabel(h, labels),
      deleteNode(h),
      createNode(h, labels, ready),
      createPool(p, sel, n),
      changePool(p, sel, n),
      deletePool(p),
      tick,
      // battery
      batteryProvision(p, h),
      batteryProvisionUnknownHost(p, h),
      batteryStart,
      releaseVM(p, h),
      // the Inventory Controller
      invOpenWindow,
      invCloseWindow,
      invRestart,
      // the Pool Controller
      ctlAddFinalizer(p),
      ctlCreatePool(p),
      ctlUpdatePool(p),
      ctlDeletePool(p),
    }
  }

  // ---------------------------------------------------------------------
  // Invariants. Each names the requirement or ADR consequence it checks.

  // PO-010, PO-011, PO-002: once the Pool Controller has nothing left to
  // do, battery holds every live Pool with its current spec, and its
  // flintlock_hosts are exactly the Hosts battery runs with that the
  // Pool's selector matches.
  val poolHostsMatchSelector: bool =
    poolControllerIdle implies POOLS.forall(p => (live(p) and not(obj(p).deleting)) implies and {
      inBattery(p),
      battery.pools.get(p).flintlockHosts == battery.hosts.filter(h => and {
        present.contains(h),
        obj(p).spec.nodeSelector.subseteq(nodes.get(h).labels),
      }),
      battery.pools.get(p).size == obj(p).spec.size,
      obj(p).status.observedGeneration == obj(p).generation,
    })

  // PO-012: once the Pool Controller is idle, a live Pool says
  // NoEligibleHost exactly when its selector matches no Host.
  val noEligibleHostWhenNoneMatch: bool =
    poolControllerIdle implies POOLS.forall(p => (live(p) and not(obj(p).deleting)) implies
      ((obj(p).status.ready.reason == NoEligibleHost) iff (resolve(p) == Set())))

  // PO-003: a Pool whose finalizer the Pool Controller removed is not in
  // battery.
  val finalizerRemovedOnlyAfterDelete: bool =
    POOLS.forall(p => (isGonePool(slot(p)) and hist.finalized.contains(p)) implies not(inBattery(p)))

  // IN-001, IN-002, IN-010, IN-011: while battery runs, its Hosts are the
  // Nodes that are Hosts, except for a change younger than the settle time
  // plus one restart window.
  val hostsFollowNodes: bool =
    battery.up implies HOSTS.forall(h =>
      (battery.hosts.contains(h) != isHost(h)) implies age(h) <= MAX_LAG)

  //@= docs/requirements/04-inventory.md#admission
  //@# When a Node that is a Host is cordoned or deleted, or its Node
  //@# report says the Host is not ready, the Inventory Controller SHALL remove
  //@# the Host from battery's Hosts.
  //
  // A cordoned, deleted or not-ready Host gets no new MicroVMs once its
  // change has settled and its restart window has closed.
  val noNewVMOnFormerHost: bool =
    hist.births.forall(b => not(b.nodeIsHost) implies b.age <= MAX_LAG)

  // IN-011: a restart applies only changes that have held for the settle
  // time, so a report that flaps faster than that restarts nothing.
  val restartOnlyForSettledChanges: bool =
    hist.restarts.foldl(true, (ok, r) => ok and r.changed != Set() and r.ages.forall(a => a >= SETTLE))

  // IN-012: battery restarts at most once per restart window.
  val restartsOncePerWindow: bool =
    hist.restarts.indices().forall(i =>
      i > 0 implies hist.restarts[i].at - hist.restarts[i - 1].at >= WINDOW)

  // All the invariants CI checks.
  val safety: bool = and {
    poolHostsMatchSelector,
    noEligibleHostWhenNoneMatch,
    finalizerRemovedOnlyAfterDelete,
    hostsFollowNodes,
    noNewVMOnFormerHost,
    restartOnlyForSettledChanges,
    restartsOncePerWindow,
  }

  // Findings: properties the requirements do not yet guarantee. Not in
  // safety; pools_test.qnt reaches each violation.

  // A gone Pool leaves nothing in battery. Fails when the Pool is deleted
  // after CreatePool but before its finalizer (#72).
  val goneLeavesNothing: bool =
    POOLS.forall(p => isGonePool(slot(p)) implies not(inBattery(p)))

  // Every Pool in battery names only Hosts battery knows. Fails from the
  // restart that removes a Host until the Pool Controller's UpdatePool;
  // meanwhile battery v0.3.3 picks the removed Host, which has no MicroVMs,
  // and fails to provision (#73).
  val poolsNameKnownHosts: bool =
    battery.up implies battery.pools.keys().forall(p =>
      battery.pools.get(p).flintlockHosts.subseteq(battery.hosts))

  // ---------------------------------------------------------------------
  // Witnesses: states quint run reports reaching, to show the simulation
  // gets to the interesting interleavings.

  // a Node's change flipped back before it settled, and restarted nothing
  val witnessFlapAbsorbed: bool = hist.absorbedFlaps > 0
  // battery restarted for a Host change
  val witnessRestart: bool = hist.restarts.length() > 0
  // one restart applied changes to two Hosts
  val witnessBatchedRestart: bool = hist.restarts.foldl(false, (w, r) => w or r.changed.size() > 1)
  // two restarts, one window apart
  val witnessTwoRestarts: bool = hist.restarts.length() > 1
  // a MicroVM was created on a Host whose Node had stopped being a Host,
  // before the change settled and was applied
  val witnessVMOnFormerHost: bool = hist.births.exists(b => not(b.nodeIsHost))
  // UpdatePool moved a Pool's flintlock_hosts
  val witnessPlacementUpdated: bool = hist.placementUpdates > 0
  // a live Pool's selector matches no Host, and it says so
  val witnessNoEligibleHost: bool =
    POOLS.exists(p => live(p) and obj(p).status.ready.reason == NoEligibleHost)
  // a Pool was deleted in battery under its finalizer
  val witnessPoolDeleted: bool = hist.finalized != Set()
  // battery tried to provision on a Host it does not know
  val witnessUnknownHostPicked: bool = hist.unknownHostPicks > 0
}
