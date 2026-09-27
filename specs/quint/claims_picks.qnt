// -*- mode: Bluespec; -*-
//
// claims.qnt's steps that take arguments, each taking them as the picks
// that the replay test reads (#62, #149, internal/controller/
// claim_replay_test.go).
//
// quint run --mbt names each step of a trace and records the step's nondet
// picks, not its arguments. claims_replay.qnt's replayStep picks its
// arguments, so its traces carry them: c (a live claim), u (an unborn
// claim), wv (a warm MicroVM), uv (an unborn MicroVM) and ev (a reported
// one). A scenario test in claims_test.qnt calls the steps with the
// arguments written out, so its trace carries none. Each step here is the
// step of claims.qnt with the same name: it picks its arguments from the
// one value given, under the names replayStep gives them, and takes the
// step as the only branch of an any, so that --mbt names the step of
// claims.qnt and not this one. So a scenario test that takes its steps from
// here writes a trace that the replay replays as it replays replayStep's.
//
// hack/claims-traces.sh writes the traces of the scenario tests it names
// (FIXED) this way, beside replayStep's random ones.
module claims_picks {
  import types.* from "./types"
  import claims as M from "./claims"

  // The environment's steps

  action createClaim(x: ClaimName): bool = {
    nondet u = Set(x).oneOf()
    any { M::createClaim(u) }
  }

  action deleteClaim(x: ClaimName): bool = {
    nondet c = Set(x).oneOf()
    any { M::deleteClaim(c) }
  }

  action renewClaim(x: ClaimName): bool = {
    nondet c = Set(x).oneOf()
    any { M::renewClaim(c) }
  }

  // battery's steps

  action dropEvent(v: VMId): bool = {
    nondet ev = Set(v).oneOf()
    any { M::dropEvent(ev) }
  }

  action replenish(v: VMId): bool = {
    nondet uv = Set(v).oneOf()
    any { M::replenish(uv) }
  }

  // The Claim Controller's steps

  action ctlAddFinalizer(x: ClaimName): bool = {
    nondet c = Set(x).oneOf()
    any { M::ctlAddFinalizer(c) }
  }

  action ctlClaimVM(x: ClaimName, v: VMId): bool = {
    nondet c = Set(x).oneOf()
    nondet wv = Set(v).oneOf()
    any { M::ctlClaimVM(c, wv) }
  }

  action ctlClaimVMLost(x: ClaimName, v: VMId): bool = {
    nondet c = Set(x).oneOf()
    nondet wv = Set(v).oneOf()
    any { M::ctlClaimVMLost(c, wv) }
  }

  action ctlClaimVMUnanswered(x: ClaimName): bool = {
    nondet c = Set(x).oneOf()
    any { M::ctlClaimVMUnanswered(c) }
  }

  action ctlPoolExhausted(x: ClaimName): bool = {
    nondet c = Set(x).oneOf()
    any { M::ctlPoolExhausted(c) }
  }

  action ctlWriteBound(x: ClaimName): bool = {
    nondet c = Set(x).oneOf()
    any { M::ctlWriteBound(c) }
  }

  action ctlHeartbeat(x: ClaimName): bool = {
    nondet c = Set(x).oneOf()
    any { M::ctlHeartbeat(c) }
  }

  action ctlHeartbeatLost(x: ClaimName): bool = {
    nondet c = Set(x).oneOf()
    any { M::ctlHeartbeatLost(c) }
  }

  action ctlHeartbeatUnanswered(x: ClaimName): bool = {
    nondet c = Set(x).oneOf()
    any { M::ctlHeartbeatUnanswered(c) }
  }

  action ctlWriteRenewed(x: ClaimName): bool = {
    nondet c = Set(x).oneOf()
    any { M::ctlWriteRenewed(c) }
  }

  action ctlHeartbeatUnknown(x: ClaimName): bool = {
    nondet c = Set(x).oneOf()
    any { M::ctlHeartbeatUnknown(c) }
  }

  action ctlEventDeleted(v: VMId): bool = {
    nondet ev = Set(v).oneOf()
    any { M::ctlEventDeleted(ev) }
  }

  action ctlExpireDeleted(x: ClaimName): bool = {
    nondet c = Set(x).oneOf()
    any { M::ctlExpireDeleted(c) }
  }

  action ctlExpireUnlisted(x: ClaimName): bool = {
    nondet c = Set(x).oneOf()
    any { M::ctlExpireUnlisted(c) }
  }

  action ctlExpiryPassed(x: ClaimName): bool = {
    nondet c = Set(x).oneOf()
    any { M::ctlExpiryPassed(c) }
  }

  action ctlListRenewed(x: ClaimName): bool = {
    nondet c = Set(x).oneOf()
    any { M::ctlListRenewed(c) }
  }

  action ctlListUnanswered(x: ClaimName): bool = {
    nondet c = Set(x).oneOf()
    any { M::ctlListUnanswered(c) }
  }

  action ctlReleaseVM(x: ClaimName): bool = {
    nondet c = Set(x).oneOf()
    any { M::ctlReleaseVM(c) }
  }

  action ctlRemoveFinalizer(x: ClaimName): bool = {
    nondet c = Set(x).oneOf()
    any { M::ctlRemoveFinalizer(c) }
  }

  action ctlReleaseLost(x: ClaimName): bool = {
    nondet c = Set(x).oneOf()
    any { M::ctlReleaseLost(c) }
  }

  action ctlReleaseUnanswered(x: ClaimName): bool = {
    nondet c = Set(x).oneOf()
    any { M::ctlReleaseUnanswered(c) }
  }

  action ctlReleasePending(x: ClaimName): bool = {
    nondet c = Set(x).oneOf()
    any { M::ctlReleasePending(c) }
  }
}
