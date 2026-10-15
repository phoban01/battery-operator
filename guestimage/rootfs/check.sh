#!/usr/bin/env bash
# The check stage of the Guest Images' root filesystem
# (guestimage/rootfs/Containerfile). The Dagger module runs it again in the
# built image (guest-image-check).
#
# usage: check.sh GUEST_AGENT_VERSION
#
# It runs inside the root filesystem, under emulation on a platform other
# than the build machine's. It needs neither a running systemd, nor KVM,
# nor a network: it reads the files. It reports every failure before it
# exits non-zero.
#
#= docs/requirements/13-guest-image.md#guest-build
#= type=test
#/ The Guest Images build SHALL run a check stage for each image
#/ that fails unless the image holds what this document requires of it.
set -uo pipefail
export PATH=/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

want_agent=${1:?usage: check.sh GUEST_AGENT_VERSION}
failures=0
ok() { printf 'ok    %s\n' "$*"; }
fail() {
	printf 'FAIL  %s\n' "$*"
	failures=$((failures + 1))
}
enabled() { [ "$(systemctl is-enabled "$1" 2>/dev/null)" = enabled ]; }

#= docs/requirements/13-guest-image.md#guest-build
#= type=test
#/ The Guest Images SHALL be a kernel image and a root filesystem
#/ image, each defined by one Containerfile in `guestimage/` and built for
#/ `linux/amd64` and `linux/arm64`.
arch=$(dpkg --print-architecture)
case $arch in
amd64 | arm64) ok "the root filesystem is built for linux/$arch" ;;
*) fail "the root filesystem is built for $arch, not amd64 or arm64" ;;
esac

#= docs/requirements/13-guest-image.md#guest-rootfs
#= type=test
#/ The Guest Images' root filesystem image SHALL be Ubuntu 24.04,
#/ with systemd as its init.
os=$(. /etc/os-release && echo "$ID $VERSION_ID")
if [ "$os" = "ubuntu 24.04" ]; then ok "the root filesystem is Ubuntu 24.04"; else fail "the root filesystem is '$os', not Ubuntu 24.04"; fi
if [ -x /lib/systemd/systemd ] && [ "$(readlink -f /sbin/init)" = "$(readlink -f /lib/systemd/systemd)" ]; then
	ok "/sbin/init is systemd"
else
	fail "/sbin/init is '$(readlink -f /sbin/init)', not systemd"
fi

#= docs/requirements/13-guest-image.md#guest-rootfs
#= type=test
#/ The Guest Images' root filesystem image SHALL contain
#/ flintlock's guest agent at the version its Containerfile pins, and SHALL
#/ enable the guest agent's `guest-agent.service` unit.
got=$(dpkg-query -W -f '${Version}' guest-agent 2>/dev/null)
if [ "$got" = "$want_agent" ]; then
	ok "the guest agent package is $got"
else
	fail "the guest agent package is '$got', not $want_agent"
fi
unit=$(systemctl cat guest-agent.service 2>/dev/null || cat /usr/lib/systemd/system/guest-agent.service /lib/systemd/system/guest-agent.service 2>/dev/null)
agent=$(sed -n 's/^ExecStart=-\{0,1\}//p' <<<"$unit" | tail -1 | awk '{print $1}')
if [ -n "$agent" ] && [ -x "$agent" ]; then
	ok "the guest agent's binary is $agent"
else
	fail "the guest agent's binary '$agent' is missing"
fi
if enabled guest-agent.service; then ok "guest-agent.service is enabled"; else fail "guest-agent.service is not enabled"; fi
if grep -qw network-online.target <<<"$(sed -n 's/^\(After\|Wants\|Requires\)=//p' <<<"$unit")"; then
	fail "the guest agent waits for network-online.target"
else
	ok "the guest agent does not wait for the network"
fi

#= docs/requirements/13-guest-image.md#guest-rootfs
#= type=test
#/ The Guest Images' root filesystem image SHALL enable
#/ `systemd-networkd`, and SHALL configure every Ethernet interface except
#/ `eth0` by DHCP for IPv4.
#
# systemd-networkd takes the first .network file, in lexical order, whose
# [Match] section matches; match_file IFACE prints it.
match_file() {
	local iface=$1 name f pattern patterns
	while IFS= read -r name; do
		f=/etc/systemd/network/$name
		[ -f "$f" ] || f=/usr/lib/systemd/network/$name
		# read splits the Name= list without expanding its globs.
		read -ra patterns <<<"$(sed -n 's/^Name=//p' "$f" | tr '\n' ' ')"
		for pattern in "${patterns[@]}"; do
			# shellcheck disable=SC2254 # the pattern is a glob on purpose
			case $iface in $pattern) echo "$f"; return 0 ;; esac
		done
	done < <(find /etc/systemd/network /usr/lib/systemd/network -maxdepth 1 -name '*.network' -printf '%f\n' 2>/dev/null | sort -u)
	return 1
}
for u in systemd-networkd.service systemd-resolved.service; do
	if enabled "$u"; then ok "$u is enabled"; else fail "$u is not enabled"; fi
done
for iface in eth1 eth2; do
	f=$(match_file "$iface") || {
		fail "no .network file matches $iface"
		continue
	}
	if grep -qx 'DHCP=ipv4' "$f" && grep -qx 'Type=ether' "$f"; then
		ok "$iface is configured by DHCP for IPv4 ($(basename "$f"))"
	else
		fail "$iface matches $(basename "$f"), which does not configure it by DHCP for IPv4"
	fi
done
timeout=$(grep -ohE -- '--timeout=[0-9]+' /etc/systemd/system/systemd-networkd-wait-online.service.d/*.conf 2>/dev/null | tail -1 | cut -d= -f2)
if [ -n "$timeout" ] && [ "$timeout" -le 10 ]; then
	ok "the wait for the network ends after ${timeout}s"
else
	fail "the wait for the network has no timeout of 10s or less"
fi

#= docs/requirements/13-guest-image.md#guest-rootfs
#= type=test
#/ The Guest Images' root filesystem image SHALL leave `eth0`
#/ unmanaged.
f=$(match_file eth0) || f=''
if [ -n "$f" ] && grep -qx 'Unmanaged=yes' "$f"; then
	ok "eth0 is unmanaged ($(basename "$f"))"
else
	fail "eth0 is not left unmanaged${f:+ ($(basename "$f"))}"
fi

#= docs/requirements/13-guest-image.md#guest-rootfs
#= type=test
#/ The Guest Images' root filesystem image SHALL hold an empty
#/ `/etc/machine-id`.
if [ -f /etc/machine-id ] && [ ! -s /etc/machine-id ]; then
	ok "/etc/machine-id is empty"
else
	fail "/etc/machine-id is missing or holds an ID"
fi

if [ "$failures" -gt 0 ]; then
	echo "$failures root filesystem check(s) failed" >&2
	exit 1
fi
echo "every root filesystem check passed"
