#!/usr/bin/env bash
# Runs /usr/libexec/battery/host-config and `network render` against Host
# configuration files that set, omit and mistype the gateway services' user
# ids, and checks the firewall rules that keep those ids from the instance
# metadata service and the Host's control ports (HI-080). Installed as
# /usr/libexec/battery/check-gateway-service-egress-cases and called by the
# check stage; `hostimage/check-gateway-service-egress.sh DIR LIBEXEC
# DEFAULTS` runs it outside the image against the sources, which
# `make host-image-lint` does.
#
# Where unprivileged user and network namespaces, nft and ip are available,
# the rendered ruleset is also loaded into a namespace of its own and
# connections are made as a listed id and others, to show what the kernel
# drops and what it lets through: the internet and the gateway service
# ports on the bridge gateway. A build container has none of that, and
# there only the rules are checked.
#
# Ported from flintlock-runner's image/check-host-service-egress.sh (its
# HI-064), with an empty default in place of its Host Services' ids.
#
#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL drop traffic from the gateway service user
#/ ids, which it reads from the Host configuration file, to the instance
#/ metadata service addresses and to the Host's own `flintlockd`, kubelet,
#/ Exec Agent and metrics ports.
set -uo pipefail
work=${1:-$(mktemp -d)}
libexec=${2:-/usr/libexec/battery}
defaults=${3:-/usr/share/battery/host.conf.defaults}
C=$work/gateway-service-egress-cases
failures=0
ok() { printf 'ok    %s\n' "$*"; }
fail() {
  printf 'FAIL  %s\n' "$*"
  failures=$((failures + 1))
}

# render CONF: host-config with CONF as the Host configuration file, then
# the firewall into $C/net. Fails when host-config refuses CONF.
render() {
  rm -rf "$C"
  mkdir -p "$C/run/not-ready.d"
  local env=(BATTERY_PATH="$PATH" BATTERY_LIB="$libexec/lib.sh" BATTERY_HOST_DEFAULTS="$defaults" BATTERY_HOST_CONF="$1"
    BATTERY_RUN="$C/run" BATTERY_NOT_READY_DIR="$C/run/not-ready.d" BATTERY_HOST_ENV="$C/run/host.env")
  env "${env[@]}" bash "$libexec/host-config" >/dev/null 2>&1 || return 1
  env "${env[@]}" BATTERY_PRIMARY_INTERFACE=eth0 BATTERY_PRIMARY_ADDRESS=192.0.2.2 bash "$libexec/network" render "$C/net" >/dev/null 2>&1
}
# service_rules prints the output chain's rules for a set of socket owners,
# one per line, without comments or indentation.
service_rules() {
  awk '/^\tchain output \{/ { on = 1; next } on && /^\t\}/ { exit } on' "$C/net/guest-firewall.nft" |
    sed -e 's/#.*//' -e 's/^[[:space:]]*//' -e '/^$/d' | grep '^meta skuid {'
}
expected_rules() {
  printf 'meta skuid { %s } ip daddr 169.254.169.254 counter drop\n' "$1"
  printf 'meta skuid { %s } ip6 daddr fd00:ec2::254 counter drop\n' "$1"
  printf 'meta skuid { %s } oifname "lo" tcp dport { %s } counter drop\n' "$1" "$2"
}
control='9090, 8090, 10248, 10250, 10255, 10256, 10270, 1338'

# The default, with no Host configuration file at all: no id is listed and
# the output chain has no rule for one.
default_uids=$(sed -n 's/^GATEWAY_SERVICE_UIDS=//p' "$defaults")
if [ -z "$default_uids" ] && grep -qx 'GATEWAY_SERVICE_UIDS=' "$defaults"; then
  ok "the default GATEWAY_SERVICE_UIDS is empty"
else
  fail "the default GATEWAY_SERVICE_UIDS is '$default_uids'"
fi
if render "$C.absent.conf"; then
  if [ -z "$(service_rules)" ]; then
    ok "without a Host configuration file the output chain has no rule for gateway service ids"
  else
    fail "without a Host configuration file the output chain has: $(service_rules)"
  fi
  if grep -qx 'BATTERY_GATEWAY_SERVICE_UIDS=' "$C/run/host.env"; then
    ok "host.env carries the empty default"
  else
    fail "host.env lacks BATTERY_GATEWAY_SERVICE_UIDS="
  fi
else
  fail "host-config or network render fails without a Host configuration file"
fi

# A configured list, with a range and spaces.
printf 'GUEST_SUBNET=10.200.4.0/22\nGATEWAY_SERVICE_PORTS=5000\nGATEWAY_SERVICE_UIDS = "1000, 100000-165535"\n' >"$C.conf"
if render "$C.conf"; then
  if [ "$(service_rules)" = "$(expected_rules '1000, 100000-165535' "$control")" ]; then
    ok "GATEWAY_SERVICE_UIDS=1000,100000-165535 is dropped to the metadata service, v4 and v6, and to the Host's control ports"
  else
    fail "the rules for GATEWAY_SERVICE_UIDS=1000,100000-165535 are: $(service_rules)"
  fi
  if grep -qx 'BATTERY_GATEWAY_SERVICE_UIDS=1000,100000-165535' "$C/run/host.env"; then
    ok "host.env carries the configured user ids"
  else
    fail "host.env's gateway service user ids are: $(grep GATEWAY_SERVICE_UIDS "$C/run/host.env")"
  fi
  for p in 10250:kubelet 10248:"kubelet health" 10270:"Exec Agent" 9090:flintlockd 1338:"containerd metrics"; do
    if service_rules | grep -Eq "tcp dport \{.* ${p%%:*}[ ,}]"; then ok "the ${p#*:} port ${p%%:*} is closed to them"; else fail "the ${p#*:} port ${p%%:*} is open to them"; fi
  done
  # Nothing else is taken from them: no rule of theirs names a gateway
  # service port or an accept.
  if service_rules | grep -Eq '5000[,} ]|accept'; then
    fail "a rule for the gateway service ids touches a gateway service port: $(service_rules)"
  else
    ok "no rule for them names a gateway service port or an accept"
  fi
else
  fail "host-config refuses GATEWAY_SERVICE_UIDS=1000,100000-165535"
fi

# flintlockd's port is closed to them even when the control ports leave it
# out.
printf 'GATEWAY_SERVICE_UIDS=2000\nHOST_CONTROL_PORTS=10250,10270\n' >"$C.conf"
if render "$C.conf"; then
  if [ "$(service_rules)" = "$(expected_rules 2000 '10250, 10270, 9090')" ]; then
    ok "flintlockd's port is added to control ports that leave it out"
  else
    fail "the rules for HOST_CONTROL_PORTS=10250,10270 are: $(service_rules)"
  fi
else
  fail "host-config refuses GATEWAY_SERVICE_UIDS=2000 with HOST_CONTROL_PORTS=10250,10270"
fi

# Root, the Exec Agent's id and anything that is not a list of ids stop
# the Host rather than dropping the wrong traffic or none. The command
# substitution is a literal value: the file is parsed, never sourced.
# shellcheck disable=SC2016
for bad in 0 0-100 root -5 5-3 4294967295 1000,900-1100 1000,1000 65532 60000-70000 '1000;reboot' '$(id -u)' '1000,' ',1000'; do
  printf 'GATEWAY_SERVICE_UIDS=%s\n' "$bad" >"$C.conf"
  if render "$C.conf"; then
    fail "host-config accepts GATEWAY_SERVICE_UIDS='$bad'"
  elif grep -q '^host configuration invalid: GATEWAY_SERVICE_UIDS' "$C/run/not-ready.d/battery-host-config" 2>/dev/null; then
    ok "host-config refuses GATEWAY_SERVICE_UIDS='$bad' and says why"
  else
    fail "host-config refuses GATEWAY_SERVICE_UIDS='$bad' without recording why"
  fi
done

# Enforcement, where this machine allows it. Each attempt loads the ruleset
# into a new user and network namespace, where this process is the given
# user, with an address on a dummy interface as the Host's primary address,
# the bridge gateway address as a second one, and default routes out of the
# dummy, which swallows what it is sent. A last rule that only counts is
# appended to the output chain, and a connection is attempted: the counters
# of the three rules above say whether they dropped it, and the last one
# whether it went on past them.
# attempt UID HOST PORT: prints "DROPPED PASSED", in packets. The inner
# script expands its own arguments.
# shellcheck disable=SC2016
attempt() {
  unshare --user --map-user="$1" --map-group="$1" --net --keep-caps bash -c '
    { ip link set lo up && ip link add d0 type dummy && ip link set d0 up &&
      ip addr add 192.0.2.2/24 dev d0 && ip addr add 10.200.4.1/22 dev d0 &&
      ip route add default via 192.0.2.1 dev d0 &&
      ip -6 addr add 2001:db8::2/64 dev d0 nodad && ip -6 route add default via 2001:db8::1 dev d0 &&
      nft -f "$1" && nft add rule inet battery output tcp dport "$3" counter comment \"passed\"; } >/dev/null 2>&1 || exit 3
    timeout 1 bash -c "exec 3<>/dev/tcp/$2/$3" 2>/dev/null
    nft list chain inet battery output | awk "
      /meta skuid \\{/ { for (i = 1; i < NF; i++) if (\$i == \"packets\") d += \$(i + 1) }
      /comment \"passed\"/ { for (i = 1; i < NF; i++) if (\$i == \"packets\") p += \$(i + 1) }
      END { print d + 0, p + 0 }"
  ' _ "$C/net/guest-firewall.nft" "$2" "$3" 2>/dev/null
}
dropped() {
  local n
  n=$(attempt "$1" "$2" "$3")
  if [ "${n% *}" -ge 1 ] 2>/dev/null && [ "${n#* }" = 0 ]; then ok "enforced: $4 is dropped"; else fail "enforced: $4 was let through (dropped, passed: $n)"; fi
}
passed() {
  local n
  n=$(attempt "$1" "$2" "$3")
  if [ "${n% *}" = 0 ] && [ "${n#* }" -ge 1 ] 2>/dev/null; then ok "enforced: $4 is let through"; else fail "enforced: $4 was dropped or never sent (dropped, passed: $n)"; fi
}
printf 'GUEST_SUBNET=10.200.4.0/22\nGATEWAY_SERVICE_PORTS=5000\nGATEWAY_SERVICE_UIDS=1000,100000-165535\n' >"$C.conf"
if ! command -v nft >/dev/null 2>&1 || ! command -v ip >/dev/null 2>&1 || ! command -v timeout >/dev/null 2>&1; then
  printf 'skip  enforcement: nft, ip or timeout is not installed\n'
elif ! render "$C.conf" || ! unshare --user --map-user=1000 --net --keep-caps true 2>/dev/null ||
  [ -z "$(attempt 1000 127.0.0.1 1)" ]; then
  # attempt prints nothing when the namespace could not be set up.
  printf 'skip  enforcement: no unprivileged user and network namespace with nftables here\n'
else
  dropped 1000 169.254.169.254 80 "a gateway service (1000) to the metadata service"
  dropped 1000 fd00:ec2::254 80 "a gateway service (1000) to the IPv6 metadata service"
  dropped 100500 169.254.169.254 80 "an id in a listed range (100500) to the metadata service"
  dropped 1000 127.0.0.1 10250 "a gateway service (1000) to the kubelet on loopback"
  dropped 1000 192.0.2.2 10248 "a gateway service (1000) to the kubelet's health port on the Host's primary address"
  dropped 1000 192.0.2.2 10270 "a gateway service (1000) to the Exec Agent on the Host's primary address"
  dropped 1000 10.200.4.1 1338 "a gateway service (1000) to the metrics port on the bridge gateway"
  passed 1000 10.200.4.1 5000 "a gateway service (1000) to a gateway service port on the bridge gateway"
  passed 1000 198.51.100.7 443 "a gateway service (1000) to an address on the internet"
  passed 4242 169.254.169.254 80 "a user that runs no gateway service (4242) to the metadata service"
fi

rm -rf "$C" "$C.conf" "$C.absent.conf"
[ "$failures" -eq 0 ]
