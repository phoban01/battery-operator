#!/usr/bin/env bash
#
# Pins the Host Image's base to its mirror on ghcr.io, in both FROM lines
# of hostimage/Containerfile, and rewrites the comment above them.
#
#   hostimage/pin-base.sh MIRROR SOURCE [CONTAINERFILE]
#
# MIRROR and SOURCE are the two references that `dagger call
# mirror-host-image-base` prints, for example:
#
#   MIRROR  ghcr.io/phoban01/battery-operator/fedora-bootc:44-20260929-f59997f5@sha256:f59997f5...
#   SOURCE  quay.io/fedora/fedora-bootc:44@sha256:f59997f5...
#
# The digest is that of the base's multi-architecture index, which the
# mirror copies whole, so one pin serves every architecture the image is
# built for.
#
# `make host-image-base-pin MIRROR=... SOURCE=...` runs it. See
# hostimage/README.md, "Updating the base image".
set -euo pipefail

if [ $# -lt 2 ] || [ $# -gt 3 ]; then
  echo "usage: $0 MIRROR SOURCE [CONTAINERFILE]" >&2
  exit 2
fi
mirror=$1
source=$2
containerfile=${3:-$(dirname "${BASH_SOURCE[0]}")/Containerfile}

fail() {
  echo "pin-base: $*" >&2
  exit 1
}

hex='[0-9a-f]{64}'
[[ $mirror =~ ^([^@]+):([^:@/]+)@(sha256:$hex)$ ]] ||
  fail "MIRROR $mirror is not <repository>:<tag>@sha256:<digest>"
mirror_repo=${BASH_REMATCH[1]}
mirror_tag=${BASH_REMATCH[2]}
digest=${BASH_REMATCH[3]}

[[ $source =~ ^([^@]+):([^:@/]+)@(sha256:$hex)$ ]] ||
  fail "SOURCE $source is not <repository>:<tag>@sha256:<digest>"
source_ref="${BASH_REMATCH[1]}:${BASH_REMATCH[2]}"
source_tag=${BASH_REMATCH[2]}
[ "${BASH_REMATCH[3]}" = "$digest" ] ||
  fail "the mirror's digest $digest is not the source's ${BASH_REMATCH[3]}"

# The mirror's tag is <source tag>-<YYYYMMDD>-<first 8 hex digits>.
[[ $mirror_tag =~ ^(.+)-([0-9]{4})([0-9]{2})([0-9]{2})-([0-9a-f]{8})$ ]] ||
  fail "the mirror's tag $mirror_tag is not <source tag>-<YYYYMMDD>-<8 hex digits>"
[ "${BASH_REMATCH[1]}" = "$source_tag" ] ||
  fail "the mirror's tag $mirror_tag is not of the source tag $source_tag"
date="${BASH_REMATCH[2]}-${BASH_REMATCH[3]}-${BASH_REMATCH[4]}"
[ "${BASH_REMATCH[5]}" = "${digest:7:8}" ] ||
  fail "the mirror's tag $mirror_tag does not end in the digest's first 8 hex digits"

[ -f "$containerfile" ] || fail "no $containerfile"
n=$(grep -Ec '^FROM (--platform=[^ ]+ )?[^ ]*bootc@sha256:[0-9a-f]{64}( |$)' "$containerfile" || true)
[ "$n" -eq 2 ] || fail "$containerfile has $n FROM lines of a bootc base by digest, not 2"

# The comment between the digest-pin requirement and the next citation
# names the source; it is replaced as a whole.
anchor='#/ is a bootc base image pinned by digest.'
grep -qxF "$anchor" "$containerfile" || fail "$containerfile has no line '$anchor'"

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
awk -v anchor="$anchor" -v ref="$mirror_repo@$digest" \
  -v c1="# The base is $mirror_repo:$mirror_tag, a copy of" \
  -v c2="# $source_ref made on $date, whose digest was" \
  -v c3="# $digest." \
  -v c4="# The digest is that of the multi-architecture index; --platform selects" \
  -v c5="# the target's image from it. hostimage/README.md says how to update it." '
  done == 0 && $0 == anchor { print; print c1; print c2; print c3; print c4; print c5; print "#"; skip = 1; done = 1; next }
  skip == 1 && /^#= / { skip = 0 }
  skip == 1 { next }
  /^FROM / && $0 ~ /bootc@sha256:[0-9a-f]+/ {
    for (i = 2; i <= NF; i++) if ($i ~ /bootc@sha256:/) { $i = ref; break }
  }
  { print }
' "$containerfile" >"$tmp"
cat "$tmp" >"$containerfile"

n=$(grep -cxF -e "FROM --platform=\$TARGETPLATFORM $mirror_repo@$digest AS base" \
  -e "FROM --platform=\$BUILDPLATFORM $mirror_repo@$digest AS fetch" "$containerfile" || true)
[ "$n" -eq 2 ] || fail "pinned $n of the 2 FROM lines; check $containerfile"
echo "pinned $mirror_repo@$digest in both FROM lines of $containerfile"
