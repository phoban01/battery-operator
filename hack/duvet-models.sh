#!/usr/bin/env bash
# Which requirements the Quint models cite (docs/requirements/README.md,
# "Model citations").
#
# Usage: hack/duvet-models.sh [--status | --write | --check]
#
# Runs `duvet report` with .duvet/models.toml, which scans only
# specs/**/*.qnt for model citations (//@= and //@#) and writes
# .duvet/reports/models.json. Then:
#
#   --status  (the default) prints every requirement with three columns:
#             implemented and tested, from .duvet/reports/report.json (run
#             `make duvet` first), and modelled, from models.json.
#   --write   rewrites the list of modelled requirements in
#             docs/requirements/README.md.
#   --check   fails if that list is out of date.
#
# It also fails if a model citation carries a type= line: a model citation
# is its own annotation type and takes none.
#
# Set DUVET to point at a duvet binary, and SKIP_REPORT=1 to reuse an
# existing models.json.
set -euo pipefail
export LC_ALL=C

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
models="$root/.duvet/reports/models.json"
report="$root/.duvet/reports/report.json"
readme="$root/docs/requirements/README.md"
begin='<!-- BEGIN modelled: written by hack/duvet-models.sh --write; do not edit -->'
end='<!-- END modelled -->'
: "${DUVET:=duvet}"

mode=${1:---status}
case "$mode" in
  --status|--write|--check) ;;
  *) echo "usage: $0 [--status | --write | --check]" >&2; exit 2 ;;
esac
command -v jq >/dev/null || { echo "jq is required" >&2; exit 2; }

if [ "${SKIP_REPORT:-0}" != 1 ]; then
  if ! out=$(cd "$root" && "$DUVET" report --config-path .duvet/models.toml --ci false 2>&1); then
    printf '%s\n' "$out" >&2
    echo "duvet report for the models failed" >&2
    exit 2
  fi
fi
[ -f "$models" ] || { echo "missing $models" >&2; exit 2; }

typed=$(jq -r '.annotations[] | select(.type != null and .type != "SPEC")
  | "\(.source):\(.line) type=\(.type)"' "$models")
if [ -n "$typed" ]; then
  printf 'model citation with a type= line; a model citation takes none:\n%s\n' "$typed" >&2
  exit 1
fi

# "<ID> <sources, comma-separated>" for every requirement, sources empty
# when it is not modelled.
modelled() {
  jq -r '
    . as $r
    | .annotations
    | to_entries[]
    | select(.value.type == "SPEC")
    | (.value.comment // "" | capture("^- \\*\\*(?<id>[A-Z]+-[0-9]+)\\*\\*") | .id) as $id
    | ($r.statuses[.key | tostring] // {}) as $s
    | [($s.related // [])[] | $r.annotations[.].source] | unique | join(",") as $src
    | if ($s.citation // 0) > 0 then "\($id) \($src)" else "\($id) " end' "$1"
}

table() {
  echo '| ID | Model |'
  echo '|----|-------|'
  modelled "$models" | sort -t- -k1,1 -k2,2n | awk 'NF == 2 { n = split($2, s, ","); out = ""
    for (i = 1; i <= n; i++) out = out (i > 1 ? ", " : "") "`" s[i] "`"
    print "| " $1 " | " out " |" }'
}

case "$mode" in
  --status)
    [ -f "$report" ] || { echo "missing $report; run make duvet first" >&2; exit 2; }
    printf '%-8s %-11s %-6s %s\n' ID implemented tested modelled
    join -a1 -e- -o 0,1.2,2.2 \
      <(jq -r '
          . as $r
          | .annotations | to_entries[] | select(.value.type == "SPEC")
          | (.value.comment // "" | capture("^- \\*\\*(?<id>[A-Z]+-[0-9]+)\\*\\*") | .id) as $id
          | ($r.statuses[.key | tostring] // {}) as $s
          | "\($id) \(if ($s.citation // 0) > 0 then "yes" else "-" end):\(if ($s.test // 0) > 0 then "yes" else "-" end)"' "$report" | sort) \
      <(modelled "$models" | awk 'NF == 2 { print $1, "yes" }' | sort) |
      sort -t- -k1,1 -k2,2n |
      awk '{ split($2, it, ":"); printf "%-8s %-11s %-6s %s\n", $1, it[1], it[2], $3 }'
    ;;
  --write|--check)
    grep -qxF "$begin" "$readme" && grep -qxF "$end" "$readme" ||
      { echo "$readme has no modelled list markers" >&2; exit 2; }
    new=$(T="$(table)" awk -v b="$begin" -v e="$end" '
      $0 == b { print; print ENVIRON["T"]; skip = 1; next }
      $0 == e { skip = 0 }
      !skip { print }' "$readme")
    if [ "$mode" = --write ]; then
      printf '%s\n' "$new" >"$readme"
    elif [ "$new" != "$(cat "$readme")" ]; then
      echo "the list of modelled requirements in docs/requirements/README.md is out of date; run make duvet-models-write" >&2
      diff <(cat "$readme") <(printf '%s\n' "$new") >&2 || true
      exit 1
    else
      echo "modelled requirements in docs/requirements/README.md are up to date"
    fi
    ;;
esac
