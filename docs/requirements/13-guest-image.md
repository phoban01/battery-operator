# Guest Images {#guest-images}

This document specifies the Guest Images: the kernel image and the root
filesystem image from which a Pool's MicroVMs boot. A Pool names them in
`spec.template.kernel.image` and `spec.template.rootVolume.containerSource`,
and `flintlockd` on each Host pulls them. They hold the least a MicroVM
needs: a kernel, systemd, flintlock's guest agent, and DHCP on the Pool's
interfaces. A Pool may name other images; these are the ones this project
builds, checks and publishes.

The sources are in `guestimage/`. They come from the real hosts trial
(`hack/real-hosts`), which built them by hand. The Dagger module builds
them (ADR 0005), and CI checks them on every change and publishes them on
a version tag.

## Build {#guest-build}

- **GI-001** The Guest Images SHALL be a kernel image and a root filesystem
  image, each defined by one Containerfile in `guestimage/` and built for
  `linux/amd64` and `linux/arm64`.
- **GI-002** The Guest Images' Containerfiles SHALL pin every base image by
  the digest of its multi-platform index.
- **GI-003** The Guest Images build SHALL verify the checksum of every file
  it downloads against a checksum recorded in the image's Containerfile,
  and SHALL fail when one does not match.
- **GI-004** The Guest Images build SHALL run a check stage for each image
  that fails unless the image holds what this document requires of it.

The published image takes a file from its check stage, so no builder can
skip that stage: an image that fails its checks is never produced. The
Ubuntu packages of the root filesystem come from Ubuntu's signed archive,
at the versions the archive has on the day of the build.

## Kernel {#guest-kernel}

- **GI-010** The Guest Images' kernel image SHALL hold Firecracker's CI
  kernel 6.1.128 for its architecture at `boot/vmlinux`, and no other
  file.
- **GI-011** The Guest Images' kernel image SHALL hold an x86-64 ELF
  `vmlinux` on `linux/amd64`, and an uncompressed arm64 `Image` on
  `linux/arm64`.

A Pool names the kernel file with `kernel.filename: boot/vmlinux`. On
`linux/arm64` Firecracker loads the uncompressed `Image`, whatever its
file name. Firecracker's CI kernel has virtio-vsock, virtio-blk, ext4 and
devtmpfs built in, so the guest agent is reachable and systemd boots
without an initrd.

The kernel has no virtio-pci. It boots on Firecracker, which gives a guest
MMIO devices, and not on Cloud Hypervisor, which gives it PCI devices
(#202). A Pool that uses Cloud Hypervisor needs a kernel of its own.

## Root filesystem {#guest-rootfs}

- **GI-020** The Guest Images' root filesystem image SHALL be Ubuntu 24.04,
  with systemd as its init.
- **GI-021** The Guest Images' root filesystem image SHALL contain
  flintlock's guest agent at the version its Containerfile pins, and SHALL
  enable the guest agent's `guest-agent.service` unit.
- **GI-022** The Guest Images' root filesystem image SHALL enable
  `systemd-networkd`, and SHALL configure every Ethernet interface except
  `eth0` by DHCP for IPv4.
- **GI-023** The Guest Images' root filesystem image SHALL leave `eth0`
  unmanaged.
- **GI-024** The Guest Images' root filesystem image SHALL hold an empty
  `/etc/machine-id`.

battery creates every Pool MicroVM with the guest agent allowed, and waits
for it before the MicroVM is available. A create hook and the Exec Agent
reach the guest only through the guest agent, over vsock. flintlock gives a
MicroVM `eth0` for Firecracker's metadata service, and the Pool's
interfaces after it, from `eth1`. The Host Image's `battery-dnsmasq` unit
answers DHCP on the guest bridge (HI-031), and so does the trial's Host.
Boot does not wait long for the network: the wait for it ends after 10
seconds. systemd derives an interface's MAC address from the machine ID,
so with one machine ID for every guest, every guest would ask DHCP for the
same address.

## Publishing {#guest-publish}

- **GI-030** When a version tag is pushed, the Guest Images SHALL be
  published to `ghcr.io/phoban01/battery-operator/guest-kernel` and
  `ghcr.io/phoban01/battery-operator/guest-rootfs`, each as one image index
  that holds `linux/amd64` and `linux/arm64`.
- **GI-031** Each index of GI-030 SHALL be tagged with the version and with
  the full SHA of the tag's commit.
- **GI-032** When it publishes an index of GI-030, the Guest Images' push
  SHALL print each tag of the index with the index's digest, as
  `<repository>:<tag>@sha256:<digest>`.

The publish job's summary lists these lines, so that the release notes and
a Pool can name each image by digest. A pull request that changes the
images pushes them to a registry inside its own check, and checks what it
pushed the same way, so the push that a tag runs has run before.
