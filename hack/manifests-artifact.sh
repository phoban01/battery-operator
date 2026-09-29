#!/usr/bin/env bash
# Usage:
#   hack/manifests-artifact.sh render <dir>
#   hack/manifests-artifact.sh push <dir> <repository> <version> <commit> [<tag>...]
#
# The Manifests as an OCI artifact that Flux's OCIRepository pulls
# (docs/requirements/06-deployment.md#manifests-artifact).
#
# render writes <dir>/install.yaml: config/release, which is config/default
# and config/exec-agent, rendered by kustomize with the images below.
# `make build-installer` runs it, and so does the Dagger module's
# Manifests function.
#
# push pushes <dir> with `flux push artifact` to <repository>:<version>,
# then tags the same artifact <commit> and each <tag>. It prints the
# artifact's reference, <repository>:<version>@<digest>, on stdout. It
# refuses a <dir> whose Operator or Exec Agent image has no digest. The
# Dagger module's ManifestsPublish and ManifestsCheck functions run it.
#
# From the environment:
#   render: KUSTOMIZE (default bin/kustomize), and IMG, EXEC_AGENT_IMG and
#           POOLMGRD_IMG, the Operator's, the Exec Agent's and battery's
#           images, as the Makefile sets them
#   push:   FLUX (default flux); REGISTRY_USERNAME and REGISTRY_PASSWORD,
#           the registry's credentials, or neither for a registry that
#           needs none, or for credentials already in $DOCKER_CONFIG
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

# The repository the artifact comes from, as its source annotation.
source_url=https://github.com/phoban01/battery-operator
# The images as the Manifests name them before render sets them.
operator_repo=ghcr.io/phoban01/battery-operator
exec_agent_repo=ghcr.io/phoban01/battery-operator/exec-agent
poolmgrd_repo=ghcr.io/liquidmetal-dev/poolmgrd

log() { printf '%s\n' "$*" >&2; }
die() {
  log "manifests-artifact: $*"
  exit 1
}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

render() {
  [ $# -eq 1 ] || die "usage: $0 render <dir>"
  local out=$1 kustomize=${KUSTOMIZE:-$root/bin/kustomize}
  : "${IMG:?set IMG to the image of the Operator}"
  : "${EXEC_AGENT_IMG:?set EXEC_AGENT_IMG to the image of the Exec Agent}"
  : "${POOLMGRD_IMG:?set POOLMGRD_IMG to the image of battery}"
  # kustomize refuses a resource outside a kustomization's own directory
  # given by an absolute path, so render config/release first, and set the
  # images in a kustomization of that one file.
  "$kustomize" build "$root/config/release" >"$work/unpinned.yaml"
  cat >"$work/kustomization.yaml" <<'EOF'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - unpinned.yaml
EOF
  #= docs/requirements/06-deployment.md#manifests-artifact
  #/ In the artifact of DP-030, the Manifests SHALL name the
  #/ Operator's and the Exec Agent's images by the version's tag and the digest
  #/ published for it, and battery's image by the battery version that
  #/ `go.mod` requires.
  (cd "$work" && "$kustomize" edit set image \
    "$operator_repo=$IMG" \
    "$exec_agent_repo=$EXEC_AGENT_IMG" \
    "$poolmgrd_repo=$POOLMGRD_IMG")
  mkdir -p "$out"
  "$kustomize" build "$work" >"$out/install.yaml"
  log "rendered $out/install.yaml"
}

# docker_config writes a Docker configuration with the credentials of
# REGISTRY_USERNAME and REGISTRY_PASSWORD for registry, and points
# DOCKER_CONFIG at it, so that the password is never on a command line.
docker_config() {
  local registry=$1
  if [ -z "${REGISTRY_USERNAME:-}" ] && [ -z "${REGISTRY_PASSWORD:-}" ]; then
    return
  fi
  [ -n "${REGISTRY_USERNAME:-}" ] && [ -n "${REGISTRY_PASSWORD:-}" ] ||
    die "set both REGISTRY_USERNAME and REGISTRY_PASSWORD, or neither"
  mkdir -p "$work/docker"
  local auth
  auth=$(printf '%s:%s' "$REGISTRY_USERNAME" "$REGISTRY_PASSWORD" | base64 | tr -d '\n')
  printf '{"auths":{"%s":{"auth":"%s"}}}\n' "$registry" "$auth" >"$work/docker/config.json"
  export DOCKER_CONFIG=$work/docker
}

push() {
  [ $# -ge 4 ] || die "usage: $0 push <dir> <repository> <version> <commit> [<tag>...]"
  local dir=$1 repo=$2 version=$3 commit=$4 flux=${FLUX:-flux}
  shift 4
  [ -f "$dir/install.yaml" ] || die "$dir has no install.yaml; run $0 render $dir first"
  [ "$(find "$dir" -mindepth 1 | wc -l)" -eq 1 ] || die "$dir holds more than install.yaml"
  # A semantic version, so that an OCIRepository's ref.semver selects it.
  [[ $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] ||
    die "version $version is not v<major>.<minor>.<patch>"
  [[ $commit =~ ^[0-9a-f]{40}$ ]] || die "commit $commit is not a full SHA"

  # A release names its own images by digest, never a tag that can move.
  local img unpinned=0
  sed -nE "s#^[[:space:]]*(- )?image:[[:space:]]*(($operator_repo|$exec_agent_repo)[:@].*)\$#\2#p" \
    "$dir/install.yaml" >"$work/images"
  [ -s "$work/images" ] || die "$dir/install.yaml names neither the Operator's image nor the Exec Agent's"
  while read -r img; do
    if [[ $img != *@sha256:* ]]; then
      log "not pinned by digest: $img"
      unpinned=1
    fi
  done <"$work/images"
  [ "$unpinned" -eq 0 ] || die "$dir/install.yaml names an image without a digest"

  docker_config "${repo%%/*}"

  #= docs/requirements/06-deployment.md#manifests-artifact
  #/ The artifact of DP-030 SHALL have a config of media type
  #/ `application/vnd.cncf.flux.config.v1+json` and a single layer of media
  #/ type `application/vnd.cncf.flux.content.v1.tar+gzip`, the media types that
  #/ `flux push artifact` gives.
  #
  #= docs/requirements/06-deployment.md#manifests-artifact
  #/ The artifact of DP-030 SHALL carry the annotation
  #/ `org.opencontainers.image.source`, the repository's URL, and the
  #/ annotation `org.opencontainers.image.revision`, the version and the
  #/ commit's full SHA as `<version>@sha1:<commit>`.
  "$flux" push artifact "oci://$repo:$version" \
    --path="$dir" \
    --source="$source_url" \
    --revision="$version@sha1:$commit" \
    --output=json >"$work/pushed.json"
  local digest
  digest=$(grep -oE '"digest": *"sha256:[0-9a-f]{64}"' "$work/pushed.json" | grep -oE 'sha256:[0-9a-f]{64}') ||
    die "flux push printed no digest: $(cat "$work/pushed.json")"

  #= docs/requirements/06-deployment.md#manifests-artifact
  #/ When a version tag is pushed, the Manifests SHALL be published
  #/ as an OCI artifact at `ghcr.io/phoban01/battery-operator/manifests`,
  #/ tagged with the version and with the commit's full SHA.
  local tags=("$commit" "$@")
  local tag
  for tag in "${tags[@]}"; do
    "$flux" tag artifact "oci://$repo:$version" --tag="$tag" >&2
  done
  log "pushed $repo:$version@$digest, tagged ${tags[*]}"
  printf '%s:%s@%s\n' "$repo" "$version" "$digest"
}

cmd=${1:-}
[ -n "$cmd" ] || die "usage: $0 render|push ..."
shift
case "$cmd" in
render) render "$@" ;;
push) push "$@" ;;
*) die "unknown command $cmd; want render or push" ;;
esac
