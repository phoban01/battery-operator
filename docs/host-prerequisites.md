# Host prerequisites

A Host is a Kubernetes Node that runs `flintlockd` and an Exec Agent, and
that the Inventory Controller has given to battery. This page states what a
Node has to provide before it can be a Host, and the Exec Agent checks most
of it on the Node itself
([ADR 0001](adr/0001-standalone-operator-over-battery-grpc.md), decision
10). A Node joins battery's Hosts only while its Exec Agent reports it ready
(IN-001).

This page is the contract. One way to meet it is the Host Image in
[`hostimage/`](../hostimage/README.md), a bootc image a Node boots from
([ADR 0007](adr/0007-reference-host-image-and-cluster-api.md)): it provides
every prerequisite below, and its requirements are
[11-host-image.md](requirements/11-host-image.md). A Node built another way
that meets the prerequisites is a Host too.

To make Hosts from the Host Image with Cluster API on AWS, use the host pool
templates in [`config/capi`](../config/capi/README.md). They boot the image
from an AMI, join each Host with kubeadm, and write its settings, such as
who may reach `flintlockd`, at first boot.

This page is the operator's view. The requirements for the checks are
[05-exec-agent.md#host-checks](requirements/05-exec-agent.md#host-checks);
the glossary's *Host prerequisites* is the short form.

## What a Node needs

| Prerequisite | Why | Checked by | Reason when missing |
|---|---|---|---|
| `flintlockd` with its exec API enabled, on the Host's internal address, with mutual TLS | battery creates MicroVMs through it, and the Exec Agent relays exec to it | `ServerInfo` over mutual TLS (EA-030) | `FlintlockdNotReady`, or `ExecDisabled` |
| KVM: a KVM device, `/dev/kvm` | `flintlockd`'s hypervisor runs every MicroVM on it | the device in sysfs and `/dev/kvm` a character device, not opened (EA-031) | `KVMUnavailable` |
| containerd's thin pool | `flintlockd` puts every MicroVM's volumes on containerd's devmapper snapshotter | looking the pool up in sysfs (EA-032) | `ThinPoolMissing` |
| The label `battery.liquidmetal-x.dev/host=true` | the Exec Agent runs only on labelled Nodes (EA-004) | the DaemonSet's node selector | no Exec Agent, so no Node report |
| Guest networking: DHCP and NAT on the bridge `br-battery`, and isolation from the cluster | a MicroVM gets its address from the Host and reaches the outside, and nothing of the cluster | not checked by the Exec Agent; the Host Image provides it (HI-030 to HI-037, HI-075 to HI-080, HI-083) | none: a MicroVM without it has no network, or too much |

A Host Image can add reasons of its own through the not ready reason
directory, `/run/battery/not-ready.d` (EA-033); the agent reports those as
`HostImageNotReady`.

### `flintlockd`

- `flintlockd` serves its whole API on the Host's internal address, the one
  the Node reports as `InternalIP`, on port 9090 as
  `config/exec-agent/daemonset.yaml` has it
  ([ADR 0002](adr/0002-battery-reaches-flintlockd-over-mtls.md)).
- It serves over TLS with a serving certificate that names that address,
  signed by the serving CA, and validates client certificates against the
  `flintlockd` client CA (`--tls-client-validate`, `--tls-client-ca`).
- Its exec service is enabled, so that `ServerInfo` reports it.
- The Host's firewall admits connections to `flintlockd`'s port only from
  the Operator's pod network and from the Host itself (ADR 0002,
  consequence 2).

`flintlockd`'s certificates come from the Exec Agent
([ADR 0003](adr/0003-host-certificates-through-kubernetes-csrs.md), EA-061
and EA-064). The agent writes the serving certificate, its key and the
client CA bundle to `/etc/battery/flintlockd` on the Host, as `tls.crt`,
`tls.key` and `client-ca.crt`, with `tls.crt` written last. So the Host
Image:

- creates `/etc/battery/flintlockd`, writable by the agent's user, 65532;
- starts `flintlockd` once `tls.crt` exists, with `--tls-cert`,
  `--tls-key` and `--tls-client-ca` naming those three files;
- restarts `flintlockd` whenever `tls.crt` or `client-ca.crt` changes. The
  agent replaces each file by renaming a new one into place, and it renews
  the certificate before two thirds of its validity have passed (EA-063).

A systemd `.path` unit with `PathExists=` on `tls.crt` does the first, and
another with `PathChanged=` on both files does the second.
The Host Image (`flintlockd.path` and `battery-flintlockd-restart.path`)
and `hack/real-hosts/host/` have both. The agent's own client certificate and
serving certificate stay in its memory and are not on the Host.

### KVM

The Host has a KVM device, `/dev/kvm`, which `flintlockd`'s hypervisor
opens. On bare metal that means virtualization is enabled in the firmware
and the `kvm_intel` or `kvm_amd` module is loaded; on a virtual machine, it
means nested virtualization.

The Exec Agent checks two things, and passes only when both hold:

- the kernel lists the device in sysfs: `/sys/class/misc/kvm/dev` exists,
  which it does once the kvm module has registered the device;
- the Host's `/dev/kvm` is a character device.

The Exec Agent does not open the device. It runs unprivileged, and an
unprivileged container may open only the devices the container runtime gave
it. `flintlockd`, running on the Host, is what opens `/dev/kvm`. Whether the
device works for `flintlockd` shows in EA-030: a `flintlockd` that cannot
run MicroVMs is for the Host Image to report, through `ServerInfo` or the
not ready reason directory.

The DaemonSet mounts the Host's `/dev` read-only at `/host/dev` and passes
`--kvm-device=/host/dev/kvm`. It mounts the directory rather than the
device so that a Host without KVM still runs the agent, which then reports
why the Host is not ready. Every container has the Host's sysfs read-only,
so `/sys/class/misc/kvm` needs no mount; `--kvm-sysfs-dir` exists for
tests.

### containerd's thin pool

containerd's devmapper snapshotter uses a device-mapper thin pool whose name
is its `pool_name`. The pool is activated whenever the Host is up. The Exec
Agent is configured with the same name, `--thin-pool`, by default
`flintlock-thinpool`, which is flintlock's default and what
the Host Image creates (the logical volume `thinpool` in the
volume group `flintlock`).

The agent looks the name up in sysfs: every device-mapper device is
`/sys/block/dm-N`, and `/sys/block/dm-N/dm/name` holds its name. The check
passes when one of them is the configured pool. sysfs is read rather than
`/dev/mapper` because every container already has the Host's sysfs,
read-only, and it lists every block device of the Host, whereas a
container's `/dev` has none of them, and `/dev/mapper/<name>` is often a
symbolic link to `../dm-N` that a `/dev/mapper` mounted on its own cannot
resolve. So the DaemonSet needs no mount for this check, and
`--sys-block-dir` exists only for tests.

The check is that the pool is present. It does not check that the device is
a thin pool rather than some other device of that name, nor how full it is.

### Guest networking

A MicroVM needs an address, a way out, and a wall between it and the
cluster. The Host provides this with flintlock's documented bridge option,
without libvirt:

- `flintlockd` puts each MicroVM's TAP device on the bridge `br-battery`
  (`--bridge-name br-battery`). The name matches `^br-.*`, which Calico's
  IP address autodetection skips, so Calico never takes the bridge's
  address as the Node's (HI-083).
- The bridge has the first address of the guest subnet, and a DHCP and DNS
  service bound to the bridge alone gives guests an address, the gateway
  and a resolver.
- The Host forwards IPv4 and masquerades the guest subnet out of its
  primary interface.
- The Host isolates guests from the cluster: it drops their traffic to the
  instance metadata service, to its own addresses except DHCP and DNS on
  the gateway, to every interface but the primary one (so pods on the Host
  and overlay tunnels), and to the cluster's node, pod and Service ranges,
  a Service's address before kube-proxy's DNAT included.

A Host may also offer services to its guests on the bridge gateway, such as
a registry mirror or a package cache. The Host Image does this through two
settings in the Host configuration file, both empty by default:

- `GATEWAY_SERVICE_PORTS` lists the TCP ports on the gateway that guests
  may reach. Only guests and the Host itself reach them (HI-078, HI-079).
- `GATEWAY_SERVICE_UIDS` lists the user ids of the processes that serve
  those ports. The Host drops their traffic to the instance metadata
  service and to its own control ports, as it does for guests (HI-080).

With both empty, guests reach only DHCP and DNS on the gateway. A Host
built another way that offers such services keeps the same limits.

The Exec Agent does not check any of this, and a Host without it is still
reported ready: its MicroVMs then have no network, or reach more than they
should. Keep the guest subnet clear of the cluster's ranges and of the
network the Host is on. The Host Image's default is `10.220.0.0/16`, which
clashes with neither AWS's default VPC, `172.31.0.0/16`, nor flintlock's
documented `192.168.100.0/24`. Its [README](../hostimage/README.md) has the
rules, and the settings in the Host configuration file that change them.

## What the Node report says

The Exec Agent rechecks every sync interval and keeps these annotations on
its Node (EA-034, EA-035):

| Annotation | Value |
|---|---|
| `battery.liquidmetal-x.dev/exec-agent-ready` | `true` or `false` |
| `battery.liquidmetal-x.dev/exec-agent-reason` | `Ready`, or the reason of the first missing prerequisite, in the order `HostImageNotReady`, `KVMUnavailable`, `ThinPoolMissing`, `FlintlockdNotReady`, `ExecDisabled` |
| `battery.liquidmetal-x.dev/exec-agent-message` | every missing prerequisite in words |
| `battery.liquidmetal-x.dev/exec-agent-address` | where the Exec Agent serves its exec API |
| `battery.liquidmetal-x.dev/flintlockd-address` | where battery reaches the Host's `flintlockd`, the endpoint the Exec Agent is configured with (`--flintlockd`) |

To see why a Node is not a Host:

```sh
kubectl get node <name> -o jsonpath='{.metadata.annotations}'
```
