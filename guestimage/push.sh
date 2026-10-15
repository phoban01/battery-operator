#!/usr/bin/env bash
# Usage: guestimage/push.sh <layout> <repository> <tag>...
#
# Pushes one of the Guest Images (docs/requirements/13-guest-image.md) to
# <repository> at every <tag>, and prints each tag with the digest it
# pushed, one per line, as <repository>:<tag>@sha256:<digest>. <layout> is
# an OCI image layout that holds one image index of the image for
# linux/amd64 and linux/arm64, as the Dagger module's guest-image-publish
# writes it.
#
# The push is crane's, from CRANE (default crane), with the credentials of
# a `crane auth login`. It checks what it pushed: that every tag names the
# same index, and that the index holds exactly linux/amd64 and
# linux/arm64. A version tag runs it against ghcr.io in CI's publish job; a
# pull request runs it against a registry of its own, through
# guestimage/push-check.sh.
set -euo pipefail

[ $# -ge 3 ] || {
	echo "usage: $0 <layout> <repository> <tag>..." >&2
	exit 2
}
layout=$1
repo=$2
shift 2
crane=${CRANE:-crane}

log() { printf 'guestimage/push: %s\n' "$*" >&2; }

# platforms REF: the sorted os/architecture of every image in the index REF,
# one per line.
platforms() {
	"$crane" manifest "$1" | tr -d ' \n\t' | grep -o '"platform":{[^}]*}' |
		while IFS= read -r p; do
			os=$(sed -n 's/.*"os":"\([^"]*\)".*/\1/p' <<<"$p")
			arch=$(sed -n 's/.*"architecture":"\([^"]*\)".*/\1/p' <<<"$p")
			echo "$os/$arch"
		done | sort
}

#= docs/requirements/13-guest-image.md#guest-publish
#/ When a version tag is pushed, the Guest Images SHALL be
#/ published to `ghcr.io/phoban01/battery-operator/guest-kernel` and
#/ `ghcr.io/phoban01/battery-operator/guest-rootfs`, each as one image index
#/ that holds `linux/amd64` and `linux/arm64`.
first="$repo:$1"
log "pushing $layout to $first"
"$crane" push "$layout" "$first" >/dev/null
digest=$("$crane" digest "$first")
if ! [[ $digest =~ ^sha256:[0-9a-f]{64}$ ]]; then
	log "the digest of $first is '$digest', not sha256:<64 hex digits>"
	exit 1
fi
got=$(platforms "$repo@$digest" | paste -sd ' ' -)
if [ "$got" != "linux/amd64 linux/arm64" ]; then
	log "the index $repo@$digest holds '$got', not linux/amd64 and linux/arm64"
	exit 1
fi

#= docs/requirements/13-guest-image.md#guest-publish
#/ Each index of GI-030 SHALL be tagged with the version and with
#/ the full SHA of the tag's commit.
for tag in "${@:2}"; do
	"$crane" tag "$first" "$tag" >/dev/null
done

#= docs/requirements/13-guest-image.md#guest-publish
#/ When it publishes an index of GI-030, the Guest Images' push
#/ SHALL print each tag of the index with the index's digest, as
#/ `<repository>:<tag>@sha256:<digest>`.
for tag in "$@"; do
	d=$("$crane" digest "$repo:$tag")
	if [ "$d" != "$digest" ]; then
		log "$repo:$tag is $d, not the index $digest"
		exit 1
	fi
	echo "$repo:$tag@$digest"
done
