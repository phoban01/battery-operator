# Guest Images

The kernel image and the root filesystem image that a Pool's MicroVMs boot
from. A Pool names them in `spec.template.kernel.image` and
`spec.template.rootVolume.containerSource`, and `flintlockd` on each Host
pulls them. The requirements are in
[docs/requirements/13-guest-image.md](../docs/requirements/13-guest-image.md)
(GI).

| Image | Holds |
|-------|-------|
| `kernel/` | Firecracker's CI kernel 6.1.128 at `boot/vmlinux`: an ELF `vmlinux` on x86_64, an uncompressed `Image` on arm64. Nothing else. |
| `rootfs/` | Ubuntu 24.04 with systemd and flintlock's guest agent v0.4.0, enabled at boot. systemd-networkd leaves `eth0` to flintlock's metadata service and configures `eth1` and after by DHCP. Each MicroVM makes its own machine ID at boot. |

The kernel has no virtio-pci. It boots on Firecracker and not on Cloud
Hypervisor (#202). A Pool with Cloud Hypervisor needs a kernel of its own.

A Pool names them like this:

```yaml
    kernel:
      image: ghcr.io/phoban01/battery-operator/guest-kernel:<version>
      filename: boot/vmlinux
    rootVolume:
      containerSource: ghcr.io/phoban01/battery-operator/guest-rootfs:<version>
```

## Which guest images

Use these for any Pool that does not run GitLab CI Jobs: the demo, a first
try of a cluster, or your own commands through the Exec Agent. They hold
only what battery and the Exec Agent need.

A Pool that [flintlock-runner](https://github.com/phoban01/flintlock-runner)
claims for GitLab CI Jobs needs flintlock-runner's guest images,
`ghcr.io/phoban01/flintlock-runner/guest-kernel` and `guest-rootfs`
([its guest README](https://github.com/phoban01/flintlock-runner/blob/main/guest/README.md)).
Their root filesystem starts from this one and adds bash, git, curl and
`gitlab-runner-helper`, which the Jobs use. Take their references from
that release's `images.txt`, which pins them by digest. The kernel is the
same in both projects.

## Build and check

Each image has one Containerfile, built for linux/amd64 and linux/arm64.
Every base image is pinned by digest, and every download by a sha256 in the
Containerfile. A check stage runs in each build (`check.sh`), and the image
takes a file from it, so an image that fails its checks is never produced.

The Dagger module builds them:

```sh
dagger call guest-image-lint                          # digest pins, checksums, shellcheck
dagger call guest-image-check --platform=linux/arm64  # build both images, run the checks again
dagger call guest-kernel --platform=linux/amd64 export-image --name=guest-kernel:dev
dagger call guest-rootfs export-image --name=guest-rootfs:dev
dagger call guest-image-publish-check                 # push both to a registry inside the check
```

The kernel is a download, so every stage runs on the build machine's
platform. The root filesystem runs apt on its own platform, under emulation
on the other one. Both images are small, so CI's `guest-image` job builds
both platforms on one amd64 runner. It runs on a pull request that changes
`guestimage/`, the Dagger module or the CI workflow.

`docker build` works too, from each image's directory:

```sh
docker build -f guestimage/rootfs/Containerfile guestimage/rootfs
```

## Publishing

A version tag publishes both, in CI's publish job, with
`dagger call guest-image-publish`. It builds each image for both platforms
and pushes one index of each with `push.sh`, tagged with the version and
the commit SHA:

```
ghcr.io/phoban01/battery-operator/guest-kernel:<version>
ghcr.io/phoban01/battery-operator/guest-rootfs:<version>
```

The job summary lists each tag with its digest. On a pull request,
`push-check.sh` runs `push.sh` against a registry inside the check, and
checks what it pushed: both tags, one index, both platforms, and the kernel
or the guest agent in each platform's image. The first release with the
Guest Images is the release after v0.3.2.

## The real hosts trial

`hack/real-hosts/up.sh images` builds both from this directory with
`docker build`, for the Host's architecture, and pushes them to the Host's
own registry on `127.0.0.1:5000`. The Host then needs no way out to
ghcr.io. `hack/kubeadm-host` runs the same step.

## Updating a pin

- The kernel: set `KERNEL_VERSION`, and `FIRECRACKER_CI_VERSION` if the
  kernel is in another release's bucket. Firecracker publishes no
  checksum, so download both architectures and pin their sha256 in
  `KERNEL_SHA256_AMD64` and `KERNEL_SHA256_ARM64`.
- The guest agent: set `GUEST_AGENT_VERSION`, and the two sha256s from the
  release's `checksums.txt`.
- A base image: resolve the digest of its multi-platform index with
  `docker buildx imagetools inspect mirror.gcr.io/library/<image>:<tag>`,
  and set it in the `FROM` line.
