#!/usr/bin/env bash
# Usage: guestimage/push-check.sh <kernel-layout> <rootfs-layout>
#
# Checks the push of the Guest Images (docs/requirements/13-guest-image.md)
# without pushing anything public. Each layout is an OCI image layout that
# holds one image index for linux/amd64 and linux/arm64, as the Dagger
# module writes it. The check:
#
#   1. starts a registry on 127.0.0.1:5000, in this container;
#   2. pushes both images to it with guestimage/push.sh, the same script a
#      version tag runs against ghcr.io, at a version tag and a commit SHA;
#   3. checks what push.sh printed, and what the registry holds: one index
#      per image, with both tags, for linux/amd64 and linux/arm64, and in
#      each platform's image the kernel or the guest agent.
#
# The Dagger module's guest-image-publish-check runs it, in CI's
# guest-image job. From the environment: CRANE (default crane) and REGISTRY
# (default registry), the registry binary of distribution.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

[ $# -eq 2 ] || {
	echo "usage: $0 <kernel-layout> <rootfs-layout>" >&2
	exit 2
}
kernel_layout=$1
rootfs_layout=$2
crane=${CRANE:-crane}
registry=${REGISTRY:-registry}

work=$(mktemp -d)
registry_pid=
cleanup() {
	if [ -n "$registry_pid" ]; then
		kill "$registry_pid" 2>/dev/null || true
	fi
	rm -rf "$work"
}
trap cleanup EXIT

failures=0
ok() { printf 'ok    %s\n' "$*"; }
fail() {
	printf 'FAIL  %s\n' "$*"
	failures=$((failures + 1))
}

version=v0.0.0-check
commit=0123456789abcdef0123456789abcdef01234567
addr=127.0.0.1:5000
prefix=localhost:5000/battery-operator

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
	echo "the registry did not start" >&2
	exit 1
}

# files REF PLATFORM: the regular files of REF's image for PLATFORM, one per
# line.
files() {
	"$crane" export --platform "$2" "$1" - | tar -tv | awk '$1 ~ /^-/ {print $NF}' | sed 's|^\./||'
}

#= docs/requirements/13-guest-image.md#guest-publish
#= type=test
#/ When a version tag is pushed, the Guest Images SHALL be
#/ published to `ghcr.io/phoban01/battery-operator/guest-kernel` and
#/ `ghcr.io/phoban01/battery-operator/guest-rootfs`, each as one image index
#/ that holds `linux/amd64` and `linux/arm64`.
#
#= docs/requirements/13-guest-image.md#guest-publish
#= type=test
#/ Each index of GI-030 SHALL be tagged with the version and with
#/ the full SHA of the tag's commit.
#
#= docs/requirements/13-guest-image.md#guest-publish
#= type=test
#/ When it publishes an index of GI-030, the Guest Images' push
#/ SHALL print each tag of the index with the index's digest, as
#/ `<repository>:<tag>@sha256:<digest>`.
for image in guest-kernel guest-rootfs; do
	repo=$prefix/$image
	layout=$kernel_layout
	[ "$image" = guest-rootfs ] && layout=$rootfs_layout
	if ! out=$(CRANE=$crane "$here/push.sh" "$layout" "$repo" "$version" "$commit"); then
		fail "push.sh failed for $image"
		continue
	fi
	printf '%s\n' "$out" | sed 's/^/      /'

	re="^$repo:($version|$commit)@(sha256:[0-9a-f]{64})\$"
	digests=()
	tags=()
	while IFS= read -r line; do
		if [[ $line =~ $re ]]; then
			tags+=("${BASH_REMATCH[1]}")
			digests+=("${BASH_REMATCH[2]}")
		else
			fail "$image: push.sh printed '$line', not <repository>:<tag>@sha256:<digest>"
		fi
	done <<<"$out"
	if [ "${tags[*]}" = "$version $commit" ]; then
		ok "$image: push.sh printed both tags, the version's and the commit's"
	else
		fail "$image: push.sh printed tags '${tags[*]}', not '$version $commit'"
	fi
	digest=${digests[0]:-}
	if [ -n "$digest" ] && [ "${digests[*]}" = "$digest $digest" ]; then
		ok "$image: both tags name one digest, $digest"
	else
		fail "$image: the tags name digests '${digests[*]}'"
		continue
	fi
	for tag in "$version" "$commit"; do
		if [ "$("$crane" digest "$repo:$tag")" = "$digest" ]; then
			ok "$image: the registry has $tag at $digest"
		else
			fail "$image: the registry does not have $tag at $digest"
		fi
	done

	manifest=$("$crane" manifest "$repo@$digest" | tr -d ' \n\t')
	if grep -q '"mediaType":"application/vnd.oci.image.index.v1+json"' <<<"$manifest" ||
		grep -q '"mediaType":"application/vnd.docker.distribution.manifest.list.v2+json"' <<<"$manifest"; then
		ok "$image: $digest is an image index"
	else
		fail "$image: $digest is not an image index"
	fi
	for platform in linux/amd64 linux/arm64; do
		arch=${platform#linux/}
		config=$("$crane" config --platform "$platform" "$repo@$digest" | tr -d ' \n\t')
		if grep -q "\"architecture\":\"$arch\"" <<<"$config" && grep -q '"os":"linux"' <<<"$config"; then
			ok "$image: the index holds an image for $platform"
		else
			fail "$image: the index holds no image for $platform"
			continue
		fi
		list=$(files "$repo@$digest" "$platform")
		case $image in
		guest-kernel)
			if [ "$list" = boot/vmlinux ]; then
				ok "$image: the $platform image holds boot/vmlinux alone"
			else
				fail "$image: the $platform image holds: $(head -5 <<<"$list" | paste -sd ' ' -)"
			fi
			;;
		guest-rootfs)
			if grep -qx 'usr/local/bin/guest-agent' <<<"$list" && grep -qx 'usr/lib/systemd/systemd' <<<"$list"; then
				ok "$image: the $platform image holds systemd and the guest agent"
			else
				fail "$image: the $platform image lacks systemd or the guest agent"
			fi
			;;
		esac
	done
done

if [ "$failures" -gt 0 ]; then
	echo "$failures push check(s) failed" >&2
	exit 1
fi
echo "every push check passed"
