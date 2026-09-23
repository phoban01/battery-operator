// -*- mode: Bluespec; -*-
//
// Scenario tests for the claim lifecycle model (claims.qnt). Each run walks
// one interleaving step by step and checks the state it reaches; quint test
// runs every definition whose name ends in Test.
module claims_test {
  import types.* from "./types"
  import claims.* from "./claims"

  // A claim bound to vm from scratch: created, finalizer, ClaimVM, status.
  run bind(c: ClaimName, vm: VMId): bool =
    createClaim(c)
      .then(ctlAddFinalizer(c))
      .then(replenish(vm))
      .then(ctlClaimVM(c, vm))
      .then(ctlWriteBound(c))

  run ticks(n: int): bool = n.reps(_ => tick)

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
      .then(ctlRelease("a"))
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

  // CL-030 with CL-012: battery is down past a Lease's expiry, expires it
  // on start, and recovery makes the claim Expired.
  run recoveryTest =
    init
      .then(bind("a", "v1"))
      .then(batteryStop)
      .then(ticks(EXPIRY + 1))
      .then(batteryStart)
      .expect(ctrl.recovering and not(holds(1)))
      .then(ctlRecover)
      .expect(and { obj("a").status.phase == Expired, not(ctrl.recovering), safety })

  // The finding behind idleMirrorsBattery: a Holder that stops renewing
  // leaves its claim Bound after battery has expired the Lease, and no
  // requirement gives the controller a step to take.
  run unrenewedClaimStaysBoundTest =
    init
      .then(bind("a", "v1"))
      .then(ticks(EXPIRY))
      .then(batteryExpire)
      .expect(and {
        not(holds(1)),
        obj("a").status.phase == Bound,
        controllerIdle,
        not(idleMirrorsBattery),
      })
}
