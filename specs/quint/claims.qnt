// -*- mode: Bluespec; -*-
//
// The claim lifecycle: the Claim Controller against battery.
//
// What it models (docs/requirements/02-claims.md, ADR 0001 consequences 2
// to 4):
//
// - the Claim Controller's steps: add the finalizer, ClaimVM, write the
//   binding, relay a renewal as Heartbeat and write battery's expiry,
//   expire a claim from the Events stream (CL-013) or its passed expiry
//   (CL-014, once ListLeases confirms it: CL-016), release with ReleaseVM
//   and then remove the finalizer, and recover after a restart;
// - battery's steps: replenish the Pool, answer ClaimVM, Heartbeat and
//   ReleaseVM, sweep the Leases nobody renewed, report deleted MicroVMs on
//   the Events stream;
// - the environment: Holders create, renew and delete claims, time passes,
//   the controller crashes (between any two of its steps, including between
//   ClaimVM and the status write), battery stops and starts, and the
//   Events stream drops events;
// - calls that fail in transit (CL-007, CL-015, CL-017, CL-022, CL-040):
//   each call to battery can fail with battery having acted (the answer is
//   lost) or not, without a crash.
//
// One Pool, a few claims and a handful of MicroVMs. Each controller step is
// one call to battery or one write to the API server; a crash loses only
// what the controller held in memory between the two.
//
// Model citations use //@= and //@# (docs/requirements/README.md, "Model
// citations"). They record what is modelled; they never count as an
// implementation or a test. battery's steps cite what the Operator assumes
// battery does (docs/requirements/10-battery.md, BA-*), and the Claim
// Controller's steps cite its requirements (02-claims.md, CL-*).
//
// The file is laid out as: parameters, state, views, init, the environment,
// battery's steps, the Claim Controller's steps, step, invariants (all in
// safety), properties the requirements do not yet guarantee (kept out of
// safety, each with its issue), and witnesses.
module claims {
  import types.* from "./types"

  // ---------------------------------------------------------------------
  // Parameters

  pure val CLAIMS: Set[ClaimName] = Set("a", "b", "c")
  // Every MicroVM battery can ever create, and the Host it lands on. A
  // deleted MicroVM's uid is never used again, so this bounds how many
  // MicroVMs a trace can create.
  pure val VM_HOST: VMId -> HostName = Map(
    "v1" -> "h1", "v2" -> "h2", "v3" -> "h1",
    "v4" -> "h2", "v5" -> "h1", "v6" -> "h2",
  )
  pure val VMS: Set[VMId] = VM_HOST.keys()
  pure val POOL: PoolName = "pool"
  pure val POOL_SIZE: int = 2
  // The Pool's heartbeat_expiry_threshold, in ticks.
  pure val EXPIRY: Time = 3
  // battery's sweep_interval, in ticks (BA-020): the longest a Lease
  // outlives its expiry, or battery's start, before a sweep deletes it.
  pure val SWEEP: Time = 2

  // ---------------------------------------------------------------------
  // State

  // What the controller holds in memory between a call to battery and the
  // status write that records its answer. A crash loses it.
  type InFlight =
    | Idle
    | ClaimAnswered(Lease)
    | HeartbeatAnswered({ leaseId: LeaseId, expiresAt: Time, renewTime: int })
    // battery released the Lease, or reported it unknown; the finalizer
    // write is next (CL-020)
    | ReleaseAnswered(LeaseId)

  type BatteryState = {
    up: bool,
    // when battery last started; its first sweep is due SWEEP after
    // (BA-022)
    upSince: Time,
    // MicroVMs not yet created
    unborn: Set[VMId],
    // AVAILABLE: warm and unleased
    warm: Set[VMId],
    // every MicroVM battery has ever handed out in ClaimVM
    handedOut: Set[VMId],
    // released or expired, and deleted
    retired: Set[VMId],
    leases: LeaseId -> Lease,
    nextLease: LeaseId,
    // MicroVM-deleted events on battery's Events stream that the Claim
    // Controller has not received yet. The stream can drop any of them,
    // and loses them all when battery stops.
    events: Set[VMId],
  }

  type ControllerState = {
    mem: ClaimName -> InFlight,
    // set on start and on reconnecting to battery, cleared by recovery
    recovering: bool,
  }

  // History, for the invariants only; nothing reads it to decide a step.
  type History = {
    // every lease id the controller has written to a claim's status
    recorded: Set[LeaseId],
    // every lease id the controller has sent to ReleaseVM
    released: Set[LeaseId],
    // how claims went Expired: "event" (CL-013), "time" (CL-014)
    expiredVia: Set[str],
    // every (claim, lease id) battery answered a ClaimVM for that claim
    // with, and the controller received (CL-008)
    answered: Set[(ClaimName, LeaseId)],
    // what happened with calls that failed in transit, for the witnesses:
    // "claimLost", "heartbeatLost", "releaseLost" (battery acted, and the
    // answer was lost)
    failures: Set[str],
    // claims whose ReleaseVM answer was lost in transit
    releaseLostBy: Set[ClaimName],
  }

  var now: Time
  var claims: ClaimName -> ClaimSlot
  var battery: BatteryState
  var ctrl: ControllerState
  var hist: History

  // ---------------------------------------------------------------------
  // Views

  def slot(c: ClaimName): ClaimSlot = claims.get(c)
  def obj(c: ClaimName): MicroVMClaim = claimIn(claims.get(c))
  def live(c: ClaimName): bool = isLive(claims.get(c))

  // battery holds a Lease until a sweep or a release deletes its row. It
  // renews any Lease it holds, whether or not its expiry has passed
  // (BA-010), and lists it in ListLeases with its expiry (BA-041).
  def holds(l: LeaseId): bool = battery.leases.keys().contains(l)
  // A Lease battery holds whose expiry has not passed yet. One past its
  // expiry that no sweep has deleted is held, but not unexpired.
  def holdsUnexpired(l: LeaseId): bool =
    holds(l) and battery.leases.get(l).expiresAt > now

  pure def maxOf(a: int, b: int): int = if (a > b) a else b

  // The latest time a sweep can delete Lease l, as BA-020 and BA-022
  // bound it: one sweep interval after its expiry, or after battery's
  // start if it expired while battery was stopped.
  def sweepDue(l: LeaseId): Time =
    maxOf(battery.leases.get(l).expiresAt, battery.upSince) + SWEEP

  // lease ids recorded in the status of a claim that still exists
  def recordedByLiveClaims: Set[LeaseId] =
    CLAIMS.filter(c => live(c)).map(c => optToSet(obj(c).status.leaseId)).flatten()

  // lease ids battery has answered that the controller has not written yet
  def inFlightLeases: Set[LeaseId] =
    CLAIMS.map(c => match ctrl.mem.get(c) {
      | ClaimAnswered(l) => Set(l.id)
      | _ => Set()
    }).flatten()

  // Leases battery holds that no claim records and the controller is not
  // about to record: the orphans of ADR 0001, consequence 2, left by a
  // crash or by a ClaimVM answer lost in transit.
  def orphans: Set[LeaseId] =
    battery.leases.keys().filter(l =>
      not(recordedByLiveClaims.contains(l)) and not(inFlightLeases.contains(l)))

  // ---------------------------------------------------------------------
  // Init

  action init = all {
    now' = 0,
    claims' = CLAIMS.mapBy(_ => Unborn),
    battery' = {
      up: true,
      upSince: 0,
      unborn: VMS,
      warm: Set(),
      handedOut: Set(),
      retired: Set(),
      leases: Map(),
      nextLease: 1,
      events: Set(),
    },
    ctrl' = { mem: CLAIMS.mapBy(_ => Idle), recovering: false },
    hist' = { recorded: Set(), released: Set(), expiredVia: Set(), answered: Set(), failures: Set(), releaseLostBy: Set() },
  }

  // ---------------------------------------------------------------------
  // Helpers for writing state

  action setClaim(c: ClaimName, o: MicroVMClaim): bool = all {
    claims' = claims.set(c, Live(o)),
  }

  action setMem(c: ClaimName, f: InFlight): bool = all {
    ctrl' = { ...ctrl, mem: ctrl.mem.set(c, f) },
  }

  //@= docs/requirements/02-claims.md#failed-calls
  //@# When battery answers a call for a claim, the Claim Controller
  //@# SHALL set the claim's condition `Synced` true.
  pure val synced: Condition = { status: CondTrue, reason: BatteryAnswered }

  //@= docs/requirements/02-claims.md#failed-calls
  //@# If a call to battery for a claim fails in transit, then the
  //@# Claim Controller SHALL set the claim's condition `Synced` false with the
  //@# reason `BatteryUnavailable`, and SHALL NOT change the claim's phase
  //@# because of that failure.
  pure val unavailable: Condition = { status: CondFalse, reason: BatteryUnavailable }

  // The claim after a call for it failed in transit: Synced false, and
  // nothing else changed.
  pure def unsynced(o: MicroVMClaim): MicroVMClaim =
    { ...o, status: { ...o.status, synced: unavailable } }

  //@= docs/requirements/10-battery.md#events
  //@# battery SHALL record `VM_DELETED_DUE_TO_EXPIRY` or
  //@# `VM_DELETED_ON_RELEASE` for a leased MicroVM only after `flintlockd` has
  //@# confirmed its deletion.
  //
  // battery deletes a released or expired MicroVM; its uid is retired, and
  // the Events stream reports the deletion. The model deletes the Lease and
  // the MicroVM in one step; in battery the Lease row goes first (BA-023).
  pure def dropLeases(b: BatteryState, ls: Set[LeaseId]): BatteryState = {
    ...b,
    leases: b.leases.keys().exclude(ls).mapBy(l => b.leases.get(l)),
    retired: b.retired.union(ls.map(l => b.leases.get(l).vm)),
    events: b.events.union(ls.map(l => b.leases.get(l).vm)),
  }

  // Leases a sweep at time t deletes: every one whose expiry is at or
  // before t (BA-020).
  pure def overdue(b: BatteryState, t: Time): Set[LeaseId] =
    b.leases.keys().filter(l => b.leases.get(l).expiresAt <= t)

  // ---------------------------------------------------------------------
  // Environment: Holders, time, crashes

  // A consumer creates a claim (the Client Library, 07-client.md).
  action createClaim(c: ClaimName): bool = all {
    isUnborn(slot(c)),
    claims' = claims.set(c, Live(newClaim(POOL, "holder"))),
    now' = now, battery' = battery, ctrl' = ctrl, hist' = hist,
  }

  // The consumer deletes its claim. With a finalizer the API server only
  // marks it; without one it is gone at once.
  action deleteClaim(c: ClaimName): bool = all {
    live(c),
    not(obj(c).deleting),
    claims' = claims.set(c,
      if (obj(c).hasFinalizer) Live({ ...obj(c), deleting: true })
      else Gone(obj(c))),
    now' = now, battery' = battery, ctrl' = ctrl, hist' = hist,
  }

  // The Holder renews a Bound claim by changing spec.renewTime (RS-023).
  // A Holder that stops renewing simply never takes this step again.
  action renewClaim(c: ClaimName): bool = all {
    live(c),
    not(obj(c).deleting),
    obj(c).status.phase == Bound,
    setClaim(c, { ...obj(c), spec: { ...obj(c).spec, renewTime: obj(c).spec.renewTime + 1 } }),
    now' = now, battery' = battery, ctrl' = ctrl, hist' = hist,
  }

  //@= docs/requirements/10-battery.md#expiry
  //@# battery SHALL delete an expired Lease only in a sweep, which
  //@# runs once every `sweep_interval` while battery runs, and which deletes
  //@# every Lease whose expiry is at or before the time of the sweep.
  //
  // Time passes. The sweep is its own step (batteryExpire), so time can
  // pass a Lease's expiry with the Lease still held. While battery runs,
  // time cannot move past sweepDue(l) until a sweep has deleted l: a sweep
  // comes within one sweep interval. The model lets it come at any time in
  // that interval, which covers the periodic sweep.
  action tick: bool = all {
    battery.up implies battery.leases.keys().forall(l => now < sweepDue(l)),
    now' = now + 1,
    claims' = claims, battery' = battery, ctrl' = ctrl, hist' = hist,
  }

  // The Operator crashes and restarts. The controller loses whatever it
  // held between a call to battery and the status write, including a
  // ClaimVM answer (ADR 0001, consequence 2), and recovers on start.
  action crashController: bool = all {
    ctrl' = { mem: CLAIMS.mapBy(_ => Idle), recovering: true },
    now' = now, claims' = claims, battery' = battery, hist' = hist,
  }

  //@= docs/requirements/10-battery.md#restarts
  //@# battery SHALL keep its Pools, MicroVMs and Leases across a
  //@# restart, in its database.
  //
  // battery stops, for a Host change (ADR 0001, consequence 1) or a crash.
  // Its database survives; calls to it fail until it is back.
  // Events not yet delivered are lost with the stream. battery replays its
  // whole outbox to a new subscriber (BA-050), so this is more pessimistic
  // than battery: the model's controller can miss what battery would
  // replay, and has to rely on CL-014 and CL-030 instead.
  action batteryStop: bool = all {
    battery.up,
    battery' = { ...battery, up: false, events: Set() },
    now' = now, claims' = claims, ctrl' = ctrl, hist' = hist,
  }

  // The Events stream drops an event (a disconnect, a slow consumer).
  action dropEvent(vm: VMId): bool = all {
    battery.events.contains(vm),
    battery' = { ...battery, events: battery.events.exclude(Set(vm)) },
    now' = now, claims' = claims, ctrl' = ctrl, hist' = hist,
  }

  //@= docs/requirements/10-battery.md#expiry
  //@# battery SHALL run its first sweep one `sweep_interval` after it
  //@# starts, not at start.
  //
  // battery starts again. The Leases that ran out while it was down are
  // still held, and renewable, until its first sweep, which is due SWEEP
  // after the start (sweepDue). The controller sees its connection
  // restored.
  action batteryStart: bool = all {
    not(battery.up),
    battery' = { ...battery, up: true, upSince: now },
    ctrl' = { ...ctrl, recovering: true },
    now' = now, claims' = claims, hist' = hist,
  }

  // ---------------------------------------------------------------------
  // battery's own steps

  // battery creates a MicroVM to keep the Pool at its size.
  action replenish(vm: VMId): bool = all {
    battery.up,
    battery.unborn.contains(vm),
    battery.warm.size() < POOL_SIZE,
    battery' = { ...battery, unborn: battery.unborn.exclude(Set(vm)), warm: battery.warm.union(Set(vm)) },
    now' = now, claims' = claims, ctrl' = ctrl, hist' = hist,
  }

  //@= docs/requirements/10-battery.md#expiry
  //@# When a sweep deletes a Lease, battery SHALL delete the Lease's
  //@# MicroVM through `flintlockd`
  //
  //@= docs/requirements/10-battery.md#expiry
  //@# If a `Heartbeat` renews a Lease after a sweep has listed it as
  //@# expired and before the sweep deletes it, then battery SHALL keep the Lease
  //@# with its renewed expiry.
  //
  // A sweep: battery deletes the Leases nobody renewed in time, and their
  // MicroVMs. It takes the Leases overdue when it runs, so a Heartbeat
  // that lands before it keeps its Lease, the race battery's guarded
  // delete settles the same way. The tick guard makes it come within one
  // sweep interval.
  action batteryExpire: bool = all {
    battery.up,
    overdue(battery, now) != Set(),
    battery' = dropLeases(battery, overdue(battery, now)),
    now' = now, claims' = claims, ctrl' = ctrl, hist' = hist,
  }

  // ---------------------------------------------------------------------
  // The Claim Controller
  //
  // Each can* is the step's guard, so that the model can say when the
  // controller has nothing left to do (controllerIdle).

  def idle(c: ClaimName): bool = ctrl.mem.get(c) == Idle

  //@= docs/requirements/02-claims.md#binding
  //@# When a claim that has no lease id in its status is
  //@# reconciled, the Claim Controller SHALL add the finalizer
  //@# `battery.liquidmetal-x.dev/release` to the claim before it calls battery's
  //@# `ClaimVM` for the claim's Pool.
  def canAddFinalizer(c: ClaimName): bool = and {
    live(c), idle(c),
    not(obj(c).deleting),
    not(obj(c).hasFinalizer),
    isNone(obj(c).status.leaseId),
  }

  action ctlAddFinalizer(c: ClaimName): bool = all {
    canAddFinalizer(c),
    setClaim(c, { ...obj(c), hasFinalizer: true }),
    now' = now, battery' = battery, ctrl' = ctrl, hist' = hist,
  }

  // The controller would call ClaimVM for c. The call can fail in transit
  // whether battery is up or not; it can succeed only while battery is up.
  def canCallClaimVM(c: ClaimName): bool = and {
    live(c), idle(c),
    not(obj(c).deleting),
    obj(c).hasFinalizer,
    obj(c).status.phase == Pending,
    isNone(obj(c).status.leaseId),
  }

  def canClaimVM(c: ClaimName): bool = canCallClaimVM(c) and battery.up

  // battery commits a Lease on vm in ClaimVM.
  pure def newLease(b: BatteryState, vm: VMId, t: Time): Lease = {
    id: b.nextLease, pool: POOL, vm: vm, host: VM_HOST.get(vm),
    claimedAt: t, expiresAt: t + EXPIRY,
  }

  pure def commitLease(b: BatteryState, l: Lease): BatteryState = {
    ...b,
    warm: b.warm.exclude(Set(l.vm)),
    handedOut: b.handedOut.union(Set(l.vm)),
    leases: b.leases.put(l.id, l),
    nextLease: b.nextLease + 1,
  }

  //@= docs/requirements/10-battery.md#claiming
  //@# When `ClaimVM` succeeds, battery SHALL choose a new lease id,
  //@# and set the Lease's expiry to the time of the claim plus the Pool's
  //@# `heartbeat_expiry_threshold`.
  //
  // ClaimVM succeeds. battery chooses the lease id and answers; the
  // controller holds the answer until it writes the status.
  action ctlClaimVM(c: ClaimName, vm: VMId): bool = all {
    canClaimVM(c),
    battery.warm.contains(vm),
    val lease = newLease(battery, vm, now)
    all {
      battery' = commitLease(battery, lease),
      setMem(c, ClaimAnswered(lease)),
      hist' = { ...hist, answered: hist.answered.union(Set((c, lease.id))) },
    },
    now' = now, claims' = claims,
  }

  //@= docs/requirements/02-claims.md#binding
  //@# If battery's `ClaimVM` for a claim fails in transit, then the
  //@# Claim Controller SHALL keep the claim in the phase `Pending` with the
  //@# condition `Bound` false and the reason `BatteryUnavailable`, and SHALL
  //@# retry `ClaimVM` with backoff.
  pure def claimUnavailable(o: MicroVMClaim): MicroVMClaim = {
    ...o,
    status: { ...o.status, phase: Pending, bound: unavailable, synced: unavailable },
  }

  //@= docs/requirements/10-battery.md#claiming
  //@# Once battery has committed a Lease in `ClaimVM`, battery SHALL
  //@# answer that `ClaimVM` with success.
  //
  // ClaimVM fails in transit after battery committed the Lease: the answer
  // is lost without a crash, and the Lease is an orphan. The claim stays
  // Pending and canCallClaimVM holds again, which is the retry. battery
  // itself answered success (BA-004); the transport lost it.
  action ctlClaimVMLost(c: ClaimName, vm: VMId): bool = all {
    canClaimVM(c),
    battery.warm.contains(vm),
    battery' = commitLease(battery, newLease(battery, vm, now)),
    setClaim(c, claimUnavailable(obj(c))),
    hist' = { ...hist, failures: hist.failures.union(Set("claimLost")) },
    now' = now, ctrl' = ctrl,
  }

  // ClaimVM fails in transit before battery acted, or battery is down.
  action ctlClaimVMUnanswered(c: ClaimName): bool = all {
    canCallClaimVM(c),
    setClaim(c, claimUnavailable(obj(c))),
    now' = now, battery' = battery, ctrl' = ctrl, hist' = hist,
  }

  //@= docs/requirements/02-claims.md#binding
  //@# If battery's `ClaimVM` fails because the Pool has no warm
  //@# MicroVM, then the Claim Controller SHALL keep the claim in the phase
  //@# `Pending` with the condition `Bound` false and the reason `PoolExhausted`,
  //@# and SHALL retry with backoff.
  //
  //@= docs/requirements/10-battery.md#claiming
  //@# If a Pool has no MicroVM in the phase `AVAILABLE`, then battery
  //@# SHALL answer `ClaimVM` for that Pool with `RESOURCE_EXHAUSTED` and create no
  //@# Lease.
  action ctlPoolExhausted(c: ClaimName): bool = all {
    canClaimVM(c),
    battery.warm == Set(),
    setClaim(c, { ...obj(c), status: { ...obj(c).status, phase: Pending, bound: { status: CondFalse, reason: PoolExhausted }, synced: synced } }),
    now' = now, battery' = battery, ctrl' = ctrl, hist' = hist,
  }

  //@= docs/requirements/02-claims.md#binding
  //@# When battery's `ClaimVM` succeeds for a claim, the Claim
  //@# Controller SHALL write the lease id, the MicroVM's uid, the Host's name as
  //@# the node name and the time of binding to the claim's status, set the phase
  //@# to `Bound` and set the condition `Bound` true, before it makes any other
  //@# call to battery for that claim.
  // The answer the controller holds for c, if it is one of these.
  pure val noLease: Lease = { id: 0, pool: "", vm: "", host: "", claimedAt: 0, expiresAt: 0 }
  pure def claimAnswer(f: InFlight): Lease = match f {
    | ClaimAnswered(l) => l
    | _ => noLease
  }
  pure def isClaimAnswer(f: InFlight): bool = match f {
    | ClaimAnswered(_) => true
    | _ => false
  }
  pure def heartbeatAnswer(f: InFlight): { leaseId: LeaseId, expiresAt: Time, renewTime: int } = match f {
    | HeartbeatAnswered(h) => h
    | _ => { leaseId: 0, expiresAt: 0, renewTime: 0 }
  }
  pure def isHeartbeatAnswer(f: InFlight): bool = match f {
    | HeartbeatAnswered(_) => true
    | _ => false
  }

  def canWriteBound(c: ClaimName): bool = live(c) and isClaimAnswer(ctrl.mem.get(c))

  // battery's answer to ClaimVM carries no expiry time, so the claim is
  // Bound with none, and CL-016 reads it with ListLeases (ctlListRenewed).
  // The renewTime the claim has now is recorded as relayed: the new Lease
  // runs from later than any renewal before it. The time of binding is
  // when battery answered ClaimVM, which can be ticks before the status
  // write, as the Claim Controller records it (#62).
  action ctlWriteBound(c: ClaimName): bool = all {
    canWriteBound(c),
    val l = claimAnswer(ctrl.mem.get(c))
    all {
      setClaim(c, {
        ...obj(c),
        observedRenewTime: obj(c).spec.renewTime,
        status: {
          phase: Bound,
          leaseId: Some(l.id),
          microVM: Some(l.vm),
          nodeName: Some(l.host),
          boundTime: Some(l.claimedAt),
          leaseExpiresAt: None,
          bound: { status: CondTrue, reason: ClaimBound },
          synced: synced,
        },
      }),
      setMem(c, Idle),
      hist' = { ...hist, recorded: hist.recorded.union(Set(l.id)) },
    },
    now' = now, battery' = battery,
  }

  // A pending renewal (glossary): the Holder has changed spec.renewTime
  // since the controller last relayed it.
  def pendingRenewal(c: ClaimName): bool =
    obj(c).spec.renewTime != obj(c).observedRenewTime

  //@= docs/requirements/02-claims.md#renewal
  //@# While the `spec.renewTime` of a Bound claim differs from the
  //@# last `renewTime` the Claim Controller relayed for it, the Claim Controller
  //@# SHALL call battery's `Heartbeat` for the claim's Lease, and SHALL write
  //@# the expiry time battery returns and the `renewTime` it relayed to the
  //@# claim's status in one write.
  def canCallHeartbeat(c: ClaimName): bool = and {
    live(c), idle(c),
    not(obj(c).deleting),
    obj(c).status.phase == Bound,
    isSome(obj(c).status.leaseId),
    pendingRenewal(c),
  }

  def canHeartbeat(c: ClaimName): bool = canCallHeartbeat(c) and battery.up

  // battery renews Lease l at time t.
  pure def renew(b: BatteryState, l: LeaseId, t: Time): BatteryState =
    { ...b, leases: b.leases.set(l, { ...b.leases.get(l), expiresAt: t + EXPIRY }) }

  //@= docs/requirements/10-battery.md#heartbeat
  //@# When battery receives a `Heartbeat` for a Lease it still holds,
  //@# battery SHALL set the Lease's expiry to the time of the `Heartbeat` plus
  //@# the Pool's `heartbeat_expiry_threshold` and answer with that expiry,
  //@# whether or not the Lease's previous expiry has passed.
  //
  // Heartbeat succeeds on any Lease battery still holds, including one
  // past its expiry that no sweep has deleted: battery moves the Lease's
  // expiry, and the controller holds battery's answer until it writes the
  // status.
  action ctlHeartbeat(c: ClaimName): bool = all {
    canHeartbeat(c),
    val l = getOr(obj(c).status.leaseId, 0)
    all {
      holds(l),
      battery' = renew(battery, l, now),
      setMem(c, HeartbeatAnswered({ leaseId: l, expiresAt: now + EXPIRY, renewTime: obj(c).spec.renewTime })),
    },
    now' = now, claims' = claims, hist' = hist,
  }

  //@= docs/requirements/02-claims.md#renewal
  //@# If battery's `Heartbeat` for a Bound claim fails in transit,
  //@# then the Claim Controller SHALL keep the claim `Bound` with the Lease
  //@# expiry time already in its status, and SHALL retry the `Heartbeat` with
  //@# backoff while the claim is Bound.
  //
  // Heartbeat fails in transit after battery renewed the Lease: battery's
  // expiry moves, the status keeps the old one, and observedRenewTime is
  // unchanged, so canCallHeartbeat holds again, which is the retry.
  action ctlHeartbeatLost(c: ClaimName): bool = all {
    canHeartbeat(c),
    val l = getOr(obj(c).status.leaseId, 0)
    all {
      holds(l),
      battery' = renew(battery, l, now),
    },
    setClaim(c, unsynced(obj(c))),
    hist' = { ...hist, failures: hist.failures.union(Set("heartbeatLost")) },
    now' = now, ctrl' = ctrl,
  }

  // Heartbeat fails in transit before battery acted, or battery is down.
  action ctlHeartbeatUnanswered(c: ClaimName): bool = all {
    canCallHeartbeat(c),
    setClaim(c, unsynced(obj(c))),
    now' = now, battery' = battery, ctrl' = ctrl, hist' = hist,
  }

  //@= docs/requirements/02-claims.md#renewal
  //@# The Claim Controller SHALL take a claim's Lease expiry time only
  //@# from battery's answer to `Heartbeat` or `ListLeases`, and SHALL NOT
  //@# compute it.
  action ctlWriteRenewed(c: ClaimName): bool = all {
    live(c),
    isHeartbeatAnswer(ctrl.mem.get(c)),
    val h = heartbeatAnswer(ctrl.mem.get(c))
    all {
      setClaim(c, {
        ...obj(c),
        observedRenewTime: h.renewTime,
        status: { ...obj(c).status, leaseExpiresAt: Some(h.expiresAt), synced: synced },
      }),
      setMem(c, Idle),
    },
    now' = now, battery' = battery, hist' = hist,
  }

  // The claim once it has gone Expired. Its observedRenewTime is left as it
  // was, so that pendingRenewalKeptWhileHeld can see a renewal the
  // controller had not relayed when the claim expired.
  pure def expired(o: MicroVMClaim): MicroVMClaim = {
    ...o,
    status:{ ...o.status, phase: Expired, bound: { status: CondFalse, reason: LeaseExpired }, synced: synced },
  }

  //@= docs/requirements/10-battery.md#heartbeat
  //@# If battery does not hold the Lease a `Heartbeat` names, then
  //@# battery SHALL answer it with `NOT_FOUND`.
  //
  //@= docs/requirements/02-claims.md#renewal
  //@# If battery answers a `Heartbeat` for a claim's Lease with
  //@# `NOT_FOUND`, or leaves the claim's Lease out of its answer to
  //@# `ListLeases`, then the Claim Controller SHALL set the claim's phase to
  //@# `Expired` and its condition `Bound` false with the reason `LeaseExpired`.
  //
  // Heartbeat fails: battery no longer holds the Lease. battery has no
  // answer for "expired" (BA-011): it renews any Lease it still holds.
  action ctlHeartbeatUnknown(c: ClaimName): bool = all {
    canHeartbeat(c),
    not(holds(getOr(obj(c).status.leaseId, 0))),
    setClaim(c, expired(obj(c))),
    now' = now, battery' = battery, ctrl' = ctrl, hist' = hist,
  }

  //@= docs/requirements/02-claims.md#renewal
  //@# When battery's `Events` stream reports that the MicroVM of a
  //@# Bound claim was deleted, the Claim Controller SHALL set the claim's phase
  //@# to `Expired` and its condition `Bound` false with the reason
  //@# `LeaseExpired`.
  def canReceiveEvent: bool = battery.up and battery.events != Set()

  // The controller receives one MicroVM-deleted event, and expires every
  // Bound claim on that MicroVM.
  action ctlEventDeleted(vm: VMId): bool = all {
    battery.up,
    battery.events.contains(vm),
    battery' = { ...battery, events: battery.events.exclude(Set(vm)) },
    claims' = CLAIMS.mapBy(c =>
      if (live(c) and obj(c).status.phase == Bound and obj(c).status.microVM == Some(vm))
        Live(expired(obj(c)))
      else slot(c)),
    hist' = if (CLAIMS.exists(c => live(c) and obj(c).status.phase == Bound and obj(c).status.microVM == Some(vm)))
      { ...hist, expiredVia: hist.expiredVia.union(Set("event")) } else hist,
    now' = now, ctrl' = ctrl,
  }

  //@= docs/requirements/02-claims.md#renewal
  //@# When a Bound claim has no pending renewal, and its status has
  //@# no Lease expiry time or one that has passed, the Claim Controller SHALL
  //@# read the claim's Lease with battery's `ListLeases`
  //
  // A claim just bound has no expiry in its status, since battery's answer
  // to ClaimVM carries none; getOr counts it as passed. A claim with a
  // pending renewal waits for its Heartbeat (CL-010) instead: battery
  // still renews a Lease past its expiry until the sweep (BA-010), so
  // expiring it first would drop a renewal the Holder asked for (#85).
  def canExpiryPassed(c: ClaimName): bool = and {
    live(c), idle(c),
    obj(c).status.phase == Bound,
    not(pendingRenewal(c)),
    getOr(obj(c).status.leaseExpiresAt, 0) <= now,
  }

  //@= docs/requirements/02-claims.md#renewal
  //@# When battery lists a Bound claim's Lease in its answer to
  //@# `ListLeases` with an expiry time that has passed, and the claim has no
  //@# pending renewal, the Claim Controller SHALL set the claim's phase to
  //@# `Expired` and its condition `Bound` false with the reason `LeaseExpired`.
  //
  //@= docs/requirements/02-claims.md#renewal
  //@# If battery answers a `Heartbeat` for a claim's Lease with
  //@# `NOT_FOUND`, or leaves the claim's Lease out of its answer to
  //@# `ListLeases`, then the Claim Controller SHALL set the claim's phase to
  //@# `Expired` and its condition `Bound` false with the reason `LeaseExpired`.
  //
  // battery lists the Lease with a passed expiry (CL-014), or does not
  // list it (CL-012). The read and the write are one step.
  action ctlExpiryPassed(c: ClaimName): bool = all {
    canExpiryPassed(c),
    battery.up,
    not(holdsUnexpired(getOr(obj(c).status.leaseId, 0))),
    setClaim(c, expired(obj(c))),
    hist' = { ...hist, expiredVia: hist.expiredVia.union(Set("time")) },
    now' = now, battery' = battery, ctrl' = ctrl,
  }

  //@= docs/requirements/02-claims.md#renewal
  //@# When a Bound claim has no pending renewal, and its status has
  //@# no Lease expiry time or one that has passed, the Claim Controller SHALL
  //@# read the claim's Lease with battery's `ListLeases`, and SHALL keep the
  //@# claim `Bound` and write the expiry time battery lists if that time has
  //@# not passed.
  //
  // battery lists the Lease with a later expiry: the claim was just bound,
  // or its status is behind battery's for another reason. The claim stays
  // Bound with battery's expiry. The read and the write are one step:
  // nothing else renews the Lease in between, because the controller
  // reconciles one claim at a time.
  action ctlListRenewed(c: ClaimName): bool = all {
    canExpiryPassed(c),
    battery.up,
    val l = getOr(obj(c).status.leaseId, 0)
    all {
      holdsUnexpired(l),
      setClaim(c, { ...obj(c), status: { ...obj(c).status, leaseExpiresAt: Some(battery.leases.get(l).expiresAt), synced: synced } }),
    },
    now' = now, battery' = battery, ctrl' = ctrl, hist' = hist,
  }

  //@= docs/requirements/02-claims.md#renewal
  //@# If the `ListLeases` call of CL-016 fails in transit, then the
  //@# Claim Controller SHALL keep the claim `Bound` and retry the call with
  //@# backoff.
  action ctlListUnanswered(c: ClaimName): bool = all {
    canExpiryPassed(c),
    setClaim(c, unsynced(obj(c))),
    now' = now, battery' = battery, ctrl' = ctrl, hist' = hist,
  }

  //@= docs/requirements/02-claims.md#release
  //@# When a claim that has a lease id is deleted, the Claim
  //@# Controller SHALL call battery's `ReleaseVM` for the Lease, and SHALL remove
  //@# the finalizer only once battery has released the Lease or reported it
  //@# unknown.
  def canCallRelease(c: ClaimName): bool = and {
    live(c), idle(c),
    obj(c).deleting,
    obj(c).hasFinalizer,
    isSome(obj(c).status.leaseId),
  }

  def canRelease(c: ClaimName): bool = canCallRelease(c) and battery.up

  //@= docs/requirements/10-battery.md#release
  //@# When battery receives a `ReleaseVM` for a Lease it holds,
  //@# battery SHALL delete the Lease's MicroVM through `flintlockd` and answer
  //@# with success only once `flintlockd` has confirmed the deletion and the
  //@# Lease is deleted.
  //
  //@= docs/requirements/10-battery.md#release
  //@# If battery does not hold the Lease a `ReleaseVM` names, then
  //@# battery SHALL answer it with `NOT_FOUND`.
  //
  // battery releases Lease l if it holds it, and deletes the MicroVM. The
  // model's release succeeds at once. UNAVAILABLE with the Lease kept
  // (BA-031) is not a step of its own: to the controller it is a ReleaseVM
  // that failed before battery acted, with the Lease still held, which is
  // ctlReleaseUnanswered.
  pure def release(b: BatteryState, l: LeaseId): BatteryState =
    dropLeases(b, Set(l).intersect(b.leases.keys()))

  // ReleaseVM succeeds. battery deletes the MicroVM; a Lease it no longer
  // holds is "unknown", which counts as released too. The controller holds
  // the answer until it removes the finalizer.
  action ctlReleaseVM(c: ClaimName): bool = all {
    canRelease(c),
    val l = getOr(obj(c).status.leaseId, 0)
    all {
      battery' = release(battery, l),
      setMem(c, ReleaseAnswered(l)),
      hist' = { ...hist, released: hist.released.union(Set(l)) },
    },
    now' = now, claims' = claims,
  }

  // The finalizer goes once battery has released the Lease. A crash
  // before this step loses the answer, and the next ReleaseVM finds the
  // Lease unknown.
  action ctlRemoveFinalizer(c: ClaimName): bool = all {
    live(c),
    match ctrl.mem.get(c) {
      | ReleaseAnswered(_) => true
      | _ => false
    },
    claims' = claims.set(c, Gone({ ...obj(c), hasFinalizer: false })),
    setMem(c, Idle),
    now' = now, battery' = battery, hist' = hist,
  }

  //@= docs/requirements/02-claims.md#release
  //@# If battery's `ReleaseVM` for a deleted claim fails in transit,
  //@# then the Claim Controller SHALL keep the finalizer and retry `ReleaseVM`
  //@# with backoff.
  //
  // ReleaseVM fails in transit after battery released the Lease. The
  // finalizer stays, canCallRelease holds again, and the retry finds the
  // Lease unknown.
  action ctlReleaseLost(c: ClaimName): bool = all {
    canRelease(c),
    val l = getOr(obj(c).status.leaseId, 0)
    all {
      battery' = release(battery, l),
      hist' = { ...hist,
        released: hist.released.union(Set(l)),
        failures: hist.failures.union(Set("releaseLost")),
        releaseLostBy: hist.releaseLostBy.union(Set(c)) },
    },
    setClaim(c, unsynced(obj(c))),
    now' = now, ctrl' = ctrl,
  }

  //@= docs/requirements/10-battery.md#release
  //@# If `flintlockd` does not confirm the deletion of a released
  //@# Lease's MicroVM, then battery SHALL answer the `ReleaseVM` with
  //@# `UNAVAILABLE`, keep the Lease
  //
  // ReleaseVM fails in transit before battery acted, battery is down, or
  // battery answers UNAVAILABLE because flintlockd has not confirmed the
  // deletion and keeps the Lease.
  action ctlReleaseUnanswered(c: ClaimName): bool = all {
    canCallRelease(c),
    setClaim(c, unsynced(obj(c))),
    now' = now, battery' = battery, ctrl' = ctrl, hist' = hist,
  }

  //@= docs/requirements/02-claims.md#release
  //@# When a claim that has no lease id is deleted, the Claim
  //@# Controller SHALL remove its finalizer without calling battery.
  def canReleasePending(c: ClaimName): bool = and {
    live(c), idle(c),
    obj(c).deleting,
    obj(c).hasFinalizer,
    isNone(obj(c).status.leaseId),
  }

  action ctlReleasePending(c: ClaimName): bool = all {
    canReleasePending(c),
    claims' = claims.set(c, Gone({ ...obj(c), hasFinalizer: false })),
    now' = now, battery' = battery, ctrl' = ctrl, hist' = hist,
  }

  //@= docs/requirements/02-claims.md#recovery
  //@# When the Claim Controller starts, and when its connection to
  //@# battery is restored, the Claim Controller SHALL reconcile every Bound claim against
  //@# battery's Leases.
  def canRecover: bool = ctrl.recovering and battery.up

  //@= docs/requirements/10-battery.md#list-leases
  //@# battery SHALL include in the answer to `ListLeases` a Lease
  //@# whose expiry has passed but that no sweep has deleted yet.
  //
  // Every Bound claim whose Lease battery no longer holds becomes Expired
  // (CL-012), and so does one whose Lease battery holds past its expiry,
  // unless the claim has a pending renewal (CL-014): that one waits for its
  // Heartbeat, which battery still answers until the sweep (#85). Leases no
  // claim records are left alone (CL-031). The controller reads the Leases
  // with ListLeases (BA-040), which lists each with its expiry and renews
  // none; that is the only way it can tell "expired" from "unknown".
  //
  // The Operator lists the Leases once for every claim, and takes this
  // step one claim at a time: claim.Recover expires a claim whose Lease
  // battery did not list, and CheckExpiry, in the same reconcile, reads a
  // Lease listed with a passed expiry again and applies CL-014.
  action ctlRecover: bool = all {
    canRecover,
    claims' = CLAIMS.mapBy(c =>
      if (live(c) and obj(c).status.phase == Bound
          and not(holdsUnexpired(getOr(obj(c).status.leaseId, 0)))
          and (not(holds(getOr(obj(c).status.leaseId, 0))) or not(pendingRenewal(c))))
        Live(expired(obj(c)))
      else slot(c)),
    ctrl' = { ...ctrl, recovering: false },
    now' = now, battery' = battery, hist' = hist,
  }

  // Whether the controller has any step it can take for any claim. Writing
  // an answer it holds counts, as does a pending recovery, and so does a
  // call it would make or retry while battery is down: a retry with backoff
  // is work still to do.
  def controllerIdle: bool = and {
    not(canRecover),
    not(canReceiveEvent),
    CLAIMS.forall(c => and {
      not(canAddFinalizer(c)),
      not(canCallClaimVM(c)),
      not(canWriteBound(c)),
      not(canCallHeartbeat(c)),
      not(canExpiryPassed(c)),
      not(live(c) and not(idle(c))),
      not(canCallRelease(c)),
      not(canReleasePending(c)),
    }),
  }

  // ---------------------------------------------------------------------
  // Step

  action step = {
    nondet c = CLAIMS.oneOf()
    nondet vm = VMS.oneOf()
    any {
      // environment
      createClaim(c),
      deleteClaim(c),
      renewClaim(c),
      tick,
      crashController,
      batteryStop,
      batteryStart,
      dropEvent(vm),
      // battery
      replenish(vm),
      batteryExpire,
      // the Claim Controller
      ctlAddFinalizer(c),
      ctlClaimVM(c, vm),
      ctlPoolExhausted(c),
      ctlWriteBound(c),
      ctlHeartbeat(c),
      ctlHeartbeatUnknown(c),
      ctlWriteRenewed(c),
      ctlReleaseVM(c),
      ctlRemoveFinalizer(c),
      ctlReleasePending(c),
      ctlRecover,
      ctlEventDeleted(vm),
      ctlExpiryPassed(c),
      ctlListRenewed(c),
      // calls that fail in transit
      ctlClaimVMLost(c, vm),
      ctlClaimVMUnanswered(c),
      ctlHeartbeatLost(c),
      ctlHeartbeatUnanswered(c),
      ctlListUnanswered(c),
      ctlReleaseLost(c),
      ctlReleaseUnanswered(c),
    }
  }

  // ---------------------------------------------------------------------
  // Invariants. Each names the requirement or ADR consequence it checks.

  //@= docs/requirements/10-battery.md#claiming
  //@# battery SHALL move a MicroVM out of the phase `AVAILABLE` in
  //@# the same transaction in which `ClaimVM` selects it, so that two `ClaimVM`
  //@# calls never lease the same MicroVM.
  //
  //@= docs/requirements/10-battery.md#claiming
  //@# When `ClaimVM` succeeds, battery SHALL choose a new lease id
  //
  //@= docs/requirements/02-claims.md#binding
  //@# The Claim Controller SHALL write to a claim's status only a
  //@# lease id that battery returned in its answer to a `ClaimVM` call for that
  //@# claim.
  //
  // A Bound claim has exactly one Lease (glossary, "Lease"). battery never
  // leases one MicroVM twice (BA-005) and never reuses a lease id (BA-003),
  // and a claim records only its own ClaimVM's answer (CL-008), so no two
  // claims record the same Lease or the same MicroVM.
  val vmLeasedToAtMostOneClaim: bool = and {
    battery.leases.keys().forall(l1 => battery.leases.keys().forall(l2 =>
      l1 != l2 implies battery.leases.get(l1).vm != battery.leases.get(l2).vm)),
    CLAIMS.forall(c1 => CLAIMS.forall(c2 =>
      (c1 != c2 and not(isUnborn(slot(c1))) and not(isUnborn(slot(c2)))) implies and {
        not(isSome(obj(c1).status.leaseId) and obj(c1).status.leaseId == obj(c2).status.leaseId),
        not(isSome(obj(c1).status.microVM) and obj(c1).status.microVM == obj(c2).status.microVM),
      })),
  }

  //@= docs/requirements/10-battery.md#claiming
  //@# battery SHALL set a MicroVM's phase to `AVAILABLE` only when it
  //@# finishes provisioning the MicroVM, so that a MicroVM that has been leased
  //@# is never leased again.
  //
  // A released MicroVM is deleted by battery and never reused (02-claims.md,
  // Release). Nothing handed out, released or expired is warm again, and
  // no Lease names a retired MicroVM.
  val releasedVMNeverReused: bool = and {
    battery.warm.intersect(battery.handedOut) == Set(),
    battery.warm.intersect(battery.retired) == Set(),
    battery.leases.keys().forall(l => not(battery.retired.contains(battery.leases.get(l).vm))),
  }

  //@= docs/requirements/02-claims.md#renewal
  //@# The Claim Controller SHALL call battery's `Heartbeat` only for
  //@# a lease id that a claim's status records.
  //
  //@= docs/requirements/10-battery.md#claiming
  //@# set the Lease's expiry to the time of the claim plus the Pool's
  //@# `heartbeat_expiry_threshold`.
  //
  //@= docs/requirements/10-battery.md#expiry
  //@# battery SHALL delete an expired Lease only in a sweep, which
  //@# runs once every `sweep_interval` while battery runs, and which deletes
  //@# every Lease whose expiry is at or before the time of the sweep.
  //
  //@= docs/requirements/10-battery.md#expiry
  //@# battery SHALL run its first sweep one `sweep_interval` after it
  //@# starts, not at start.
  //
  // The crash-window orphan of 02-claims.md, Binding, is bounded. Nothing
  // renews an orphan (CL-019), so its expiry stays the Pool's
  // heartbeat_expiry_threshold after it was claimed (BA-003). While battery
  // runs it is gone within that threshold plus one sweep interval
  // (BA-020), or within one sweep interval of battery starting again if it
  // ran out while battery was down (BA-022). 02-claims.md, Binding, and
  // the correction note under ADR 0001, consequence 2, state this bound
  // (#86, #106). The ADR's text as first written promises the threshold
  // alone, which is orphanGoneByExpiry below, and does not hold.
  val orphanLeaseBounded: bool =
    orphans.forall(l => and {
      battery.leases.get(l).expiresAt == battery.leases.get(l).claimedAt + EXPIRY,
      battery.up implies now <= maxOf(battery.leases.get(l).claimedAt + EXPIRY, battery.upSince) + SWEEP,
    })

  // CL-020: the finalizer is removed only after battery has released the
  // Lease. A claim that is gone never leaves a Lease behind in battery.
  val finalizerRemovedOnlyAfterRelease: bool =
    CLAIMS.forall(c => isGone(slot(c)) implies
      not(obj(c).status.leaseId.optToSet().exists(l => holds(l))))

  // CL-001: a Lease is claimed and recorded only under the finalizer, so a
  // claim cannot be deleted out from under a Lease it holds.
  val leaseOnlyUnderFinalizer: bool =
    CLAIMS.forall(c => live(c) implies (
      (isSome(obj(c).status.leaseId) or ctrl.mem.get(c) != Idle) implies obj(c).hasFinalizer))

  //@= docs/requirements/02-claims.md#renewal
  //@# The Claim Controller SHALL take a claim's Lease expiry time only
  //@# from battery's answer to `Heartbeat` or `ListLeases`, and SHALL NOT
  //@# compute it.
  //
  //@= docs/requirements/10-battery.md#heartbeat
  //@# battery SHALL set the Lease's expiry to the time of the `Heartbeat` plus
  //@# the Pool's `heartbeat_expiry_threshold`
  //
  // The expiry a consumer reads is one battery gave (CL-011), never later
  // than the one battery will enforce, since a Heartbeat only moves it on
  // (BA-010).
  val statusExpiryIsBatterys: bool =
    CLAIMS.forall(c => live(c) implies match obj(c).status.leaseId {
      | Some(l) => holds(l) implies
          getOr(obj(c).status.leaseExpiresAt, -1) <= battery.leases.get(l).expiresAt
      | None => true
    })

  //@= docs/requirements/02-claims.md#recovery
  //@# The Claim Controller SHALL NOT release a Lease that battery
  //@# holds and no claim records.
  val onlyRecordedLeasesReleased: bool = hist.released.subseteq(hist.recorded)

  // CL-002 and RS-024: a Bound claim carries everything a consumer needs.
  // Its Lease expiry time is not among them at first: battery's answer to
  // ClaimVM carries none, and CL-016 reads it with ListLeases after the
  // binding is written.
  val boundClaimIsComplete: bool =
    CLAIMS.forall(c => (live(c) and obj(c).status.phase == Bound) implies and {
      isSome(obj(c).status.leaseId),
      isSome(obj(c).status.microVM),
      isSome(obj(c).status.nodeName),
      isSome(obj(c).status.boundTime),
      obj(c).status.bound.status == CondTrue,
    })

  //@= docs/requirements/02-claims.md#binding
  //@# The Claim Controller SHALL write to a claim's status only a
  //@# lease id that battery returned in its answer to a `ClaimVM` call for that
  //@# claim.
  //
  // A Lease whose answer was lost, in a crash or in transit, is never
  // adopted by any claim.
  val recordedOnlyFromOwnAnswer: bool =
    CLAIMS.forall(c => not(isUnborn(slot(c))) implies
      obj(c).status.leaseId.optToSet().forall(l => hist.answered.contains((c, l))))

  // All the invariants CI checks.
  val safety: bool = and {
    recordedOnlyFromOwnAnswer,
    expiredOnlyAfterLeaseEnds,
    pendingRenewalKeptWhileHeld,
    vmLeasedToAtMostOneClaim,
    releasedVMNeverReused,
    orphanLeaseBounded,
    finalizerRemovedOnlyAfterRelease,
    leaseOnlyUnderFinalizer,
    statusExpiryIsBatterys,
    onlyRecordedLeasesReleased,
    boundClaimIsComplete,
    idleMirrorsBattery,
  }

  //@= docs/requirements/02-claims.md#renewal
  //@# When battery lists a Bound claim's Lease in its answer to
  //@# `ListLeases` with an expiry time that has passed, and the claim has no
  //@# pending renewal, the Claim Controller SHALL set the claim's phase to
  //@# `Expired`
  //
  //@= docs/requirements/02-claims.md#recovery
  //@# While a claim is `Bound` and the Lease expiry time in its
  //@# status has not passed, the Claim Controller SHALL reconcile the claim
  //@# again once that time has passed.
  //
  // A Bound claim's Lease exists in battery, or the claim goes Expired
  // (02-claims.md, Recovery). As a state invariant: once the controller has
  // nothing left to do, every Bound claim's Lease is one battery holds and
  // has not run out. CL-032 keeps a Bound claim from going quiet, CL-013
  // makes it prompt, and CL-014 makes it hold when the Events stream drops
  // the deletion (#45).
  val idleMirrorsBattery: bool =
    controllerIdle implies
      CLAIMS.forall(c => (live(c) and obj(c).status.phase == Bound) implies
        holdsUnexpired(getOr(obj(c).status.leaseId, 0)))

  // A claim goes Expired only once battery no longer holds its Lease, or
  // the Lease has run out: the Holder never loses a MicroVM battery still
  // leases to it. CL-014 alone broke it after a crash between a Heartbeat
  // and its status write (#57), or a Heartbeat answer lost in transit;
  // CL-016's ListLeases before expiring makes it hold.
  val expiredOnlyAfterLeaseEnds: bool =
    CLAIMS.forall(c => (live(c) and obj(c).status.phase == Expired) implies
      not(holdsUnexpired(getOr(obj(c).status.leaseId, 0))))

  // A claim that goes Expired with a renewal the controller has not
  // relayed does so only once battery would refuse a Heartbeat for its
  // Lease, that is once no Lease row is left (BA-010, BA-011). A Lease past
  // its expiry is still renewable until the sweep, and expiring the claim
  // then would drop a renewal the Holder asked for in time (#85). CL-014
  // and CL-016 wait while a renewal is pending, and CL-010 relays it
  // first. expired() leaves observedRenewTime as it was, so the pending
  // renewal is still visible on the Expired claim.
  //
  // A claim with no pending renewal can still go Expired while battery
  // holds its Lease past its expiry, before the sweep: that is what
  // Expired means (02-claims.md, Renewal), and no renewal is lost.
  val pendingRenewalKeptWhileHeld: bool =
    CLAIMS.forall(c => (live(c) and obj(c).status.phase == Expired and pendingRenewal(c)) implies
      not(holds(getOr(obj(c).status.leaseId, 0))))

  // ---------------------------------------------------------------------
  // Properties the requirements do not yet guarantee. Each is a finding,
  // filed as an issue, kept out of safety, and reached by a scenario test
  // in claims_test.qnt that fails once the model changes to close it.

  // ADR 0001, consequence 2, as first written: battery expires an orphan
  // after the Pool's heartbeat_expiry_threshold. 02-claims.md, Binding,
  // said so too. #86 corrected 02-claims.md, and #106 added a dated
  // correction note under the ADR, both to the bound of orphanLeaseBounded.
  // This property keeps the old wording, to show why it was wrong.
  // Not in safety: the sweep deletes it up to one sweep interval later
  // (BA-020), and up to one sweep interval after battery restarts
  // (BA-022), so it outlives the threshold (#86).
  // orphanLeaseBounded is the bound battery gives. claims_test.qnt,
  // orphanOutlivesExpiryTest, reaches it.
  val orphanGoneByExpiry: bool =
    orphans.forall(l => battery.up implies now <= battery.leases.get(l).claimedAt + EXPIRY)

  // ---------------------------------------------------------------------
  // Witnesses: states quint run reports reaching, to show the simulation
  // gets to the interesting interleavings. quint run counts the traces in
  // which each becomes true. hack/quint.sh fails if one is never reached.

  // a crash-window orphan exists
  val witnessOrphan: bool = orphans != Set()
  // a claim has gone Expired
  val witnessExpired: bool = CLAIMS.exists(c => live(c) and obj(c).status.phase == Expired)
  // a Bound claim has been released and is gone
  val witnessReleased: bool = CLAIMS.exists(c => isGone(slot(c)) and isSome(obj(c).status.leaseId))
  // an orphan exists beside a Bound claim, as after the retry that follows
  // a crash in the window
  val witnessOrphanBesideBound: bool =
    orphans != Set() and CLAIMS.exists(c => live(c) and obj(c).status.phase == Bound)
  // a claim went Expired from battery's Events stream (CL-013)
  val witnessExpiredByEvent: bool = hist.expiredVia.contains("event")
  // a claim went Expired from its passed expiry time (CL-014)
  val witnessExpiredByTime: bool = hist.expiredVia.contains("time")
  // an orphan left by a ClaimVM answer lost in transit, with no crash
  val witnessOrphanFromLostAnswer: bool =
    hist.failures.contains("claimLost") and orphans != Set()
  // a ReleaseVM whose answer was lost has been retried, and the retry found
  // the Lease unknown: the claim holds that answer, or is gone
  val witnessReleasedAfterLostAnswer: bool =
    hist.releaseLostBy.exists(c => isGone(slot(c)) or match ctrl.mem.get(c) {
      | ReleaseAnswered(_) => true
      | _ => false
    })
  // battery runs and holds a Lease past its expiry, waiting for the sweep
  // (BA-020)
  val witnessUnsweptLease: bool =
    battery.up and battery.leases.keys().exists(l => not(holdsUnexpired(l)))
  // the controller can relay a renewal for a Lease past its expiry that
  // battery still holds, so the Heartbeat renews it (BA-010);
  // claims_test.qnt, lateHeartbeatRenewsTest, takes that step
  val witnessLateHeartbeat: bool =
    CLAIMS.exists(c => canHeartbeat(c) and holds(getOr(obj(c).status.leaseId, 0))
      and not(holdsUnexpired(getOr(obj(c).status.leaseId, 0))))
  // the controller is idle while a claim is Bound: idleMirrorsBattery's
  // left-hand side, with a claim for its right-hand side to check, so the
  // invariant is not checked only where it holds trivially (#65)
  val witnessIdleWithBound: bool =
    controllerIdle and CLAIMS.exists(c => live(c) and obj(c).status.phase == Bound)
  // the same after battery has restarted, which loses the events on its
  // stream, so CL-014 is what keeps the claim honest
  val witnessIdleWithBoundAfterRestart: bool = battery.upSince > 0 and witnessIdleWithBound
  // a claim has recorded an expiry for a Lease battery holds: the
  // left-hand side of statusExpiryIsBatterys
  val witnessExpiryRecordedForHeldLease: bool =
    CLAIMS.exists(c => live(c) and isSome(obj(c).status.leaseExpiresAt)
      and holds(getOr(obj(c).status.leaseId, 0)))
  // a claim went Expired with a renewal the controller had not relayed:
  // the left-hand side of pendingRenewalKeptWhileHeld
  val witnessExpiredWithPendingRenewal: bool =
    CLAIMS.exists(c => live(c) and obj(c).status.phase == Expired and pendingRenewal(c))
}
