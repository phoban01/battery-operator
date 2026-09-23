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
//   (CL-014), release under the finalizer, and recover after a restart;
// - battery's steps: replenish the Pool, answer ClaimVM, Heartbeat and
//   ReleaseVM, expire Leases nobody renews, report deleted MicroVMs on the
//   Events stream;
// - the environment: Holders create, renew and delete claims, time passes,
//   the controller crashes (between any two of its steps, including between
//   ClaimVM and the status write), battery stops and starts, and the
//   Events stream drops events.
//
// One Pool, a few claims and a handful of MicroVMs. Each controller step is
// one call to battery or one write to the API server; a crash loses only
// what the controller held in memory between the two.
//
// Model citations use //@= and //@# (docs/requirements/README.md, "Model
// citations"). They record what is modelled; they never count as an
// implementation or a test.
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

  // ---------------------------------------------------------------------
  // State

  // What the controller holds in memory between a call to battery and the
  // status write that records its answer. A crash loses it.
  type InFlight =
    | Idle
    | ClaimAnswered(Lease)
    | HeartbeatAnswered({ leaseId: LeaseId, expiresAt: Time, renewTime: int })

  type BatteryState = {
    up: bool,
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

  def holds(l: LeaseId): bool = battery.leases.keys().contains(l)
  // battery answers Heartbeat only for a Lease it holds that has not run
  // out; one past its expiry that the sweep has not reached yet is expired.
  def holdsUnexpired(l: LeaseId): bool =
    holds(l) and battery.leases.get(l).expiresAt > now

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
  // about to record: the crash-window orphans of ADR 0001, consequence 2.
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
      unborn: VMS,
      warm: Set(),
      handedOut: Set(),
      retired: Set(),
      leases: Map(),
      nextLease: 1,
      events: Set(),
    },
    ctrl' = { mem: CLAIMS.mapBy(_ => Idle), recovering: false },
    hist' = { recorded: Set(), released: Set(), expiredVia: Set() },
  }

  // ---------------------------------------------------------------------
  // Helpers for writing state

  action setClaim(c: ClaimName, o: MicroVMClaim): bool = all {
    claims' = claims.set(c, Live(o)),
  }

  action setMem(c: ClaimName, f: InFlight): bool = all {
    ctrl' = { ...ctrl, mem: ctrl.mem.set(c, f) },
  }

  // battery deletes a released or expired MicroVM; its uid is retired, and
  // the Events stream reports the deletion.
  pure def dropLeases(b: BatteryState, ls: Set[LeaseId]): BatteryState = {
    ...b,
    leases: b.leases.keys().exclude(ls).mapBy(l => b.leases.get(l)),
    retired: b.retired.union(ls.map(l => b.leases.get(l).vm)),
    events: b.events.union(ls.map(l => b.leases.get(l).vm)),
  }

  // Leases battery's sweep expires at time t.
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

  // Time passes. While battery runs, its sweep expires a Lease before time
  // moves past the Lease's expiry, so a Lease never outlives it by a tick.
  action tick: bool = all {
    battery.up implies overdue(battery, now) == Set(),
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

  // battery stops, for a Host change (ADR 0001, consequence 1) or a crash.
  // Its database survives; calls to it fail until it is back.
  // Events not yet delivered are lost with the stream.
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

  // battery starts again, and expires the Leases that ran out while it was
  // down. The controller sees its connection restored.
  action batteryStart: bool = all {
    not(battery.up),
    battery' = { ...dropLeases(battery, overdue(battery, now)), up: true },
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

  // battery expires the Leases nobody renewed and deletes their MicroVMs.
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

  def canClaimVM(c: ClaimName): bool = and {
    live(c), idle(c), battery.up,
    not(obj(c).deleting),
    obj(c).hasFinalizer,
    obj(c).status.phase == Pending,
    isNone(obj(c).status.leaseId),
  }

  // ClaimVM succeeds. battery chooses the lease id and answers; the
  // controller holds the answer until it writes the status.
  action ctlClaimVM(c: ClaimName, vm: VMId): bool = all {
    canClaimVM(c),
    battery.warm.contains(vm),
    val lease: Lease = {
      id: battery.nextLease, pool: POOL, vm: vm, host: VM_HOST.get(vm),
      claimedAt: now, expiresAt: now + EXPIRY,
    }
    all {
      battery' = {
        ...battery,
        warm: battery.warm.exclude(Set(vm)),
        handedOut: battery.handedOut.union(Set(vm)),
        leases: battery.leases.put(lease.id, lease),
        nextLease: battery.nextLease + 1,
      },
      setMem(c, ClaimAnswered(lease)),
    },
    now' = now, claims' = claims, hist' = hist,
  }

  //@= docs/requirements/02-claims.md#binding
  //@# If battery's `ClaimVM` fails because the Pool has no warm
  //@# MicroVM, then the Claim Controller SHALL keep the claim in the phase
  //@# `Pending` with the condition `Bound` false and the reason `PoolExhausted`,
  //@# and SHALL retry with backoff.
  action ctlPoolExhausted(c: ClaimName): bool = all {
    canClaimVM(c),
    battery.warm == Set(),
    setClaim(c, { ...obj(c), status: { ...obj(c).status, phase: Pending, bound: { status: CondFalse, reason: PoolExhausted } } }),
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
          boundTime: Some(now),
          leaseExpiresAt: Some(l.expiresAt),
          bound: { status: CondTrue, reason: ClaimBound },
        },
      }),
      setMem(c, Idle),
      hist' = { ...hist, recorded: hist.recorded.union(Set(l.id)) },
    },
    now' = now, battery' = battery,
  }

  //@= docs/requirements/02-claims.md#renewal
  //@# When the `spec.renewTime` of a Bound claim changes, the Claim
  //@# Controller SHALL call battery's `Heartbeat` for the claim's Lease and write
  //@# the expiry time battery returns to the claim's status.
  def canHeartbeat(c: ClaimName): bool = and {
    live(c), idle(c), battery.up,
    not(obj(c).deleting),
    obj(c).status.phase == Bound,
    isSome(obj(c).status.leaseId),
    obj(c).spec.renewTime != obj(c).observedRenewTime,
  }

  // Heartbeat succeeds: battery moves the Lease's expiry, and the
  // controller holds battery's answer until it writes the status.
  action ctlHeartbeat(c: ClaimName): bool = all {
    canHeartbeat(c),
    val l = getOr(obj(c).status.leaseId, 0)
    all {
      holdsUnexpired(l),
      battery' = { ...battery, leases: battery.leases.set(l, { ...battery.leases.get(l), expiresAt: now + EXPIRY }) },
      setMem(c, HeartbeatAnswered({ leaseId: l, expiresAt: now + EXPIRY, renewTime: obj(c).spec.renewTime })),
    },
    now' = now, claims' = claims, hist' = hist,
  }

  //@= docs/requirements/02-claims.md#renewal
  //@# The Claim Controller SHALL take a claim's Lease expiry time only
  //@# from battery's answer to `Heartbeat` or `ClaimVM`, and SHALL NOT compute it.
  action ctlWriteRenewed(c: ClaimName): bool = all {
    live(c),
    isHeartbeatAnswer(ctrl.mem.get(c)),
    val h = heartbeatAnswer(ctrl.mem.get(c))
    all {
      setClaim(c, {
        ...obj(c),
        observedRenewTime: h.renewTime,
        status: { ...obj(c).status, leaseExpiresAt: Some(h.expiresAt) },
      }),
      setMem(c, Idle),
    },
    now' = now, battery' = battery, hist' = hist,
  }

  //@= docs/requirements/02-claims.md#renewal
  //@# If battery reports a claim's Lease as unknown or expired, then
  //@# the Claim Controller SHALL set the claim's phase to `Expired` and its
  //@# condition `Bound` false with the reason `LeaseExpired`.
  pure def expired(o: MicroVMClaim): MicroVMClaim = {
    ...o,
    observedRenewTime: o.spec.renewTime,
    status: { ...o.status, phase: Expired, bound: { status: CondFalse, reason: LeaseExpired } },
  }

  // Heartbeat fails: battery no longer holds the Lease, or it has run out.
  action ctlHeartbeatUnknown(c: ClaimName): bool = all {
    canHeartbeat(c),
    not(holdsUnexpired(getOr(obj(c).status.leaseId, 0))),
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
  //@# When the Lease expiry time battery gave for a Bound claim has
  //@# passed and no `Heartbeat` for that claim has succeeded since, the Claim
  //@# Controller SHALL set the claim's phase to `Expired` and its condition
  //@# `Bound` false with the reason `LeaseExpired`.
  //
  // The controller knows a Heartbeat succeeded only from the answer it
  // holds (HeartbeatAnswered) or from the expiry it wrote. A crash loses
  // the first.
  def canExpiryPassed(c: ClaimName): bool = and {
    live(c), idle(c),
    obj(c).status.phase == Bound,
    getOr(obj(c).status.leaseExpiresAt, 0) <= now,
  }

  action ctlExpiryPassed(c: ClaimName): bool = all {
    canExpiryPassed(c),
    setClaim(c, expired(obj(c))),
    hist' = { ...hist, expiredVia: hist.expiredVia.union(Set("time")) },
    now' = now, battery' = battery, ctrl' = ctrl,
  }

  //@= docs/requirements/02-claims.md#release
  //@# When a claim that has a lease id is deleted, the Claim
  //@# Controller SHALL call battery's `ReleaseVM` for the Lease, and SHALL remove
  //@# the finalizer only once battery has released the Lease or reported it
  //@# unknown.
  def canRelease(c: ClaimName): bool = and {
    live(c), idle(c), battery.up,
    obj(c).deleting,
    obj(c).hasFinalizer,
    isSome(obj(c).status.leaseId),
  }

  // ReleaseVM, then the finalizer. battery deletes the MicroVM; a Lease it
  // no longer holds is "unknown", which also lets the finalizer go.
  action ctlRelease(c: ClaimName): bool = all {
    canRelease(c),
    val l = getOr(obj(c).status.leaseId, 0)
    all {
      battery' = dropLeases(battery, Set(l).intersect(battery.leases.keys())),
      claims' = claims.set(c, Gone({ ...obj(c), hasFinalizer: false })),
      hist' = { ...hist, released: hist.released.union(Set(l)) },
    },
    now' = now, ctrl' = ctrl,
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

  // Every Bound claim whose Lease battery no longer holds becomes Expired
  // (CL-012). Leases no claim records are left alone (CL-031).
  action ctlRecover: bool = all {
    canRecover,
    claims' = CLAIMS.mapBy(c =>
      if (live(c) and obj(c).status.phase == Bound
          and not(holdsUnexpired(getOr(obj(c).status.leaseId, 0))))
        Live(expired(obj(c)))
      else slot(c)),
    ctrl' = { ...ctrl, recovering: false },
    now' = now, battery' = battery, hist' = hist,
  }

  // Whether the controller has any step it can take for any claim. Writing
  // an answer it holds counts, as does a pending recovery.
  def controllerIdle: bool = and {
    not(canRecover),
    not(canReceiveEvent),
    CLAIMS.forall(c => and {
      not(canAddFinalizer(c)),
      not(canClaimVM(c)),
      not(canWriteBound(c)),
      not(canHeartbeat(c)),
      not(canExpiryPassed(c)),
      not(live(c) and not(idle(c))),
      not(canRelease(c)),
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
      ctlRelease(c),
      ctlReleasePending(c),
      ctlRecover,
      ctlEventDeleted(vm),
      ctlExpiryPassed(c),
    }
  }

  // ---------------------------------------------------------------------
  // Invariants. Each names the requirement or ADR consequence it checks.

  // Glossary, "Lease": a Bound claim has exactly one Lease. battery never
  // leases one MicroVM twice, and no two claims record the same Lease or
  // the same MicroVM.
  val vmLeasedToAtMostOneClaim: bool = and {
    battery.leases.keys().forall(l1 => battery.leases.keys().forall(l2 =>
      l1 != l2 implies battery.leases.get(l1).vm != battery.leases.get(l2).vm)),
    CLAIMS.forall(c1 => CLAIMS.forall(c2 =>
      (c1 != c2 and not(isUnborn(slot(c1))) and not(isUnborn(slot(c2)))) implies and {
        not(isSome(obj(c1).status.leaseId) and obj(c1).status.leaseId == obj(c2).status.leaseId),
        not(isSome(obj(c1).status.microVM) and obj(c1).status.microVM == obj(c2).status.microVM),
      })),
  }

  // 02-claims.md, Release: a released MicroVM is deleted by battery and
  // never reused. Nothing handed out, released or expired is warm again,
  // and no Lease names a retired MicroVM.
  val releasedVMNeverReused: bool = and {
    battery.warm.intersect(battery.handedOut) == Set(),
    battery.warm.intersect(battery.retired) == Set(),
    battery.leases.keys().forall(l => not(battery.retired.contains(battery.leases.get(l).vm))),
  }

  // ADR 0001, consequence 2, and the prose under 02-claims.md, Binding: the
  // crash-window orphan is bounded. Nothing renews an orphan, and while
  // battery runs it is gone within the Pool's heartbeat_expiry_threshold of
  // being claimed. (While battery is down nothing expires; the orphan goes
  // as soon as battery is back, in batteryStart.)
  val orphanLeaseBounded: bool =
    orphans.forall(l => and {
      battery.leases.get(l).expiresAt == battery.leases.get(l).claimedAt + EXPIRY,
      battery.up implies now <= battery.leases.get(l).claimedAt + EXPIRY,
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

  // CL-011 and ADR 0001, consequence 3: the expiry a consumer reads is one
  // battery gave, never later than the one battery will enforce.
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
  val boundClaimIsComplete: bool =
    CLAIMS.forall(c => (live(c) and obj(c).status.phase == Bound) implies and {
      isSome(obj(c).status.leaseId),
      isSome(obj(c).status.microVM),
      isSome(obj(c).status.nodeName),
      isSome(obj(c).status.boundTime),
      isSome(obj(c).status.leaseExpiresAt),
      obj(c).status.bound.status == CondTrue,
    })

  // All the invariants CI checks.
  val safety: bool = and {
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
  //@# When the Lease expiry time battery gave for a Bound claim has
  //@# passed and no `Heartbeat` for that claim has succeeded since, the Claim
  //@# Controller SHALL set the claim's phase to `Expired`
  //
  // ADR 0001, consequence 4: a Bound claim's Lease exists in battery, or
  // the claim goes Expired. As a state invariant: once the controller has
  // nothing left to do, every Bound claim's Lease is one battery holds and
  // has not run out. CL-013 makes it prompt, and CL-014 makes it hold when
  // the Events stream drops the deletion (#45).
  val idleMirrorsBattery: bool =
    controllerIdle implies
      CLAIMS.forall(c => (live(c) and obj(c).status.phase == Bound) implies
        holdsUnexpired(getOr(obj(c).status.leaseId, 0)))

  // A claim goes Expired only once battery no longer holds its Lease, or
  // the Lease has run out: the Holder never loses a MicroVM battery still
  // leases to it. Not in safety: CL-014 breaks it after a crash between a
  // Heartbeat and its status write (#57); claims_test.qnt reaches it.
  val expiredOnlyAfterLeaseEnds: bool =
    CLAIMS.forall(c => (live(c) and obj(c).status.phase == Expired) implies
      not(holdsUnexpired(getOr(obj(c).status.leaseId, 0))))

  // ---------------------------------------------------------------------
  // Witnesses: states quint run reports reaching, to show the simulation
  // gets to the interesting interleavings. quint run counts the traces in
  // which each becomes true.

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
}
