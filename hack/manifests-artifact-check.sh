#!/usr/bin/env bash
# Usage: hack/manifests-artifact-check.sh <dir>
#
# Checks the Manifests' OCI artifact end to end without pushing it
# anywhere public (docs/requirements/06-deployment.md#manifests-artifact).
# <dir> is what `make build-installer` rendered, with the images of IMG and
# EXEC_AGENT_IMG, each pinned by digest. The check:
#
#   1. starts a registry on 127.0.0.1:5000, in this container;
#   2. pushes <dir> to it with hack/manifests-artifact.sh push, the same
#      command a release runs against ghcr.io;
#   3. fetches the artifact's manifest by the version's tag and by the
#      commit's, and pulls the artifact with `flux pull artifact`, which
#      extracts the first layer as a tar+gzip, as source-controller does
#      for an OCIRepository with no layerSelector;
#   4. runs internal/manifests' TestManifestsArtifact against what it
#      fetched: the media types, the annotations, the tags, and the objects
#      and images of install.yaml.
#
# The Dagger module's ManifestsCheck function runs it, in CI's manifests
# job. From the environment: IMG and EXEC_AGENT_IMG; FLUX (default flux)
# and REGISTRY (default registry), the flux CLI and the registry binary of
# distribution; KUSTOMIZE, as `make test` sets it.
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

[ $# -eq 1 ] || {
  echo "usage: $0 <dir>" >&2
  exit 2
}
dir=$(cd "$1" && pwd)
: "${IMG:?set IMG to the image of the Operator that dir names}"
: "${EXEC_AGENT_IMG:?set EXEC_AGENT_IMG to the image of the Exec Agent that dir names}"
flux=${FLUX:-flux}
registry=${REGISTRY:-registry}

log() { printf 'manifests-artifact-check: %s\n' "$*" >&2; }

work=$(mktemp -d)
registry_pid=
cleanup() {
  if [ -n "$registry_pid" ]; then
    kill "$registry_pid" 2>/dev/null || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

version=v0.0.0-check
commit=0123456789abcdef0123456789abcdef01234567
addr=127.0.0.1:5000
repo=localhost:5000/battery-operator/manifests

cat >"$work/registry.yml" <<EOF
version: 0.1
log:
  level: warn
storage:
  filesystem:
    rootdirectory: $work/registry
http:
  addr: $addr
EOF
OTEL_TRACES_EXPORTER=none "$registry" serve "$work/registry.yml" >"$work/registry.log" 2>&1 &
registry_pid=$!
for _ in $(seq 1 50); do
  if curl -fsS "http://$addr/v2/" >/dev/null 2>&1; then
    break
  fi
  sleep 0.2
done
curl -fsS "http://$addr/v2/" >/dev/null || {
  cat "$work/registry.log" >&2
  log "the registry did not start"
  exit 1
}

art=$work/artifact
mkdir -p "$art"
log "pushing $dir to $repo:$version"
FLUX=$flux hack/manifests-artifact.sh push "$dir" "$repo" "$version" "$commit" >"$art/pushed"
cat "$art/pushed" >&2

accept='Accept: application/vnd.oci.image.manifest.v1+json'
curl -fsS -H "$accept" "http://$addr/v2/battery-operator/manifests/manifests/$version" >"$art/manifest.json"
curl -fsS -H "$accept" "http://$addr/v2/battery-operator/manifests/manifests/$commit" >"$art/manifest-commit.json"
mkdir -p "$art/content"
"$flux" pull artifact "oci://$repo:$version" --output="$art/content" >&2
cp "$dir/install.yaml" "$art/rendered.yaml"
echo "the manifest of $repo:$version:" >&2
cat "$art/manifest.json" >&2
echo >&2

log "checking the artifact"
MANIFESTS_ARTIFACT=$art \
  MANIFESTS_ARTIFACT_REPOSITORY=$repo \
  MANIFESTS_ARTIFACT_VERSION=$version \
  MANIFESTS_ARTIFACT_COMMIT=$commit \
  go test ./internal/manifests -run '^TestManifestsArtifact$' -count=1 -v | tee "$work/test.log"
# go test passes a test that skips; this one must have run.
grep -q '^--- PASS: TestManifestsArtifact ' "$work/test.log" || {
  log "TestManifestsArtifact did not run"
  exit 1
}
log "ok: $(cat "$art/pushed")"
