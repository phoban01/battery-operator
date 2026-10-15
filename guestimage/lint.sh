#!/usr/bin/env bash
# Usage: guestimage/lint.sh
#
# Lints the Guest Images' sources (docs/requirements/13-guest-image.md)
# without building them: every base image is pinned by digest, every
# download is checked against a pinned sha256, and the scripts parse and
# pass shellcheck. GUEST_IMAGE_REQUIRE_SHELLCHECK=1 fails when shellcheck
# is missing, as the Dagger module's guest-image-lint sets it. Without it,
# a machine with no shellcheck skips that step.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
cd "$here"

failures=0
ok() { printf 'ok    %s\n' "$*"; }
fail() {
	printf 'FAIL  %s\n' "$*"
	failures=$((failures + 1))
}

containerfiles=(kernel/Containerfile rootfs/Containerfile)

#= docs/requirements/13-guest-image.md#guest-build
#= type=test
#/ The Guest Images' Containerfiles SHALL pin every base image by
#/ the digest of its multi-platform index.
for f in "${containerfiles[@]}"; do
	while read -r image; do
		case $image in
		scratch) ;;
		*@sha256:*)
			if [[ $image =~ @sha256:[0-9a-f]{64}$ ]]; then
				ok "$f: $image is pinned by digest"
			else
				fail "$f: $image has a malformed digest"
			fi
			;;
		*)
			# A FROM that names an earlier stage is not a base image.
			if grep -qiE "^FROM .* AS $image\$" "$f"; then
				ok "$f: $image is a stage of the file"
			else
				fail "$f: $image is not pinned by digest"
			fi
			;;
		esac
	done < <(sed -nE 's/^FROM[[:space:]]+(--platform=[^[:space:]]+[[:space:]]+)?([^[:space:]]+).*/\2/p' "$f")
done

#= docs/requirements/13-guest-image.md#guest-build
#= type=test
#/ The Guest Images build SHALL verify the checksum of every file
#/ it downloads against a checksum recorded in the image's Containerfile,
#/ and SHALL fail when one does not match.
for f in "${containerfiles[@]}"; do
	# Every download is a curl -o <file>, followed in the same RUN by
	# `echo "<sum>  <file>" | sha256sum -c`, which fails the build on a
	# mismatch.
	text=$(sed -e ':a' -e '/\\$/N; s/\\\n//; ta' "$f")
	while read -r out; do
		if grep -F -- "-o $out" <<<"$text" | grep -qF "  $out\" | sha256sum -c"; then
			ok "$f: the download to $out is checked with sha256sum -c"
		else
			fail "$f: the download to $out is not checked with sha256sum -c"
		fi
	done < <(grep -oE 'curl [^&]*-o [^[:space:]]+' <<<"$text" | sed -E 's/.*-o ([^[:space:]]+)$/\1/')
	while read -r arg; do
		if [[ ${arg#*=} =~ ^[0-9a-f]{64}$ ]]; then
			ok "$f: ${arg%%=*} is a sha256"
		else
			fail "$f: ${arg%%=*} is '${arg#*=}', not a sha256"
		fi
	done < <(sed -nE 's/^ARG ([A-Z0-9_]*SHA256[A-Z0-9_]*=.*)/\1/p' "$f")
	if grep -qE '^ARG [A-Z0-9_]*SHA256' "$f" || ! grep -q curl "$f"; then
		ok "$f: records the checksums of its downloads"
	else
		fail "$f: downloads files but records no checksum"
	fi
done

scripts=(lint.sh push.sh push-check.sh kernel/check.sh rootfs/check.sh)
for s in "${scripts[@]}"; do
	if bash -n "$s"; then ok "$s parses"; else fail "$s does not parse"; fi
done
if command -v shellcheck >/dev/null; then
	if shellcheck -x "${scripts[@]}"; then ok "shellcheck passes"; else fail "shellcheck fails"; fi
elif [ "${GUEST_IMAGE_REQUIRE_SHELLCHECK:-}" = 1 ]; then
	fail "shellcheck is not installed"
else
	echo "skip  shellcheck is not installed"
fi

if [ "$failures" -gt 0 ]; then
	echo "$failures lint check(s) failed" >&2
	exit 1
fi
echo "every lint check passed"
