#!/usr/bin/bash
# Build step of the fetch stage: download every binary that does not come
# from a signed package repository into /out, verifying each one.
#
#   fetch.sh VERSIONS ARCH
#
# ARCH is the architecture of the image being built, as BuildKit's
# TARGETARCH names it: amd64 or arm64.
set -euo pipefail
# shellcheck source=hostimage/versions.env
. "${1:-/build/versions.env}"

#= docs/requirements/11-host-image.md#image-build
#/ The Host Image SHALL be built for the `x86_64` and `aarch64`
#/ architectures.
# This stage runs on the build machine's architecture and only downloads,
# verifies and unpacks, so it runs no binary it fetches. What it fetches is
# for the target architecture: each project names its assets in its own
# way, and each asset has its own checksum in the versions file.
arch=${2:?usage: fetch.sh VERSIONS ARCH}
case "$arch" in
amd64) uname_arch=x86_64 ch_asset=cloud-hypervisor-static ;;
arm64) uname_arch=aarch64 ch_asset=cloud-hypervisor-static-aarch64 ;;
*)
  echo "the Host Image is built for amd64 and arm64, not '$arch'" >&2
  exit 1
  ;;
esac
suffix=${arch^^}
# sum NAME: the checksum of NAME's download for this architecture.
sum() {
  local v=${1}_SHA256_$suffix
  printf '%s' "${!v:-}"
}
mkdir -p /dl /out/usr/bin

#= docs/requirements/11-host-image.md#image-build
#/ The Host Image build SHALL verify the checksum of every binary
#/ it downloads against a checksum recorded in the versions file and SHALL
#/ fail when one does not match.
# fetch URL FILE SHA256: https only, and no file is used before its checksum
# matched. An empty checksum fails closed.
fetch() {
  [ -n "$3" ] || {
    echo "no checksum is recorded for $1" >&2
    exit 1
  }
  curl --proto '=https' --tlsv1.2 -fsSL --retry 5 --retry-all-errors -o "/dl/$2" "$1"
  echo "$3  /dl/$2" | sha256sum -c -
}

#= docs/requirements/11-host-image.md#image-build
#/ The Host Image build SHALL complete on a machine that has neither
#/ `/dev/kvm` nor credentials for any cloud provider.
# Everything comes from GitHub releases and package repositories over https,
# and nothing the build runs needs a hypervisor.
gh=https://github.com

fetch "$gh/containerd/containerd/releases/download/$CONTAINERD_VERSION/containerd-${CONTAINERD_VERSION#v}-linux-$arch.tar.gz" containerd.tgz "$(sum CONTAINERD)"
tar -xzf /dl/containerd.tgz -C /out/usr --no-same-owner bin/containerd bin/containerd-shim-runc-v2 bin/ctr

fetch "$gh/opencontainers/runc/releases/download/$RUNC_VERSION/runc.$arch" runc "$(sum RUNC)"
install -m 0755 /dl/runc /out/usr/bin/runc

fc=firecracker-$FIRECRACKER_VERSION-$uname_arch
fetch "$gh/firecracker-microvm/firecracker/releases/download/$FIRECRACKER_VERSION/$fc.tgz" firecracker.tgz "$(sum FIRECRACKER)"
mkdir -p /dl/fc
tar -xzf /dl/firecracker.tgz -C /dl/fc --no-same-owner
install -m 0755 "/dl/fc/release-$FIRECRACKER_VERSION-$uname_arch/$fc" /out/usr/bin/firecracker
install -m 0755 "/dl/fc/release-$FIRECRACKER_VERSION-$uname_arch/jailer-$FIRECRACKER_VERSION-$uname_arch" /out/usr/bin/jailer

# The image calls the binary cloud-hypervisor-static on every architecture.
fetch "$gh/cloud-hypervisor/cloud-hypervisor/releases/download/$CLOUD_HYPERVISOR_VERSION/$ch_asset" cloud-hypervisor-static "$(sum CLOUD_HYPERVISOR)"
install -m 0755 /dl/cloud-hypervisor-static /out/usr/bin/cloud-hypervisor-static

fetch "$gh/liquidmetal-dev/flintlock/releases/download/$FLINTLOCK_VERSION/flintlockd_$arch" flintlockd "$(sum FLINTLOCK)"
install -m 0755 /dl/flintlockd /out/usr/bin/flintlockd

# The signing key of the Kubernetes package repository. dnf verifies the
# kubelet and kubeadm packages against it, so it is the download to pin. It
# is one file for every architecture.
k8s_minor=$(echo "$KUBERNETES_VERSION" | cut -d. -f1,2)
fetch "https://pkgs.k8s.io/core:/stable:/$k8s_minor/rpm/repodata/repomd.xml.key" kubernetes.key "$KUBERNETES_REPO_KEY_SHA256"
install -D -m 0644 /dl/kubernetes.key /out/etc/pki/rpm-gpg/RPM-GPG-KEY-kubernetes
