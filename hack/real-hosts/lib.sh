#!/usr/bin/env bash
# Shared settings and helpers of the real hosts trial (README.md). Sourced by
# up.sh, down.sh and smoke.sh; not run on its own.
#
# The Host is one Linux machine with KVM, reached through a driver:
#
#   HOST_DRIVER=lima  (the default) a Lima VM with nested virtualization on
#                     a Mac, which up.sh creates and down.sh deletes. The
#                     scripts run on the Mac, or on a Linux machine that
#                     reaches it with `mac <command>` (an OrbStack machine).
#   HOST_DRIVER=ssh   a Linux machine that already exists (bare metal, or a
#                     cloud instance with nested virtualization), reached
#                     with `ssh ${HOST_SSH}`. up.sh installs onto it and
#                     down.sh leaves it alone.
#
# Every command for the Host goes through host_run or host_root, so the
# rest does not care which driver it is.

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../.." && pwd)"

HOST_DRIVER="${HOST_DRIVER:-lima}"
case "${HOST_DRIVER}" in
lima | ssh) ;;
*) printf 'FAIL: HOST_DRIVER is lima or ssh, not %s\n' "${HOST_DRIVER}" >&2; exit 1 ;;
esac

# The Node's name, and with the lima driver the VM's. The Host is the k3s
# server, schedulable, and the only Host.
NODE_NAME="${NODE_NAME:-bo-host-1}"

# The lima driver's VM.
VM_CPUS="${VM_CPUS:-4}"
VM_MEMORY="${VM_MEMORY:-6GiB}"
VM_DISK="${VM_DISK:-40GiB}"
VM_TEMPLATE="${VM_TEMPLATE:-template:ubuntu-24.04}"

# The ssh driver's machine: an ssh destination, extra ssh options, and the
# address the API server is reached on from here (by default the
# destination's host part).
HOST_SSH="${HOST_SSH:-}"
SSH_OPTS="${SSH_OPTS:-}"

# The kubeconfig up.sh writes. With the lima driver the API server is
# reached through Lima's forward of the VM's 6443 to the Mac's
# API_HOST_PORT.
STATE_DIR="${STATE_DIR:-${HERE}/.state}"
KUBECONFIG_OUT="${STATE_DIR}/kubeconfig"
API_HOST_PORT="${API_HOST_PORT:-16443}"

# Versions. flintlockd v0.15.2 is the oldest battery v0.3.3 provisions on
# (battery's flintlockclient.MinFlintlockVersion).
K3S_VERSION="${K3S_VERSION:-v1.35.8+k3s1}"
CONTAINERD_VERSION="${CONTAINERD_VERSION:-1.7.22}"
FIRECRACKER_VERSION="${FIRECRACKER_VERSION:-v1.12.1}"
FLINTLOCK_VERSION="${FLINTLOCK_VERSION:-v0.15.2}"
REGISTRY_VERSION="${REGISTRY_VERSION:-3.1.2}"
GUEST_AGENT_VERSION="${GUEST_AGENT_VERSION:-0.4.0}"
CERT_MANAGER_VERSION="${CERT_MANAGER_VERSION:-v1.20.2}"

# The Operator's images; latest, or a main commit's SHA.
IMAGE_TAG="${IMAGE_TAG:-latest}"

# The MicroVM kernel and root filesystem images, built by up.sh and pushed
# to the Host's own registry on 127.0.0.1:5000 (flintlockd's containerd
# pulls over plain HTTP only from localhost).
MICROVM_REGISTRY=127.0.0.1:5000
KERNEL_IMAGE="${MICROVM_REGISTRY}/bo-trial/kernel:6.1"
ROOTFS_IMAGE="${MICROVM_REGISTRY}/bo-trial/rootfs:24.04"

log() { printf '\033[1m==> %s\033[0m\n' "$*" >&2; }
warn() { printf '\033[33mWARN: %s\033[0m\n' "$*" >&2; }
die() { printf '\033[31mFAIL: %s\033[0m\n' "$*" >&2; exit 1; }

# mac_run runs a command on the Mac. OrbStack's `mac` rewrites every
# argument that looks like an absolute path into the Mac's view of the Linux
# machine (/dev/kvm becomes ~/OrbStack/<machine>/dev/kvm), so the command
# goes through as one quoted string for sh instead.
mac_run() {
	if [[ "$(uname -s)" == Darwin ]]; then
		"$@"
	elif command -v mac >/dev/null 2>&1; then
		mac sh -c "$(printf '%q ' "$@")"
	else
		die "not on macOS and no 'mac' command to reach it"
	fi
}

limactl_() { mac_run limactl "$@"; }

# keep_awake stops the Mac from idle-sleeping for KEEP_AWAKE_SECS (two
# hours by default). A Mac asleep freezes the VM with it: a wait that should
# take minutes takes hours, and the Pods' probes fail on waking. caffeinate
# ends by itself, so an interrupted run leaves nothing behind for long. Only
# the lima driver has a Mac to keep awake.
keep_awake() {
	[[ "${HOST_DRIVER}" == lima ]] || return 0
	mac_run caffeinate -i -t "${KEEP_AWAKE_SECS:-7200}" >/dev/null 2>&1 &
	disown
}

# ssh_ runs ssh to the ssh driver's machine. ssh joins its arguments into
# one string for the remote shell, so the command goes through quoted.
#
# The scripts run hundreds of commands on the Host, so they share one
# connection: a master started on its own, detached from every file
# descriptor (a master that ControlMaster=auto starts inside a command
# keeps that command's stdout open, and $(...) around it never returns),
# kept for ten minutes after the last command. Its socket is under TMPDIR:
# a path under .state can pass the length limit of a Unix socket's.
SSH_CONTROL="${TMPDIR:-/tmp}/bo-trial-ssh-%C"
ssh_() {
	[[ -n "${HOST_SSH}" ]] || die "HOST_DRIVER=ssh needs HOST_SSH, an ssh destination such as ubuntu@host"
	# SSH_OPTS is split on purpose: it is a list of options.
	# shellcheck disable=SC2086
	if ! ssh -o ControlPath="${SSH_CONTROL}" ${SSH_OPTS} -O check "${HOST_SSH}" >/dev/null 2>&1; then
		# shellcheck disable=SC2086
		ssh -o BatchMode=yes -o ControlMaster=yes -o ControlPath="${SSH_CONTROL}" -o ControlPersist=10m \
			${SSH_OPTS} -fN "${HOST_SSH}" </dev/null >/dev/null 2>&1 ||
			die "cannot reach ${HOST_SSH} over ssh"
	fi
	# shellcheck disable=SC2086
	ssh -o BatchMode=yes -o ControlMaster=no -o ControlPath="${SSH_CONTROL}" \
		${SSH_OPTS} "${HOST_SSH}" -- "$(printf '%q ' "$@")"
}

# host_run runs a command on the Host as its login user; stdin is passed on.
host_run() {
	case "${HOST_DRIVER}" in
	lima) limactl_ shell --workdir / "${NODE_NAME}" "$@" ;;
	ssh) ssh_ "$@" ;;
	esac
}

# host_root runs a command on the Host as root; stdin is passed on. The ssh
# driver's user needs sudo without a password, or to be root.
host_root() { host_run sudo "$@"; }

# host_put copies a local file onto the Host at an absolute path, as root.
host_put() {
	local src="$1" dst="$2" mode="${3:-0644}"
	host_root install -D -m "${mode}" /dev/stdin "${dst}" <"${src}"
}

vm_exists() { limactl_ list --quiet 2>/dev/null | grep -qx "${NODE_NAME}"; }

vm_status() { limactl_ list --format '{{.Status}}' "${NODE_NAME}" 2>/dev/null; }

# host_iface is the Host's interface for its default route, unless
# HOST_IFACE names one: eth0 on Lima, whatever the machine has otherwise.
host_iface() {
	if [[ -n "${HOST_IFACE:-}" ]]; then
		echo "${HOST_IFACE}"
		return
	fi
	# awk reads all its input: a reader that stops early (exit, head) can kill
	# the command on the Host with SIGPIPE, which pipefail makes fatal.
	host_run ip -4 route show default | awk '!found {for (i = 1; i < NF; i++) if ($i == "dev") { print $(i + 1); found = 1 }}'
}

# host_ip is the Host's address on that interface: the Node's InternalIP,
# where flintlockd serves and where the Exec Agent serves.
host_ip() {
	host_run ip -4 -o addr show dev "$(host_iface)" | awk '!found { sub("/.*", "", $4); print $4; found = 1 }'
}

# host_arch is the Host's architecture as Go and OCI name it: arm64 or
# amd64. The MicroVM images and the Consumer are built for it.
host_arch() {
	case "$(host_run uname -m)" in
	aarch64 | arm64) echo arm64 ;;
	x86_64) echo amd64 ;;
	*) die "the Host is $(host_run uname -m); the trial supports arm64 and amd64" ;;
	esac
}

# api_address is the address the kubeconfig reaches the API server on, and
# which the API server's certificate has to name.
api_address() {
	case "${HOST_DRIVER}" in
	lima) if [[ "$(uname -s)" == Darwin ]]; then echo 127.0.0.1; else echo "${MAC_HOST:-host.internal}"; fi ;;
	ssh) echo "${HOST_API_ADDRESS:-${HOST_SSH#*@}}" ;;
	esac
}

kubectl_() { KUBECONFIG="${KUBECONFIG_OUT}" kubectl "$@"; }
