#!/usr/bin/env bash
# Runs /usr/libexec/battery/host-config and devmapper-config against Host
# configuration files that set, omit and mistype MICROVM_BASE_IMAGE_SIZE,
# and checks the containerd configuration that containerd.service starts
# containerd with (HI-087 to HI-089). Installed as
# /usr/libexec/battery/check-microvm-disk-size-cases and called by the
# check stage; `hostimage/check-microvm-disk-size.sh DIR LIBEXEC DEFAULTS
# CONFIG UNITS` runs it outside the image against the sources, which
# `make host-image-lint` does.
#
# Where containerd is installed, as in the check stage, `containerd config
# dump` reads the rendered file the way the daemon does.
set -uo pipefail
work=${1:-$(mktemp -d)}
libexec=${2:-/usr/libexec/battery}
defaults=${3:-/usr/share/battery/host.conf.defaults}
config=${4:-/etc/containerd/config.toml}
units=${5:-/usr/lib/systemd/system}
C=$work/microvm-disk-size-cases
failures=0
ok() { printf 'ok    %s\n' "$*"; }
fail() {
  printf 'FAIL  %s\n' "$*"
  failures=$((failures + 1))
}
expect() {
  local what=$1
  shift
  if "$@" >/dev/null 2>&1; then ok "$what"; else fail "$what"; fi
}
refute() {
  local what=$1
  shift
  if "$@" >/dev/null 2>&1; then fail "$what"; else ok "$what"; fi
}

reset() {
  rm -rf "$C"
  mkdir -p "$C/run/not-ready.d"
}
env_for() {
  printf '%s\n' BATTERY_PATH="$PATH" BATTERY_LIB="$libexec/lib.sh" BATTERY_HOST_DEFAULTS="$defaults" BATTERY_HOST_CONF="$1" \
    BATTERY_RUN="$C/run" BATTERY_NOT_READY_DIR="$C/run/not-ready.d" BATTERY_HOST_ENV="$C/run/host.env" \
    BATTERY_CONTAINERD_CONFIG="${2:-$config}" BATTERY_CONTAINERD_RUN_CONFIG="$C/run/containerd.toml"
}
# host_config CONF: host-config with CONF as the Host configuration file.
host_config() {
  local e
  mapfile -t e < <(env_for "$1")
  env "${e[@]}" bash "$libexec/host-config"
}
# devmapper_config TEMPLATE: devmapper-config with the host.env in $C.
devmapper_config() {
  local e
  mapfile -t e < <(env_for "$C/none.conf" "$1")
  env "${e[@]}" bash "$libexec/devmapper-config"
}
# size_in FILE prints the devmapper table's base_image_size value.
size_in() {
  awk '/^[[:space:]]*\[/ { t = $0; gsub(/[[:space:]]/, "", t) }
    t == "[plugins.\"io.containerd.snapshotter.v1.devmapper\"]" && $1 == "base_image_size" { print $3 }' "$1"
}
# changed_lines FILE: how many lines of FILE, header aside, differ from the
# template.
changed_lines() {
  tail -n +2 "$1" | diff "$config" - | grep -c '^>'
}

#= docs/requirements/11-host-image.md#image-storage
#= type=test
#/ The Host Image SHALL configure containerd's devicemapper
#/ snapshotter with the MicroVM base image size that it reads from the Host
#/ configuration file, and with 10 GB when none is set.
expect "the default MICROVM_BASE_IMAGE_SIZE is 10GB" grep -qx 'MICROVM_BASE_IMAGE_SIZE=10GB' "$defaults"
if [ "$(size_in "$config")" = '"10GB"' ]; then
  ok "the image's containerd configuration has base_image_size 10GB"
else
  fail "the image's containerd configuration has base_image_size $(size_in "$config")"
fi
reset
if host_config "$C/absent.conf" >/dev/null 2>&1 && devmapper_config "$config" >/dev/null 2>&1; then
  expect "without a Host configuration file host.env has the default" grep -qx 'BATTERY_MICROVM_BASE_IMAGE_SIZE=10GB' "$C/run/host.env"
  if [ "$(size_in "$C/run/containerd.toml")" = '"10GB"' ]; then ok "and containerd gets base_image_size 10GB"; else fail "containerd gets base_image_size $(size_in "$C/run/containerd.toml")"; fi
else
  fail "host-config and devmapper-config fail without a Host configuration file"
fi
for c in '100GB:"100GB"' '"250GB":"250GB"' '0512GB:"512GB"' '2TB:"2TB"' '1GB:"1GB"' '1024MB:"1024MB"' '16TB:"16TB"' '16777216MB:"16777216MB"'; do
  value=${c%%:*}
  want=${c#*:}
  reset
  printf '# written by cloud-init\nMICROVM_BASE_IMAGE_SIZE=%s\n' "$value" >"$C.conf"
  if ! host_config "$C.conf" >/dev/null 2>&1 || ! devmapper_config "$config" >/dev/null 2>&1; then
    fail "MICROVM_BASE_IMAGE_SIZE=$value is refused"
    continue
  fi
  got=$(size_in "$C/run/containerd.toml")
  if [ "$got" = "$want" ]; then ok "MICROVM_BASE_IMAGE_SIZE=$value gives containerd base_image_size $want"; else fail "MICROVM_BASE_IMAGE_SIZE=$value gives base_image_size $got, want $want"; fi
  n=$(changed_lines "$C/run/containerd.toml")
  if [ "$n" -le 1 ]; then ok "and changes nothing else in containerd's configuration"; else fail "and changes $n lines of containerd's configuration"; fi
done
expect "containerd.service writes the configuration before containerd starts" grep -qx 'ExecStartPre=/usr/libexec/battery/devmapper-config' "$units/containerd.service"
expect "containerd starts with the written configuration" grep -qx 'ExecStart=/usr/bin/containerd --config /run/battery/containerd.toml' "$units/containerd.service"
expect "containerd starts after battery-host-config writes host.env" grep -qE '^After=.*battery-host-config\.service' "$units/containerd.service"
if [ -r "$C/run/containerd.toml" ] && command -v containerd >/dev/null 2>&1; then
  if containerd --config "$C/run/containerd.toml" config dump >"$C/dump.toml" 2>"$C/dump.err"; then
    if grep -qE "^[[:space:]]*base_image_size = ['\"]16777216MB['\"]$" "$C/dump.toml"; then
      ok "containerd reads the written base_image_size"
    else
      fail "containerd config dump has $(grep base_image_size "$C/dump.toml")"
    fi
  else
    printf 'skip  containerd config dump cannot run here: %s\n' "$(head -n1 "$C/dump.err")"
  fi
else
  printf 'skip  containerd is not installed\n'
fi

#= docs/requirements/11-host-image.md#image-storage
#= type=test
#/ If the MicroVM base image size in the Host configuration file
#/ is not a whole number of megabytes, gigabytes or terabytes from 1 GB to
#/ 16 TB, then the Host Image SHALL NOT start `flintlockd` and SHALL report
#/ the Host as not ready with the reason.
# The last two are literal on purpose.
# shellcheck disable=SC2016
for value in '' 10 10G 10gb 10GiB 1.5GB -10GB 0GB 512MB 1023MB 17TB 16777217MB 999999999GB '10GB;reboot' '$(reboot)GB'; do
  reset
  printf 'MICROVM_BASE_IMAGE_SIZE=%s\n' "$value" >"$C.conf"
  refute "host-config refuses MICROVM_BASE_IMAGE_SIZE='$value'" host_config "$C.conf"
  if grep -q '^host configuration invalid: MICROVM_BASE_IMAGE_SIZE' "$C/run/not-ready.d/battery-host-config" 2>/dev/null; then
    ok "and records why"
  else
    fail "and records why: $(cat "$C/run/not-ready.d/battery-host-config" 2>/dev/null)"
  fi
  refute "and writes no host.env" test -e "$C/run/host.env"
done
# An invalid Host configuration fails battery-host-config.service, which
# battery-thin-pool.service requires, which flintlockd.service requires.
expect "the thin pool requires the Host configuration" grep -qE '^Requires=.*battery-host-config\.service' "$units/battery-thin-pool.service"
expect "flintlockd requires the thin pool" grep -qE '^Requires=.*battery-thin-pool\.service' "$units/flintlockd.service"

#= docs/requirements/11-host-image.md#image-storage
#= type=test
#/ If the Host environment file is absent when containerd starts,
#/ then the Host Image SHALL start containerd with the base image size of the
#/ image's own containerd configuration.
reset
if devmapper_config "$config" >/dev/null 2>&1; then
  ok "devmapper-config succeeds without host.env"
  n=$(changed_lines "$C/run/containerd.toml")
  if [ "$n" = 0 ]; then ok "and writes the image's containerd configuration unchanged"; else fail "and changes $n lines of it"; fi
else
  fail "devmapper-config fails without host.env"
fi
reset
printf 'BATTERY_GUEST_SUBNET=10.220.0.0/16\n' >"$C/run/host.env"
if devmapper_config "$config" >/dev/null 2>&1 && [ "$(changed_lines "$C/run/containerd.toml")" = 0 ]; then
  ok "a host.env without the size keeps the image's base_image_size"
else
  fail "a host.env without the size does not keep the image's base_image_size"
fi
refute "containerd.service does not require the Host configuration" grep -qE '^(Requires|BindsTo|Requisite)=.*battery-host-config' "$units/containerd.service"
# A template that does not set the size once is the image's own mistake,
# and devmapper-config stops rather than guess which line to change.
reset
printf 'BATTERY_MICROVM_BASE_IMAGE_SIZE=20GB\n' >"$C/run/host.env"
grep -v 'base_image_size' "$config" >"$C.no-size.toml"
refute "devmapper-config refuses a template without base_image_size" devmapper_config "$C.no-size.toml"
sed 's/^\([[:space:]]*\)base_image_size = .*/&\n\1base_image_size = "5GB"/' "$config" >"$C.two-sizes.toml"
refute "devmapper-config refuses a template that sets base_image_size twice" devmapper_config "$C.two-sizes.toml"
printf 'BATTERY_MICROVM_BASE_IMAGE_SIZE=20GB"\n' >"$C/run/host.env"
refute "devmapper-config refuses a size host-config would not write" devmapper_config "$config"

rm -rf "$C" "$C".*
[ "$failures" -eq 0 ]
