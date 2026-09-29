#!/usr/bin/env bash
# Usage: hostimage/lint.sh
#
# Lints the Host Image sources without building anything: `bash -n` and
# the shellcheck tool over every script; the digest pin and the versions file; the
# thin-pool cases against stand-in tools; the flintlockd access and
# certificate cases (HI-067 to HI-072); the guest isolation cases (HI-075
# to HI-077); the nft syntax of the rendered firewall; and
# `systemd-analyze verify` where it is installed.
#
# HOST_IMAGE_REQUIRE_SHELLCHECK=1 makes a missing shellcheck a failure
# instead of a skip, and HOST_IMAGE_REQUIRE_NFT=1 does the same for an nft
# that cannot check the firewall. The Dagger module's host-image-lint, which
# CI runs, sets both.
set -uo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
cd "$here/.." || exit 1
failed=0
fail() {
  echo "FAIL  $*"
  failed=1
}

scripts=(hostimage/check.sh hostimage/check-thin-pool.sh hostimage/check-flintlockd-access.sh hostimage/check-flintlockd-certs.sh
  hostimage/check-guest-isolation.sh hostimage/check-selinux-contexts.sh hostimage/check-labels.sh hostimage/lint.sh
  hostimage/build/*.sh hostimage/rootfs/usr/libexec/battery/*)
for s in "${scripts[@]}"; do
  bash -n "$s" || fail "bash -n $s"
done
echo "ok    bash -n: ${#scripts[@]} scripts"

if command -v shellcheck >/dev/null 2>&1; then
  if shellcheck --shell=bash --severity=style --external-sources "${scripts[@]}"; then
    echo "ok    shellcheck"
  else
    fail shellcheck
  fi
elif [ -n "${HOST_IMAGE_REQUIRE_SHELLCHECK:-}" ]; then
  fail "HOST_IMAGE_REQUIRE_SHELLCHECK is set but shellcheck is not installed"
else
  echo "skip  shellcheck is not installed"
fi

#= docs/requirements/11-host-image.md#image-build
#= type=test
#/ The Host Image SHALL be defined by one Containerfile whose base
#/ is a bootc base image pinned by digest.
n=$(find hostimage -name 'Containerfile*' -o -name 'Dockerfile*' | wc -l)
if [ "$n" -eq 1 ]; then echo "ok    one Containerfile"; else fail "$n Containerfiles under hostimage/"; fi
# Every FROM names either an earlier stage or an image by sha256 digest.
stages=" "
while read -r _ a b c d; do
  ref=$a
  case "$a" in --platform=*) ref=$b; b=$c; c=$d ;; esac
  case "$ref" in
  *@sha256:????????????????????????????????????????????????????????????????) echo "ok    FROM $ref is pinned by digest" ;;
  *) case "$stages" in *" $ref "*) ;; *) fail "FROM $ref is neither a stage nor pinned by digest" ;; esac ;;
  esac
  [ "${b,,}" = as ] && stages="$stages$c "
done < <(grep -E '^FROM ' hostimage/Containerfile)
grep -Eq '^FROM --platform=linux/amd64 .*bootc@sha256:' hostimage/Containerfile || fail "the base is not a bootc image for linux/amd64"

#= docs/requirements/11-host-image.md#image-build
#= type=test
#/ The Host Image build SHALL verify the checksum of every binary
#/ it downloads against a checksum recorded in the versions file and SHALL
#/ fail when one does not match.
# Every download of the build goes through fetch(), which checks before use,
# and the versions file is nothing but KEY=value lines.
if grep -nE '\b(curl|wget)\b' hostimage/build/*.sh hostimage/Containerfile | grep -v '^hostimage/build/fetch.sh:[0-9]*:  curl --proto' | grep -v '^[^:]*:[0-9]*:#' | grep -q .; then
  fail "a download outside fetch() in hostimage/build/fetch.sh"
else
  echo "ok    every download goes through fetch()"
fi
if grep -vE '^(#.*|[A-Z][A-Z0-9_]*=[A-Za-z0-9._-]*|)$' hostimage/versions.env | grep -q .; then
  fail "hostimage/versions.env has a line that is not KEY=value"
else
  echo "ok    hostimage/versions.env is KEY=value lines"
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
if hostimage/check-thin-pool.sh "$tmp" hostimage/rootfs/usr/libexec/battery/thin-pool hostimage/rootfs/usr/libexec/battery/lib.sh; then
  echo "ok    thin-pool cases"
else
  fail "thin-pool cases"
fi

#= docs/requirements/11-host-image.md#image-flintlockd
#= type=test
#/ The Host Image SHALL refuse connections to `flintlockd`'s port
#/ from the Host's own processes unless they belong to the Exec Agent's user
#/ id, which it reads from the Host configuration file with a default when
#/ none is set.
#
#= docs/requirements/11-host-image.md#image-flintlockd
#= type=test
#/ The Host Image SHALL drop every connection to `flintlockd`'s
#/ port that arrives from outside the Host unless its source address is in
#/ the Operator's pod network, which it reads from the Host configuration
#/ file.
# The same cases the check stage runs in the image, here against the
# sources, so that the rendered rules are checked without a build. Where
# this machine has nft and unprivileged user and network namespaces they
# also load the output chain and show what the kernel refuses.
if hostimage/check-flintlockd-access.sh "$tmp" hostimage/rootfs/usr/libexec/battery hostimage/rootfs/usr/share/battery/host.conf.defaults \
  hostimage/rootfs/usr/lib/systemd/system/flintlockd.service; then
  echo "ok    flintlockd access cases"
else
  fail "flintlockd access cases"
fi

#= docs/requirements/11-host-image.md#image-flintlockd
#= type=test
#/ The Host Image SHALL start `flintlockd` only once the serving
#/ certificate, its key and the client CA bundle exist in
#/ `/etc/battery/flintlockd`, and SHALL restart `flintlockd` when the serving
#/ certificate or the client CA bundle changes.
# The path units and flintlockd-certs, against stand-ins for systemctl,
# chown and restorecon.
if hostimage/check-flintlockd-certs.sh "$tmp" hostimage/rootfs/usr/libexec/battery hostimage/rootfs/usr/lib/systemd/system \
  hostimage/rootfs/usr/share/battery/host.conf.defaults; then
  echo "ok    flintlockd certificate cases"
else
  fail "flintlockd certificate cases"
fi

#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL forward traffic from the guest subnet only
#/ out of the Host's primary interface, and SHALL drop traffic from the guest
#/ subnet to every other interface of the Host.
#
#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL drop traffic from the guest subnet whose
#/ destination before any destination NAT on the Host is in a protected CIDR.
#
#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL drop traffic from the guest subnet to every
#/ address of the Host other than the bridge gateway address.
# The check stage's cases against the sources. Where this machine has nft
# and unprivileged user and network namespaces they also load the rules
# into a stand-in Host and show a guest reach the outside and DNS on the
# gateway, and not another port on the gateway, the Host's primary
# address, a pod on the Host or a protected Service.
if hostimage/check-guest-isolation.sh "$tmp" hostimage/rootfs/usr/libexec/battery hostimage/rootfs/usr/share/battery/host.conf.defaults; then
  echo "ok    guest isolation cases"
else
  fail "guest isolation cases"
fi

#= docs/requirements/11-host-image.md#kernel-and-kvm
#= type=test
#/ The Host Image SHALL label the Host environment file and the not ready
#/ reason directory it writes under `/run/battery` so that the containers of
#/ pods on the Host can read them, under the base image's SELinux policy and
#/ without changing the domain of any container or the label of any other
#/ path.
# The check stage's cases against the sources, and the module's rules. The
# labels themselves are looked up in the check stage, where the module is
# installed into the base image's policy.
if hostimage/check-selinux-contexts.sh "$tmp" hostimage/rootfs/usr/libexec/battery hostimage/selinux hostimage/rootfs/usr/lib/tmpfiles.d/battery.conf \
  hostimage/rootfs/usr/share/battery/host.conf.defaults; then
  echo "ok    SELinux context cases"
else
  fail "SELinux context cases"
fi

# The nft syntax of the guest firewall, rendered from the sources for the
# defaults and for a Host configuration file that sets every list. `nft -c`
# parses and checks the ruleset against the kernel without loading it, and
# needs a netlink socket for that.
lib=hostimage/rootfs/usr/libexec/battery
render_nft() {
  local conf=$1 dir=$2
  mkdir -p "$dir/run/not-ready.d"
  local env=(BATTERY_PATH="$PATH" BATTERY_LIB="$lib/lib.sh" BATTERY_HOST_DEFAULTS=hostimage/rootfs/usr/share/battery/host.conf.defaults
    BATTERY_HOST_CONF="$conf" BATTERY_RUN="$dir/run" BATTERY_NOT_READY_DIR="$dir/run/not-ready.d" BATTERY_HOST_ENV="$dir/run/host.env")
  env "${env[@]}" bash "$lib/host-config" >/dev/null 2>&1 &&
    env "${env[@]}" BATTERY_PRIMARY_INTERFACE=eth0 BATTERY_PRIMARY_ADDRESS=192.0.2.10 bash "$lib/network" render "$dir/net" >/dev/null 2>&1
}
printf 'PROTECTED_CIDRS=10.0.0.0/16,10.96.0.0/12\nFLINTLOCKD_CLIENT_CIDRS=10.244.0.0/16\n' >"$tmp/nft-set.conf"
if ! command -v nft >/dev/null 2>&1; then
  if [ -n "${HOST_IMAGE_REQUIRE_NFT:-}" ]; then fail "HOST_IMAGE_REQUIRE_NFT is set but nft is not installed"; else echo "skip  nft is not installed"; fi
else
  for c in "$tmp/absent.conf" "$tmp/nft-set.conf"; do
    d=$tmp/nft-$(basename "$c" .conf)
    if ! render_nft "$c" "$d"; then
      fail "host-config or network render fails for $(basename "$c")"
      continue
    fi
    out=$(nft -c -f "$d/net/guest-firewall.nft" 2>&1)
    status=$?
    # Unprivileged, nft can still check the ruleset in a network namespace
    # of its own, where this process is root.
    case "$status:$out" in
    0:*) ;;
    *"Operation not permitted"* | *"netlink"* | *"Protocol not supported"*)
      if command -v unshare >/dev/null 2>&1 &&
        uout=$(unshare --user --map-root-user --net nft -c -f "$d/net/guest-firewall.nft" 2>&1); then
        status=0 out=$uout
      fi
      ;;
    esac
    case "$status:$out" in
    0:*) echo "ok    nft accepts the firewall rendered for $(basename "$c")" ;;
    *"Operation not permitted"* | *"netlink"* | *"Protocol not supported"*)
      if [ -n "${HOST_IMAGE_REQUIRE_NFT:-}" ]; then
        fail "HOST_IMAGE_REQUIRE_NFT is set but nft cannot open netlink here: $(echo "$out" | head -n1)"
      else
        echo "skip  nft cannot open netlink here: $(echo "$out" | head -n1)"
      fi
      ;;
    *) fail "nft rejects the firewall rendered for $(basename "$c"): $out" ;;
    esac
  done
fi

if command -v systemd-analyze >/dev/null 2>&1; then
  # Outside the image the binaries and most dependencies are absent, so
  # only syntax problems are taken from the output.
  out=$(systemd-analyze verify --man=no hostimage/rootfs/usr/lib/systemd/system/*.service hostimage/rootfs/usr/lib/systemd/system/*.path 2>&1 |
    grep -E 'Unknown (key|section)|Failed to parse|Invalid|Missing|ignoring line' || true)
  if [ -z "$out" ]; then echo "ok    systemd-analyze verify finds no syntax problem"; else fail "systemd-analyze verify: $out"; fi
else
  echo "skip  systemd-analyze is not installed"
fi

exit "$failed"
