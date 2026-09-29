#!/usr/bin/env bash
# Usage: hack/capi-check.sh
#
# `make capi-check`: renders the Cluster API host pools (config/capi, and
# the filled-in pools of internal/manifests/testdata/capi) with kustomize,
# and validates every object with kubeconform, strictly, against the CRDs
# of the pinned Cluster API and CAPA releases: Cluster API's core and
# kubeadm bootstrap CRDs, and CAPA's. The CRDs are downloaded once, checked
# against the checksums below, and converted to JSON schemas by
# hack/crdschema. The targeted checks of the templates are Go tests,
# internal/manifests/capi_test.go, which `make test` runs.
#
# From the environment, as the Makefile sets them:
#   KUSTOMIZE, KUBECONFORM  the pinned binaries
#   CAPI_SCHEMA_CACHE       where the downloaded CRDs are kept
#                           (default: bin/capi-schemas)
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

: "${KUSTOMIZE:=bin/kustomize}"
: "${KUBECONFORM:=bin/kubeconform}"
: "${CAPI_SCHEMA_CACHE:=$root/bin/capi-schemas}"

# The Cluster API release and the CAPA release built against it
# (sigs.k8s.io/cluster-api in CAPA's go.mod). Change a version and its
# checksums together, and take the checksums from the downloaded files.
CAPI_VERSION=v1.13.6
CAPA_VERSION=v2.13.0
crds=(
  "https://github.com/kubernetes-sigs/cluster-api/releases/download/$CAPI_VERSION/core-components.yaml 0d213d04c6b233650111af3fde6167394bf06db081efb917552419313c6d9020"
  "https://github.com/kubernetes-sigs/cluster-api/releases/download/$CAPI_VERSION/bootstrap-components.yaml 5be1761c6b5c56cb010b68a1e7a8f8f3f8abb50e675ba4b512ae34f779289b38"
  "https://github.com/kubernetes-sigs/cluster-api-provider-aws/releases/download/$CAPA_VERSION/infrastructure-components.yaml 6dad6c6e553ed3d2fbddc91b2c2b6926a531e056a8574ad3c045b2607876c128"
)

log() { printf '%s\n' "$*" >&2; }
die() {
  log "capi-check: $*"
  exit 1
}

for tool in "$KUSTOMIZE" "$KUBECONFORM"; do
  [ -x "$tool" ] || command -v "$tool" >/dev/null || die "$tool is not installed; run make capi-check"
done

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# ---- the CRDs ----
cache=$CAPI_SCHEMA_CACHE/capi-$CAPI_VERSION-capa-$CAPA_VERSION
mkdir -p "$cache"
files=()
for entry in "${crds[@]}"; do
  url=${entry% *}
  sum=${entry##* }
  f=$cache/$(basename "$(dirname "$url")")-$(basename "$url")
  if [ ! -s "$f" ]; then
    log "downloading $url"
    curl --proto '=https' --tlsv1.2 -fsSL --retry 3 -o "$f.tmp" "$url"
    mv "$f.tmp" "$f"
  fi
  echo "$sum  $f" | sha256sum -c --quiet - || {
    rm -f "$f"
    die "$url does not match its checksum"
  }
  files+=("$f")
done
schemas=$work/schemas
go run ./hack/crdschema "$schemas" "${files[@]}" >&2

# ---- render ----
renders=()
for dir in config/capi internal/manifests/testdata/capi; do
  out=$work/$(echo "$dir" | tr / -).yaml
  "$KUSTOMIZE" build "$dir" >"$out" || die "kustomize build $dir failed"
  n=$(grep -c '^kind:' "$out" || true)
  [ "$n" -gt 0 ] || die "kustomize build $dir rendered nothing"
  log "rendered $dir: $n objects"
  renders+=("$out")
done

# ---- schemas ----
# Every object is a Cluster API or CAPA kind, so the CRD schemas are the
# only location, and nothing is fetched while validating. A kind with no
# schema is refused, never skipped.
"$KUBECONFORM" -strict -summary -output text \
  -schema-location "$schemas/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json" \
  "${renders[@]}" >"$work/kubeconform.txt" 2>&1 || {
  cat "$work/kubeconform.txt" >&2
  die "kubeconform failed"
}
cat "$work/kubeconform.txt" >&2
grep -q 'Invalid: 0, Errors: 0, Skipped: 0' "$work/kubeconform.txt" ||
  die "kubeconform reported objects it did not validate"
log "capi-check: every object is valid against Cluster API $CAPI_VERSION and CAPA $CAPA_VERSION"
