#!/usr/bin/env bash
# Writes the traces of the claim lifecycle model that the Claim Controller's
# replay test replays (#62, internal/controller/claim_replay_test.go).
#
# Usage: hack/claims-traces.sh           write them to $CLAIMS_TRACES_DIR
#        hack/claims-traces.sh --check   fail unless the ones there are
#                                        what the model writes now
#
# quint run simulates specs/quint/claims_replay.qnt, whose replayStep takes
# claims.qnt's steps weighted towards the controller's, checking claims.qnt's
# safety invariant, with a fixed seed. Of the traces it writes, the ones
# kept are the first CLAIMS_TRACES_BASE, and for every step the first
# CLAIMS_TRACES_PER that take it, so that the few kept replay every step of
# the model, the rare ones included. Each is written gzipped, with the
# header's creation time dropped, so that the same model and seed write the
# same files and the test replays the same steps every time.
#
# QUINT names the quint binary. CLAIMS_TRACES_POOL, CLAIMS_TRACES_STEPS and
# CLAIMS_TRACES_SEED set how many traces are simulated, how long, and the
# seed; the defaults are what the committed traces were written with.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
: "${QUINT:=quint}"
: "${CLAIMS_TRACES_POOL:=600}"
: "${CLAIMS_TRACES_STEPS:=60}"
: "${CLAIMS_TRACES_SEED:=62}"
: "${CLAIMS_TRACES_BASE:=12}"
: "${CLAIMS_TRACES_PER:=3}"
: "${CLAIMS_TRACES_DIR:=internal/controller/testdata/claims-traces}"

tmp=$(mktemp -d)
trap 'rm -rf "${tmp:?}"' EXIT

# write DIR: the traces kept, into DIR.
write() {
  local dir=$1 f n keep
  mkdir -p "$dir/pool"
  if ! "$QUINT" run specs/quint/claims_replay.qnt --step replayStep --mbt \
      --invariant safety --seed "$CLAIMS_TRACES_SEED" --n-threads 1 \
      --n-traces "$CLAIMS_TRACES_POOL" --max-samples "$CLAIMS_TRACES_POOL" \
      --max-steps "$CLAIMS_TRACES_STEPS" \
      --out-itf "$dir/pool/trace-{seq}.itf.json" >"$dir/log" 2>&1; then
    cat "$dir/log" >&2
    echo "FAIL: quint run failed on specs/quint/claims_replay.qnt" >&2
    return 1
  fi
  # "N step" for every step trace N takes, by trace.
  for f in "$dir"/pool/trace-*.itf.json; do
    n=${f##*/trace-}
    n=${n%.itf.json}
    grep -o '"mbt::actionTaken":"[A-Za-z]*"' "$f" | cut -d'"' -f4 | sort -u | sed "s/^/$n /"
  done | sort -n -k1,1 -s >"$dir/steps"
  keep=$({
    seq 0 $((CLAIMS_TRACES_BASE - 1))
    awk -v per="$CLAIMS_TRACES_PER" '$2 != "init" && seen[$2]++ < per { print $1 }' "$dir/steps"
  } | sort -nu)
  for n in $keep; do
    sed -E 's/^\{"#meta":\{[^}]*\},/{/' "$dir/pool/trace-$n.itf.json" |
      gzip -n -9 >"$dir/trace-$n.itf.json.gz"
  done
}

if [ "${1:-}" = "--check" ]; then
  echo "==> $0 --check"
  write "$tmp"
  want=$(cd "$tmp" && ls trace-*.itf.json.gz)
  have=$(cd "$CLAIMS_TRACES_DIR" && ls trace-*.itf.json.gz 2>/dev/null || true)
  stale=0
  [ "$want" = "$have" ] || stale=1
  for f in $want; do
    [ -f "$CLAIMS_TRACES_DIR/$f" ] && cmp -s <(gzip -dc "$tmp/$f") <(gzip -dc "$CLAIMS_TRACES_DIR/$f") || stale=1
  done
  if [ "$stale" = 1 ]; then
    echo "FAIL: $CLAIMS_TRACES_DIR is not what the model writes now; run make claims-traces" >&2
    exit 1
  fi
  echo "ok: $CLAIMS_TRACES_DIR is up to date"
  exit 0
fi

write "$tmp"
mkdir -p "$CLAIMS_TRACES_DIR"
rm -f "$CLAIMS_TRACES_DIR"/trace-*.itf.json.gz
cp "$tmp"/trace-*.itf.json.gz "$CLAIMS_TRACES_DIR"/
echo "wrote $(ls "$CLAIMS_TRACES_DIR"/trace-*.itf.json.gz | wc -l | tr -d ' ') traces to $CLAIMS_TRACES_DIR"
