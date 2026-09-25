// -*- mode: Bluespec; -*-
//
// The traces the Claim Controller's replay test replays (#62,
// internal/controller/claim_replay_test.go).
//
// claims.qnt's step picks a claim and a MicroVM first, and then one of the
// steps it can take with them. Most controller steps can be taken for one
// claim and one MicroVM only, while crashes, battery restarts, ticks and
// deletions can nearly always be taken, so a simulated trace spends most of
// its steps on those, deletes its claims early, and seldom binds, renews or
// releases one. replayStep takes the same steps, and only those, but picks
// better: a live claim for the controller's steps, an unborn one to create,
// a warm MicroVM to claim, an unborn one to replenish, and a reported one
// for the Events stream. It also weights them: of every ROLL steps, FAULTS
// are faults (a crash, a restart, a dropped event, a call that fails in
// transit) and SLOW let time pass or delete a claim. While the controller
// holds a Heartbeat answer, LINGER of them leave it held. Every step of
// replayStep is a step of claims.step, so its traces are traces of the
// claim lifecycle model; they only reach the controller's steps more often.
//
// The picks are named after what they are for, and the replay test reads
// each step's arguments from them: c (a live claim), u (an unborn claim),
// wv (a warm MicroVM), uv (an unborn MicroVM) and ev (a reported one).
//
// hack/claims-traces.sh writes the traces, with a fixed seed.
module claims_replay {
  import types.* from "./types"
  import claims.* from "./claims"

  pure val ROLL: int = 20
  pure val FAULTS: int = 2
  pure val SLOW: int = 3
  // While the controller holds a Heartbeat answer, and has not yet heard
  // that battery ended the Lease, LINGER of every ROLL steps are ones that
  // leave the answer held: time passes, battery sweeps, stops and starts,
  // and the controller recovers or receives an event. So the traces reach
  // what #133 and #135 are about: battery ends a Lease while the claim's
  // own step is half done, and the controller remembers it for the claim's
  // next reconcile.
  pure val LINGER: int = 14

  def lingers: bool = CLAIMS.exists(x => and {
    isHeartbeatAnswer(ctrl.mem.get(x)),
    not(knownDeleted(x)),
    not(knownUnlisted(x)),
  })
  // s if it has anything in it, and otherwise every: a pick from an empty
  // set has to come from somewhere, and the step it is for is disabled.
  pure def orAll(s: Set[a], every: Set[a]): Set[a] = if (s != Set()) s else every

  action replayStep = {
    nondet c = orAll(CLAIMS.filter(x => live(x)), CLAIMS).oneOf()
    nondet u = orAll(CLAIMS.filter(x => isUnborn(slot(x))), CLAIMS).oneOf()
    nondet wv = orAll(battery.warm, VMS).oneOf()
    nondet uv = orAll(battery.unborn, VMS).oneOf()
    nondet ev = orAll(battery.events, VMS).oneOf()
    nondet roll = 1.to(ROLL).oneOf()
    if (lingers and roll > ROLL - LINGER) any {
      tick,
      batteryExpire,
      batteryStop,
      batteryStart,
      ctlRecover,
      ctlEventDeleted(ev),
    } else if (roll <= FAULTS) any {
      crashController,
      batteryStop,
      batteryStart,
      dropEvent(ev),
      ctlClaimVMLost(c, wv),
      ctlClaimVMUnanswered(c),
      ctlHeartbeatLost(c),
      ctlHeartbeatUnanswered(c),
      ctlListUnanswered(c),
      ctlReleaseLost(c),
      ctlReleaseUnanswered(c),
    } else if (roll <= FAULTS + SLOW) any {
      deleteClaim(c),
      tick,
      batteryExpire,
    } else any {
      // tick is here too, so that time passes when nothing else can happen
      tick,
      createClaim(u),
      renewClaim(c),
      replenish(uv),
      ctlAddFinalizer(c),
      ctlClaimVM(c, wv),
      ctlPoolExhausted(c),
      ctlWriteBound(c),
      ctlHeartbeat(c),
      ctlHeartbeatUnknown(c),
      ctlWriteRenewed(c),
      ctlReleaseVM(c),
      ctlRemoveFinalizer(c),
      ctlReleasePending(c),
      ctlRecover,
      ctlEventDeleted(ev),
      ctlExpireDeleted(c),
      ctlExpireUnlisted(c),
      ctlExpiryPassed(c),
      ctlListRenewed(c),
    }
  }
}
