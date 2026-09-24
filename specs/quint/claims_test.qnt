// -*- mode: Bluespec; -*-
//
// Scenario tests for the claim lifecycle model (claims.qnt). Each run walks
// one interleaving step by step and checks the state it reaches; quint test
// runs every definition whose name ends in Test.
module claims_test {
  import types.* from "./types"
  import claims.* from "./claims"

  // A claim bound to vm from scratch: created, finalizer, ClaimVM, status,
  // and the Lease's expiry read with ListLeases (CL-016), since battery's
  // answer to ClaimVM carries none.
  run bind(c: ClaimName, vm: VMId): bool =
    createClaim(c)
      .then(ctlAddFinalizer(c))
      .then(replenish(vm))
      .then(ctlClaimVM(c, vm))
      .then(ctlWriteBound(c))
      .then(ctlListRenewed(c))

  run ticks(n: int): bool = n.reps(_ => tick)

  // CL-011 and CL-016: the binding is written with no expiry time, and
  // ListLeases gives battery's.
  run boundWithoutExpiryTest =
    init
      .then(createClaim("a"))
      .then(ctlAddFinalizer("a"))
      .then(replenish("v1"))
      .then(ctlClaimVM("a", "v1"))
      .then(ctlWriteBound("a"))
      .expect(and {
        obj("a").status.phase == Bound,
        obj("a").status.leaseExpiresAt == None,
        canExpiryPassed("a"),
        not(controllerIdle),
        safety,
      })
      .then(ctlListRenewed("a"))
      .expect(and {
        obj("a").status.leaseExpiresAt == Some(EXPIRY),
        controllerIdle,
        safety,
      })

  run bindTest =
    init
      .then(bind("a", "v1"))
      .expect(and {
        obj("a").status.phase == Bound,
        obj("a").status.leaseId == Some(1),
        obj("a").status.microVM == Some("v1"),
        obj("a").status.nodeName == Some("h1"),
        obj("a").status.leaseExpiresAt == Some(EXPIRY),
        obj("a").hasFinalizer,
        safety,
      })

  // CL-003: an empty Pool keeps the claim Pending, and nothing is leased.
  run poolExhaustedTest =
    init
      .then(createClaim("a"))
      .then(ctlAddFinalizer("a"))
      .then(ctlPoolExhausted("a"))
      .expect(and {
        obj("a").status.phase == Pending,
        obj("a").status.bound == { status: CondFalse, reason: PoolExhausted },
        battery.leases.keys() == Set(),
      })

  // CL-001: ClaimVM waits for the finalizer.
  run claimVMNeedsFinalizerTest =
    init
      .then(createClaim("a"))
      .then(replenish("v1"))
      .then(ctlClaimVM("a", "v1"))
      .fail()

  // ADR 0001, consequence 2: a crash between ClaimVM and the status write
  // orphans the Lease, and the retry claims a second MicroVM. The orphan
  // is never renewed, and battery expires it after the threshold.
  run crashWindowOrphanTest =
    init
      .then(createClaim("a"))
      .then(ctlAddFinalizer("a"))
      .then(replenish("v1"))
      .then(replenish("v2"))
      .then(ctlClaimVM("a", "v1"))
      .then(crashController)
      .then(ctlRecover)
      .then(ctlClaimVM("a", "v2"))
      .then(ctlWriteBound("a"))
      .expect(and {
        orphans == Set(1),
        obj("a").status.leaseId == Some(2),
        safety,
      })
      .then(ticks(EXPIRY))
      .then(batteryExpire)
      .expect(and { orphans == Set(), not(holds(1)), safety })

  // CL-020: release, then the finalizer; the MicroVM is never warm again.
  run releaseTest =
    init
      .then(bind("a", "v1"))
      .then(deleteClaim("a"))
      .expect(obj("a").deleting and live("a"))
      .then(ctlReleaseVM("a"))
      .expect(and { live("a"), obj("a").hasFinalizer, not(holds(1)) })
      .then(ctlRemoveFinalizer("a"))
      .expect(and {
        isGone(slot("a")),
        not(holds(1)),
        battery.retired.contains("v1"),
        safety,
      })
      .then(replenish("v1"))
      .fail()

  // CL-021: a claim that never bound goes without a call to battery.
  run deletePendingClaimTest =
    init
      .then(createClaim("a"))
      .then(ctlAddFinalizer("a"))
      .then(deleteClaim("a"))
      .then(ctlReleasePending("a"))
      .expect(and { isGone(slot("a")), battery.leases.keys() == Set() })

  // CL-010 and CL-011: a renewal becomes Heartbeat, and the status takes
  // battery's expiry.
  run renewTest =
    init
      .then(bind("a", "v1"))
      .then(tick)
      .then(renewClaim("a"))
      .then(ctlHeartbeat("a"))
      .then(ctlWriteRenewed("a"))
      .expect(and {
        obj("a").status.leaseExpiresAt == Some(1 + EXPIRY),
        battery.leases.get(1).expiresAt == 1 + EXPIRY,
        safety,
      })

  // CL-012: a renewal of a Lease battery has expired makes the claim
  // Expired.
  run renewAfterExpiryTest =
    init
      .then(bind("a", "v1"))
      .then(ticks(EXPIRY))
      .then(batteryExpire)
      .then(renewClaim("a"))
      .then(ctlHeartbeatUnknown("a"))
      .expect(and {
        obj("a").status.phase == Expired,
        obj("a").status.bound == { status: CondFalse, reason: LeaseExpired },
        safety,
      })

  // CL-030 with CL-012: battery is down past a Lease's expiry. The Lease
  // is still held when battery starts, since the first sweep is a sweep
  // interval later (BA-022), but recovery reads it with ListLeases, sees
  // that its expiry has passed (BA-041), and makes the claim Expired. The
  // sweep then deletes the Lease.
  run recoveryTest =
    init
      .then(bind("a", "v1"))
      .then(batteryStop)
      .then(ticks(EXPIRY + 1))
      .then(batteryStart)
      .expect(and { ctrl.recovering, holds(1), not(holdsUnexpired(1)) })
      .then(ctlRecover)
      .expect(and { obj("a").status.phase == Expired, not(ctrl.recovering), safety })
      .then(ticks(SWEEP))
      .then(batteryExpire)
      .expect(and { not(holds(1)), safety })

  // BA-022: after a restart, time cannot pass one sweep interval with a
  // Lease that expired while battery was down still unswept.
  run sweepDueAfterRestartTest =
    init
      .then(bind("a", "v1"))
      .then(batteryStop)
      .then(ticks(EXPIRY + 1))
      .then(batteryStart)
      .then(ticks(SWEEP))
      .then(tick)
      .fail()

  // BA-022 with BA-010: a Lease that expired while battery was down can
  // still be renewed after the start, until the first sweep.
  run renewedAfterRestartTest =
    init
      .then(bind("a", "v1"))
      .then(batteryStop)
      .then(ticks(EXPIRY + 1))
      .then(batteryStart)
      .then(renewClaim("a"))
      .expect(witnessLateHeartbeat)
      .then(ctlHeartbeat("a"))
      .then(ctlWriteRenewed("a"))
      .expect(and {
        obj("a").status.phase == Bound,
        obj("a").status.leaseExpiresAt == Some(EXPIRY + 1 + EXPIRY),
        holdsUnexpired(1),
        safety,
      })

  // BA-010 and BA-020: a Lease past its expiry waits for the sweep, and a
  // Heartbeat in that time renews it.
  run lateHeartbeatRenewsTest =
    init
      .then(bind("a", "v1"))
      .then(ticks(EXPIRY + 1))
      .expect(and { holds(1), not(holdsUnexpired(1)), witnessUnsweptLease })
      .then(renewClaim("a"))
      .expect(witnessLateHeartbeat)
      .then(ctlHeartbeat("a"))
      .then(ctlWriteRenewed("a"))
      .expect(and {
        obj("a").status.phase == Bound,
        obj("a").status.leaseExpiresAt == Some(EXPIRY + 1 + EXPIRY),
        battery.leases.get(1).expiresAt == EXPIRY + 1 + EXPIRY,
        safety,
      })

  // BA-020: while battery runs, time cannot pass a Lease's expiry by a
  // sweep interval without a sweep.
  run sweepWithinIntervalTest =
    init
      .then(bind("a", "v1"))
      .then(ticks(EXPIRY + SWEEP))
      .then(tick)
      .fail()

  // #85: the Holder renews before the expiry, and the controller has not
  // relayed the renewal when the expiry passes. battery still holds the
  // Lease until the sweep and would renew it (BA-010), so the claim is not
  // expired on time while the renewal is pending (CL-014, CL-016); the
  // Heartbeat goes first (CL-010), and the claim keeps its Lease.
  run renewalNotLostToExpiryTest =
    init
      .then(bind("a", "v1"))
      .then(ticks(EXPIRY - 1))
      .then(renewClaim("a"))
      .then(tick)
      .expect(and { not(canExpiryPassed("a")), canHeartbeat("a"), holds(1), not(holdsUnexpired(1)) })
      .then(ctlExpiryPassed("a"))
      .fail()

  run renewalKeptPastExpiryTest =
    init
      .then(bind("a", "v1"))
      .then(ticks(EXPIRY - 1))
      .then(renewClaim("a"))
      .then(tick)
      .then(ctlHeartbeat("a"))
      .then(ctlWriteRenewed("a"))
      .expect(and {
        obj("a").status.phase == Bound,
        obj("a").status.leaseExpiresAt == Some(EXPIRY + EXPIRY),
        holdsUnexpired(1),
        safety,
      })

  // #85: a renewal relayed only after the sweep is too late. battery
  // answers NOT_FOUND, and CL-012 expires the claim; battery would have
  // refused the Heartbeat, so pendingRenewalKeptWhileHeld holds.
  run renewalAfterSweepTooLateTest =
    init
      .then(bind("a", "v1"))
      .then(ticks(EXPIRY - 1))
      .then(renewClaim("a"))
      .then(ticks(2))
      .then(batteryExpire)
      .then(ctlHeartbeatUnknown("a"))
      .expect(and { obj("a").status.phase == Expired, pendingRenewal("a"), not(holds(1)), safety })

  // With no renewal pending, the claim goes Expired as soon as battery
  // lists its expiry as passed, before the sweep deletes the Lease (CL-014).
  run unrenewedClaimExpiresBeforeSweepTest =
    init
      .then(bind("a", "v1"))
      .then(ticks(EXPIRY))
      .expect(and { holds(1), not(holdsUnexpired(1)), canExpiryPassed("a") })
      .then(ctlExpiryPassed("a"))
      .expect(and { obj("a").status.phase == Expired, holds(1), safety })

  // #85 across a restart of battery: the Holder renews while battery is
  // down and the Lease runs out. Recovery finds the Lease held past its
  // expiry, but the renewal is pending, so the claim stays Bound and the
  // Heartbeat renews it before the first sweep (BA-022).
  run renewalKeptAcrossRestartTest =
    init
      .then(bind("a", "v1"))
      .then(batteryStop)
      .then(renewClaim("a"))
      .then(ticks(EXPIRY + 1))
      .then(batteryStart)
      .then(ctlRecover)
      .expect(and { obj("a").status.phase == Bound, holds(1), canHeartbeat("a"), safety })
      .then(ctlHeartbeat("a"))
      .then(ctlWriteRenewed("a"))
      .expect(and { obj("a").status.phase == Bound, holdsUnexpired(1), safety })

  // The finding of #86: a crash-window orphan outlives the Pool's
  // heartbeat_expiry_threshold by up to a sweep interval, which the prose
  // under 02-claims.md, Binding, and ADR 0001, consequence 2, do not allow
  // for. orphanLeaseBounded, the bound battery gives, still holds. #86
  // corrects the prose; the model already has battery's bound.
  run orphanOutlivesExpiryTest =
    init
      .then(createClaim("a"))
      .then(ctlAddFinalizer("a"))
      .then(replenish("v1"))
      .then(ctlClaimVM("a", "v1"))
      .then(crashController)
      .then(ctlRecover)
      .then(ticks(EXPIRY + 1))
      .expect(and {
        orphans == Set(1),
        not(orphanGoneByExpiry),
        safety,
      })

  // CL-013 (#45): battery expires an unrenewed Lease, and the Events stream
  // reports the deleted MicroVM; the claim goes Expired.
  run unrenewedClaimExpiresByEventTest =
    init
      .then(bind("a", "v1"))
      .then(ticks(EXPIRY))
      .then(batteryExpire)
      .expect(and { not(holds(1)), obj("a").status.phase == Bound, not(controllerIdle) })
      .then(ctlEventDeleted("v1"))
      .expect(and {
        obj("a").status.phase == Expired,
        obj("a").status.bound == { status: CondFalse, reason: LeaseExpired },
        controllerIdle,
        safety,
      })

  // CL-014 (#45): the Events stream drops the deletion; the expiry battery
  // gave has passed, so the claim goes Expired anyway.
  run unrenewedClaimExpiresByTimeTest =
    init
      .then(bind("a", "v1"))
      .then(ticks(EXPIRY))
      .then(batteryExpire)
      .then(dropEvent("v1"))
      .expect(and { obj("a").status.phase == Bound, canExpiryPassed("a") })
      .then(ctlExpiryPassed("a"))
      .expect(and { obj("a").status.phase == Expired, controllerIdle, safety })

  // The finding of #57: a crash between a successful Heartbeat and its
  // status write leaves the old expiry in the status. CL-014 alone would
  // expire a claim whose Lease battery has just renewed. The relayed
  // renewTime is written with the expiry (CL-010), so the renewal is still
  // pending after the crash, nothing expires the claim, and the Heartbeat
  // is sent again.
  run renewedClaimKeptAfterCrashTest =
    init
      .then(bind("a", "v1"))
      .then(ticks(2))
      .then(renewClaim("a"))
      .then(ctlHeartbeat("a"))
      .then(crashController)
      .then(ctlRecover)
      .then(tick)
      .expect(and { not(canExpiryPassed("a")), canHeartbeat("a"), holdsUnexpired(1) })
      .then(ctlExpiryPassed("a"))
      .fail()

  run renewedClaimRenewedAgainAfterCrashTest =
    init
      .then(bind("a", "v1"))
      .then(ticks(2))
      .then(renewClaim("a"))
      .then(ctlHeartbeat("a"))
      .then(crashController)
      .then(ctlRecover)
      .then(tick)
      .then(ctlHeartbeat("a"))
      .then(ctlWriteRenewed("a"))
      .expect(and {
        obj("a").status.phase == Bound,
        obj("a").status.leaseExpiresAt == Some(3 + EXPIRY),
        not(canExpiryPassed("a")),
        safety,
      })

  // CL-007 and CL-008: ClaimVM fails in transit after battery committed
  // the Lease. The claim stays Pending with BatteryUnavailable, the retry
  // binds a second MicroVM, and the first Lease is an orphan that expires.
  run claimVMAnswerLostTest =
    init
      .then(createClaim("a"))
      .then(ctlAddFinalizer("a"))
      .then(replenish("v1"))
      .then(replenish("v2"))
      .then(ctlClaimVMLost("a", "v1"))
      .expect(and {
        obj("a").status.phase == Pending,
        obj("a").status.bound == { status: CondFalse, reason: BatteryUnavailable },
        obj("a").status.synced == { status: CondFalse, reason: BatteryUnavailable },
        orphans == Set(1),
        canClaimVM("a"),
        safety,
      })
      .then(tick)
      .then(ctlClaimVM("a", "v2"))
      .then(ctlWriteBound("a"))
      .expect(and {
        obj("a").status.leaseId == Some(2),
        obj("a").status.synced.status == CondTrue,
        orphans == Set(1),
        safety,
      })
      .then(ticks(EXPIRY - 1))
      .then(batteryExpire)
      .expect(and { orphans == Set(), obj("a").status.phase == Bound, safety })

  // CL-015: Heartbeat fails in transit after battery renewed. The claim
  // stays Bound with the old expiry, the renewal stays pending, and the
  // retry renews again; nothing expires the claim in between, even once
  // the old expiry has passed.
  run heartbeatAnswerLostTest =
    init
      .then(bind("a", "v1"))
      .then(ticks(2))
      .then(renewClaim("a"))
      .then(ctlHeartbeatLost("a"))
      .expect(and {
        obj("a").status.phase == Bound,
        obj("a").status.leaseExpiresAt == Some(EXPIRY),
        obj("a").status.synced.status == CondFalse,
        battery.leases.get(1).expiresAt == 2 + EXPIRY,
        canHeartbeat("a"),
        safety,
      })
      .then(tick)
      .expect(not(canExpiryPassed("a")))
      .then(ctlHeartbeat("a"))
      .then(ctlWriteRenewed("a"))
      .expect(and {
        obj("a").status.phase == Bound,
        obj("a").status.leaseExpiresAt == Some(3 + EXPIRY),
        obj("a").status.synced.status == CondTrue,
        safety,
      })

  // CL-017: while battery is down, ListLeases fails and the claim whose
  // status expiry has passed stays Bound. Once battery is back, recovery
  // finds the Lease gone and the claim goes Expired.
  run listLeasesUnavailableTest =
    init
      .then(bind("a", "v1"))
      .then(batteryStop)
      .then(ticks(EXPIRY + 1))
      .expect(canExpiryPassed("a"))
      .then(ctlListUnanswered("a"))
      .expect(and {
        obj("a").status.phase == Bound,
        obj("a").status.synced == { status: CondFalse, reason: BatteryUnavailable },
        safety,
      })
      .then(batteryStart)
      .then(ctlRecover)
      .expect(and { obj("a").status.phase == Expired, safety })

  // CL-022 with CL-020: ReleaseVM fails in transit after battery released
  // the Lease. The finalizer stays; the retry finds the Lease unknown, and
  // the finalizer goes.
  run releaseAnswerLostTest =
    init
      .then(bind("a", "v1"))
      .then(deleteClaim("a"))
      .then(ctlReleaseLost("a"))
      .expect(and {
        live("a"),
        obj("a").hasFinalizer,
        not(holds(1)),
        canRelease("a"),
        safety,
      })
      .then(ctlReleaseVM("a"))
      .then(ctlRemoveFinalizer("a"))
      .expect(and { isGone(slot("a")), safety })

  // CL-022: battery answers UNAVAILABLE and keeps the Lease (flintlockd has
  // not confirmed the deletion). The finalizer stays until a retry
  // releases it.
  run releaseUnavailableTest =
    init
      .then(bind("a", "v1"))
      .then(deleteClaim("a"))
      .then(ctlReleaseUnanswered("a"))
      .expect(and { live("a"), obj("a").hasFinalizer, holds(1), safety })
      .then(ctlReleaseVM("a"))
      .then(ctlRemoveFinalizer("a"))
      .expect(and { isGone(slot("a")), not(holds(1)), safety })

  // CL-020: a crash between ReleaseVM and the finalizer write loses the
  // answer; the next ReleaseVM finds the Lease unknown and the claim goes.
  run crashBetweenReleaseAndFinalizerTest =
    init
      .then(bind("a", "v1"))
      .then(deleteClaim("a"))
      .then(ctlReleaseVM("a"))
      .then(crashController)
      .then(ctlRecover)
      .expect(and { live("a"), obj("a").hasFinalizer, canRelease("a") })
      .then(ctlReleaseVM("a"))
      .then(ctlRemoveFinalizer("a"))
      .expect(and { isGone(slot("a")), safety })
}
