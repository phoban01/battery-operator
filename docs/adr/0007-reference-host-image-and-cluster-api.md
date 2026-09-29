# 0007. A reference Host Image built with bootc, and Cluster API host pools

- **Status:** Accepted
- **Date:** 2026-09-29

## Context

A Host is a Node that runs MicroVMs: battery places them there through its
`flintlockd`, and the Exec Agent runs on it. Until now battery-operator has
only said what a Host needs (`docs/host-prerequisites.md`): KVM, containerd's
thin pool, `flintlockd` over mutual TLS on the Host's address (ADR 0002),
with certificates the Exec Agent writes (ADR 0003), and the Host label. How
a Host is built was left to the user. The real hosts trial (`hack/real-hosts`)
builds one by hand, on Ubuntu, for tests only.

The maintainer wants to provision Hosts with Cluster API, from an image
built with bootc.

flintlock-runner already has both:

- a bootc Host Image (`image/`, on Fedora bootc), with containerd and the
  thin pool, Firecracker, `flintlockd` served over mutual TLS from the Exec
  Agent's certificates, a KVM check, the guest bridge with DHCP, NAT and
  isolation from the cluster, SELinux, kubelet, and the Host label;
- Cluster API host-pool templates (`deploy/capi`): an AWS machine template,
  a kubeadm config template, a MachineDeployment and a MachineHealthCheck.

Almost all of this is a battery-operator Host. Only a cache volume for
flintlock-runner's Host Services is flintlock-runner's own. Any other
consumer of battery-operator would need the same image.

## Decision

1. **battery-operator provides a reference Host Image,** built with bootc.
   It meets `docs/host-prerequisites.md`, which stays the contract: a Host
   built another way that meets the prerequisites is still a Host.
2. **battery-operator provides Cluster API host-pool templates** that boot
   the Host Image and join it with kubeadm. They are templates the user
   parameterises per cluster: the image, the machine size, the network, the
   pool size and the Host's settings.
3. **kubeadm is the bootstrap.** The kubeadm config template carries what
   the image needs at first boot: the Node label
   `battery.liquidmetal-x.dev/host=true` and the Host's settings, such as
   the addresses allowed to reach `flintlockd`.
4. **The image and the templates move from flintlock-runner,** with their
   requirements and checks. `flr` names become `battery` names.
   flintlock-runner then builds on battery-operator's image and adds only
   what is its own.
5. **The Dagger module builds the Host Image** from its bootc Containerfile,
   and CI checks and publishes it as it does the other images (ADR 0005). A
   bootc image is an operating system, not one binary, so it keeps its
   Containerfile; ADR 0005's rule of one definition per image still holds.

## Consequences

1. A user can provision Hosts with Cluster API from a published image,
   without building one.
2. battery-operator now maintains an operating system image: its package
   pins, its base image digest, its SELinux module and its security fixes.
   Its CI builds and checks it on every change to it.
3. The Host Image's requirements become battery-operator's, in a new
   requirements document with its own prefix. flintlock-runner withdraws
   its copies and names the IDs that replace them.
4. The first proof is a local VM, booted from the image with kubeadm
   user-data like Cluster API's. The real hosts trial runs k3s, which a
   kubeadm Node cannot join, so the proof needs a kubeadm control plane of
   its own.
5. The Cluster API templates start with AWS, because that is what exists.
   Other infrastructure providers are templates added later.

## Open questions

- Whether battery-operator also publishes the image for linux/arm64. The
  first build is linux/amd64, as flintlock-runner's is.

  > **Answered, 2026-09-29 (#178):** yes. The Host Image is built for
  > linux/amd64 and linux/arm64 from the one Containerfile, each on a CI
  > runner of its own architecture, and a tag publishes one
  > multi-architecture image (HI-002). The local proof of #169 runs the
  > arm64 image on an Apple silicon Mac. The AMI that `publish-ami.sh`
  > makes stays x86_64. The decision stands.

- Which cloud account, and which Cluster API infrastructure provider, the
  templates are tested against in CI, beyond rendering and schema checks.
