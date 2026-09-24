#!/usr/bin/env bash
# Per-PR requirement coverage gate (docs/requirements/README.md, "The coverage gate").
#
# Usage: hack/duvet-coverage.sh [--no-regressions BASE_REPORT | --base REF] [ID...]
#
# IDs are requirement identifiers such as SC-020. A range PREFIX-NNN..MMM (or
# PREFIX-NNN..PREFIX-MMM) expands to every identifier in the spec between the
# two numbers; identifiers explicitly listed have to exist.
#
# Runs `duvet report`, reads .duvet/reports/report.json and exits non-zero
# listing every identifier that lacks an implementation citation or a test
# citation. Set DUVET to point at a duvet binary, and SKIP_REPORT=1 to reuse
# an existing report.json.
#
# Model citations (docs/requirements/README.md, "Model citations") never
# count: the gate reads report.json only. When .duvet/reports/models.json
# exists, an ID the Quint models cite is marked "modelled" in the output.
#
# The citation ratchet (docs/requirements/README.md, "The citation ratchet")
# covers every requirement, not only the owned ones:
#
#   --no-regressions BASE_REPORT  compares report.json with BASE_REPORT, the
#       base branch's report.json, and fails for every requirement that had
#       both an implementation and a test citation there and has lost either
#       one. A requirement the head marks (withdrawn) is skipped.
#   --base REF  builds BASE_REPORT first: duvet's report of the merge base of
#       REF and HEAD, in a temporary git worktree.
#
# With either, the IDs are optional; without them only the ratchet runs.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
report="$root/.duvet/reports/report.json"
models="$root/.duvet/reports/models.json"
: "${DUVET:=duvet}"

usage() {
  echo "usage: $0 [--no-regressions BASE_REPORT | --base REF] [ID...]" >&2
  exit 2
}

base_report=""
base_ref=""
case "${1:-}" in
  --no-regressions) [ "$#" -ge 2 ] || usage; base_report=$2; shift 2 ;;
  --base) [ "$#" -ge 2 ] || usage; base_ref=$2; shift 2 ;;
esac
[ "$#" -gt 0 ] || [ -n "$base_report$base_ref" ] || usage
command -v jq >/dev/null || { echo "jq is required" >&2; exit 2; }

# --base: duvet's report of the merge base, from a detached worktree.
if [ -n "$base_ref" ]; then
  base_sha=$(git -C "$root" merge-base "$base_ref" HEAD) || { echo "no merge base of $base_ref and HEAD" >&2; exit 2; }
  tmp=$(mktemp -d)
  trap 'git -C "$root" worktree remove --force "$tmp/base" >/dev/null 2>&1 || true; rm -rf "$tmp"' EXIT
  git -C "$root" worktree add --detach --quiet "$tmp/base" "$base_sha"
  if ! out=$(cd "$tmp/base" && "$DUVET" report --ci false 2>&1); then
    printf '%s\n' "$out" >&2
    echo "duvet report failed on the merge base $base_sha" >&2
    exit 2
  fi
  base_report="$tmp/base-report.json"
  cp "$tmp/base/.duvet/reports/report.json" "$base_report"
  echo "ratchet base: merge base of $base_ref and HEAD, $base_sha"
fi
if [ -n "$base_report" ] && [ ! -f "$base_report" ]; then
  echo "missing base report $base_report" >&2
  exit 2
fi

if [ "${SKIP_REPORT:-0}" != 1 ]; then
  if ! out=$(cd "$root" && rm -rf .duvet/requirements && "$DUVET" report 2>&1); then
    printf '%s\n' "$out" >&2
    echo "duvet report failed" >&2
    exit 2
  fi
  # Only for the "modelled" marks; make duvet is what checks the models report.
  (cd "$root" && "$DUVET" report --config-path .duvet/models.toml --ci false >/dev/null 2>&1) || rm -f "$models"
fi
[ -f "$report" ] || { echo "missing $report" >&2; exit 2; }

# One line per requirement in the spec: "<ID> <spec-annotation-index>".
known=$(jq -r '
  .annotations
  | to_entries[]
  | select(.value.type == "SPEC")
  | (.value.comment // "" | capture("^- \\*\\*(?<id>[A-Z]+-[0-9]+)\\*\\*") | .id) as $id
  | "\($id) \(.key)"' "$report")

lookup() { # lookup ID -> annotation index or empty
  awk -v id="$1" '$1 == id { print $2; exit }' <<<"$known"
}

ids=()
for arg in "$@"; do
  for tok in ${arg//,/ }; do
    if [[ "$tok" =~ ^([A-Z]+)-([0-9]+)\.\.(([A-Z]+)-)?([0-9]+)$ ]]; then
      prefix=${BASH_REMATCH[1]}; from=$((10#${BASH_REMATCH[2]})); to=$((10#${BASH_REMATCH[5]}))
      if [ -n "${BASH_REMATCH[4]}" ] && [ "${BASH_REMATCH[4]}" != "$prefix" ]; then
        echo "bad range $tok: prefixes differ" >&2; exit 2
      fi
      for ((n = from; n <= to; n++)); do
        id=$(printf '%s-%03d' "$prefix" "$n")
        [ -n "$(lookup "$id")" ] && ids+=("$id")
      done
    else
      ids+=("$tok")
    fi
  done
done

# IDs the models cite, from the models report; never part of the verdict.
modelled=""
if [ -f "$models" ]; then
  modelled=$(jq -r '
    . as $r
    | .annotations | to_entries[]
    | select(.value.type == "SPEC")
    | select(($r.statuses[.key | tostring].citation // 0) > 0)
    | .value.comment // "" | capture("^- \\*\\*(?<id>[A-Z]+-[0-9]+)\\*\\*") | .id' "$models")
fi
mark() { grep -qxF "$1" <<<"$modelled" && echo " (modelled)" || true; }

failed=0
for id in ${ids[@]+"${ids[@]}"}; do
  idx=$(lookup "$id")
  if [ -z "$idx" ]; then
    echo "FAIL $id: not a requirement in docs/requirements" >&2
    failed=1
    continue
  fi
  read -r citation test todo < <(jq -r --arg i "$idx" \
    '.statuses[$i] | "\(.citation // 0) \(.test // 0) \(.todo // 0)"' "$report")
  missing=()
  [ "$citation" -gt 0 ] || missing+=("implementation citation")
  [ "$test" -gt 0 ] || missing+=("test citation")
  if [ "${#missing[@]}" -gt 0 ]; then
    note=""
    [ "$todo" -gt 0 ] && note=" (only a todo annotation)"
    echo "FAIL $id: missing $(IFS="+"; echo "${missing[*]}" | sed "s/+/ and /")$note$(mark "$id")" >&2
    failed=1
  else
    echo "ok   $id$(mark "$id")"
  fi
done

if [ "$failed" -ne 0 ]; then
  echo "coverage gate failed; see docs/requirements/README.md for the annotation syntax" >&2
  exit 1
fi

[ -n "$base_report" ] || exit 0

# The ratchet. One line per requirement in a report, "<ID> <implementation>
# <test>": what duvet's statuses count for the requirement's implementation
# and test citations, 0 when it has none.
coverage() {
  jq -r '
    . as $r
    | .annotations | to_entries[]
    | select(.value.type == "SPEC")
    | (.value.comment // "" | capture("^- \\*\\*(?<id>[A-Z]+-[0-9]+)\\*\\*") | .id) as $id
    | ($r.statuses[.key | tostring] // {}) as $s
    | "\($id) \($s.citation // 0) \($s.test // 0)"' "$1"
}

# A withdrawn requirement (README rule 5) is a bullet "- **ID** (withdrawn)"
# with no keyword, so duvet doesn't extract it: read those from the head's
# Markdown.
withdrawn=$(grep -ohE '^- \*\*[A-Z]+-[0-9]+\*\*[[:space:]]+\(withdrawn\)' "$root"/docs/requirements/*.md |
  sed -E 's/^- \*\*([A-Z]+-[0-9]+)\*\*.*/\1/' || true)

# The head's lines come first, then the base's. Every requirement with both
# citations on the base must keep both on the head, unless the head has
# withdrawn it.
if ! awk -v withdrawn="$withdrawn" '
  BEGIN { n = split(withdrawn, w, "\n"); for (i = 1; i <= n; i++) gone[w[i]] = 1 }
  NR == FNR { impl[$1] = $2; test[$1] = $3; next }
  $2 > 0 && $3 > 0 {
    held++
    id = $1
    if (id in gone) { printf "skip %s: withdrawn\n", id; next }
    if (!(id in impl)) {
      printf "FAIL %s: no longer in the spec; a retired requirement keeps its number and is marked (withdrawn)\n", id > "/dev/stderr"
      bad++
      next
    }
    lost = ""
    if (impl[id] == 0) lost = "its implementation citation"
    if (test[id] == 0) lost = lost (lost == "" ? "" : " and ") "its test citation"
    if (lost != "") { printf "FAIL %s: lost %s, which the base has\n", id, lost > "/dev/stderr"; bad++ }
  }
  END {
    if (bad) exit 1
    printf "ratchet ok: every requirement cited in code and in a test on the base still is (%d)\n", held
  }' <(coverage "$report") <(coverage "$base_report"); then
  echo "citation ratchet failed: restore the citations, or mark a retired requirement (withdrawn); see docs/requirements/README.md, \"The citation ratchet\"" >&2
  exit 1
fi
