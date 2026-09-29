# Host Image

The reference Host Image: a bootc container image that a Node boots from
to become a Host ([ADR 0007](../docs/adr/0007-reference-host-image-and-cluster-api.md)).
Its requirements are in
[docs/requirements/11-host-image.md](../docs/requirements/11-host-image.md).
It meets the [Host prerequisites](../docs/host-prerequisites.md), and adds
the guest network a MicroVM needs. A Node built another way that meets the
prerequisites is a Host too.

The image carries containerd with the devicemapper snapshotter, Firecracker
and its jailer, Cloud Hypervisor, `flintlockd`, the kubelet and `kubeadm`,
cloud-init, and the units that make the thin pool, the guest bridge and the
guest firewall at boot. Nothing is pushed to a Host after it boots. It comes
from flintlock-runner's `image/`, with `flr` names made `battery` names.

| Path | What it is |
|------|------------|
| `Containerfile` | The one definition of the image; base pinned by digest |
| `versions.env` | The one versions file: every version and every checksum |
| `build/` | The build steps the Containerfile runs |
| `rootfs/` | Copied to `/`: units, `/usr/libexec/battery` scripts, configuration |
| `selinux/` | The policy module |
| `check.sh`, `check-thin-pool.sh`, `check-flintlockd-access.sh`, `check-flintlockd-certs.sh`, `check-guest-isolation.sh`, `check-gateway-service-egress.sh`, `check-selinux-contexts.sh` | The check stage, run inside the built image; all but `check.sh` run in `make host-image-lint` too |
| `check-labels.sh`, `lint.sh` | Checks that run outside the image |
| `publish-ami.sh` | Makes an AMI of a published image, on request only; see [Publishing an AMI](#publishing-an-ami) |
| `pin-base.sh` | Pins a copy of the base in the Containerfile (`make host-image-base-pin`); see [Updating the base image](#updating-the-base-image) |

To make Hosts from the image with Cluster API on AWS, publish it as an AMI
and use the host pool templates in [config/capi](../config/capi/README.md).

## Building

The Dagger module builds the image (ADR 0005 and ADR 0007), for
linux/amd64:

```sh
make host-image         # build it; the check stage is part of the build
make host-image-check   # build it, run the checks again in it, and compare its labels
make host-image-lint    # bash -n, shellcheck, the digest pin, the check cases and nft syntax; no build
```

Each target calls a function of the Dagger module: `dagger call
host-image`, `host-image-check` and `host-image-lint`. `host-image` passes
every `*_VERSION` of `versions.env` as a build argument, so that the
Containerfile can make OCI labels of them. A build without those
arguments, or with one that disagrees with the file, fails.

CI runs `host-image-lint` and `host-image-check` on every pull request that
changes `hostimage/`, the Dagger module or the workflow. On a version tag it
publishes the image as `ghcr.io/phoban01/battery-operator/host-image`,
tagged with the commit and the version.

The build needs network access to quay.io, github.com, pkgs.k8s.io and the
Fedora mirrors. It needs neither `/dev/kvm` nor a cloud account. On a
machine that is not x86_64 it runs under emulation, except for the `fetch`
stage, which only downloads and therefore runs natively.

The published image depends on a file that only the check stage produces, so
no builder can skip that stage: an image that fails its checks does not
exist.

### Changing a version

Edit `versions.env`, and only there. Every download has a `*_SHA256` beside
its version; take it from the checksum file the project publishes with the
release, or compute it from the downloaded artifact, and never from anywhere
else. A version bumped without its checksum fails the build.

The kubelet and `kubeadm` are RPMs from pkgs.k8s.io, verified by dnf against
the repository key, and the key is the pinned download. cloud-init is a
Fedora RPM, verified by signature; the build asks for exactly
`CLOUD_INIT_VERSION` and fails when the Fedora repositories no longer carry
it, which is the moment to bump it.

Kubernetes is pinned to the v1.35 line because it is the last that supports
containerd 1.x.

### Updating the base image

The base is `quay.io/fedora/fedora-bootc:44`, pinned by digest. quay.io
keeps only the moving tag `44`, and deletes an old digest within days of a
rebuild. A pin of quay.io therefore breaks the build soon after it is made
(#174). So the Containerfile pins a copy of the base in this repository's
registry, `ghcr.io/phoban01/battery-operator/fedora-bootc`, which keeps
every copy. Until the first copy exists, the Containerfile still pins
quay.io.

To update the base:

1. Run the workflow `mirror host image base` from the repository's Actions
   tab. It runs `dagger call mirror-host-image-base`, which copies
   `quay.io/fedora/fedora-bootc:44` as it is now, with the index of every
   architecture. The copy's tag is the source tag, the date and the first 8
   hex digits of the digest, for example `44-20260929-f59997f5`. The copy
   has the same digest as the source.
2. Take the `make host-image-base-pin` command from the job summary, and run
   it on a new branch. It puts the digest in both `FROM` lines, and writes
   the source tag, the source digest and the date in the comment above them.
3. Open a pull request. CI builds the image on the new base and runs its
   checks.

A person with write access to the package can also run the copy from their
machine:

```sh
dagger call mirror-host-image-base --username=<user> --password=env://GITHUB_TOKEN
```

The package on ghcr.io must be public, so that CI and a Host can pull the
base without credentials. A package that a workflow creates starts private:
make it public once, by hand, after the first copy.

Nothing updates the base on a schedule or from a pull request. A person
updates it on purpose, and a pull request shows the change.

## What happens at boot

| Unit | Does | Requirement |
|------|------|-------------|
| `systemd-modules-load` (`modules-load.d/battery.conf`) | loads `kvm`, `vhost_vsock`, `dm_thin_pool`, `tun`, `bridge` | HI-010 |
| `battery-host-config` | validates the Host configuration file, writes `/run/battery/host.env`, labelled for containers | HI-050 to HI-052, HI-065 |
| `battery-kvm` | refuses unless `/dev/kvm` opens for reading and writing | HI-011 |
| `battery-thin-pool` | creates the thin pool once; leaves an existing one alone | HI-020 to HI-023 |
| `battery-network` | bridge `flbr0`, forwarding, NAT, guest firewall, `flintlockd`'s endpoint and who may connect to it | HI-030, HI-032 to HI-037, HI-067, HI-069, HI-070, HI-075 to HI-080 |
| `battery-dnsmasq` | DHCP and DNS on the bridge | HI-031 |
| `battery-flintlockd-certs` | makes `/etc/battery/flintlockd` for the Exec Agent, owned by its user id and labelled for its container | HI-072 |
| `containerd` | one containerd for the kubelet and for `flintlockd` | HI-040 |
| `flintlockd.path` | starts `flintlockd` once the Exec Agent has written its certificates | HI-071 |
| `flintlockd` | `Requires=` the KVM gate, the thin pool, the bridge, the certificate directory and containerd; serves mutual TLS on the Host's internal address; a stop or restart leaves the MicroVMs running | HI-040, HI-041, HI-043, HI-067, HI-068, HI-073 |
| `battery-flintlockd-restart.path` | restarts `flintlockd` when its serving certificate or client CA bundle changes | HI-071, HI-072 |
| `battery-kubelet-config` | writes the kubelet's labels, the Host label among them, and its reservation | HI-060, HI-061, HI-074 |
| `kubelet` | started by `kubeadm join` from the bootstrap configuration | |

`bootc-fetch-apply-updates.timer` and its service are masked (HI-062).

`flintlockd` serves its whole API, the exec API included, on port 9090 of
the Host's internal address with mutual TLS, for battery in the Operator's
pod and for the Exec Agent on the Host; the HTTP gateway stays off. See
[flintlockd's clients](#flintlockds-clients) and
[flintlockd's certificates](#flintlockds-certificates). A guest cannot
reach it: the guest firewall drops everything that arrives on the bridge
except DHCP, DNS and the gateway service ports on the gateway.

## Host configuration file

`/etc/battery/host.conf`, written by cloud-init at first boot from the
Host's bootstrap configuration, for example a kubeadm config template's
`files`. `KEY=value` lines, `#` comments, optional quotes. The file is
parsed, never sourced. A key that is absent, or the whole file when it is
absent, takes the default from `/usr/share/battery/host.conf.defaults`.
Unknown keys are skipped and their values are not kept. It holds settings
and no secrets: nothing on the Host reads a credential from it or from
user-data.

| Key | Default | Meaning |
|-----|---------|---------|
| `GUEST_SUBNET` | `10.220.0.0/16` | The bridge's IPv4 subnet, /29 or larger. The first address is the gateway. Keep it clear of the cluster's node, pod and Service ranges. See [Networking notes](#networking-notes) |
| `THIN_POOL_DEVICE` | empty | Block device for the thin pool. Empty means detect the unused instance-store disk |
| `PROTECTED_CIDRS` | empty | Comma-separated IPv4 CIDRs no guest may reach, by address or through a Service: the node, pod and Service CIDRs of the cluster, and anything else. Empty protects nothing off the Host, and `battery-network` logs a warning |
| `HOST_RESERVE_VCPU` | `2` | CPUs the Host's own Node offers to pods |
| `HOST_RESERVE_MEMORY_MB` | `4096` | Memory the Host's own Node offers to pods |
| `HOST_CONTROL_PORTS` | `9090,8090,10248,10250,10255,10256,10270,1338` | The Host's own ports, dropped for guests by name as well as by the final drop |
| `EXEC_AGENT_UID` | `65532` | The one user id of the Host's own processes that may connect to `flintlockd`, and the owner of `/etc/battery/flintlockd`: the Exec Agent's; 1 to 4294967294, never 0. See below |
| `GATEWAY_SERVICE_PORTS` | empty | Comma-separated TCP ports on the bridge gateway that guests may reach, for services the Host runs for its MicroVMs. Only guests and the Host itself reach them. None may be in `HOST_CONTROL_PORTS`. Empty leaves guests only DHCP and DNS on the gateway (HI-078, HI-079) |
| `GATEWAY_SERVICE_UIDS` | empty | Comma-separated user ids, and ranges `LOW-HIGH`, of the processes that serve those ports. Their traffic to the instance metadata service, `HOST_CONTROL_PORTS` and `flintlockd`'s port is dropped. Never 0, never `EXEC_AGENT_UID`, no overlaps. Empty drops nothing (HI-080) |
| `FLINTLOCKD_CLIENT_CIDRS` | empty | Comma-separated IPv4 CIDRs from off the Host that may connect to `flintlockd`: the Operator's pod network, where battery runs. Empty admits none, so battery cannot reach the Host. See below |

```yaml
# in a KubeadmConfigTemplate
files:
  - path: /etc/battery/host.conf
    permissions: "0644"
    content: |
      PROTECTED_CIDRS=10.0.0.0/16,192.168.0.0/16,10.96.0.0/12
      FLINTLOCKD_CLIENT_CIDRS=192.168.0.0/16
      HOST_RESERVE_VCPU=2
      HOST_RESERVE_MEMORY_MB=4096
```

An invalid file fails `battery-host-config`, and with it every unit that
needs the settings, `flintlockd` included; the reason is reported as below.

### The thin pool device

With `THIN_POOL_DEVICE` empty, the device is a disk that already belongs to
the `flintlock` volume group, or else the first whole disk whose model says
`Instance Storage`, that holds no mounted filesystem, is not under the root
filesystem and is blank. EBS volumes are never detected; name one to use it.
A named device that turns out to be the root disk, which Nitro's NVMe
enumeration makes possible, is replaced by the other unused NVMe disk when
there is exactly one and refused otherwise. A device with any filesystem or
partition signature is refused, never wiped. Once `flintlock/thinpool`
exists the unit does nothing but activate it, so the pool and the images in
it survive a reboot and an in-place upgrade.

The volume group is laid out as the pool (95%), its metadata (1%) and 4%
left free for the pool's autoextension. The logical volume `thinpool` in
the volume group `flintlock` is the pool `flintlock-thinpool`, which is
containerd's `pool_name` and the Exec Agent's `--thin-pool`.

### flintlockd's clients

battery creates and deletes MicroVMs through every Host's `flintlockd`, from
the Operator's pod, and the Exec Agent relays exec requests to its own
Host's `flintlockd` (ADR 0002). `flintlockd` therefore serves on the Host's
internal address, and each client presents a certificate from the
`flintlockd` client CA (HI-067, HI-068). The Host Image follows the Exec
Agent's conventions exactly:

| What | Value | Where the Exec Agent sets it |
|------|-------|------------------------------|
| Endpoint | the Host's internal address, port 9090 | `config/exec-agent/daemonset.yaml`: `--flintlockd=$(HOST_IP):9090` |
| Certificate directory | `/etc/battery/flintlockd` | the same: `--flintlockd-cert-dir`; `DefaultFlintlockdCertDir` in `internal/execagent/config.go` |
| Files | `tls.crt`, `tls.key` (mode 0600), `client-ca.crt`; `tls.crt` written last | `FlintlockdCertFile`, `FlintlockdKeyFile`, `FlintlockdClientCAFile` in `internal/execagent/certificates.go` |
| The Exec Agent's user id | 65532 | the Exec Agent image's user (`.dagger/main.go`); the DaemonSet sets no `runAsUser` |
| Not ready reason directory | `/run/battery/not-ready.d` | `DefaultNotReadyDir` in `internal/hostcheck/hostcheck.go`, and the DaemonSet's hostPath |

The internal address is the IPv4 address of the interface that carries the
default route, which `battery-network` writes to
`/run/battery/flintlockd.env` at boot. It is the address the kubelet
reports as the Node's `InternalIP`, and so the Exec Agent's `$(HOST_IP)` and
the address its serving certificate names, unless the kubelet is given
another with `--node-ip`. A Host where the two differ has no `flintlockd`
where the Exec Agent looks, and the agent reports it not ready.

`flintlockd` admits any certificate its client CA signed, so the firewall
(`battery-network`, table `inet battery`) narrows who may try to the two
clients there are. The input chain's first rules, for connections from off
the Host:

```
iifname != { "lo", "flbr0" } tcp dport 9090 ip saddr @flintlockd_clients counter accept
iifname != { "lo", "flbr0" } tcp dport 9090 counter drop
```

`flintlockd_clients` is `FLINTLOCKD_CLIENT_CIDRS`: the Operator's pod
network, the cluster's pod CIDR or the part of it the Operator's pods are
given. It is empty by default, which admits nothing from off the Host, and
`battery-network` logs a warning. A CNI that masquerades pod traffic to
other Nodes presents the source Node's address instead; list the node CIDR
too on such a cluster. IPv6 connections to the port are dropped.

The Exec Agent reaches `flintlockd` on the Host's internal address over
loopback, which the input rules leave alone, so the output chain decides:

```
oifname "lo" tcp dport 9090 meta skuid != <EXEC_AGENT_UID> counter reject with tcp reset
```

Every connection to port 9090 over loopback, to any address, from a socket
owned by any other user id is reset before it is made: root's, and any
other DaemonSet's. `flintlockd`'s replies come from port 9090 and pass.
Both rules are loaded with the rest of the firewall before `flintlockd`
starts (HI-069, HI-070).

- 65532 is also the user of many distroless images, so the rule admits any
  process of the Host that runs as it, not the Exec Agent alone. Each still
  has to present a certificate from the client CA. A dedicated `runAsUser`
  on the Exec Agent's DaemonSet, with `EXEC_AGENT_UID` set to match, would
  narrow it.
- The Exec Agent runs in the Host's network namespace, which rules out a
  user namespace for its pod, so the id in the pod is the id the Host's
  kernel sees.
- To talk to `flintlockd` on a Host by hand, use a client certificate from
  the client CA and run as that user id, for example with
  `setpriv --reuid=65532 --regid=65532 --clear-groups`.

The checks render both rules for the defaults and for configured values and
refuse `0`, anything that is not a user id and anything that is not a list
of IPv4 CIDRs. Where unprivileged user and network namespaces with nftables
are available (not in the image build) they also load them: a connection
from the configured user id is let through, on the internal address and on
loopback, and one from root or any other id refused; a connection from a
second network namespace with an address in `FLINTLOCKD_CLIENT_CIDRS` is let
through, and one from outside it dropped. On a booted Host,
`nft list chain inet battery input` and `... output` show the counters.

### flintlockd's certificates

The Exec Agent obtains `flintlockd`'s serving certificate and the client CA
bundle through certificate signing requests the Operator signs, generates
the key on the Host, and writes all three to `/etc/battery/flintlockd`
(ADR 0003). The Host Image fetches nothing and holds no key. It:

- makes the directory at boot (`battery-flintlockd-certs`), mode 0700,
  owned by `EXEC_AGENT_UID`, before the kubelet starts the Exec Agent, whose
  DaemonSet mounts it as a hostPath of type `Directory`;
- starts `flintlockd` only once `tls.crt` exists (`flintlockd.path`), and
  `flintlockd.service` checks all three files with `ConditionPathExists=`,
  so `flintlockd` has no `[Install]` of its own and is not started at boot
  without them. Until then the Exec Agent finds no `flintlockd` answering
  and reports the Host not ready;
- restarts `flintlockd` when `tls.crt` or `client-ca.crt` changes
  (`battery-flintlockd-restart.path`), because `flintlockd` reads them only
  when it starts (flintlock#1235). `/usr/libexec/battery/flintlockd-certs
  changed` labels the files again, compares their checksums with the ones
  `flintlockd` last started with, which `flintlockd.service`'s
  `ExecStartPre` records in `/run/battery/flintlockd-certs.loaded`, and runs
  `systemctl try-restart flintlockd` only when they differ, so the path
  unit's own events, labelling among them, restart nothing.

A restart leaves the MicroVMs running (HI-073), and cuts every exec stream
open at the time, which the Exec Agent reports as a failure; the reasons,
from flintlock's source, are under "flintlockd" in
`docs/requirements/11-host-image.md`.

A `flintlockd` stopped by hand is started again by `flintlockd.path` while
`tls.crt` exists: stop `flintlockd.path` first. When a gate fails,
`flintlockd.path` fails with it; after fixing the cause, restart
`flintlockd.path`.

`check-flintlockd-certs.sh` checks the units and runs `flintlockd-certs`
against stand-ins for `systemctl`, `chown` and `restorecon`. That systemd
watches the files and starts the units as described has not been seen on a
booted Host.

### The kubelet

`battery-kubelet-config` writes `/run/battery/kubelet.env`, and
`kubelet.service.d/20-battery.conf` appends it after kubeadm's arguments:

- `--node-labels=battery.liquidmetal-x.dev/host-image=<id>,battery.liquidmetal-x.dev/firecracker=<version>,battery.liquidmetal-x.dev/cloud-hypervisor=<version>,battery.liquidmetal-x.dev/host=true`,
  where `<id>` is the first 32 hexadecimal digits of the booted image's
  digest from `bootc status`, or `unknown` when bootc reports none.
- `--system-reserved=cpu=…,memory=…`: the machine's capacity minus the Host
  reserve, so that the Node's allocatable is the Host reserve (less the
  kubelet's own eviction threshold).

The kubelet merges repeated `--node-labels` flags, so labels given through
`kubeletExtraArgs` in the kubeadm join configuration stay. Where both set
the same key, this flag comes last and its value wins. The Cluster API host
pools in [config/capi](../config/capi/README.md) set the Host label there
too. A taint that keeps ordinary pods off a Host belongs in the join
configuration and is untouched.

`battery.liquidmetal-x.dev/host=true` is the Host label (HI-074). The Exec
Agent's DaemonSet selects it, so every Host runs an Exec Agent, which then
checks the Host and reports it ready or not.

## Not ready reasons

A unit that refuses to let `flintlockd` start says why in a file:

```
/run/battery/not-ready.d/<unit>      one line, the reason, newline-terminated
```

- The file's name is the unit's name without `.service`.
- The file exists exactly while the condition holds. The unit writes it
  atomically before it fails and removes it when it next succeeds. The
  directory is on `/run`, so a reboot starts clean.
- An empty or absent directory means no unit has anything to report. It does
  not by itself mean `flintlockd` is healthy.
- The Exec Agent reports every file in its Host's Node report, as
  `HostImageNotReady` (EA-033).

| File | First words of the reason | When |
|------|---------------------------|------|
| `battery-kvm` | `KVM is unavailable: ` | `/dev/kvm` is missing, not a character device, or cannot be opened (HI-011) |
| `battery-thin-pool` | `no thin pool device is available: ` | none named and none detected, or the named one is missing, mounted, the root disk, or not blank (HI-022, HI-023) |
| `battery-host-config` | `host configuration invalid: ` | the Host configuration file does not parse or validate |
| `battery-flintlockd-certs` | `cannot label /etc/battery/flintlockd` | the Exec Agent's certificate directory cannot be labelled for its container (HI-072) |

The units are oneshots and do not retry, and `flintlockd.path` fails with
them. After fixing the cause,
`systemctl restart battery-thin-pool flintlockd.path` (or a reboot) clears
it.

## SELinux

The image keeps the base image's policy enforcing (HI-012): it does not
touch `/etc/selinux/config`, adds no `selinux=0` or `enforcing=0` kernel
argument, never calls `setenforce` and makes no domain permissive.

- **containerd and the kubelet** are labelled by the base image's
  `container-selinux` (`container_runtime_exec_t`, `kubelet_exec_t`) and run
  in the domains that package gives them.
- **`flintlockd`, Firecracker, Cloud Hypervisor and the
  `/usr/libexec/battery` scripts** are `bin_t` under `/usr/bin` and
  `/usr/libexec`. Started by systemd they run as `unconfined_service_t`,
  which is how the targeted policy runs any service it has no module for.
  That is a property of the base policy, not a relaxation by this image, and
  it is per service: every confined domain stays confined. Writing a
  confining policy for the hypervisor processes is future work and needs a
  booted Host to develop against.
- **dnsmasq** is the one component the base policy stops (HI-013). It runs
  confined as `dnsmasq_t`, which may read its configuration only under
  `/etc`. The image generates that configuration at every boot and keeps it
  in `/run/battery`. The module `selinux/battery.te` gives `/run/battery`
  its own type, `battery_run_t`, and allows `dnsmasq_t` to search the
  directory and read the files in it. Nothing else changes for any domain.
- **The paths pods on the Host read** (HI-065). A pod's containers, the
  Exec Agent's among them, run as `container_t`, which may use only files
  labelled for containers. The module's file contexts
  (`selinux/battery.fc`) label the two paths with a type the base image's
  `container-selinux` already grants `container_t`, and no rule in the
  module names a container domain:

  | Path | Label | `container_t` may |
  |------|-------|-------------------|
  | `/run/battery/host.env` | `container_ro_file_t:s0` | read |
  | `/run/battery/not-ready.d` and the reasons in it | `container_ro_file_t:s0` | read |

  Everything else under `/run/battery` stays `battery_run_t`, and nothing
  else is relabelled. `/etc/battery`, with the Host configuration file in
  it, keeps the base policy's `etc_t`.
- **The Exec Agent's certificate directory** (HI-072). The Exec Agent runs
  as `container_t` at a level its pod is given, and writes
  `/etc/battery/flintlockd` through a hostPath mount. The same file
  contexts label the directory and everything in it `container_file_t:s0`,
  which `container_t` may read and write; `flintlockd` runs as
  `unconfined_service_t` and reads it whatever its label. A file the agent
  creates there inherits the type from the directory but takes its pod's
  level, and a later pod of the DaemonSet, at other categories, could
  neither read it nor rename over it. So `flintlockd-certs` labels the
  directory and every file in it again at boot and whenever `tls.crt` or
  `client-ca.crt` changes, which returns them to `s0`. The key is then
  readable by any container given the directory by a hostPath mount and
  running as `EXEC_AGENT_UID`, which the file mode (0600) and the
  directory's (0700) still require. A fixed `seLinuxOptions.level` on the
  Exec Agent's DaemonSet would make the relabelling unnecessary but not
  wrong.

### Why these labels

The read-only paths are `container_ro_file_t` rather than
`container_file_t`: the policy lets containers read that type and write
none of it, so a compromised container still cannot forge the bridge
gateway address or clear a not ready reason even if its mount were
writable.

The level is `s0`, with no categories. Every container runs at `s0` plus
two categories of its own, and the MCS constraint lets a process use a file
only when its level dominates the file's; `s0` is dominated by every level,
so each container can read the paths whatever categories it gets. The
tradeoff is that `s0` is not private to one pod: any other container given
these paths by a hostPath mount could read them. hostPath mounts are for
privileged workloads in a cluster that enforces Pod Security, and the files
hold nothing secret.

### Keeping the labels

`/run` is a tmpfs, rebuilt at every boot, and a rename keeps the label of
the file renamed. So the labels come from the module's file contexts and
are applied where each path is made:

- systemd-tmpfiles labels `/run/battery/not-ready.d` when it creates it,
  and `z` lines in `tmpfiles.d/battery.conf` restore both labels, without
  recursing, whenever tmpfiles runs and the paths exist.
- `battery-host-config` writes `host.env` to a temporary file in
  `/run/battery`, which would be `battery_run_t`, and runs `restorecon -F`
  on it before the rename; the file contexts give the temporary names
  `.host.env.*` the same label. A not ready reason is labelled the same way
  before it takes its name, and the directory when a unit has to make it.

`restorecon` runs only where SELinux is enabled, so the check stage and
`make host-image-lint` run the same scripts. `check-selinux-contexts.sh`
checks the ordering with a stand-in for `restorecon`, and in the check stage
looks every path up with `matchpathcon` in the policy the module was
installed into.

### What has been verified

In flintlock-runner, the module compiled against the base image's policy
and installed with `semodule -n`, and `matchpathcon` in that policy gave
each path above its label. `sesearch` on that policy shows `container_t` (a
`svirt_sandbox_domain` and an `mcs_constrained_type`) may read
`container_ro_file_t` and not write it, and may read and write
`container_file_t`. None of it has been enforced on a booted Host: the
first boot on real hardware should be followed by
`ausearch -m avc -ts boot`, and anything it shows is a bug in this section.

### Pods run confined (HI-066)

containerd labels a pod only when its CRI plugin is told to; without it
every container the kubelet starts is unlabelled and runs unconfined, and an
enforcing Host enforces nothing between its pods and itself.
`/etc/containerd/config.toml` sets `enable_selinux = true` in
`[plugins."io.containerd.grpc.v1.cri"]`, the `PluginConfig.EnableSelinux`
of containerd v1.7.22's CRI plugin (`pkg/cri/config/config.go`; false by
default, when the plugin calls `selinux.SetDisabled()`). With it, every
container of every pod the kubelet starts on a Host runs in the domain the
base policy's `lxc_contexts` names, `container_t`, at a level with
categories of its own, unless the pod's `seLinuxOptions` ask for another
level or type. That covers the Exec Agent, the CNI and kube-proxy
DaemonSets, and anything else scheduled onto a Host. The one exception is
containerd's own: a container with `privileged: true` gets no label
(`pkg/cri/sbserver/container_create.go`) and runs unconfined, as a
privileged container is meant to. kube-proxy and most CNI agents are
privileged; the Exec Agent is not.

The MicroVMs are not affected. `flintlockd` v0.15.2 talks to containerd's
own API for its content store, images, snapshots and leases only; it never
creates a containerd container or task (`NewContainer` appears only in its
client interface and mock), and it starts Firecracker and Cloud Hypervisor
itself as child processes (`process.DetachedStart` in
`infrastructure/microvm/firecracker/create.go`, `exec.Command` in
`infrastructure/microvm/cloudhypervisor/create.go`). `enable_selinux` is
read by the CRI plugin alone, so the hypervisor processes keep the
`unconfined_service_t` of `flintlockd.service` described above.

The check stage runs `containerd config dump` in the image, which merges the
file over containerd's defaults without starting it, and requires the CRI
plugin's `enable_selinux` to be `true`; it also checks that the base
policy's container process context is `container_t`.

Not verified until a Host boots: that containerd starts with the setting on
an enforcing kernel, that pods' containers show `container_t` with
categories of their own (`ps -eZ`), and that no AVC denials follow
(`ausearch -m avc -ts boot`). An unprivileged CNI or storage DaemonSet that
touches host paths is the most likely thing to need its own
`seLinuxOptions`.

## Networking notes

- Guest networking is flintlock's documented bridge option: TAP devices on
  `flbr0`, with DHCP and NAT, and no libvirt. The image adds isolation. The
  bridge is the default rather than macvtap, because macvtap does not work
  on AWS, whose network drops unknown MAC addresses, and it would put guests
  on the cloud network.
- The default guest subnet is `10.220.0.0/16`. It clashes with neither
  AWS's default VPC range, `172.31.0.0/16`, nor flintlock's documented
  `192.168.100.0/24`, and it stays clear of kubeadm's Service range, and the
  default pod ranges of Flannel, Calico, k3s and Docker. Override it
  wherever the cluster uses that range.
- A guest reaches the outside, DHCP and DNS on the gateway, and nothing of
  the cluster (HI-075 to HI-077). The forward chain lets a guest's traffic
  out of the primary interface only, so a pod on the Host and the tunnels of
  an overlay network are dropped whatever their address. It drops a
  protected destination twice: by the address in the packet, and by the
  address the guest asked for before kube-proxy's DNAT
  (`ct original ip daddr`). So list the cluster's Service range in
  `PROTECTED_CIDRS` next to its node and pod ranges: a Service is then
  dropped wherever its pods are. The input chain accepts a guest only for
  DHCP and DNS on the gateway, and on the broadcast address for DHCP.
- `GATEWAY_SERVICE_PORTS` adds TCP ports on the gateway to that list
  (HI-078). The input chain drops those ports on every interface but the
  bridge and loopback, so a pod on the Host and anything off it cannot
  reach them (HI-079). `GATEWAY_SERVICE_UIDS` names the user ids that serve
  them. The output chain drops their traffic to the metadata service, v4
  and v6, and over loopback to the control ports and `flintlockd`'s port
  (HI-080). Both are empty by default, and then neither adds a rule.
  `check-gateway-service-egress.sh` checks the output chain's rules, and
  connects as a listed id where it can make namespaces.
- `check-guest-isolation.sh` checks the rules, and where it can make
  unprivileged user and network namespaces with bridges, veth and nftables
  (a developer machine, not the image build), it loads them into a stand-in
  Host. A stand-in guest then reaches an outside address, and DNS and a
  listed gateway service port on the gateway, and does not reach an
  unlisted port on the gateway, the Host's
  primary address, a pod behind its own interface on the Host, or a
  protected Service address that a DNAT rule sends outside every protected
  range.
- `br_netfilter` is loaded because kubeadm's preflight wants it. With it,
  bridged guest-to-guest frames traverse the forward chain, whose last rule
  drops traffic into the bridge that is not a reply, so guests cannot reach
  each other. That is intended.
- `flintlockd` listens on the Host's internal address only, not on
  loopback or the bridge gateway. A guest's traffic to any address of the
  Host arrives on the bridge and is dropped there, so it cannot reach
  `flintlockd` even if something (kube-proxy can) sets `route_localnet`.

## Publishing an AMI

The Cluster API host pools in [config/capi](../config/capi/README.md) boot
the image from an AMI. `publish-ami.sh` makes one from a published image
(HI-009):

```sh
make host-image-ami HOST_IMG=ghcr.io/phoban01/battery-operator/host-image:v0.2.0 \
  HOST_IMAGE_AMI_ARGS="--bucket my-import-bucket --region eu-west-1"
```

The script pulls the image into root's podman storage. It then runs
`bootc-image-builder --type ami`, pinned by digest. The builder uploads the
disk image to the S3 bucket, imports it as a snapshot through the
`vmimport` service role, and registers the AMI. The script then tags the
AMI:

| Tag | Value |
|-----|-------|
| `battery.liquidmetal-x.dev/image-digest` | The digest of the container image |
| `battery.liquidmetal-x.dev/kubernetes-version` | The image's Kubernetes version, from its OCI label. A pool that boots the AMI sets this as its `kubernetesVersion` |
| `battery.liquidmetal-x.dev/host-image` | `true` |

It needs podman, sudo, the `aws` command line and AWS credentials, from
the environment or `~/.aws`. You create the bucket and the `vmimport` role
once. The image must be in a registry, so that it has a digest; an image
built only locally has none, and the script refuses it. `--dry-run` prints
the commands and runs nothing.

Nothing else in this directory needs AWS, and nothing runs the script
unless you ask: no other make target and no CI workflow calls it.
`lint.sh` checks both, and checks a dry run.

The script has not run against AWS: the project has no AWS account yet. It
comes from flintlock-runner, which had none either.

The AMI is made outside the Dagger module on purpose. bootc-image-builder
runs as a privileged podman container that reads root's container storage,
and it needs AWS credentials. Neither fits a Dagger function or a CI job
that runs without cloud credentials (HI-007).

## Upgrading

Until in-place upgrade is specified, an upgrade is a new machine from a new
image. The image is nevertheless built for the in-place path: automatic
updates are masked, all state is under `/var`, which `bootc upgrade` does
not touch, and an existing thin pool is kept, so a drained Host that runs
`bootc upgrade` and reboots comes back with its pulled images. The
`host-image` label changes with the digest, which is how a rollout can be
observed.
