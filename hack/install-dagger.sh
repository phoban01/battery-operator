#!/usr/bin/env bash
# Installs the Dagger CLI at DAGGER_VERSION into DIR (default .devbox/bin),
# unless the binary there already reports that version. devbox's init hook
# runs it: nixpkgs only has Dagger 0.8.8.
#
# Usage: DAGGER_VERSION=0.21.9 hack/install-dagger.sh [DIR]
#
# The release archive for the host's OS and architecture is checked against
# the release's checksums.txt before it is unpacked.
set -euo pipefail

: "${DAGGER_VERSION:?set DAGGER_VERSION, for example 0.21.9}"
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
dir=${1:-"$root/.devbox/bin"}
bin="$dir/dagger"

if [ -x "$bin" ] && "$bin" version 2>/dev/null | grep -q "^dagger v$DAGGER_VERSION "; then
  exit 0
fi

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  armv7l) arch=armv7 ;;
  *) echo "install-dagger: unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac

archive="dagger_v${DAGGER_VERSION}_${os}_${arch}.tar.gz"
base="https://github.com/dagger/dagger/releases/download/v${DAGGER_VERSION}"
echo "installing dagger $DAGGER_VERSION ($os/$arch) into $dir" >&2

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL -o "$tmp/$archive" "$base/$archive"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"

want=$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")
if command -v sha256sum >/dev/null; then
  got=$(sha256sum "$tmp/$archive" | awk '{ print $1 }')
else
  got=$(shasum -a 256 "$tmp/$archive" | awk '{ print $1 }')
fi
if [ -z "$want" ] || [ "$want" != "$got" ]; then
  echo "install-dagger: checksum mismatch for $archive (want ${want:-none}, got $got)" >&2
  exit 1
fi

tar -xzf "$tmp/$archive" -C "$tmp" dagger
mkdir -p "$dir"
mv -f "$tmp/dagger" "$bin"
chmod +x "$bin"
