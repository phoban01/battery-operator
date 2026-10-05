#!/usr/bin/bash
# Build step of the image stage: check the build arguments against the
# versions file, then install the packaged components at their pinned
# versions.
set -euo pipefail

# `make host-image` passes the versions as build arguments so that they can
# become OCI labels, and a build argument is an environment variable here.
# One that disagrees with the file fails the build, so the file stays the
# only place a version is decided.
names="CONTAINERD_VERSION RUNC_VERSION FIRECRACKER_VERSION CLOUD_HYPERVISOR_VERSION FLINTLOCK_VERSION KUBERNETES_VERSION CLOUD_INIT_VERSION"
declare -A args=()
for v in $names; do
  args[$v]=${!v:-}
done
# shellcheck source=hostimage/versions.env
. /usr/share/battery/versions.env
for v in $names; do
  file=${!v:-}
  arg=${args[$v]}
  [ -n "$file" ] || {
    echo "versions.env pins no $v" >&2
    exit 1
  }
  [ "$arg" = "$file" ] || {
    echo "build argument $v='$arg' is not versions.env's '$file'; build with 'make host-image'" >&2
    exit 1
  }
done

#= docs/requirements/11-host-image.md#image-build
#/ The Host Image SHALL contain containerd, Firecracker with its
#/ jailer, Cloud Hypervisor, `flintlockd`, the kubelet, `kubeadm` and
#/ cloud-init at the versions pinned in one versions file that the
#/ Containerfile reads.
k8s_minor=$(echo "$KUBERNETES_VERSION" | cut -d. -f1,2)
# enabled=0: a Host never installs packages, and a new kubelet is a new image.
cat >/etc/yum.repos.d/kubernetes.repo <<EOF
[kubernetes]
name=Kubernetes $k8s_minor
baseurl=https://pkgs.k8s.io/core:/stable:/$k8s_minor/rpm/
enabled=0
gpgcheck=1
repo_gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-kubernetes
EOF

# The kubernetes-cni package installs into /opt/cni/bin.
mkdir -p /var/opt
dnf -y --enablerepo=kubernetes --setopt=install_weak_deps=False install \
  "cloud-init-$CLOUD_INIT_VERSION" \
  "kubelet-${KUBERNETES_VERSION#v}" "kubeadm-${KUBERNETES_VERSION#v}" \
  kubernetes-cni cri-tools \
  dnsmasq nftables iproute lvm2 device-mapper-persistent-data device-mapper-event e2fsprogs \
  conntrack-tools socat ethtool iptables-nft jq util-linux \
  container-selinux policycoreutils

#= docs/requirements/11-host-image.md#kubernetes-node
#/ The Host Image SHALL make `/opt`, `/usr/local` and
#/ `/usr/libexec/kubernetes` links to `/var/opt`, `/var/usrlocal` and
#/ `/var/libexec/kubernetes`
# A Host can write only to /etc, /var, /run and /tmp, and a Kubernetes node
# writes in three more places. A CNI DaemonSet installs its plugin into
# /opt/cni/bin (Flannel's does), and containerd looks there; charts keep data
# under /opt. Some DaemonSets install into /usr/local/bin. The kubelet makes
# its volume plugin directory under /usr/libexec/kubernetes, and drivers
# install there. Each becomes a link into /var, as on Fedora CoreOS.
# tmpfiles.d creates the targets at boot: the base's own drop-in makes
# /var/opt and /var/usrlocal, and battery.conf the rest.
#
# The CNI plugins move to /usr, which an upgrade replaces; tmpfiles.d copies
# them to /var/opt/cni/bin at boot. Nothing else may be in /opt, and
# /usr/local may hold only the base's empty directories: anything else would
# be lost behind the link.
mkdir -p /usr/libexec/cni
cp -a /opt/cni/bin/. /usr/libexec/cni/
rm -rf /opt/cni /var/opt/cni
left=$(
  find /opt -mindepth 1 -print -quit
  find /usr/local -mindepth 1 ! -type d -print -quit
)
[ -z "$left" ] || {
  echo "/opt or /usr/local is not empty: $left" >&2
  exit 1
}
[ ! -e /usr/libexec/kubernetes ] || {
  echo "a package installs into /usr/libexec/kubernetes, which the link would hide" >&2
  exit 1
}
# Replacing these system directories with links is the point here.
# shellcheck disable=SC2114
rm -rf /opt /usr/local
ln -s var/opt /opt
ln -s ../var/usrlocal /usr/local
ln -s ../../var/libexec/kubernetes /usr/libexec/kubernetes
dnf clean all
