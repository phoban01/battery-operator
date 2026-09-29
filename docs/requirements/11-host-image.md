# Host Image {#host-image}

This document specifies the Host Image: the bootable container image, built
with bootc, from which a Node can boot as a Host
([ADR 0007](../adr/0007-reference-host-image-and-cluster-api.md)). The image
carries everything the Host prerequisites ask for, and the guest networking
a MicroVM needs, so that a Host is complete when it boots and nothing is
pushed to it afterwards. It is one way to meet the Host prerequisites
(`docs/host-prerequisites.md`), not the only one: a Node built another way
that meets them is a Host too. The sources are in `hostimage/`, and they can
be built and checked without a cloud account and without KVM.

The Host Image comes from flintlock-runner's `image/`, and so do these
requirements. Each keeps the number it had in flintlock-runner's
`11-host-image.md`, so the history reads straight across. The gaps are the
requirements not carried over: those flintlock-runner had withdrawn
(HI-042, HI-044, HI-063), the one about its Host Services (HI-064), and the
one about publishing an AMI (HI-009), which belongs with the Cluster API
host-pool templates. HI-064 comes back in a generic form as HI-080,
under a new number because its scope changed. A new requirement takes the next
free number above HI-080.

## Build {#image-build}

- **HI-001** The Host Image SHALL be defined by one Containerfile whose base
  is a bootc base image pinned by digest.
- **HI-002** The Host Image SHALL be built for the `x86_64` architecture.
- **HI-003** The Host Image SHALL contain containerd, Firecracker with its
  jailer, Cloud Hypervisor, `flintlockd`, the kubelet, `kubeadm` and
  cloud-init at the versions pinned in one versions file that the
  Containerfile reads.
- **HI-004** The Host Image build SHALL verify the checksum of every binary
  it downloads against a checksum recorded in the versions file and SHALL
  fail when one does not match.
- **HI-005** The Host Image SHALL record the pinned version of each component
  of HI-003 as an OCI image label and in a versions file on the image's
  `/usr` tree.
- **HI-006** The Host Image SHALL NOT contain any credential, private key or
  token.
- **HI-007** The Host Image build SHALL complete on a machine that has
  neither `/dev/kvm` nor credentials for any cloud provider.
- **HI-008** The Host Image build SHALL run a check stage inside the built
  container image that fails unless every component of HI-003 reports its
  pinned version and every unit this document requires is enabled.

The Dagger module builds the image from its Containerfile, as ADR 0005 and
ADR 0007 say, and CI publishes it on a version tag. The Kubernetes version
is part of the image because the kubelet is, so a Host boots from an image
whose Kubernetes version the cluster it joins supports. Turning the
container image into a machine image for a cloud, an AMI for example, is
not part of the build.

## Kernel and KVM {#kernel-and-kvm}

- **HI-010** The Host Image SHALL load the `kvm`, `vhost_vsock`,
  `dm_thin_pool`, `tun` and `bridge` kernel modules at boot.
- **HI-011** If `/dev/kvm` is absent or unusable at boot, then the Host Image
  SHALL NOT start `flintlockd` and SHALL report the Host as not ready with
  the reason that KVM is unavailable.
- **HI-012** The Host Image SHALL NOT disable SELinux globally.
- **HI-013** Where a component cannot run under the base image's SELinux
  policy, the Host Image SHALL ship a policy module that relaxes confinement
  for that component's domain only.
- **HI-065** The Host Image SHALL label the Host environment file and the
  not ready reason directory it writes under `/run/battery` so that the
  containers of pods on the Host can read them, under the base image's
  SELinux policy and without changing the domain of any container or the
  label of any other path.
- **HI-066** The Host Image SHALL configure the container runtime interface
  of containerd to run every unprivileged container of a Kubernetes pod under
  SELinux confinement, in the container domain the base image's policy
  assigns or the one the pod names, rather than unconfined.
- **HI-072** The Host Image SHALL create `/etc/battery/flintlockd` owned by
  the Exec Agent's user id, and SHALL label that directory and every file in
  it so that the Exec Agent's containers can write them and `flintlockd` can
  read them, under the base image's SELinux policy and without changing the
  domain of any container.

HI-011 reports through the not ready reason directory, which the Exec Agent
reads (EA-033). The SELinux requirements exist because the quick fix,
setting the whole Host permissive, removes a layer of isolation between the
MicroVMs' hypervisor processes and the Host.

HI-066 exists because containerd applies no SELinux label to a pod unless
its container runtime interface is told to, and an unlabelled container runs
unconfined: an enforcing Host would then enforce nothing between its pods
and itself. The setting reaches only the pods the kubelet starts;
`flintlockd` drives containerd through its own API, so the MicroVMs keep the
confinement HI-013 gives them. containerd leaves a privileged container
unlabelled by design, which is why HI-066 speaks of unprivileged containers:
the Exec Agent is not privileged, and a privileged pod is unconfined
whatever the Host does.

HI-065 exists because, with HI-066, a pod's containers run in the ordinary
container domain, which may read only files labelled for containers.
Without it the Exec Agent cannot read the not ready reasons of HI-011 and
HI-022, which it treats as not ready, so no Host would ever become ready. A
DaemonSet that needs the Host's settings, such as the bridge gateway, reads
the Host environment file the same way. Labelling those two paths for
containers is the narrow fix.

HI-072 is HI-065 for the Exec Agent's certificate directory (ADR 0003,
consequence 7). The Exec Agent runs in the ordinary container domain and
writes the directory through a hostPath mount, so the directory and its
files carry the container file type that domain may write, at level `s0`.
A file the Exec Agent creates carries the level of the pod that created it,
which a later pod of the DaemonSet, with categories of its own, could
neither read nor replace; so the Host Image labels the files again whenever
they change, which returns them to `s0`. `flintlockd` runs unconfined, as a
systemd service without a policy module of its own, and reads them whatever
their label.

## Storage {#image-storage}

- **HI-020** The Host Image SHALL create the containerd devicemapper thin
  pool at boot on the block device named in the Host configuration file, or,
  when none is named, on the unused instance-store device it detects.
- **HI-021** If the thin pool already exists at boot, then the Host Image
  SHALL NOT recreate it or wipe its device.
- **HI-022** If no device is named and none is detected, then the Host Image
  SHALL NOT start `flintlockd` and SHALL report the Host as not ready with
  the reason that no thin pool device is available.
- **HI-023** The Host Image SHALL NOT select a device that holds a mounted
  filesystem or the root volume, and SHALL treat a device that already backs
  the thin pool as the thin pool device rather than as in use.
- **HI-024** The Host Image SHALL keep all `flintlockd` and containerd state
  under `/var`.

Instance-store devices survive a reboot and do not survive a stop or a
terminate. HI-021 is therefore what makes an in-place upgrade cheap: after
`bootc upgrade` and a reboot the thin pool and the images pulled into it are
still there, where replacing the machine starts cold. HI-023 comes from a
bug flintlock-runner fixed: a disk that carries the pool's own LVM
structures looks in use, and must not be refused for it.

## Networking {#image-networking}

- **HI-030** The Host Image SHALL create a Linux bridge at boot with the
  guest subnet from the Host configuration file and SHALL configure
  `flintlockd` to attach TAP interfaces to it.
- **HI-031** The Host Image SHALL run a DHCP and DNS service bound to the
  bridge so that guests obtain an address, gateway and resolver without
  static configuration.
- **HI-032** The Host Image SHALL enable IPv4 forwarding and SHALL configure
  source NAT from the guest subnet to the Host's primary interface.
- **HI-033** The Host Image SHALL drop traffic from the guest subnet to the
  instance metadata service addresses.
- **HI-034** The Host Image SHALL drop traffic from the guest subnet to the
  Host's own `flintlockd`, kubelet, Exec Agent and metrics ports.
- **HI-035** The Host Image SHALL drop traffic from the guest subnet to every
  protected CIDR listed in the Host configuration file.
- **HI-036** The Host Image SHALL allow traffic from the guest subnet to the
  bridge gateway address only on the DHCP and DNS ports and the gateway
  service ports, and SHALL keep every other port on the gateway closed to
  guests.
- **HI-037** The Host Image SHALL use a default guest subnet that the Host
  configuration file can override, so that it can be kept clear of the
  cluster's node, pod and service ranges.
- **HI-075** The Host Image SHALL forward traffic from the guest subnet only
  out of the Host's primary interface, and SHALL drop traffic from the guest
  subnet to every other interface of the Host.
- **HI-076** The Host Image SHALL drop traffic from the guest subnet whose
  destination before any destination NAT on the Host is in a protected CIDR.
- **HI-077** The Host Image SHALL drop traffic from the guest subnet to every
  address of the Host other than the bridge gateway address.
- **HI-078** The Host Image SHALL allow TCP traffic from the guest subnet to
  the bridge gateway address on the gateway service ports, which it reads
  from the Host configuration file, and SHALL allow it on no port when
  none are set.
- **HI-079** The Host Image SHALL drop traffic to the gateway service ports on
  the bridge gateway address that arrives on any interface other than the
  bridge and loopback.
- **HI-080** The Host Image SHALL drop traffic from the gateway service user
  ids, which it reads from the Host configuration file, to the instance
  metadata service addresses and to the Host's own `flintlockd`, kubelet,
  Exec Agent and metrics ports.

Guest networking follows flintlock's documented bridge option: `flintlockd`
puts each MicroVM's TAP device on the bridge `flbr0`, and the Host gives
guests DHCP and NAT there, without libvirt. The Host Image adds isolation.
The bridge is the default rather than macvtap, because macvtap does not
work on AWS, whose network drops unknown MAC addresses, and it would put
guests on the cloud network.

The default guest subnet is `10.220.0.0/16`. It clashes with neither the
range of AWS's default VPC, `172.31.0.0/16`, nor flintlock's documented
`192.168.100.0/24`, and it stays clear of the usual Kubernetes defaults:
kubeadm's Service range `10.96.0.0/12`, Flannel's `10.244.0.0/16`,
Calico's `192.168.0.0/16`, k3s's `10.42.0.0/16` and `10.43.0.0/16`, and
Docker's `172.17.0.0/16`. A cluster that uses it overrides it (HI-037).

A Host that configures itself at boot does not know its peers, so HI-035
takes ranges: the user lists the node, pod and Service CIDRs of the cluster,
and anything else a MicroVM has no business reaching.

A MicroVM may reach the outside and nothing of the cluster. HI-075 to
HI-077 close the ways round HI-035 that a Host in a cluster has:

- A pod on the same Host is reached through its own interface on the Host,
  not through the primary interface, so HI-075 drops it whatever its
  address. So are the tunnel interfaces of an overlay network.
- kube-proxy translates the address of a Service into the address of one of
  its pods before the Host forwards the packet. HI-076 compares the address
  the guest asked for, so a protected Service range drops a Service's
  traffic even when its pods are outside every protected range. The same
  holds for a NodePort on another Node.
- The Host's own addresses, the primary one included, answer a guest only on
  the gateway (HI-036). HI-077 drops the rest, which also keeps the
  Kubernetes API away from a guest where the Host reaches it on an address
  of its own.

The Host still forwards a guest's traffic to any address outside the
protected ranges, private addresses included, so the protected CIDRs name
every range of the cluster: its nodes, its pods and its Services.

A Host may run services for its MicroVMs on the bridge gateway, such as a
registry mirror or a package cache. The gateway service ports
(`GATEWAY_SERVICE_PORTS`) open those ports to guests (HI-078), and the
gateway service user ids (`GATEWAY_SERVICE_UIDS`) name the processes that
serve them. Both are empty by default, so a guest reaches only DHCP and DNS
on the gateway, as HI-036 says. A gateway service port cannot be a control
port of HI-034, because the gateway's accept would open that port to guests.

The services answer only guests and the Host itself (HI-079). A pod on the
Host, and anything off it, arrives on another interface and is dropped.

The services run in the Host's own network namespace, so the guest subnet
rules do not apply to their own traffic. They act for guests, and a build
service may run a guest's commands as one of its user ids. HI-080 keeps
those ids from the metadata service and the Host's control ports, as
HI-033 and HI-034 do for guests. Root cannot be a gateway service user id,
nor can the Exec Agent's user id of HI-070: dropping their traffic would
cut the Host off from the metadata service and the kubelet, and the Exec
Agent off from `flintlockd`.

HI-078 to HI-080 come from flintlock-runner's HI-036 and HI-064, which
opened the gateway to its Host Services and kept their user ids from the
metadata service and the control ports. flintlock-runner set its own ports
and ids by default; here the settings are generic and empty by default.

## flintlockd {#image-flintlockd}

- **HI-040** The Host Image SHALL run `flintlockd`, its containerd and every
  hypervisor process they start as systemd services outside the cgroup of
  any Kubernetes pod.
- **HI-041** The Host Image SHALL start `flintlockd` only after the thin
  pool and the bridge are present.
- **HI-043** The Host Image SHALL enable the `flintlockd` exec API.
- **HI-067** The Host Image SHALL configure `flintlockd` to serve its gRPC
  API only with TLS, on port 9090 of the Host's internal address, with the
  serving certificate and key that the Exec Agent writes to
  `/etc/battery/flintlockd`.
- **HI-068** The Host Image SHALL configure `flintlockd` to require a client
  certificate on every connection and to verify it against the `flintlockd`
  client CA bundle that the Exec Agent writes to `/etc/battery/flintlockd`.
- **HI-069** The Host Image SHALL drop every connection to `flintlockd`'s
  port that arrives from outside the Host unless its source address is in
  the Operator's pod network, which it reads from the Host configuration
  file.
- **HI-070** The Host Image SHALL refuse connections to `flintlockd`'s port
  from the Host's own processes unless they belong to the Exec Agent's user
  id, which it reads from the Host configuration file with a default when
  none is set.
- **HI-071** The Host Image SHALL start `flintlockd` only once the serving
  certificate, its key and the client CA bundle exist in
  `/etc/battery/flintlockd`, and SHALL restart `flintlockd` when the serving
  certificate or the client CA bundle changes.
- **HI-073** When the Host Image stops or restarts `flintlockd`, it SHALL
  stop the `flintlockd` process alone and SHALL leave every hypervisor
  process that `flintlockd` started running.

battery creates and deletes MicroVMs by calling every Host's `flintlockd`
from the Operator's pod, so `flintlockd` serves on the Host's internal
address with mutual TLS (ADR 0002). The Exec Agent obtains the Host's
certificates, through certificate signing requests the Operator signs
(ADR 0003), and it connects to its own Host's `flintlockd` as battery does,
with a client certificate of its own. The Host Image holds no key and
fetches nothing: it reads what the Exec Agent writes, and it follows the
Exec Agent's conventions exactly, from `config/exec-agent/daemonset.yaml`
and `internal/execagent`: the directory `/etc/battery/flintlockd`; in it
`tls.crt`, `tls.key` and `client-ca.crt`, of which `tls.crt` is always
written last; the endpoint `$(HOST_IP):9090`, the Node's internal address;
and user id 65532, the Exec Agent image's user. The internal address is the
one the Node reports, which is the address of the Host's interface with the
default route unless the kubelet is told otherwise.

`flintlockd` has one listening endpoint and one client CA, and it admits any
certificate that CA signed, with the whole API. The client CA signs only for
battery and the Exec Agents, but a certificate stolen from one Host's Exec
Agent would open every other Host's `flintlockd`. HI-069 and HI-070 narrow
that to the two places a legitimate client connects from: battery, in the
Operator's pod, and the Exec Agent on the Host itself, which reaches the
internal address over loopback. The Operator's pod network is a list of
CIDRs in the Host configuration file and is empty by default, which admits
nothing from outside the Host: it depends on the cluster the Host joins, so
the Host's bootstrap configuration sets it. A `flintlockd` that authorized
clients by the identity in their certificate would remove the exposure;
that is for flintlock (flintlock#1242).

The Exec Agent's user id is 65532 by default because that is the user of
the Exec Agent's image, which the DaemonSet does not override. It is also
the user of many distroless images, so HI-070 keeps out every process of the
Host except those that run as the Exec Agent's user id, not every process
but the Exec Agent. It still refuses root and every other pod in the Host's
network namespace, and each connection it lets through must present a
certificate from the client CA.

HI-071 exists because `flintlockd` reads its certificate, key and client CA
once, when it starts (`pkg/auth/tls.go:18-48` of flintlock v0.15.2), and
reloads nothing until flintlock#1235. So the Host Image does not start it
before the Exec Agent has written them, and restarts it when the Exec Agent
renews the serving certificate or the client CA bundle changes. Until then
the Exec Agent finds no `flintlockd` answering and reports the Host not
ready, so battery is never given a Host without certificates (ADR 0003,
consequence 5).

HI-073 exists because HI-071 restarts `flintlockd` each time the Exec Agent
renews the serving certificate, and a restart must not end the MicroVMs on
the Host. The trial on a real Host (`hack/real-hosts`) saw a MicroVM keep
running through a restart of `flintlockd` with `KillMode=process`.

A restart leaves the MicroVMs running. In flintlock v0.15.2, the version the
Host Image pins, Firecracker and Cloud Hypervisor are started detached by
default (`pkg/defaults/defaults.go:28` and `:38`), in a session of their own
(`pkg/process/process.go:16-24`, called from
`infrastructure/microvm/firecracker/create.go:97` and
`infrastructure/microvm/cloudhypervisor/create.go:116`), with nothing tying
their lives to `flintlockd`'s. `flintlockd.service` has `KillMode=process`,
so systemd stops `flintlockd` alone and leaves the hypervisor processes in
the unit's cgroup. When `flintlockd` starts again it resyncs every MicroVM
spec (`internal/command/run/run.go:262`,
`infrastructure/controllers/microvm_controller.go:56-63`). A MicroVM whose
hypervisor process named in its pid file is alive is reported as running
(`infrastructure/microvm/firecracker/provider.go:127-166`;
`infrastructure/microvm/cloudhypervisor/provider.go:76-149`), so the plan
neither creates it again (`core/steps/microvm/create.go:45`) nor starts it
(`core/steps/microvm/start.go:52`), and its tap device is left alone because
it exists (`core/steps/network/interface_create.go:61`). Its sockets are
under `/run/flintlock`, which a restart does not touch.

What a restart does cut is every gRPC stream open at the time. systemd stops
`flintlockd` with `SIGTERM`, which `flintlockd` does not handle (it waits
for `os.Interrupt` only, `internal/command/run/run.go:110`), so it exits at
once rather than through `GracefulStop`. A command running through the exec
API when the certificate is renewed therefore loses its stream, and the Exec
Agent reports that as a stream failure, never as success (EA-020). What
happens to the command inside the guest then is the guest agent's business
and has not been established. Renewal is rare, before two thirds of a
certificate's lifetime (EA-063).

HI-040 is the one place where this design refuses to be Kubernetes-native.
Firecracker processes are children of `flintlockd`; inside a pod they would
share its cgroup, and a pod restart or an eviction would kill every MicroVM
on the Host. Kubernetes manages the Node, and systemd manages what runs the
MicroVMs.

## Host configuration {#host-configuration}

- **HI-050** The Host Image SHALL read per-Host settings from one Host
  configuration file that cloud-init writes at first boot, containing the
  guest subnet, the thin pool device, the protected CIDRs and the Host
  reserve.
- **HI-051** If the Host configuration file is absent, then the Host Image
  SHALL boot with its defaults for every setting.
- **HI-052** The Host Image SHALL NOT read any secret from the Host
  configuration file or from instance user-data.

The Host configuration file is `/etc/battery/host.conf`. The Host's
bootstrap configuration writes it, for example a kubeadm config template's
`files`.

## Kubernetes node {#kubernetes-node}

- **HI-060** The Host Image SHALL register its kubelet with the label
  `battery.liquidmetal-x.dev/host-image` set to a value derived from the
  image digest, and with one label per hypervisor carrying its pinned
  version.
- **HI-061** The Host Image SHALL configure the kubelet to reserve all CPU
  and memory beyond the configured Host reserve, so that the Host's Node
  offers only the Host reserve to pods and leaves the rest to the MicroVMs
  that battery places on the Host.
- **HI-062** The Host Image SHALL disable automatic bootc updates, so that
  an operating system update is applied only to a drained Host.
- **HI-074** The Host Image SHALL register its kubelet with the label
  `battery.liquidmetal-x.dev/host` set to `true`.

The version labels are facts about the image: a Firecracker snapshot
restores only on the same CPU model, Firecracker version and host kernel,
and the image label names the last two. A taint that keeps ordinary pods
off a Host is policy of the cluster the Host joins, so it belongs in the
kubeadm join configuration rather than in the image.

HI-074 is the Host label. The Exec Agent runs only on Nodes with that label
(EA-004), and the Inventory Controller gives battery only those Nodes. Every
Host that boots this image has `flintlockd`, KVM and the thin pool that the
Exec Agent checks for, so the image sets the label itself. The Exec Agent
still reports a Host not ready when a check fails.

In-place upgrade, draining a Host and then running `bootc upgrade` and
rebooting, is the reason to build on bootc, but its orchestration is not
specified yet. Until it is, an upgrade is a new machine from a new image.
