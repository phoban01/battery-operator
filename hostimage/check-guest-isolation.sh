#!/usr/bin/env bash
# Runs /usr/libexec/battery/host-config and `network render` against a Host
# configuration file with protected CIDRs, and checks the guest firewall
# rules that keep a MicroVM away from the cluster: the Host's own addresses,
# the Host's other interfaces, and Services behind destination NAT (HI-075
# to HI-077), the gateway service ports that open the gateway to guests
# and to nothing else (HI-078, HI-079), and the guest allowed CIDRs that
# open chosen addresses in a protected range and nothing else (HI-084 to
# HI-086). Installed as
# /usr/libexec/battery/check-guest-isolation-cases and
# called by the check stage; `hostimage/check-guest-isolation.sh DIR LIBEXEC
# DEFAULTS` runs it outside the image against the sources, which
# `make host-image-lint` does.
#
# Where this machine has nft, ip, nsenter and unprivileged user and network
# namespaces, the ruleset is also loaded into a stand-in Host, and a stand-in
# guest connects to the outside, to the gateway, to the Host's primary
# address, to a pod on the Host and to a Service that the Host translates.
# A pod on the Host, and the Host itself, connect to a gateway service port.
# The guest also connects to an allowed and a protected address of a
# stand-in VPC endpoint, to allowed Services, and to the metadata service,
# the Host and a pod on the Host, which the allowed list names too.
#
#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ Where the Host configuration file lists guest allowed CIDRs, the
#/ Host Image SHALL treat an address in a guest allowed CIDR as outside
#/ every protected CIDR.
#
#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL drop traffic from the guest subnet to the
#/ instance metadata service addresses, to every address of the Host other
#/ than the bridge gateway address, to the Host's own `flintlockd`, kubelet,
#/ Exec Agent and metrics ports, and to every interface of the Host other
#/ than its primary interface, whatever the guest allowed CIDRs hold.
#
#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ If an entry of the guest allowed CIDRs in the Host configuration
#/ file is not an IPv4 CIDR, then the Host Image SHALL NOT start
#/ `flintlockd` and SHALL report the Host as not ready with the reason that
#/ the Host configuration file is invalid.
#
#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL allow TCP traffic from the guest subnet to
#/ the bridge gateway address on the gateway service ports, which it reads
#/ from the Host configuration file, and SHALL allow it on no port when
#/ none are set.
#
#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL drop traffic to the gateway service ports on
#/ the bridge gateway address that arrives on any interface other than the
#/ bridge and loopback.
#
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
set -uo pipefail
work=${1:-$(mktemp -d)}
libexec=${2:-/usr/libexec/battery}
defaults=${3:-/usr/share/battery/host.conf.defaults}
C=$work/guest-isolation-cases
failures=0
ok() { printf 'ok    %s\n' "$*"; }
fail() {
  printf 'FAIL  %s\n' "$*"
  failures=$((failures + 1))
}

# The stand-in Host: a guest subnet, a primary address, a pod network on
# the Host and a Service range, of which the node and Service ranges are
# protected and the pod network is not. The Host serves TCP ports 5000 and
# 3128 on the gateway; 1234 is a port it does not list.
gateway=10.200.4.1
guest=10.200.4.10
primary=192.0.2.10
outside=192.0.2.1
pod=10.244.0.2
service=10.96.0.10
# A VPC interface endpoint in the protected node range: the guest may reach
# one of its addresses and not the other. A second Service is allowed too.
# The allowed list also names the metadata service, the Host's primary
# address and the pod, which stay closed to guests.
endpoint=10.0.5.20
endpoint_other=10.0.5.21
allowed_service=10.96.0.20
metadata=169.254.169.254
allowed="$endpoint/32, $allowed_service/32,$metadata/32, $primary/32,$pod/32"
nftf=$C/net/guest-firewall.nft
# The guest bridge, as lib.sh names it.
bridge=$(sed -n 's/^BATTERY_BRIDGE=//p' "$libexec/lib.sh")

# render CONF: host-config with CONF as the Host configuration file, then
# the firewall into $nftf. Fails when host-config refuses CONF.
render() {
  rm -rf "$C"
  mkdir -p "$C/run/not-ready.d"
  local env=(BATTERY_PATH="$PATH" BATTERY_LIB="$libexec/lib.sh" BATTERY_HOST_DEFAULTS="$defaults" BATTERY_HOST_CONF="$1"
    BATTERY_RUN="$C/run" BATTERY_NOT_READY_DIR="$C/run/not-ready.d" BATTERY_HOST_ENV="$C/run/host.env")
  env "${env[@]}" bash "$libexec/host-config" >/dev/null 2>&1 || return 1
  env "${env[@]}" BATTERY_PRIMARY_INTERFACE=eth0 BATTERY_PRIMARY_ADDRESS=$primary bash "$libexec/network" render "$C/net" >/dev/null 2>&1
}

# chain NAME prints the rules of a rendered chain, one per line, without
# comments or indentation, and with the bridge's name as BRIDGE.
chain() {
  awk -v c="$1" '$0 ~ "^\tchain " c " \\{" { on = 1; next } on && /^\t\}/ { exit } on' "$nftf" |
    sed -e 's/#.*//' -e 's/^[[:space:]]*//' -e '/^$/d' -e "s/\"$bridge\"/\"BRIDGE\"/g"
}

# HI-078: with no Host configuration file, the default opens no port on the
# gateway beyond DHCP and DNS, and nothing else is written for it.
if render "$C.absent.conf"; then
  if grep -qx 'BATTERY_GATEWAY_SERVICE_PORTS=' "$C/run/host.env"; then
    ok "host.env carries the empty default of GATEWAY_SERVICE_PORTS"
  else
    fail "host.env lacks BATTERY_GATEWAY_SERVICE_PORTS="
  fi
  accepts=$(chain input | grep '^iifname "BRIDGE".*accept$' | sed "s/10\\.220\\.0\\.1/GW/g")
  want=$(printf '%s\n' 'iifname "BRIDGE" ip daddr { 255.255.255.255, GW } udp dport 67 accept' \
    'iifname "BRIDGE" ip daddr GW udp dport 53 accept' 'iifname "BRIDGE" ip daddr GW tcp dport 53 accept')
  if [ "$accepts" = "$want" ]; then
    ok "by default guests reach only DHCP and DNS on the gateway"
  else
    fail "by default the input chain accepts from guests: $(printf '%s\n' "$accepts" | tr '\n' ';')"
  fi
  if chain input | grep -q '^iifname != { "BRIDGE", "lo" }'; then
    fail "by default the input chain has a rule for gateway service ports"
  else
    ok "by default the input chain has no rule for gateway service ports"
  fi
else
  fail "host-config or network render fails without a Host configuration file"
fi

# A port that is also a control port, or flintlockd's, would open it to
# guests, since the gateway's accept comes before the control ports' drop.
# Those and anything that is not a list of ports stop the Host.
# shellcheck disable=SC2016
for bad in 10250 9090 5000,10270 0 65536 http 5000,5000 '5000,' '5000;reboot' '$(id -u)'; do
  printf 'GATEWAY_SERVICE_PORTS=%s\n' "$bad" >"$C.conf"
  if render "$C.conf"; then
    fail "host-config accepts GATEWAY_SERVICE_PORTS='$bad'"
  elif grep -q '^host configuration invalid: GATEWAY_SERVICE_PORTS' "$C/run/not-ready.d/battery-host-config" 2>/dev/null; then
    ok "host-config refuses GATEWAY_SERVICE_PORTS='$bad' and says why"
  else
    fail "host-config refuses GATEWAY_SERVICE_PORTS='$bad' without recording why"
  fi
done

# HI-084: by default the allowed list is empty, and the set has no
# elements, so the protected drops skip nothing.
if render "$C.absent.conf"; then
  if grep -qx 'BATTERY_GUEST_ALLOWED_CIDRS=' "$C/run/host.env"; then
    ok "host.env carries the empty default of GUEST_ALLOWED_CIDRS"
  else
    fail "host.env lacks BATTERY_GUEST_ALLOWED_CIDRS="
  fi
  if awk '/^\tset guest_allowed \{/ { on = 1; next } on && /^\t\}/ { exit } on' "$nftf" | grep -q elements; then
    fail "by default the guest_allowed set has elements"
  else
    ok "by default the guest_allowed set is empty"
  fi
fi

# HI-086: an entry that is not an IPv4 CIDR stops the Host, and says why.
# shellcheck disable=SC2016
for bad in 10.0.5.20 10.0.5.300/32 10.0.5.20/33 fd00::/64 '10.0.5.20/32,' '10.0.5.20/32,,10.0.5.21/32' \
  '10.0.5.20/32;reboot' '$(id -u)'; do
  printf 'GUEST_ALLOWED_CIDRS=%s\n' "$bad" >"$C.conf"
  if render "$C.conf"; then
    fail "host-config accepts GUEST_ALLOWED_CIDRS='$bad'"
  elif grep -q '^host configuration invalid: GUEST_ALLOWED_CIDRS' "$C/run/not-ready.d/battery-host-config" 2>/dev/null; then
    ok "host-config refuses GUEST_ALLOWED_CIDRS='$bad' and says why"
  else
    fail "host-config refuses GUEST_ALLOWED_CIDRS='$bad' without recording why"
  fi
done

printf 'GUEST_SUBNET=10.200.4.0/22\nPROTECTED_CIDRS=10.0.0.0/16,10.96.0.0/12\nGATEWAY_SERVICE_PORTS = "5000, 03128"\nGUEST_ALLOWED_CIDRS="%s"\n' "$allowed" >"$C.conf"
if ! render "$C.conf"; then
  fail "host-config or network render fails for $(tr '\n' ' ' <"$C.conf")"
  rm -rf "$C" "$C.conf" "$C.absent.conf"
  exit 1
fi

# HI-078 and HI-079: the listed ports, in plain decimal, open on the gateway
# to guests, and closed to every other interface but loopback.
input=$(chain input)
if grep -qx 'BATTERY_GATEWAY_SERVICE_PORTS=5000,3128' "$C/run/host.env"; then
  ok "host.env carries the configured gateway service ports"
else
  fail "host.env's gateway service ports are: $(grep GATEWAY_SERVICE_PORTS "$C/run/host.env")"
fi
if printf '%s\n' "$input" | grep -qxF "iifname \"BRIDGE\" ip daddr $gateway tcp dport { 5000, 3128 } accept"; then
  ok "guests may reach TCP ports 5000 and 3128 on the gateway"
else
  fail "the input chain does not accept guests on ports 5000 and 3128 of the gateway: $(printf '%s\n' "$input" | tr '\n' ';')"
fi
if printf '%s\n' "$input" | grep -qxF "iifname != { \"BRIDGE\", \"lo\" } ip daddr $gateway tcp dport { 5000, 3128 } drop"; then
  ok "ports 5000 and 3128 on the gateway are dropped on every interface but the bridge and loopback"
else
  fail "the input chain does not drop ports 5000 and 3128 of the gateway from other interfaces"
fi
if [ "$(printf '%s\n' "$input" | grep -c '5000')" = 2 ]; then
  ok "no other rule names a gateway service port"
else
  fail "other rules name port 5000: $(printf '%s\n' "$input" | grep 5000 | tr '\n' ';')"
fi

# HI-075: after the drops, a guest's packet leaves by the primary
# interface or not at all.
forward=$(chain forward)
want=$(printf '%s\n' 'iifname "BRIDGE" oifname "eth0" accept' 'iifname "BRIDGE" drop')
if printf '%s\n' "$forward" | grep -A1 -xF 'iifname "BRIDGE" oifname "eth0" accept' | diff -q - <(echo "$want") >/dev/null; then
  ok "the forward chain lets guests out of the primary interface and drops them towards every other"
else
  fail "the forward chain's rules for the bridge are: $(printf '%s\n' "$forward" | grep '^iifname "BRIDGE"' | tr '\n' ';')"
fi
bridge_accepts=$(printf '%s\n' "$forward" | grep '^iifname "BRIDGE"' | grep -c 'accept$')
if [ "$bridge_accepts" = 1 ]; then
  ok "the primary interface is the only way out the forward chain accepts for guests"
else
  fail "the forward chain has $bridge_accepts accepts for guests"
fi

# HI-076: the destination the guest asked for, before kube-proxy's DNAT.
original=$(printf '%s\n' "$forward" | grep -n -xF 'iifname "BRIDGE" ct original ip daddr @protected ct original ip daddr != @guest_allowed drop' | cut -d: -f1)
accept=$(printf '%s\n' "$forward" | grep -n -xF 'iifname "BRIDGE" oifname "eth0" accept' | cut -d: -f1)
if [ -n "$original" ] && [ -n "$accept" ] && [ "$original" -lt "$accept" ]; then
  ok "guest traffic whose original destination is protected is dropped before guests are let out"
else
  fail "the forward chain does not drop the original destination in @protected before its accept"
fi

# HI-084: the allowed list, as written, and both protected drops skip it.
if grep -qxF "BATTERY_GUEST_ALLOWED_CIDRS=$endpoint/32,$allowed_service/32,$metadata/32,$primary/32,$pod/32" "$C/run/host.env"; then
  ok "host.env carries the configured guest allowed CIDRs"
else
  fail "host.env's guest allowed CIDRs are: $(grep GUEST_ALLOWED_CIDRS "$C/run/host.env")"
fi
if grep -qxF "		elements = { $endpoint/32, $allowed_service/32, $metadata/32, $primary/32, $pod/32 }" "$nftf"; then
  ok "the guest_allowed set holds the configured CIDRs"
else
  fail "the guest_allowed set does not hold the configured CIDRs"
fi
protected=$(printf '%s\n' "$forward" | grep -n -xF 'iifname "BRIDGE" ip daddr @protected ip daddr != @guest_allowed drop' | cut -d: -f1)
if [ -n "$protected" ] && [ -n "$accept" ] && [ "$protected" -lt "$accept" ]; then
  ok "guest traffic to a protected address that is not allowed is dropped before guests are let out"
else
  fail "the forward chain does not drop @protected less @guest_allowed before its accept"
fi

# HI-085: the allowed set appears in those two drops and nowhere else, so
# it accepts nothing, and the metadata drops come before both.
uses=$(sed 's/#.*//' "$nftf" | grep '@guest_allowed' | sed -e 's/^[[:space:]]*//' -e "s/\"$bridge\"/\"BRIDGE\"/g")
want=$(printf '%s\n' "$forward" | grep -F '@guest_allowed')
if [ -n "$uses" ] && [ "$uses" = "$want" ] && [ "$(printf '%s\n' "$uses" | wc -l)" = 2 ]; then
  ok "only the two protected drops name @guest_allowed"
else
  fail "the rules that name @guest_allowed are: $(printf '%s\n' "$uses" | tr '\n' ';')"
fi
meta=$(printf '%s\n' "$forward" | grep -n -xF "iifname \"BRIDGE\" ip daddr $metadata drop" | cut -d: -f1)
if [ -n "$meta" ] && [ -n "$protected" ] && [ "$meta" -lt "$protected" ] && [ -n "$original" ] && [ "$meta" -lt "$original" ]; then
  ok "the metadata service is dropped before the protected drops, whatever the allowed list holds"
else
  fail "the forward chain does not drop the metadata service before the protected drops"
fi

# HI-077: every rule of the input chain that accepts from the bridge names
# the gateway, or is DHCP's broadcast, and the chain ends with a drop.
input=$(chain input)
stray=$(printf '%s\n' "$input" | grep '^iifname "BRIDGE".*accept$' |
  grep -vE "ip daddr ($gateway|\\{ 255\\.255\\.255\\.255, $gateway \\}) " || true)
if [ -z "$stray" ]; then
  ok "every input rule that accepts from guests names the gateway, or the broadcast address for DHCP"
else
  fail "input rules accept guests on other addresses: $stray"
fi
if [ "$(printf '%s\n' "$input" | tail -n 1)" = 'iifname "BRIDGE" drop' ]; then
  ok "the input chain drops everything else from guests"
else
  fail "the input chain's last rule is: $(printf '%s\n' "$input" | tail -n 1)"
fi

# Enforcement, where this machine allows it. A new user and network
# namespace is the Host, with the rendered ruleset, the bridge at the
# gateway, eth0 with the primary address, and cni0 to a pod. kube-proxy is
# a DNAT rule that sends the Service to an address outside every protected
# range. The guest, the outside and the pod are network namespaces of their
# own. Nothing listens anywhere, so a connection that arrives is refused
# and one that is dropped times out.
# The inner script prints one line per attempt, NAME reached|dropped, and
# "pod forwarded" when the Host let a guest's packet out to the pod.
# shellcheck disable=SC2016
enforce() {
  unshare --user --map-root-user --net bash -c '
    set -u
    nftf=$1 gateway=$2 guest=$3 primary=$4 outside=$5 pod=$6 service=$7 bridge=$8
    endpoint=$9 endpoint_other=${10} allowed_service=${11} metadata=${12}
    ns() { unshare --net sleep 30 >/dev/null 2>&1 & echo $!; }
    inns() { local p=$1; shift; nsenter -t "$p" -n "$@"; }
    {
      g=$(ns) o=$(ns) p=$(ns)
      sleep 0.3
      ip link set lo up &&
      sysctl -q -w net.ipv4.ip_forward=1 &&
      ip link add "$bridge" type bridge && ip addr add $gateway/22 dev "$bridge" && ip link set "$bridge" up &&
      ip link add gh0 type veth peer name g0 && ip link set gh0 master "$bridge" && ip link set gh0 up &&
      ip link set g0 netns "$g" &&
      ip link add eth0 type veth peer name o0 && ip addr add $primary/24 dev eth0 && ip link set eth0 up &&
      ip link set o0 netns "$o" &&
      ip link add cni0 type veth peer name p0 && ip addr add 10.244.0.1/24 dev cni0 && ip link set cni0 up &&
      ip link set p0 netns "$p" &&
      inns "$g" sh -c "ip link set lo up && ip addr add $guest/22 dev g0 && ip link set g0 up && ip route add default via $gateway" &&
      inns "$o" sh -c "ip link set lo up && ip addr add $outside/24 dev o0 && ip link set o0 up" &&
      inns "$o" sh -c "ip addr add $endpoint/32 dev o0 && ip addr add $endpoint_other/32 dev o0 && ip addr add $metadata/32 dev o0" &&
      ip route add $endpoint/32 dev eth0 && ip route add $endpoint_other/32 dev eth0 && ip route add $metadata/32 dev eth0 &&
      inns "$p" sh -c "ip link set lo up && ip addr add $pod/24 dev p0 && ip link set p0 up && ip route add default via 10.244.0.1" &&
      nft -f "$nftf" &&
      nft -f - <<EOF
table ip probe {
	chain forward {
		type filter hook forward priority filter + 10; policy accept;
		oifname "cni0" counter
	}
}
table ip kube_proxy {
	chain prerouting {
		type nat hook prerouting priority dstnat; policy accept;
		ip daddr $service tcp dport 80 dnat to $outside:80
		ip daddr $service tcp dport 81 dnat to $endpoint:81
		ip daddr $allowed_service tcp dport 80 dnat to $outside:80
		ip daddr $allowed_service tcp dport 81 dnat to $endpoint_other:81
	}
}
EOF
    } >/dev/null 2>&1 || exit 3
    # probe NAME FROM HOST PORT connects from the namespace of process
    # FROM, or from the Host itself when FROM is "host".
    probe() {
      local out
      if [ "$2" = host ]; then
        out=$(timeout 2 bash -c "exec 3<>/dev/tcp/$3/$4" 2>&1)
      else
        out=$(inns "$2" timeout 2 bash -c "exec 3<>/dev/tcp/$3/$4" 2>&1)
      fi
      case $? in
      124) echo "$1 dropped" ;;
      *) case $out in *refused*) echo "$1 reached" ;; *) echo "$1 unknown" ;; esac ;;
      esac
    }
    try() { probe "$1" "$g" "$2" "$3"; }
    try outside $outside 80
    try gateway $gateway 53
    try gateway-service $gateway 5000
    try gateway-other $gateway 1234
    probe pod-to-service "$p" $gateway 5000
    probe host-to-service host $gateway 5000
    try primary $primary 22
    try pod $pod 80
    # The pod cannot answer a guest either way, because the Host drops
    # replies to the bridge from anywhere but the primary interface; a
    # chain after the guest firewall counts what got past it.
    n=$(nft list chain ip probe forward | sed -n "s/.*oifname \"cni0\" counter packets \([0-9]*\) .*/\1/p")
    [ "${n:-0}" = 0 ] || echo "pod forwarded"
    try service $service 80
    try service-to-allowed $service 81
    try endpoint $endpoint 443
    try endpoint-other $endpoint_other 443
    try allowed-service $allowed_service 80
    try allowed-service-to-protected $allowed_service 81
    try metadata $metadata 80
    kill "$g" "$o" "$p" 2>/dev/null
  ' _ "$nftf" $gateway $guest $primary $outside $pod $service "$bridge" $endpoint $endpoint_other $allowed_service $metadata 2>/dev/null
}
if ! command -v nft >/dev/null 2>&1 || ! command -v ip >/dev/null 2>&1 || ! command -v nsenter >/dev/null 2>&1; then
  printf 'skip  enforcement: nft, ip or nsenter is not installed\n'
else
  results=$(enforce)
  if ! printf '%s\n' "$results" | grep -qx 'outside reached'; then
    printf 'skip  enforcement: no unprivileged user and network namespaces with bridges, veth and nftables here\n'
  else
    result() { printf '%s\n' "$results" | sed -n "s/^$1 //p"; }
    ok "enforced: a guest reaches an address outside the Host through the primary interface"
    if [ "$(result gateway)" = reached ]; then ok "enforced: a guest reaches DNS on the gateway"; else fail "enforced: DNS on the gateway is $(result gateway)"; fi
    if [ "$(result gateway-service)" = reached ]; then ok "enforced: a guest reaches gateway service port 5000"; else fail "enforced: gateway service port 5000 is $(result gateway-service) for a guest"; fi
    if [ "$(result gateway-other)" = dropped ]; then ok "enforced: a guest's traffic to port 1234 on the gateway, which is not listed, is dropped"; else fail "enforced: port 1234 on the gateway is $(result gateway-other)"; fi
    if [ "$(result pod-to-service)" = dropped ]; then ok "enforced: a pod on the Host cannot reach gateway service port 5000"; else fail "enforced: gateway service port 5000 is $(result pod-to-service) for a pod on the Host"; fi
    if [ "$(result host-to-service)" = reached ]; then ok "enforced: the Host itself reaches gateway service port 5000"; else fail "enforced: gateway service port 5000 is $(result host-to-service) for the Host itself"; fi
    if [ "$(result primary)" = dropped ]; then ok "enforced: a guest's traffic to the Host's primary address is dropped, although the allowed list names it"; else fail "enforced: the Host's primary address is $(result primary)"; fi
    if [ "$(result pod | xargs)" = dropped ]; then ok "enforced: a guest's traffic to a pod on the Host, outside every protected range and in the allowed list, is dropped"; else fail "enforced: a pod on the Host is $(result pod | xargs)"; fi
    if [ "$(result service)" = dropped ]; then ok "enforced: a guest's traffic to a protected Service address is dropped after kube-proxy's DNAT"; else fail "enforced: a protected Service address is $(result service)"; fi
    # HI-084 and HI-085, against the allowed list.
    if [ "$(result endpoint)" = reached ]; then ok "enforced: a guest reaches an allowed address inside a protected range"; else fail "enforced: an allowed address inside a protected range is $(result endpoint)"; fi
    if [ "$(result endpoint-other)" = dropped ]; then ok "enforced: a guest's traffic to the rest of the protected range is dropped"; else fail "enforced: a protected address next to an allowed one is $(result endpoint-other)"; fi
    if [ "$(result allowed-service)" = reached ]; then ok "enforced: a guest reaches an allowed Service address that DNAT sends outside every protected range"; else fail "enforced: an allowed Service address is $(result allowed-service)"; fi
    if [ "$(result allowed-service-to-protected)" = dropped ]; then ok "enforced: an allowed Service address that DNAT sends to a protected address is dropped"; else fail "enforced: an allowed Service sent to a protected address is $(result allowed-service-to-protected)"; fi
    if [ "$(result service-to-allowed)" = dropped ]; then ok "enforced: a protected Service address that DNAT sends to an allowed address is dropped"; else fail "enforced: a protected Service sent to an allowed address is $(result service-to-allowed)"; fi
    if [ "$(result metadata)" = dropped ]; then ok "enforced: the metadata service stays dropped although the allowed list names it"; else fail "enforced: the metadata service, in the allowed list, is $(result metadata)"; fi
  fi
fi

rm -rf "$C" "$C.conf" "$C.absent.conf"
[ "$failures" -eq 0 ]
