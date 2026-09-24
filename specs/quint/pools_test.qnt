// -*- mode: Bluespec; -*-
//
// Scenario tests for the pools and inventory model (pools.qnt). Each run
// walks one interleaving step by step and checks the state it reaches;
// quint test runs every definition whose name ends in Test.
module pools_test {
  import types.* from "./types"
  import pools.* from "./pools"

  run ticks(n: int): bool = n.reps(_ => tick)

  // A Pool declared to battery from scratch: created, finalizer, CreatePool.
  run declare(p: PoolName, sel: Set[Label], n: int): bool =
    createPool(p, sel, n)
      .then(ctlAddFinalizer(p))
      .then(ctlCreatePool(p))

  // A change that has settled goes through one restart window.
  run applySettled: bool =
    invOpenWindow
      .then(ticks(WINDOW))
      .then(invRestart)
      .then(batteryStart)

  // PO-001, PO-010: the Pool's flintlock_hosts are the Hosts its selector
  // matches. h1 and h2 are in zone a.
  run declareTest =
    init
      .then(declare("p1", Set(ZONE_A), 1))
      .expect(and {
        battery.pools.get("p1").flintlockHosts == Set("h1", "h2"),
        obj("p1").status.observedGeneration == 1,
        poolControllerIdle,
        safety,
      })

  // PO-011: relabelling a Node changes the Hosts the selector matches, and
  // the Pool Controller updates battery without a new generation.
  run relabelUpdatesPoolTest =
    init
      .then(declare("p1", Set(ZONE_A), 1))
      .then(relabel("h2", Set(ZONE_B)))
      .expect(and { hostsChanged("p1"), not(poolControllerIdle) })
      .then(ctlUpdatePool("p1"))
      .expect(and {
        battery.pools.get("p1").flintlockHosts == Set("h1"),
        obj("p1").generation == 1,
        poolControllerIdle,
        safety,
      })

  // PO-002 and PO-012: a new selector that matches no Host is sent with
  // UpdatePool, and the Pool says NoEligibleHost.
  run noEligibleHostTest =
    init
      .then(declare("p1", Set(ZONE_A), 1))
      .then(changePool("p1", Set(("zone", "c")), 1))
      .then(ctlUpdatePool("p1"))
      .expect(and {
        battery.pools.get("p1").flintlockHosts == Set(),
        obj("p1").status.observedGeneration == 2,
        obj("p1").status.ready == { status: CondFalse, reason: NoEligibleHost },
        safety,
      })

  // PO-003: DeletePool, then the finalizer.
  run deletePoolTest =
    init
      .then(declare("p1", Set(ZONE_A), 1))
      .then(deletePool("p1"))
      .expect(and { live("p1"), inBattery("p1") })
      .then(ctlDeletePool("p1"))
      .expect(and { isGonePool(slot("p1")), not(inBattery("p1")), safety })

  // IN-011: a report that flaps back within the settle time restarts
  // nothing, and battery keeps the Host.
  run flapAbsorbedTest =
    init
      .then(flipReport("h1"))
      .then(tick)
      .then(flipReport("h1"))
      .then(ticks(SETTLE + WINDOW))
      .expect(and {
        hist.absorbedFlaps == 1,
        hist.restarts == List(),
        battery.hosts == HOSTS,
        not(canOpenWindow),
        safety,
      })

  // IN-002, IN-010, IN-011: a cordoned Host stays in battery until its
  // change has settled and the restart window has closed, and then leaves.
  // Time does not pass a settled change the controller has not acted on.
  run cordonRemovesHostTest =
    init
      .then(cordon("h1"))
      .then(ticks(SETTLE))
      .expect(and { canOpenWindow, battery.hosts.contains("h1") })
      .then(tick)
      .fail()

  run cordonRemovesHostAfterWindowTest =
    init
      .then(cordon("h1"))
      .then(ticks(SETTLE))
      .then(invOpenWindow)
      .then(ticks(WINDOW - 1))
      .expect(and { battery.hosts.contains("h1"), age("h1") == MAX_LAG - 1 })
      .then(tick)
      .expect(and { canCloseWindow, age("h1") == MAX_LAG, safety })
      .then(invRestart)
      .expect(and { not(battery.up), inv.config == Set("h2", "h3") })
      .then(batteryStart)
      .expect(and {
        battery.hosts == Set("h2", "h3"),
        hist.restarts.length() == 1,
        safety,
      })

  // IN-001: until the change is applied, battery may still place a
  // MicroVM on the cordoned Host; within the bound, that is allowed.
  run vmOnCordonedHostWithinBoundTest =
    init
      .then(declare("p1", Set(ZONE_B), 1))
      .then(cordon("h3"))
      .then(ticks(SETTLE))
      .then(invOpenWindow)
      .then(ticks(WINDOW))
      .then(batteryProvision("p1", "h3"))
      .expect(and { witnessVMOnFormerHost, noNewVMOnFormerHost, safety })

  // IN-012: two changes that settle within one window go into one restart.
  run batchedRestartTest =
    init
      .then(cordon("h1"))
      .then(tick)
      .then(flipReport("h3"))
      .then(tick)
      .then(invOpenWindow)
      .then(ticks(WINDOW))
      .then(invRestart)
      .then(batteryStart)
      .expect(and {
        battery.hosts == Set("h2"),
        hist.restarts.length() == 1,
        hist.restarts[0].changed == Set("h1", "h3"),
        safety,
      })

  // IN-011, IN-012: a change that has not settled when a window closes
  // waits for its own window, so the next restart is more than a window
  // later.
  run restartsAWindowApartTest =
    init
      .then(cordon("h1"))
      .then(ticks(SETTLE))
      .then(invOpenWindow)
      .then(ticks(WINDOW - 1))
      .then(cordon("h2"))
      .then(tick)
      .then(invRestart)
      .then(batteryStart)
      .expect(and { battery.hosts == Set("h2", "h3"), not(canOpenWindow) })
      .then(tick)
      .expect(canOpenWindow)
      .then(applySettled)
      .expect(and {
        battery.hosts == Set("h3"),
        hist.restarts.length() == 2,
        hist.restarts[1].at - hist.restarts[0].at == WINDOW + 1,
        safety,
      })

  // A window whose changes flapped back closes without a restart.
  run windowClosesWithoutRestartTest =
    init
      .then(cordon("h1"))
      .then(ticks(SETTLE))
      .then(invOpenWindow)
      .then(uncordon("h1"))
      .then(ticks(WINDOW))
      .then(invRestart)
      .fail()

  // ---------------------------------------------------------------------
  // Findings: each reaches a violation of a property the requirements do
  // not yet guarantee, and fails once they do.

  // #72: nothing orders CreatePool after the finalizer, so a
  // Pool deleted in between is gone from the API server and stays in
  // battery.
  run poolDeletedBeforeFinalizerTest =
    init
      .then(createPool("p1", Set(ZONE_A), 1))
      .then(ctlCreatePool("p1"))
      .then(deletePool("p1"))
      .expect(and {
        isGonePool(slot("p1")),
        inBattery("p1"),
        not(goneLeavesNothing),
        safety,
      })

  // #73: battery restarts without a Host while a Pool still names
  // it. PickHost picks the removed Host, which has none of the Pool's
  // MicroVMs, and provisioning fails until the Pool Controller's
  // UpdatePool drops it.
  run removedHostStallsPoolTest =
    init
      .then(declare("p1", Set(ZONE_A), 2))
      .then(batteryProvision("p1", "h1"))
      .then(cordon("h2"))
      .then(ticks(SETTLE))
      .then(applySettled)
      .expect(and {
        battery.hosts == Set("h1", "h3"),
        battery.pools.get("p1").flintlockHosts == Set("h1", "h2"),
        leastLoaded("p1") == Set("h2"),
        not(poolsNameKnownHosts),
        safety,
      })
      .then(batteryProvisionUnknownHost("p1", "h2"))
      .expect(battery.vms.size() == 1)
      .then(ctlUpdatePool("p1"))
      .then(batteryProvision("p1", "h1"))
      .expect(and { battery.vms.size() == 2, poolsNameKnownHosts, safety })
}
