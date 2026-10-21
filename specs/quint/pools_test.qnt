// -*- mode: Bluespec; -*-
//
// Scenario tests for the pools and inventory model (pools.qnt). Each run
// walks one interleaving step by step and checks the state it reaches;
// quint test runs every definition whose name ends in Test.
module pools_test {
  import types.* from "./types"
  import pools.* from "./pools"

  run ticks(n: int): bool = n.reps(_ => tick)

  // A fresh install: battery holds every Host with its TLS settings, and
  // the Inventory Controller, just started, gives every Host the
  // certificate once (DP-035).
  run fresh: bool = initWith(false).then(invRefresh)

  // A Pool declared to battery from scratch: created, finalizer, CreatePool.
  run declare(p: PoolName, sel: Set[Label], n: int): bool =
    createPool(p, sel, n)
      .then(ctlAddFinalizer(p))
      .then(ctlCreatePool(p))

  // PO-001, PO-010: the Pool's flintlock_hosts are the Hosts its selector
  // matches. h1 and h2 are in zone a.
  run declareTest =
    fresh
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
    fresh
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
    fresh
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
    fresh
      .then(declare("p1", Set(ZONE_A), 1))
      .then(deletePool("p1"))
      .expect(and { live("p1"), inBattery("p1") })
      .then(ctlDeletePool("p1"))
      .expect(and { isGonePool(slot("p1")), not(inBattery("p1")), safety })

  // PO-042, PO-044, BA-070 (#199): battery refuses to delete a Pool while
  // a claim holds one of its MicroVMs. The Pool Controller gives it its
  // drained spec, so battery adds no MicroVM, and takes none itself; once
  // the claim releases its MicroVM, battery deletes the Pool with the
  // available one.
  run deleteFilledPoolTest =
    fresh
      .then(declare("p1", Set(ZONE_A), 2))
      .then(batteryProvision("p1", "h1"))
      .then(batteryProvision("p1", "h2"))
      .then(leaseVM("p1", "h1"))
      .then(deletePool("p1"))
      .expect(and { deletionRefused("p1"), not(canDelete("p1")) })
      .then(ctlDrainSpec("p1"))
      .expect(and {
        battery.pools.get("p1").size == 0,
        not(canProvision("p1")),
        vmsOf("p1").size() == 2,
        deletionRefused("p1"),
        poolControllerIdle,
        safety,
      })
      .then(releaseVM("p1", "h1"))
      .then(ctlDeletePool("p1"))
      .expect(and {
        isGonePool(slot("p1")),
        not(inBattery("p1")),
        battery.vms == Set(),
        hist.drainedVMs == 1,
        safety,
      })

  // CL-063 (#229): no claim leases a MicroVM of a deleted Pool, though
  // battery still holds the Pool with an available one.
  run noLeaseOnDeletedPoolTest =
    fresh
      .then(declare("p1", Set(ZONE_A), 2))
      .then(batteryProvision("p1", "h1"))
      .then(batteryProvision("p1", "h2"))
      .then(leaseVM("p1", "h1"))
      .then(deletePool("p1"))
      .then(ctlDrainSpec("p1"))
      .expect(and { deletionRefused("p1"), vmsOf("p1").exists(v => not(v.leased)) })
      .then(leaseVM("p1", "h2"))
      .fail()

  // BA-070: battery deletes a Pool no claim holds with its available
  // MicroVMs, in one DeletePool.
  run deleteAvailablePoolTest =
    fresh
      .then(declare("p1", Set(ZONE_A), 1))
      .then(batteryProvision("p1", "h1"))
      .then(deletePool("p1"))
      .expect(not(deletionRefused("p1")))
      .then(ctlDeletePool("p1"))
      .expect(and { not(inBattery("p1")), battery.vms == Set(), safety })

  // BA-070: DeletePool is refused while a claim holds a MicroVM.
  run deleteRefusedWhileLeasedTest =
    fresh
      .then(declare("p1", Set(ZONE_A), 1))
      .then(batteryProvision("p1", "h1"))
      .then(leaseVM("p1", "h1"))
      .then(deletePool("p1"))
      .then(ctlDeletePool("p1"))
      .fail()

  // PO-001 (#72): CreatePool waits for the stored finalizer, so a Pool
  // deleted before it goes at once and leaves nothing in battery.
  run createBeforeFinalizerTest =
    fresh
      .then(createPool("p1", Set(ZONE_A), 1))
      .then(ctlCreatePool("p1"))
      .fail()

  run poolDeletedBeforeFinalizerTest =
    fresh
      .then(createPool("p1", Set(ZONE_A), 1))
      .then(deletePool("p1"))
      .expect(and { isGonePool(slot("p1")), not(inBattery("p1")), goneLeavesNothing, safety })

  // PO-004 over PO-012 (03-pools.md, Placement): a spec battery refuses
  // whose selector matches no Host says Rejected, and battery's Pool keeps
  // the Hosts of the spec it accepted. Once battery accepts a spec again,
  // the Pool says NoEligibleHost.
  run rejectedOverNoEligibleHostTest =
    fresh
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
  // The Inventory Controller (04-inventory.md, ADR 0008)

  // IN-011: a report that flaps back within the settle time reaches
  // battery not at all.
  run flapAbsorbedTest =
    fresh
      .then(flipReport("h1"))
      .then(tick)
      .then(flipReport("h1"))
      .then(ticks(SETTLE + 1))
      .expect(and {
        hist.absorbedFlaps == 1,
        hist.cordons == 0,
        HOSTS.forall(h => active(h)),
        inv.published == HOSTS,
        safety,
      })

  // IN-011, IN-015, IN-018: a cordoned Node's Host stays uncordoned in
  // battery until its change has settled; then time does not pass until
  // the Inventory Controller has taken it from the Pool Controller and
  // cordoned it, and battery keeps it.
  run cordonedNode: bool =
    fresh
      .then(cordon("h1"))
      .then(ticks(SETTLE))

  run settledCordonActsAtOnceTest =
    cordonedNode
      .expect(and { active("h1"), canUnpublish("h1"), not(canCordon("h1")) })
      .then(tick)
      .fail()

  run cordonTest =
    cordonedNode
      .then(invUnpublish("h1"))
      .expect(canCordon("h1"))
      .then(invCordon("h1"))
      .expect(and {
        held("h1"),
        bhost("h1").cordoned,
        not(inv.published.contains("h1")),
        witnessCordoned,
        safety,
      })
      .then(tick)
      .expect(safety)

  // IN-017: the Node is uncordoned; once that has settled, the Host is
  // uncordoned in battery and given back to the Pool Controller.
  run uncordonTest =
    cordonedNode
      .then(invUnpublish("h1"))
      .then(invCordon("h1"))
      .then(uncordon("h1"))
      .then(ticks(SETTLE))
      .expect(canRegister("h1"))
      .then(invRegister("h1"))
      .expect(and {
        active("h1"),
        inv.published.contains("h1"),
        hist.uncordons == 1,
        hist.adds == 0,
        safety,
      })

  // IN-001: until the change settles, battery may still place a MicroVM
  // on the cordoned Node's Host; within the bound, that is allowed.
  run vmOnCordonedHostWithinBoundTest =
    fresh
      .then(declare("p1", Set(ZONE_B), 1))
      .then(cordon("h3"))
      .then(ticks(SETTLE))
      .then(batteryProvision("p1", "h3"))
      .expect(and { witnessVMOnFormerHost, noNewVMOnFormerHost, safety })

  // BA-084: once the Host is cordoned, battery places nothing on it, even
  // while a Pool still names it.
  run noVMOnCordonedHostTest =
    fresh
      .then(declare("p1", Set(ZONE_A), 2))
      .then(cordon("h2"))
      .then(ticks(SETTLE))
      .then(invUnpublish("h2"))
      .then(invCordon("h2"))
      .expect(and { battery.pools.get("p1").flintlockHosts.contains("h2"), eligible("p1") == Set("h1") })
      .then(batteryProvision("p1", "h2"))
      .fail()

  // IN-016, BA-083: a deleted Node's Host is cordoned, and battery refuses
  // to remove it while a Pool names it and a claim holds a MicroVM on it.
  // The Inventory Controller tries again, and the removal goes through
  // once the Pool Controller has dropped it and the claim has ended.
  run deletedNodeWithVM: bool =
    fresh
      .then(declare("p1", Set(ZONE_A), 2))
      .then(batteryProvision("p1", "h1"))
      .then(batteryProvision("p1", "h2"))
      .then(leaseVM("p1", "h2"))
      .then(deleteNode("h2"))
      .then(ticks(SETTLE))
      .then(invUnpublish("h2"))
      .then(invCordon("h2"))

  run removalRefusedTest =
    deletedNodeWithVM
      .expect(and { wantsRemoved("h2"), not(removable("h2")) })
      .then(invRemove("h2"))
      .fail()

  run removalWaitsForThePoolsTest =
    deletedNodeWithVM
      .then(invRemoveRefused("h2"))
      .then(tick)
      .then(ctlUpdatePool("p1"))
      .expect(and { battery.pools.get("p1").flintlockHosts == Set("h1"), not(removable("h2")), held("h2") })
      .then(invRemoveRefused("h2"))
      .then(releaseVM("p1", "h2"))
      .expect(removable("h2"))
      .then(invRemove("h2"))
      .expect(and {
        not(held("h2")),
        hist.removeRefusals == 2,
        witnessRemovalRefused,
        witnessRemoved,
        safety,
      })

  // IN-017, IN-018: a Node deleted and created again: the Host leaves
  // battery, and comes back with AddHost; the Pool names it again only
  // after battery holds it.
  run joiningHostTest =
    fresh
      .then(declare("p1", Set(ZONE_A), 1))
      .then(deleteNode("h2"))
      .then(ticks(SETTLE))
      .then(invUnpublish("h2"))
      .then(ctlUpdatePool("p1"))
      .then(invCordon("h2"))
      .then(invRemove("h2"))
      .then(createNode("h2", Set(ZONE_A), true))
      .then(ticks(SETTLE))
      .expect(and { canRegister("h2"), not(held("h2")), not(hostsChanged("p1")) })
      .then(invRegister("h2"))
      .expect(and { active("h2"), hist.adds == 1, hostsChanged("p1"), safety })
      .then(ctlUpdatePool("p1"))
      .expect(and { battery.pools.get("p1").flintlockHosts == Set("h1", "h2"), poolsNameHeldHosts, safety })

  // BA-086, IN-020, IN-016: battery starts on a v0.4.0 database. Its Hosts
  // have no TLS settings, and time does not pass until each has them. The
  // placeholder Host has no Node, so once that has settled it is cordoned
  // and removed.
  run upgradeTest =
    initWith(true)
      .expect(and { canSecure("h1"), held(PLACEHOLDER) })
      .then(tick)
      .fail()

  run upgradeSecuresHostsTest =
    initWith(true)
      .then(invSecure("h1"))
      .then(invSecure("h2"))
      .then(invSecure("h3"))
      .then(invSecure(PLACEHOLDER))
      .then(invRefresh)
      .expect(and { heldHosts.forall(h => bhost(h).tls), hist.tlsUpdates == 4, safety })
      .then(ticks(SETTLE))
      .expect(and { canCordon(PLACEHOLDER), not(inv.published.contains(PLACEHOLDER)) })
      .then(invCordon(PLACEHOLDER))
      .then(invRemove(PLACEHOLDER))
      .expect(and { not(held(PLACEHOLDER)), witnessPlaceholderRemoved, witnessUpgradedHost, safety })

  // Before it has its TLS settings, battery cannot reach a Host from a
  // v0.4.0 database, and provisioning there fails.
  run unreachableBeforeSecuredTest =
    initWith(true)
      .then(declare("p1", Set(ZONE_A), 1))
      .then(batteryProvisionUnreachable("p1", "h1"))
      .expect(and { hist.unreachablePicks == 1, battery.vms == Set(), safety })

  // DP-035, BA-081: a renewal reaches battery through UpdateHost on every
  // Host, once the kubelet has put it in the volume, and time does not
  // pass meanwhile.
  run renewalTest =
    fresh
      .then(renewCertificate)
      .expect(and { not(canRefresh), certificateFollowsSecret })
      .then(tick)
      .then(mountCertificate)
      .expect(canRefresh)
      .then(tick)
      .fail()

  run renewalRefreshedTest =
    fresh
      .then(renewCertificate)
      .then(tick)
      .then(mountCertificate)
      .then(invRefresh)
      .expect(and {
        HOSTS.forall(h => bhost(h).cert == 1),
        hist.renewalRefreshes == 1,
        witnessRenewalRefreshed,
        safety,
      })

  // DP-035: after the Operator starts again, every Host gets the
  // certificate once more.
  run operatorRestartTest =
    fresh
      .then(restartOperator)
      .expect(canRefresh)
      .then(invRefresh)
      .expect(and { hist.refreshes == 2, hist.renewalRefreshes == 0, safety })
}
