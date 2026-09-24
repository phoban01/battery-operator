#!/usr/bin/env bash
# Checks the Quint models in specs/quint (specs/quint/README.md).
#
# Usage: hack/quint.sh
#
#   1. quint typecheck on every module;
#   2. quint test on every *_test.qnt;
#   3. quint run on every model, checking its invariants and failing if a
#      witness is never reached, which would mean the simulation is too
#      short to reach the interleaving it names.
#
# A property the requirements do not yet guarantee (a finding filed as an
# issue) is not in a model's invariants. A scenario test reaches its
# violation instead, so the test fails once the model changes to fix it.
#
# QUINT names the quint binary. QUINT_MAX_STEPS and QUINT_MAX_SAMPLES size
# the simulations; QUINT_SEED makes them repeatable.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
: "${QUINT:=quint}"
: "${QUINT_MAX_STEPS:=60}"
: "${QUINT_MAX_SAMPLES:=20000}"
seed=()
[ -n "${QUINT_SEED:-}" ] && seed=(--seed "$QUINT_SEED")

plain() { sed 's/\x1b\[[0-9;]*m//g'; }

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

# check MODEL INVARIANT [WITNESS...]
check() {
  local model=$1 invariant=$2
  shift 2
  echo "==> quint run $model --invariant $invariant"
  if ! "$QUINT" run "$model" --invariant "$invariant" --witnesses "$@" \
      --max-steps "$QUINT_MAX_STEPS" --max-samples "$QUINT_MAX_SAMPLES" "${seed[@]}" >"$tmp" 2>&1; then
    cat "$tmp"
    echo "FAIL $model: quint run failed or found a violation of $invariant" >&2
    return 1
  fi
  plain <"$tmp" | sed -n '/No violation/,$p'
  local unreached
  unreached=$(plain <"$tmp" | sed -n 's/^\([A-Za-z0-9_]*\) was witnessed in 0 trace.*/\1/p')
  if [ -n "$unreached" ]; then
    echo "FAIL $model: never reached $(echo $unreached); raise QUINT_MAX_STEPS or QUINT_MAX_SAMPLES" >&2
    return 1
  fi
}

for f in specs/quint/*.qnt; do
  echo "==> quint typecheck $f"
  "$QUINT" typecheck "$f"
done

for f in specs/quint/*_test.qnt; do
  echo "==> quint test $f"
  "$QUINT" test "$f" "${seed[@]}"
done

check specs/quint/claims.qnt safety \
  witnessOrphan witnessOrphanBesideBound witnessExpired witnessReleased \
  witnessExpiredByEvent witnessExpiredByTime \
  witnessOrphanFromLostAnswer witnessReleasedAfterLostAnswer \
  witnessUnsweptLease witnessLateHeartbeat witnessIdleWithBound \
  witnessIdleWithBoundAfterRestart witnessExpiryRecordedForHeldLease \
  witnessExpiredWithPendingRenewal

check specs/quint/certificates.qnt safety \
  witnessAgentFullyCertified witnessForeignApprovalFailed witnessForeignApprovalSigned \
  witnessAnotherHostDenied witnessSubjectIgnored witnessApprovedAwaitingCA

check specs/quint/pools.qnt safety \
  witnessFlapAbsorbed witnessRestart witnessBatchedRestart witnessTwoRestarts \
  witnessRenewalRestart witnessRenewalWithHostChange \
  witnessVMOnFormerHost witnessPlacementUpdated witnessNoEligibleHost \
  witnessPoolDeleted witnessDrainedForPool witnessRejectedWithoutHost
