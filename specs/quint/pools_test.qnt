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

  // A change that removes a Host, once settled, goes through one restart
  // window and the drain (IN-013); no Pool names the Host in these uses.
  run applySettled: bool =
    invOpenWindow
      .then(ticks(WINDOW))
      .then(invDrain)
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
      .then(invDrain)
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
      .then(invDrain)
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
      .then(invDrain)
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

  // PO-001 (#72): CreatePool waits for the stored finalizer, so a Pool
  // deleted before it goes at once and leaves nothing in battery.
  run createBeforeFinalizerTest =
    init
      .then(createPool("p1", Set(ZONE_A), 1))
      .then(ctlCreatePool("p1"))
      .fail()

  run poolDeletedBeforeFinalizerTest =
    init
      .then(createPool("p1", Set(ZONE_A), 1))
      .then(deletePool("p1"))
      .expect(and { isGonePool(slot("p1")), not(inBattery("p1")), goneLeavesNothing, safety })

  // PO-004 over PO-012 (03-pools.md, Placement): a spec battery refuses
  // whose selector matches no Host says Rejected, and battery's Pool keeps
  // the Hosts of the spec it accepted. Once battery accepts a spec again,
  // the Pool says NoEligibleHost.
  run rejectedOverNoEligibleHostTest =
    init
      .then(declare("p1", Set(ZONE_A), 1))
      .then(changePool("p1", Set(("zone", "c")), 1))
      .then(changePoolExpiry("p1", 0))
      .expect(and { not(canUpdate("p1")), canReject("p1") })
      .then(ctlRejectPool("p1"))
      .expect(and {
        obj("p1").status.ready == rejectedCondition,
        resolve("p1") == Set(),
        battery.pools.get("p1").flintlockHosts == Set("h1", "h2"),
        obj("p1").status.observedGeneration == 1,
        poolControllerIdle,
        witnessRejectedWithoutHost,
        safety,
      })
      .then(changePoolExpiry("p1", EXPIRY))
      .then(ctlUpdatePool("p1"))
      .expect(and {
        obj("p1").status.ready == { status: CondFalse, reason: NoEligibleHost },
        battery.pools.get("p1").flintlockHosts == Set(),
        poolControllerIdle,
        safety,
      })

  // ---------------------------------------------------------------------
  // IN-013 (#73): the Pools drop a leaving Host before the restart that
  // removes it, and gain a joining Host only after the restart that adds
  // it.

  // p1 selects h1 and h2 and has a MicroVM on h1; h2 is cordoned, and its
  // change has settled and its window closed.
  run cordonNamedHost: bool =
    init
      .then(declare("p1", Set(ZONE_A), 2))
      .then(batteryProvision("p1", "h1"))
      .then(cordon("h2"))
      .then(ticks(SETTLE))
      .then(invOpenWindow)
      .then(ticks(WINDOW))

  // battery does not restart without h2 while p1 names it: first the
  // Hosts that remain are published, and even then not before the Pool
  // Controller has updated p1. Nor may the drain timeout pass while the
  // Pool Controller still has p1 to update.
  run restartWaitsForThePoolsTest =
    cordonNamedHost
      .then(invRestart)
      .fail()

  run drainedToPoolsNamingLeaving: bool =
    cordonNamedHost
      .then(invDrain)
      .expect(and { inv.published == Set("h1", "h3"), hostsChanged("p1"), poolsNamingLeaving == Set("p1") })

  run restartWaitsForThePoolsAfterDrainTest =
    drainedToPoolsNamingLeaving
      .then(invRestart)
      .fail()

  run drainTimeoutWaitsForThePoolControllerTest =
    drainedToPoolsNamingLeaving
      .then(invRestartPastDrain)
      .fail()

  // The Pool Controller drops h2, battery restarts without it, and p1
  // replenishes on h1: battery never picks a Host it does not know.
  // This is the scenario #73 found stalling the Pool.
  run removedHostDrainedTest =
    cordonNamedHost
      .then(invDrain)
      .then(ctlUpdatePool("p1"))
      .expect(and { battery.pools.get("p1").flintlockHosts == Set("h1"), poolsNamingLeaving == Set() })
      .then(invRestart)
      .then(batteryStart)
      .expect(and {
        battery.hosts == Set("h1", "h3"),
        leastLoaded("p1") == Set("h1"),
        poolsNameKnownHosts,
        witnessDrainedForPool,
        safety,
      })
      .then(batteryProvision("p1", "h1"))
      .expect(and { battery.vms.size() == 2, safety })

  // h2 is deleted and comes back: p1 names it again only after battery
  // has restarted with it.
  run joiningHostAfterRestartTest =
    init
      .then(declare("p1", Set(ZONE_A), 1))
      .then(deleteNode("h2"))
      .then(ctlUpdatePool("p1"))
      .then(ticks(SETTLE))
      .then(applySettled)
      .then(createNode("h2", Set(ZONE_A), true))
      .then(ticks(SETTLE))
      .then(invOpenWindow)
      .then(ticks(WINDOW))
      .expect(and { inv.published == Set("h1", "h3"), not(hostsChanged("p1")) })
      .then(invRestart)
      .expect(and { not(battery.up), battery.pools.get("p1").flintlockHosts == Set("h1") })
      .then(batteryStart)
      .expect(and { hostsChanged("p1"), poolsNameKnownHosts, safety })
      .then(ctlUpdatePool("p1"))
      .expect(and { battery.pools.get("p1").flintlockHosts == Set("h1", "h2"), safety })

  // The drain timeout: a Pool whose spec battery refuses (PO-004) is one
  // the Pool Controller cannot update, since UpdatePool cannot carry the
  // Hosts without the spec, so once it has nothing else to do the restart
  // goes ahead, and battery can pick the removed Host for that Pool.
  // poolsNameKnownHosts holds for every other Pool.
  run refusedPastDrainTest =
    init
      .then(declare("p1", Set(ZONE_A), 1))
      .then(changePoolExpiry("p1", 0))
      .then(ctlRejectPool("p1"))
      .then(cordon("h2"))
      .then(ticks(SETTLE))
      .then(invOpenWindow)
      .then(ticks(WINDOW))
      .then(invDrain)
      .expect(and { poolControllerIdle, poolsNamingLeaving == Set("p1") })
      .then(invRestartPastDrain)
      .then(batteryStart)
      .expect(and { hist.staleAfterDrain == Set("p1"), poolsNameKnownHosts, safety })
      .then(batteryProvisionUnknownHost("p1", "h2"))
      .expect(and { witnessUnknownHostPicked, safety })

  // DP-007: a renewed client certificate restarts battery, one restart
  // window later, and battery then presents it. The restart removes no
  // Host, so there is nothing to drain (IN-013): it goes straight ahead.
  run renewalRestartsBatteryTest =
    init
      .then(renewCertificate)
      .expect(and { canOpenWindow, battery.cert != secret.cert, certificateFollowsSecret })
      .then(invOpenWindow)
      .then(ticks(WINDOW))
      .expect(and { leaving == Set(), inv.published == remaining, poolsNamingLeaving == Set() })
      .then(invRestart)
      .then(batteryStart)
      .expect(and {
        battery.cert == secret.cert,
        hist.restarts.length() == 1,
        hist.restarts[0].renewed,
        hist.restarts[0].changed == Set(),
        not(canOpenWindow),
        safety,
      })

  // DP-008: a renewal while a window is open for a Host change goes into
  // the same restart, after the drain for the Host it removes.
  run renewalJoinsHostWindowTest =
    init
      .then(cordon("h1"))
      .then(ticks(SETTLE))
      .then(invOpenWindow)
      .then(tick)
      .then(renewCertificate)
      .expect(not(canOpenWindow))
      .then(ticks(WINDOW - 1))
      .then(invDrain)
      .then(invRestart)
      .then(batteryStart)
      .expect(and {
        battery.hosts == Set("h2", "h3"),
        battery.cert == secret.cert,
        hist.restarts.length() == 1,
        hist.restarts[0].renewed,
        hist.restarts[0].changed == Set("h1"),
        not(canOpenWindow),
        safety,
      })
}
