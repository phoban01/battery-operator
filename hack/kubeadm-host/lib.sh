#!/usr/bin/env bash
# Shared settings and helpers of the kubeadm Host proof (README.md). Sourced
# by up.sh, smoke.sh and down.sh; not run on its own.
#
# Two VMs on an Apple silicon Mac, on the Mac's shared NAT network:
#
#   CP_NAME    a Lima VM with a kubeadm control plane and Calico, which
#              also runs the Operator and cert-manager.
#   NODE_NAME  the Host: a vfkit VM booted from a disk made of the Host
#              Image, with nested virtualization and a second disk for the
#              thin pool. cloud-init joins it with user-data rendered from
#              config/capi's KubeadmConfigTemplate.
#
# The real hosts trial's helpers (../real-hosts/lib.sh) reach the Host over
# ssh, its HOST_DRIVER=ssh, so its images step and smoke test run against
# this Host unchanged.

KH_HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# The Host's name: its Node's and its VM's.
NODE_NAME="${NODE_NAME:-bo-kubeadm-host-1}"
STATE_DIR="${STATE_DIR:-${KH_HERE}/.state}"
HOST_DRIVER=ssh
# Set once the Host has an address (host_address).
HOST_SSH="${HOST_SSH:-}"
SSH_OPTS="${SSH_OPTS:-}"
# shellcheck source=hack/real-hosts/lib.sh
source "${KH_HERE}/../real-hosts/lib.sh"

# The control plane VM.
CP_NAME="${CP_NAME:-bo-kubeadm-cp}"
CP_CPUS="${CP_CPUS:-2}"
CP_MEMORY="${CP_MEMORY:-3GiB}"
CP_DISK="${CP_DISK:-20GiB}"
CP_TEMPLATE="${CP_TEMPLATE:-template:ubuntu-24.04}"
# The pod and Service ranges stay clear of the Host's guest subnet
# (10.220.0.0/16) and the Mac's networks, which are in 192.168.0.0/16:
# Calico's default pool would overlap them.
POD_CIDR="${POD_CIDR:-10.244.0.0/16}"
SERVICE_CIDR="${SERVICE_CIDR:-10.96.0.0/12}"
# Calico's version is the Hosts page's (site/hosts.md).
CALICO_VERSION="${CALICO_VERSION:-v3.32.2}"
LOCAL_PATH_VERSION="${LOCAL_PATH_VERSION:-v0.0.37}"

# The Host VM.
HOST_CPUS="${HOST_CPUS:-4}"
HOST_MEMORY_MIB="${HOST_MEMORY_MIB:-8192}"
HOST_ROOT_GIB="${HOST_ROOT_GIB:-20}"
HOST_THIN_POOL_GIB="${HOST_THIN_POOL_GIB:-20}"
# A fixed, locally administered MAC address, so that the Host's address can
# be found in the Mac's DHCP leases.
HOST_MAC_ADDRESS="${HOST_MAC_ADDRESS:-5a:94:ef:b0:6b:01}"
# The user cloud-init makes for the scripts, through the template's users.
HOST_USER="${HOST_USER:-battery}"

# The Host Image, built from this checkout (make host-image) and loaded into
# Docker under this name.
HOST_IMG="${HOST_IMG:-localhost/battery-host-image:dev}"

# vfkit, downloaded onto the Mac and checked. v0.6.4 is the first release
# with both --nested and --cloud-init. The checksum was computed from the
# release's signed binary.
VFKIT_VERSION="${VFKIT_VERSION:-v0.6.4}"
VFKIT_SHA256="${VFKIT_SHA256:-0ed83fc8ca7aa708598835480dba1362406aa7cd1dab3b27464eb76327d9652d}"

# KUBERNETES_VERSION is the Host Image's (hostimage/versions.env), and the
# control plane's too.
KUBERNETES_VERSION="$(sed -n 's/^KUBERNETES_VERSION=//p' "${REPO_ROOT}/hostimage/versions.env")"

# The Host's disks, vfkit and its logs live on the Mac, in MAC_WORK_DIR: vz
# reads the disks from the Mac's own filesystem. MAC_MOUNT is where the Mac's
# filesystem appears on this machine (OrbStack's /mnt/mac), or empty on the
# Mac itself.
if [[ "$(uname -s)" == Darwin ]]; then MAC_MOUNT=""; else MAC_MOUNT="${MAC_MOUNT:-/mnt/mac}"; fi
mac_home() { mac_run sh -c 'printf %s "$HOME"'; }
MAC_WORK_DIR="${MAC_WORK_DIR:-$(mac_home)/.cache/bo-kubeadm-host}"
# The same directory as seen from here.
LOCAL_WORK_DIR="${MAC_MOUNT}${MAC_WORK_DIR}"

KUBECONFIG_OUT="${STATE_DIR}/kubeconfig"
SSH_KEY="${STATE_DIR}/id_ed25519"

cp_run() { limactl_ shell --workdir / "${CP_NAME}" "$@"; }
cp_root() { cp_run sudo "$@"; }
cp_exists() { limactl_ list --quiet 2>/dev/null | grep -qx "${CP_NAME}"; }
cp_status() { limactl_ list --format '{{.Status}}' "${CP_NAME}" 2>/dev/null; }

# cp_ip is the control plane's address on the shared network (lima0).
cp_ip() {
	cp_run ip -4 -o addr show dev lima0 | awk '!found { sub("/.*", "", $4); print $4; found = 1 }'
}

# shared_gateway is the shared network's gateway: the Mac's address on it.
shared_gateway() {
	cp_run ip -4 route show dev lima0 | awk '/^default/ && !found { print $3; found = 1 }'
}

# shared_cidr is the shared network's range: the Nodes' addresses.
shared_cidr() {
	cp_run ip -4 route show dev lima0 scope link | awk '!found && $1 ~ /\// { print $1; found = 1 }'
}

# host_address is the Host's address, from the Mac's DHCP leases by its MAC
# address. macOS writes the address without leading zeros in each octet.
host_address() {
	local mac
	mac="$(awk -F: '{ for (i = 1; i <= NF; i++) { sub(/^0/, "", $i); printf "%s%s", $i, (i < NF ? ":" : "") } }' <<<"${HOST_MAC_ADDRESS}")"
	mac_run cat /var/db/dhcpd_leases 2>/dev/null |
		awk -v mac="1,${mac}" '
			/ip_address=/ { split($0, a, "="); ip = a[2] }
			/hw_address=/ { split($0, a, "="); if (a[2] == mac) found = ip }
			END { if (found != "") print found }'
}

# use_host points the real hosts helpers at the Host over ssh, as HOST_USER
# with the proof's key, through an ssh configuration file. The Host is on
# the Mac's shared network, which the Mac and an OrbStack machine route to.
# HOST_SSH_VIA_MAC=1 sends ssh through the Mac's nc for a machine that
# does not.
use_host() {
	local ip
	ip="$(host_address)"
	[[ -n "${ip}" ]] || die "the Host ${NODE_NAME} has no address in the Mac's DHCP leases yet"
	HOST_SSH="${HOST_USER}@${ip}"
	mkdir -p "${STATE_DIR}"
	{
		printf 'Host *\n'
		printf '  IdentityFile %s\n  IdentitiesOnly yes\n' "${SSH_KEY}"
		printf '  StrictHostKeyChecking no\n  UserKnownHostsFile /dev/null\n  LogLevel ERROR\n'
		printf '  ServerAliveInterval 15\n'
		[[ -z "${HOST_SSH_VIA_MAC:-}" ]] || printf '  ProxyCommand mac nc %%h %%p\n'
	} >"${STATE_DIR}/ssh_config"
	SSH_OPTS="-F ${STATE_DIR}/ssh_config"
}

# keep_mac_awake is keep_awake for this proof, whose Host is not a Lima VM.
keep_mac_awake() {
	mac_run caffeinate -i -t "${KEEP_AWAKE_SECS:-7200}" >/dev/null 2>&1 &
	disown
}
