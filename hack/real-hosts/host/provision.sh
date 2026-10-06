#!/usr/bin/env bash
# Makes a Linux machine a Host (docs/host-prerequisites.md). Run as root on it
# by up.sh, with the versions in its environment, after up.sh has copied
# the units, the helper scripts and containerd's configuration in.
# Idempotent: each download is skipped when that version is installed.
#
# What it sets up, and which Host prerequisite each is:
# - KVM: /dev/kvm, from the machine or its nested virtualization; only
#   checked here.
# - containerd's thin pool: bo-thin-pool.service creates the device-mapper
#   thin pool flintlock-thinpool on two loop-backed sparse files at every
#   boot, before containerd starts.
# - containerd v1.7 for flintlockd, on /run/containerd/containerd.sock, with
#   the devmapper snapshotter on that pool. It is not k3s's containerd,
#   which keeps its own socket under /run/k3s.
# - Firecracker, and flintlockd with its exec API, serving with mutual TLS
#   on the Host's address (FLINTLOCKD_IFACE) once the Exec Agent has
#   written its certificates to /etc/battery/flintlockd (flintlockd.path).
# - The bridge br-battery, which flintlockd requires one of (--bridge-name or
#   --parent-iface) to start, and puts each MicroVM's tap device on.
# - A network for the MicroVMs on br-battery, 10.220.0.0/24: dnsmasq
#   (bo-dnsmasq.service) gives each guest an address, the bridge 10.220.0.1
#   as its gateway, and the bridge as its DNS server, which forwards to the
#   Host's resolvers. bo-guest-nat.service turns on IP forwarding and
#   masquerades the subnet out through the Host.
# - A registry on 127.0.0.1:5000 for the MicroVM images: flintlockd's
#   containerd pulls over plain HTTP only from localhost.
set -euo pipefail

: "${CONTAINERD_VERSION:?}" "${FIRECRACKER_VERSION:?}" "${FLINTLOCK_VERSION:?}" "${REGISTRY_VERSION:?}" "${DNSMASQ_VERSION:?}"

export DEBIAN_FRONTEND=noninteractive
# Ubuntu's needrestart would list the services to restart after each apt
# install; nothing here needs that.
export NEEDRESTART_SUSPEND=1
arch=arm64
uarch=aarch64
[[ "$(uname -m)" == aarch64 ]] || { arch=amd64; uarch=x86_64; }

[[ -c /dev/kvm ]] || { echo "no /dev/kvm: this machine has no KVM" >&2; exit 1; }

# thin-provisioning-tools gives the kernel's dm-thin-pool its userspace;
# dmsetup and losetup create the pool.
if ! command -v dmsetup >/dev/null || ! command -v thin_check >/dev/null || ! command -v iptables >/dev/null; then
	apt-get update -qq
	apt-get install -y -qq thin-provisioning-tools dmsetup curl ca-certificates iproute2 iptables >/dev/null
fi
# dnsmasq for the guests' DHCP and DNS, at the pinned version. dnsmasq-base
# is the binary alone: it has no unit of its own that would serve every
# interface, so bo-dnsmasq.service is the only dnsmasq that runs.
if [[ "$(dpkg-query -W -f '${Version}' dnsmasq-base 2>/dev/null)" != "${DNSMASQ_VERSION}" ]]; then
	apt-get update -qq
	apt-get install -y -qq --allow-downgrades "dnsmasq-base=${DNSMASQ_VERSION}" >/dev/null
fi
# The thin pool's target and the loop devices under it, and tun for
# flintlockd's tap devices. linux-modules-extra carries dm-thin-pool on some
# Ubuntu kernels.
if ! modprobe dm_thin_pool 2>/dev/null; then
	apt-get install -y -qq "linux-modules-extra-$(uname -r)" >/dev/null
	modprobe dm_thin_pool
fi
modprobe loop
modprobe tun
cat >/etc/modules-load.d/bo-trial.conf <<'EOF'
dm_thin_pool
loop
tun
EOF

fetch() { curl -fsSL --retry 3 -o "$2" "$1"; }
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

# containerd, the upstream release (its devmapper snapshotter is built in).
if [[ "$(/usr/local/bin/containerd --version 2>/dev/null | awk '{print $3}')" != "v${CONTAINERD_VERSION}" ]]; then
	fetch "https://github.com/containerd/containerd/releases/download/v${CONTAINERD_VERSION}/containerd-${CONTAINERD_VERSION}-linux-${arch}.tar.gz" "${tmp}/containerd.tgz"
	tar -C /usr/local -xzf "${tmp}/containerd.tgz"
fi

# Firecracker and its jailer.
if [[ "$(/usr/local/bin/firecracker --version 2>/dev/null | head -1 | awk '{print $2}')" != "${FIRECRACKER_VERSION}" ]]; then
	fetch "https://github.com/firecracker-microvm/firecracker/releases/download/${FIRECRACKER_VERSION}/firecracker-${FIRECRACKER_VERSION}-${uarch}.tgz" "${tmp}/fc.tgz"
	tar -C "${tmp}" -xzf "${tmp}/fc.tgz"
	install -m 0755 "${tmp}/release-${FIRECRACKER_VERSION}-${uarch}/firecracker-${FIRECRACKER_VERSION}-${uarch}" /usr/local/bin/firecracker
	install -m 0755 "${tmp}/release-${FIRECRACKER_VERSION}-${uarch}/jailer-${FIRECRACKER_VERSION}-${uarch}" /usr/local/bin/jailer
fi

# flintlockd.
if ! /usr/local/bin/flintlockd version --short 2>/dev/null | grep -qx "${FLINTLOCK_VERSION#v}"; then
	fetch "https://github.com/liquidmetal-dev/flintlock/releases/download/${FLINTLOCK_VERSION}/flintlockd_${arch}" "${tmp}/flintlockd_${arch}"
	fetch "https://github.com/liquidmetal-dev/flintlock/releases/download/${FLINTLOCK_VERSION}/checksums.txt" "${tmp}/checksums.txt"
	(cd "${tmp}" && grep " flintlockd_${arch}\$" checksums.txt | sha256sum -c --quiet -)
	install -m 0755 "${tmp}/flintlockd_${arch}" /usr/local/bin/flintlockd
fi

# The registry for the MicroVM images.
if [[ ! -x /usr/local/bin/registry ]] || ! /usr/local/bin/registry --version 2>/dev/null | grep -q "v${REGISTRY_VERSION}"; then
	fetch "https://github.com/distribution/distribution/releases/download/v${REGISTRY_VERSION}/registry_${REGISTRY_VERSION}_linux_${arch}.tar.gz" "${tmp}/registry.tgz"
	tar -C "${tmp}" -xzf "${tmp}/registry.tgz" registry
	install -m 0755 "${tmp}/registry" /usr/local/bin/registry
fi
install -d -m 0755 /etc/bo-trial /var/lib/bo-registry
cat >/etc/bo-trial/registry.yml <<'EOF'
version: 0.1
storage:
  filesystem:
    rootdirectory: /var/lib/bo-registry
  delete:
    enabled: true
http:
  addr: 127.0.0.1:5000
log:
  level: warn
EOF

# DHCP and DNS for the MicroVMs, on br-battery alone. dnsmasq adds loopback to
# the interfaces it serves unless told not to. The range leaves room for
# addresses given by hand below .10 and above .250; a MicroVM gets a new
# MAC address every time, so leases are short. DNS goes to the Host's
# upstream resolvers, not to systemd-resolved's stub on 127.0.0.53.
cat >/etc/bo-trial/dnsmasq.conf <<'EOF'
interface=br-battery
except-interface=lo
bind-interfaces
no-hosts
resolv-file=/run/systemd/resolve/resolv.conf
dhcp-range=10.220.0.10,10.220.0.250,255.255.255.0,1h
dhcp-option=option:router,10.220.0.1
dhcp-option=option:dns-server,10.220.0.1
dhcp-authoritative
dhcp-rapid-commit
dhcp-leasefile=/var/lib/bo-trial/dnsmasq.leases
EOF
# IP forwarding from boot; bo-guest-nat.service turns it on too.
echo 'net.ipv4.ip_forward = 1' >/etc/sysctl.d/90-bo-trial.conf

# Where the Exec Agent writes flintlockd's serving certificate, its key and
# the client CA bundle (config/exec-agent/daemonset.yaml): writable by the
# agent's user, 65532, and read by flintlockd as root.
install -d -m 0755 -o 65532 -g 65532 /etc/battery/flintlockd
# The not ready reason directory the agent reads (EA-033).
install -d -m 0755 /run/battery/not-ready.d
install -d -m 0755 /var/lib/flintlock /var/lib/bo-trial/thinpool

systemctl daemon-reload
systemctl enable --now bo-thin-pool.service bo-bridge.service containerd.service bo-registry.service \
	bo-dnsmasq.service bo-guest-nat.service
# A changed configuration takes effect on a second run too.
systemctl restart bo-dnsmasq.service
systemctl restart bo-guest-nat.service
# flintlockd starts when its certificates appear, and restarts when they
# change; its service itself is not enabled.
systemctl enable --now flintlockd.path flintlockd-certs.path

dmsetup status flintlock-thinpool >/dev/null
ctr version >/dev/null
systemctl is-active --quiet bo-dnsmasq.service
iptables -t nat -S POSTROUTING | grep -q bo-trial
echo "$(hostname): thin pool, containerd $(containerd --version | awk '{print $3}'), $(firecracker --version | head -1), flintlockd $(flintlockd version --short 2>/dev/null), dnsmasq ${DNSMASQ_VERSION} on br-battery"
