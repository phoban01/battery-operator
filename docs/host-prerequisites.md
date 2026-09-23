# Host prerequisites

A Host is a Kubernetes Node that runs `flintlockd` and an Exec Agent, and
that the Inventory Controller has given to battery. This project ships no
Host Image ([ADR 0001](adr/0001-standalone-operator-over-battery-grpc.md),
decision 9): it states what a Node has to provide before it can be a Host,
and the Exec Agent checks it on the Node itself (decision 10). A Node joins
battery's Hosts only while its Exec Agent reports it ready (IN-001).

This page is the operator's view. The requirements are
[05-exec-agent.md#host-checks](requirements/05-exec-agent.md#host-checks);
the glossary's *Host prerequisites* is the short form.

## What a Node needs

| Prerequisite | Why | Checked by | Reason when missing |
|---|---|---|---|
| `flintlockd` with its exec API enabled, on the Host's internal address, with mutual TLS | battery creates MicroVMs through it, and the Exec Agent relays exec to it | `ServerInfo` over mutual TLS (EA-030) | `FlintlockdNotReady`, or `ExecDisabled` |
| KVM: `/dev/kvm` that opens for reading and writing | `flintlockd`'s hypervisor runs every MicroVM on it | opening the device (EA-031) | `KVMUnavailable` |
| containerd's thin pool | `flintlockd` puts every MicroVM's volumes on containerd's devmapper snapshotter | looking the pool up in sysfs (EA-032) | `ThinPoolMissing` |
| The label `battery.liquidmetal-x.dev/host=true` | the Exec Agent runs only on labelled Nodes (EA-004) | the DaemonSet's node selector | no Exec Agent, so no Node report |

A Host Image can add reasons of its own through the not ready reason
directory (EA-033); the agent reports those as `HostImageNotReady`.

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

Until #31, the Exec Agent's client certificate for `flintlockd`, the serving
CA and the Exec Agent's own serving certificate are files on the Host under
`/etc/battery/exec-agent`, readable by user 65532. #31 replaces them with
certificates the Exec Agent requests itself
([ADR 0003](adr/0003-host-certificates-through-kubernetes-csrs.md)).

### KVM

`/dev/kvm` exists and opens for reading and writing, as it has to for
`flintlockd`'s hypervisor. On bare metal that means virtualization is
enabled in the firmware and the `kvm_intel` or `kvm_amd` module is loaded;
on a virtual machine, nested virtualization.

The Exec Agent runs as user 65532 without privileges, so for it to open the
device:

- the device's mode has to let it: systemd's default udev rules make
  `/dev/kvm` mode `0666`; a Host with `0660` needs the `kvm` group added to
  the Exec Agent's pod as a supplemental group;
- the container runtime has to give the container the device. An
  unprivileged container may open only the devices it was given, whatever
  is mounted into it, so the cluster gives the Exec Agent `/dev/kvm` with a
  device plugin or a CDI device, as a patch to
  `config/exec-agent/daemonset.yaml`. Without it the agent reports
  `KVMUnavailable` with `operation not permitted`.

The DaemonSet mounts the Host's `/dev` read-only at `/host/dev` and passes
`--kvm-device=/host/dev/kvm`. It mounts the directory rather than the
device so that a Host without KVM still runs the agent, which then reports
why the Host is not ready.

### containerd's thin pool

containerd's devmapper snapshotter uses a device-mapper thin pool whose name
is its `pool_name`. The pool is activated whenever the Host is up. The Exec
Agent is configured with the same name, `--thin-pool`, by default
`flintlock-thinpool`, which is flintlock's default and what
flintlock-runner's Host Image creates (the logical volume `thinpool` in the
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
