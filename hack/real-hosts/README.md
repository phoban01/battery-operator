# Real hosts trial

The whole stack on one real Linux machine with KVM: k3s, cert-manager,
battery-operator's Manifests with battery's `poolmgrd` as the Operator's
sidecar, and a Host with containerd's thin pool, Firecracker and
`flintlockd` v0.15.2 serving with mutual TLS. The smoke test then takes a
Pool and a claim through their lives against real MicroVMs.

The machine is the Node, the k3s server and the only Host. One Host is
enough to prove the path; placement across several Hosts is left to the
e2e suite.

This is a manual harness, not the e2e suite (`test/e2e`, ADR 0006), which
runs the fake `flintlockd` on kind. It needs KVM, which CI has not got.

## The Host

`HOST_DRIVER` chooses how the scripts reach the Host (`lib.sh`):

| Driver | The Host | `up.sh` | `down.sh` |
|---|---|---|---|
| `lima` (default) | A Lima VM, `bo-host-1`, with nested virtualization on an Apple silicon Mac | creates and starts it | deletes it |
| `ssh` | A Linux machine that already exists: bare metal, or a cloud instance with nested virtualization, reached with `ssh ${HOST_SSH}` | installs onto it | leaves it alone |

### `lima`

- An Apple silicon Mac of the M3 generation or later, on macOS 15 or later,
  with [Lima](https://lima-vm.io) 2.2. Lima's nested virtualization for
  `vz` needs both.
- About 6 GiB of memory and 4 CPUs free for the VM (`VM_MEMORY`,
  `VM_CPUS`), and about 10 GiB of disk: the VM's 40 GiB disk is sparse.
- The Mac awake: `up.sh` and `smoke.sh` run `caffeinate -i` on it for two
  hours (`KEEP_AWAKE_SECS`), since a sleeping Mac freezes the VM.

The scripts run on the Mac, or on a Linux machine that reaches the Mac with
`mac <command>`, such as an OrbStack machine: `lib.sh` sends every Lima
command through it, and the kubeconfig reaches the Mac as `host.internal`.
Only the API server's 6443 is forwarded, to the Mac's 16443
(`API_HOST_PORT`).

### `ssh`

```sh
export HOST_DRIVER=ssh HOST_SSH=ubuntu@203.0.113.10
```

- Ubuntu 24.04, arm64 or amd64, with `/dev/kvm`, about 6 GiB of memory, 4
  CPUs and 10 GiB of disk free. `host/provision.sh` uses apt and loads the
  kernel modules for the thin pool and tap devices.
- `HOST_SSH`'s user with sudo without a password. `SSH_OPTS` adds ssh
  options, such as `-p 2222 -i key`. The scripts share one ssh connection.
- The machine's 6443 reachable from where the scripts run, at the address
  in `HOST_SSH` or at `HOST_API_ADDRESS` (and `HOST_API_PORT`).
- The machine is changed for good: k3s, containerd, Firecracker, flintlockd
  and their units are installed on it. Use a machine you can throw away.

### Both

- `docker` to build the MicroVM images for the Host's architecture, which
  `up.sh` reads from the Host; `kubectl`; and Go, or `devbox`, for the smoke
  test's Consumer.
- Network access from the Host to GitHub, get.k3s.io, ghcr.io and Docker
  Hub.
- The Host's address is the one on the interface of its default route, or
  on `HOST_IFACE`.

## Steps

```sh
hack/real-hosts/up.sh      # everything, idempotent; or: up.sh machine host k3s images deploy
hack/real-hosts/smoke.sh   # or: smoke.sh pool claim restart delete
export KUBECONFIG=hack/real-hosts/.state/kubeconfig
hack/real-hosts/down.sh    # lima: deletes bo-host-1, and nothing else; ssh: leaves the machine
```

`IMAGE_TAG=<main commit SHA>` deploys the Operator and the Exec Agent at
that commit instead of `latest`.

### `up.sh`

| Step | What it does | What it proves |
|---|---|---|
| `machine` | `lima`: creates and starts `bo-host-1`, Lima `vz`, `--nested-virt`, 4 CPUs, 6 GiB, 40 GiB, Ubuntu 24.04, on Lima's default network, with no mounts. `ssh`: checks the machine answers and has sudo. | The Host has `/dev/kvm` and an address. |
| `host` | Runs `host/provision.sh`: the thin pool `flintlock-thinpool` on loop devices (`bo-thin-pool.service`), containerd v1.7.22 with the devmapper snapshotter for `flintlockd` only, Firecracker v1.12.1, `flintlockd` v0.15.2 (checksum-verified), the bridge `flbr0`, a registry on `127.0.0.1:5000`, and `/etc/battery/flintlockd` owned by 65532. | The Host prerequisites of `docs/host-prerequisites.md`, but for `flintlockd`'s certificates, which come in `deploy`. |
| `k3s` | k3s v1.35.8 server, on the Host's address, with the Node named `bo-host-1` (`NODE_NAME`) and labelled `battery.liquidmetal-x.dev/host=true`. Writes `.state/kubeconfig`. | A Ready, schedulable Node whose InternalIP is where `flintlockd` and the Exec Agent serve. |
| `images` | Builds the MicroVM kernel (Firecracker's CI 6.1 kernel) and root filesystem (Ubuntu 24.04, systemd, flintlock's guest-agent v0.4.0) for the Host's architecture, and pushes both to the Host's registry. | Images `flintlockd` can pull on the Host. |
| `deploy` | cert-manager v1.20.2, then `config/default` and `config/exec-agent` as they ship, at `IMAGE_TAG` (`manifests/`). Waits for the Host's Node report. | The Exec Agent gets its certificates through CSRs the Operator signs, writes `flintlockd`'s, `flintlockd.path` starts `flintlockd` with them, and the agent reports its Host ready: KVM, thin pool, and `ServerInfo` over mutual TLS with exec enabled. |

### `smoke.sh`

| Step | What it does | What it proves |
|---|---|---|
| `pool` | Applies `smoke/trial.yaml`: a namespace, a Holder, and a Pool of 2 MicroVMs (1 vCPU, 1024 MiB, one tap interface on `flbr0`) with a create hook. | The Inventory Controller gave the Host to battery; battery created the MicroVMs through `flintlockd` over mutual TLS, reached their guest-agents, ran the create hook, and the Pool is Ready with 2 Firecracker processes on the Host. |
| `claim` | Builds `smoke/main.go` (build tag `realhosts`) for the Host's architecture and runs it on the Host: it claims a MicroVM with `pkg/claimclient` as the Holder, runs `uname -a` in it through the Exec Agent, prints the output, and releases the claim. | The Client Library's whole path on a real Host: bind, the claim token, the Exec Agent's TLS and authorization, the relay to `flintlockd`'s exec API and the guest-agent, release. |
| `restart` | Restarts `flintlockd` under a running MicroVM, then claims again. | Whether a MicroVM survives a restart of its Host's `flintlockd`, and whether the Pool still serves claims afterwards. |
| `delete` | Deletes the Pool. | The Pool goes, and every Firecracker process with it. |

The Consumer runs on the Host because the Exec Agent serves on the Host's
address, which the machine running the scripts may not route to (Lima's
network does not route to the Mac).

## How the Host is put together

`host/` has the units and scripts. They mirror flintlock-runner's Host
Image (`image/rootfs/usr/lib/systemd/system/flintlockd.service`) where they
can, with what battery-operator's ADR 0002 and ADR 0003 change:

- `flintlockd` serves on the Host's internal address, not loopback, with
  `--tls-cert`, `--tls-key`, `--tls-client-validate` and `--tls-client-ca`
  from `/etc/battery/flintlockd`, instead of `--insecure`
  (`host/flintlockd-run.sh`). `--enable-exec-api`, `--bridge-name flbr0`,
  `KillMode=process` and `Restart=always` are the Host Image's.
- `flintlockd.service` is not enabled. `flintlockd.path` starts it once the
  Exec Agent has written `tls.crt`, which it writes last, and
  `flintlockd-certs.path` restarts it through `flintlockd-restart.service`
  when `tls.crt` or `client-ca.crt` changes.
- k3s runs its own containerd, so `flintlockd`'s containerd has no CRI
  plugin and serves only `flintlockd`. The Host Image has one containerd for
  both.
- The thin pool is on loop devices over sparse files, put together at every
  boot; the Host Image has a volume group. The name, `flintlock-thinpool`,
  is the same.
- There is no firewall on `flintlockd`'s port (ADR 0002, consequence 2).

## What the first runs found

On a Lima VM on an M4 Mac, at `main` of 25 September 2026:

- A Pool `flintlockd` v0.15.2 refuses is accepted by the CRD (#141): a
  MicroVM needs at least 1024 MiB and one network interface.
- battery v0.3.3 provisions an `ImmediateOnLease` or `ReplaceOnDelete` Pool
  once per reconciler, so a Pool whose first provisioning fails never fills
  (#142).
- A Pool's status does not say why it is not filling; battery reports a
  failed provision nowhere the Operator can read (#143).
- A sleeping Mac freezes the VM for 15 minutes at a time. `caffeinate` now
  keeps it awake.

With `smoke/trial.yaml` fixed for the first, every step of `smoke.sh`
passes, `restart` included: a MicroVM survives a restart of `flintlockd`.
