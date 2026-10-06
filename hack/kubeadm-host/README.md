# kubeadm Host proof

A Node booted from the [Host Image](../../hostimage/README.md) and joined
by kubeadm becomes a working Host
([ADR 0007](../../docs/adr/0007-reference-host-image-and-cluster-api.md),
consequence 4). The proof boots the image in a VM on an Apple silicon Mac,
with cloud-init user-data rendered from the Cluster API
KubeadmConfigTemplate in [config/capi](../../config/capi/README.md), joins
it to a kubeadm control plane in a second VM, deploys battery-operator
from this checkout's Manifests, and runs the
[real hosts trial](../real-hosts/README.md)'s smoke test on it.

This is a manual harness, like the trial. It needs KVM, which CI has not
got.

## What it proves

- The Host Image boots from a disk that `bootc install to-disk` makes of
  it, on arm64, with SELinux enforcing.
- cloud-init runs the user-data that Cluster API's kubeadm bootstrap
  provider (CABPK) would make of the KubeadmConfigTemplate. It writes
  `/etc/battery/host.conf` from the template's `files`, and runs
  `kubeadm join`. The Node takes its name from
  `{{ ds.meta_data.local_hostname }}`, gets the Host label and the Host
  taint, and CABPK's success file is written.
- At boot the image makes the thin pool on the second disk, the guest
  bridge and the firewall, and labels the Node with its image digest and
  the Firecracker and Cloud Hypervisor versions.
- Calico runs on the Host with SELinux enforcing. A pod on the Host that
  does not use the host's network gets a pod address, reaches a pod on the
  control plane and a ClusterIP Service, and resolves the Service's name
  through the cluster's DNS.
- The Exec Agent gets its certificates through CSRs the Operator signs,
  `flintlockd` starts with them, and the agent reports the Host ready.
- The trial's smoke steps pass on the Host: a Pool of two MicroVMs becomes
  Ready; a claim runs `uname -a` in a MicroVM through the Exec Agent; a
  MicroVM reaches `https://github.com` through the guest network; a
  MicroVM survives a restart of `flintlockd`; and deleting the Pool
  deletes its MicroVMs.
- With Calico running, a MicroVM reaches no pod and no Service: not a pod
  on the control plane, not a pod on its own Host, not a ClusterIP
  Service and not the API server's Service (HI-035, HI-075, HI-076).

It does not prove the AWS side of the templates: the AWSMachineTemplate,
the MachineDeployment and the MachineHealthCheck are rendered but not used.
Only the KubeadmConfigTemplate is.

## How it works

| Piece | What |
|---|---|
| Control plane | A Lima VM, `bo-kubeadm-cp`: Ubuntu 24.04, 2 CPUs, 3 GiB. containerd, `kubeadm init` at the Host Image's `KUBERNETES_VERSION` (`hostimage/versions.env`), Calico v3.31.2 from the manifest the [Hosts page](../../site/hosts.md) applies, checked against its SHA-256, and local-path storage. Its taint is removed, so it runs the Operator and cert-manager. |
| Host Image | Built from this checkout with `make host-image` for linux/arm64, and loaded into Docker. |
| Disk | `bootc install to-disk`, run from the image itself in a privileged container, onto a sparse 20 GiB raw file. A blank 20 GiB file is the thin pool's disk. |
| Host | A [vfkit](https://github.com/crc-org/vfkit) VM, `bo-kubeadm-host-1`, 4 CPUs, 8 GiB, with nested virtualization, EFI, the two disks, and the user-data as a NoCloud seed. |
| User-data | `pool/` is a Host pool like `config/capi/pools/example`: it builds `config/capi/host-pool` with the `host-pool-params` Component. `userdata/main.go` turns its KubeadmConfigTemplate into cloud-config the way CABPK does: the `files` and `users`, the join configuration as a kubeadm v1beta4 `JoinConfiguration` with a bootstrap token, and `kubeadm join` in `runcmd`. It fails on any template field it does not map. |
| images, deploy, smoke | The trial's own steps (`../real-hosts/up.sh images deploy` and `../real-hosts/smoke.sh`), with the trial's `ssh` driver pointed at the Host. The trial's registry runs on the Host in podman, on `127.0.0.1:5000`. `smoke.sh` adds two steps of its own: `pods` and `isolation`. |

The pool differs from a real one in these ways, each for the Mac and not
for the templates:

- `host.conf` names the thin pool's disk, `/dev/vdb`. A virtio disk has no
  `Instance Storage` model, so the image does not detect it.
- A user with an ssh key and sudo, through the KubeadmConfig's `users`
  (`pool/users.yaml`), for the scripts.
- A route to the control plane through the Mac, through the
  KubeadmConfig's `preKubeadmCommands` (`pool/network.yaml`). macOS makes
  each VM's port on its shared network private, so two VMs on it cannot
  reach each other directly. The Mac routes between them. `up.sh` adds the
  route back on the control plane.

Calico differs from the Hosts page's in three settings, each for the Mac:

- Its IP pool is `POD_CIDR`, `10.244.0.0/16`. Calico's default,
  `192.168.0.0/16`, overlaps the Mac's networks. The pod and Service
  ranges also stay clear of the Host's guest subnet, `10.220.0.0/16`.
- The pool encapsulates in VXLAN, not IP-in-IP. The Mac routes TCP and UDP
  between the VMs but not IP protocol 4: each Node sent IP-in-IP packets
  and neither received one. A cloud network that admits protocol 4
  between Nodes needs no change.
- `IP` is empty, not `autodetect`, and the control plane's Node has its
  address on the shared network in the annotation
  `projectcalico.org/IPv4Address`. With `IP` empty, calico-node keeps a
  Node's address when it has one, and autodetects it when it has none
  (`configureIPsAndSubnets` in Calico's `startup.go`). Calico's default
  method, `first-found`, takes the first interface it does not exclude, and
  on a Lima VM that is Lima's user-mode network, whose address every Lima
  VM shares. The Host has no annotation, so its calico-node runs
  `first-found`, as on a cloud Node, and must skip the guest bridge
  (HI-083).

Two more things stand in for what a cloud cluster has:

- The template starts the kubelet with `--cloud-provider=external`. The
  kubelet then leaves the Node's addresses empty and taints it
  `node.cloudprovider.kubernetes.io/uninitialized` until a cloud
  controller manager initializes it. There is none here, so `up.sh` sets
  the Node's `InternalIP` and removes the taint, as the AWS cloud
  controller manager would.
- Nothing changes how calico-node runs on the Host. It and its init
  containers are privileged, so they run as `spc_t` and may write
  `/opt/cni/bin` and `/etc/cni/net.d` (HI-066). See "Pods run confined" in
  the Host Image README.

## What it needs

- An Apple silicon Mac of the M3 generation or later, on macOS 15 or later,
  for nested virtualization, with [Lima](https://lima-vm.io) 2.2. `up.sh`
  downloads vfkit v0.6.4 and checks its checksum.
- 6 CPUs and 11 GiB of memory free for the two VMs, and about 20 GiB of
  disk: the Host's disk takes about 7 GiB on the Mac, the control plane
  about 5 GiB, and the Host Image about 2.5 GiB in Docker.
- The scripts run on a Linux machine that reaches the Mac with `mac
  <command>` and routes to the Mac's shared network, such as an OrbStack
  machine, with Docker, `kubectl`, and `devbox` or Go. `HOST_SSH_VIA_MAC=1`
  sends ssh to the Host through the Mac for a machine that does not route
  there. Docker runs `bootc install` in a privileged container, which needs
  loop devices.
- The Mac awake: the scripts run `caffeinate -i` for two hours.
- Network access to GitHub, pkgs.k8s.io, quay.io, ghcr.io, Docker Hub and
  the Fedora mirrors.

The Host's disks, vfkit and its logs are in `~/.cache/bo-kubeadm-host` on
the Mac. `console.log` there is the Host's serial console.

## Steps

```sh
hack/kubeadm-host/up.sh      # or: up.sh controlplane image disk host join images deploy
hack/kubeadm-host/smoke.sh   # or: smoke.sh pods pool claim network isolation restart delete
export KUBECONFIG=hack/kubeadm-host/.state/kubeconfig
hack/kubeadm-host/down.sh    # stops the Host, deletes bo-kubeadm-cp and the Host's disks
```

Run them inside devbox (`devbox run -- hack/kubeadm-host/up.sh`), or with
Go and `kubectl` on the `PATH`.

| Step | What it does |
|---|---|
| `controlplane` | Creates and starts `bo-kubeadm-cp` on Lima's `vzNAT` network, and runs `controlplane/provision.sh` on it. Writes `.state/kubeconfig`. |
| `image` | `make host-image` for linux/arm64, as `HOST_IMG`, unless Docker has it. `REBUILD_IMAGE=1` builds it again. |
| `disk` | Installs `HOST_IMG` onto `host-root.raw` and makes a blank `host-thin-pool.raw`, unless the root disk exists. `REBUILD_DISK=1` makes both again. |
| `host` | Renders the user-data and starts vfkit. |
| `join` | Waits for the Host's address in the Mac's DHCP leases, adds the route to it, waits for the Node, initializes it as above, and waits for it to be Ready. |
| `images`, `deploy` | Starts the registry on the Host, then the trial's steps. |

`smoke.sh` runs the trial's steps (`pool`, `claim`, `network`, `restart`,
`delete`) and two of its own:

| Step | What it checks |
|---|---|
| `pods` | An echo pod on the Host, which tolerates the Host taint, selects the Host label and does not use the host's network, gets an address in `POD_CIDR`. From it, `curl` reaches an echo pod on the control plane by its address and through a ClusterIP Service, and `nslookup` resolves the Service's name. |
| `isolation` | The Host itself reaches the control plane's echo pod, the Host's echo pod, the ClusterIP Service and the API server's Service. A claimed MicroVM then tries each, and each attempt must time out: the Host drops it. It needs the `pods` step's pods and the `claim` step's Consumer. |

`up.sh` is idempotent: each step does only what is missing. The Host
boots only once from its user-data: to boot it again from new user-data,
run `down.sh` (or stop vfkit and set `REBUILD_DISK=1`), then `up.sh`.

`IMAGE_TAG=<main commit SHA>` deploys the Operator and the Exec Agent at
that commit instead of `latest`, as in the trial.

## What the first run found

On an M4 Mac, macOS 26.5, at `main` of 29 September 2026:

- The Host Image's `/opt` is a directory of the read-only image, not a link
  to `/var/opt` as the image assumed. Flannel could not install its plugin
  into `/opt/cni/bin`. The image now makes `/opt/cni` a link to
  `/var/opt/cni`, where its plugins are copied at boot.
- Flannel's unprivileged init containers cannot write `/opt/cni/bin` under
  the Host's SELinux policy, as the Host Image README expected of such a
  DaemonSet. The proof runs them as `spc_t`.
- The Host's `/usr/local` is read-only, so the trial's smoke test takes the
  Consumer's path on the Host from `CONSUMER_BIN`.
- Everything else worked as the Host Image README says. The Exec Agent ran
  as `container_t` with categories of its own, `flintlockd` as
  `unconfined_service_t`, and battery reached `flintlockd` through the
  `flintlockd_clients` rule.

## What the Calico run found

The first runs used Flannel. Its main container runs as `container_t` and
cannot write `/run/flannel`, so no pod on the Host without the host's
network got a sandbox (#210). The proof now runs Calico, as the Hosts page
does. On an M4 Mac, at `main` of 5 October 2026, with SELinux enforcing:

- calico-node and its three init containers (`upgrade-ipam`,
  `install-cni`, `ebpf-bootstrap`) are privileged and ran as `spc_t`, as
  did BIRD and kube-proxy. v3.32.2 installs no flex-volume driver.
  `install-cni` wrote its plugins to `/opt/cni/bin`, that is
  `/var/opt/cni/bin` (`bin_t`), and its configuration to `/etc/cni/net.d`.
  `/run/calico` was `container_var_run_t`, `/var/lib/calico`
  `container_var_lib_t`, and `/var/log/calico` `container_log_t`, from the
  base policy.
- The journal had no AVC denial from Calico. The Host Image needed no
  change. It had two other kinds, neither from Calico: `bootc` probing
  SELinux at boot (`chcon` with a test label), and the test pod's
  `nslookup` trying `io_uring`, which `container_t` may not use. It then
  resolved the name without it.
- Felix chose iptables in its nftables backend, in the `ip filter`, `ip
  nat`, `ip mangle` and `ip raw` tables. The Host's `inet battery` chains
  hook at priority `filter - 10`, before Calico's and kube-proxy's, and a
  drop there is final. No Calico rule names the guest bridge or the guest
  subnet.
- IP-in-IP between the VMs did not pass the Mac, so the proof uses VXLAN
  (see above).

## What the first-found run found

The Host Image's guest bridge was `flbr0`, which is not on Calico's
interface exclude list (#212). It is now `virbr-battery`. The proof now
runs Calico v3.31.2 with its default method, `first-found`, on the Host.
On an M4 Mac, on 6 October 2026:

- The Host had `enp0s1` (192.168.64.27/24), `virbr-battery`
  (10.220.0.1/16) and `vxlan.calico`, and no `flbr0`. `flintlockd` ran
  with `--bridge-name virbr-battery`.
- The Host's calico-node logged `Using autodetected IPv4 address on
  interface enp0s1: 192.168.64.27/24`. The Calico Node's BGP address was
  192.168.64.27/24.
- The control plane kept 192.168.64.26/24 from its annotation and
  autodetected nothing.
- Every smoke step passed: `pods`, `pool`, `claim`, `network`,
  `isolation`, `restart` and `delete`.
- `enp0s1` comes before the bridge in the Host's interface order, so on
  this Host first-found takes it whatever the bridge's name. The proof
  shows the image works with first-found. `hostimage/check.sh` checks the
  name against Calico's list.
