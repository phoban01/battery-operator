#!/usr/bin/env bash
# Checks the Quint models in specs/quint exhaustively up to a bound, with
# Apalache (specs/quint/README.md, "Bounded checks").
#
# Usage: hack/quint-verify.sh [CHECK...]
#
# With no CHECK, runs them all. The checks are:
#
#   MODEL:safety      every behaviour of MODEL of up to QUINT_VERIFY_STEPS
#                     steps keeps its invariants (claims, certificates,
#                     pools);
#   claims:PROPERTY   a liveness property of claims.qnt: no behaviour of up
#                     to QUINT_VERIFY_LIVENESS_STEPS steps that ends in a
#                     loop meets the property's assumptions and never gets
#                     there;
#   claims:witnessPROPERTY
#                     the property's witness, which must be violated: some
#                     such behaviour meets the assumptions and reaches the
#                     property's left-hand side, so the property is not
#                     checked only where it holds vacuously.
#
# `make quint`'s simulations go much deeper and are the everyday check; this
# is the exhaustive one, and it is slow. CI runs it in its own workflow,
# .github/workflows/quint-verify.yml, one check per job.
#
# quint compiles each model to its JSON form, and Apalache checks that
# directly: this is what `quint verify` does, without the gRPC server it
# talks to, which with quint 0.32.0 can leave `quint verify` waiting after
# Apalache has answered.
#
# Apalache needs Java 17 or later. It is installed into
# $QUINT_HOME/apalache-dist-VERSION (default ~/.quint), where `quint
# verify` looks for it, checked against its release checksum, unless it is
# there already. A counterexample, as TLA+ and as an ITF trace, is left in
# _apalache-out/ at the repository root.
#
# QUINT names the quint binary. QUINT_VERIFY_STEPS and
# QUINT_VERIFY_LIVENESS_STEPS set the bounds.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
: "${QUINT:=quint}"
: "${QUINT_HOME:=$HOME/.quint}"
: "${QUINT_VERIFY_STEPS:=5}"
: "${QUINT_VERIFY_LIVENESS_STEPS:=10}"

# The Apalache that quint 0.32.0's `quint verify` runs, and the sha256 of
# its apalache.tgz from the release's sha256sum.txt. .dagger/main.go pins
# the same.
apalache_version=0.56.1
apalache_sum=91125e5a3646b9c9d3a7d921d3323f321fac5071909f72b3960c66ff2f998ee1
apalache="$QUINT_HOME/apalache-dist-$apalache_version/apalache/bin/apalache-mc"

all=(
  claims:safety certificates:safety pools:safety
  claims:deletedClaimGone claims:witnessDeletedClaimGone
  claims:pendingClaimBinds claims:witnessPendingClaimBinds
  claims:unrenewedClaimExpires claims:witnessUnrenewedClaimExpires
  claims:orphanEventuallyGone claims:witnessOrphanEventuallyGone
)

if [ $# -eq 0 ]; then
  set -- "${all[@]}"
fi

if ! command -v java >/dev/null; then
  echo "hack/quint-verify.sh: Apalache needs Java 17 or later; or run: dagger call quint-verify" >&2
  exit 1
fi

if [ ! -x "$apalache" ]; then
  echo "==> installing Apalache $apalache_version into $QUINT_HOME"
  dist=$(dirname "$(dirname "$(dirname "$apalache")")")
  tgz=$(mktemp)
  curl -fsSLo "$tgz" "https://github.com/apalache-mc/apalache/releases/download/v$apalache_version/apalache.tgz"
  if command -v sha256sum >/dev/null; then
    echo "$apalache_sum  $tgz" | sha256sum -c -
  else
    echo "$apalache_sum  $tgz" | shasum -a 256 -c -
  fi
  mkdir -p "$dist"
  tar -xzf "$tgz" -C "$dist"
  rm -f "$tgz"
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
out="$root/_apalache-out"

# json MODEL: the model compiled to the JSON form Apalache reads.
json() {
  local model=$1
  if [ ! -f "$work/$model.qnt.json" ]; then
    "$QUINT" compile --target json --main "$model" "specs/quint/$model.qnt" >"$work/$model.qnt.json"
  fi
  echo "$work/$model.qnt.json"
}

# run CHECK: one check, as the usage above describes it.
run() {
  local model=${1%%:*} name=${1#*:} args expect=hold steps
  case "$name" in
    safety)
      steps=$QUINT_VERIFY_STEPS
      args=(--next=step --inv=safety) ;;
    witness*)
      steps=$QUINT_VERIFY_LIVENESS_STEPS expect=violate
      args=(--next=stepOrStutter "--temporal=$name") ;;
    *)
      steps=$QUINT_VERIFY_LIVENESS_STEPS
      args=(--next=stepOrStutter "--temporal=$name") ;;
  esac
  echo "==> $1, up to $steps steps"
  local start=$SECONDS rc=0 log="$work/$model-$name.log"
  "$apalache" check --init=init "${args[@]}" --length="$steps" \
    --out-dir="$out/$model-$name" "$(json "$model")" >"$log" 2>&1 || rc=$?
  local took="($((SECONDS - start))s)"
  # Apalache exits 12 when it finds a counterexample.
  case "$expect:$rc" in
    hold:0) echo "    holds $took" ;;
    violate:12) echo "    violated, as a witness should be $took" ;;
    hold:12)
      grep -v '^ *>\|^PASS\|holds\.\|disabled' "$log" | tail -40
      echo "FAIL $1: a counterexample of up to $steps steps, in $out/$model-$name" >&2
      return 1 ;;
    violate:0)
      echo "FAIL $1: no behaviour of up to $steps steps meets the assumptions and reaches the left-hand side; raise QUINT_VERIFY_LIVENESS_STEPS" >&2
      return 1 ;;
    *)
      tail -40 "$log"
      echo "FAIL $1: Apalache failed (exit $rc)" >&2
      return 1 ;;
  esac
}

failed=0
for check in "$@"; do
  run "$check" || failed=1
done
exit $failed
