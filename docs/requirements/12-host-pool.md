# Host Pool Templates {#host-pool-templates}

This document specifies the Host Pool Templates: the Cluster API templates
in `config/capi/` that boot Hosts from the Host Image's AMI on AWS and join
them to a workload cluster with kubeadm
([ADR 0007](../adr/0007-reference-host-image-and-cluster-api.md)). They are
templates the user fills in per cluster: the image, the instance type, the
subnet, the pool size and the Host's settings. They go to the management
cluster, not to the workload cluster where the Operator runs. How to fill
them in is in `config/capi/README.md`.

The templates come from flintlock-runner's `deploy/capi`, and so do most of
these requirements. The table maps each one to the flintlock-runner
requirement it replaces, where there is one:

| Here | flintlock-runner |
|------|------------------|
| HP-001 | KF-001 |
| HP-004 | KF-004 |
| HP-005 | KF-005 |
| HP-010 | KF-003 |
| HP-021 | KF-002 |
| HP-030 | KF-112, for the Hosts |

The AMI the templates boot is made by HI-009, in `11-host-image.md`.

## Host pool {#host-pool}

- **HP-001** The Host Pool Templates SHALL define each Host pool as one
  Cluster API MachineDeployment with an AWSMachineTemplate of its own,
  which names a single instance type, an AMI by explicit id and a subnet by
  explicit id.
- **HP-002** The Host Pool Templates SHALL take a Host pool's name, cluster,
  size, Kubernetes version, instance type, AMI, subnet, health check
  interval, drain timeout and Host configuration file from one settings
  object per pool, which is never applied to a cluster.
- **HP-003** The Host Pool Templates SHALL default a Host pool's Kubernetes
  version to the version that the Host Image pins.
- **HP-004** The Host Pool Templates SHALL set no node drain timeout on a
  Host pool's Machines that is shorter than the Exec Agent's default drain
  timeout.
- **HP-005** The Host Pool Templates SHALL define a MachineHealthCheck for
  each Host pool that replaces a Machine whose Node stays not ready beyond
  the interval its host-pool.yaml sets.
- **HP-006** The Host Pool Templates SHALL attach to every Host of a pool the
  additional security groups that the pool's settings list by id.

The pool size is the MachineDeployment's replica count. One instance type
per pool keeps the capacity of a pool simple to reason about, and one AMI
by id means every Host of a pool boots the same image. An AMI found by
lookup could change under a running pool.

The Host Image carries the kubelet, so a Host pool's Kubernetes version has
to be the one in the AMI's Host Image. `hostimage/publish-ami.sh` tags each
AMI with it. HP-003 makes the template's default the version the Host Image
pins today; a pool that boots an older AMI sets the older version.

HP-004 exists because the Exec Agent holds a drain of its Host's Node open
while claims are Bound on it (EA-040), for at most its drain timeout, one
hour by default. If Cluster API gave up on the drain sooner, it would
delete the Machine under Bound claims.

HP-006 exists because battery reaches each Host's `flintlockd` on TCP port
9090 from the Operator's pod (ADR 0002), and CAPA's default node security
group does not open that port. One of the pool's groups admits it from the
Operator's pod network, the ranges of `FLINTLOCKD_CLIENT_CIDRS`, and from
the node range on a CNI that masquerades pod traffic between Nodes. The
Host's own firewall still admits only `FLINTLOCKD_CLIENT_CIDRS` (HI-069).

A known gap: the MachineHealthCheck of HP-005 watches the Host's own Node.
A Host that cannot run MicroVMs, for want of KVM (HI-011) or of a thin pool
device (HI-022), keeps a ready Node, and reports the reason through the
Exec Agent's Node report instead (EA-031, EA-033). The Inventory
Controller then takes the Host out of battery's Hosts, but the Machine stays
in the pool until someone replaces it.

## Host configuration {#host-configuration}

- **HP-010** The Host Pool Templates SHALL write the Host configuration file
  `/etc/battery/host.conf` through the files of each Host pool's
  KubeadmConfigTemplate, with only keys that the Host Image's defaults file
  names.
- **HP-011** The Host Pool Templates SHALL set `FLINTLOCKD_CLIENT_CIDRS` and
  `PROTECTED_CIDRS` in every Host configuration file they write.

cloud-init writes the file at first boot, in its network stage, and the
Host Image reads it after that (HI-050). A key the Host Image does not know
is skipped on the Host, so a misspelt key would pass unnoticed there;
HP-010 catches it before the Host boots. The defaults file is
`hostimage/rootfs/usr/share/battery/host.conf.defaults`.

HP-011 names the two settings that depend on the cluster the Host joins.
Without `FLINTLOCKD_CLIENT_CIDRS`, battery cannot reach the Host's
`flintlockd` (HI-069). Without `PROTECTED_CIDRS`, a MicroVM can reach the
cluster's nodes, pods and Services (HI-035, HI-076).

## Joining the cluster {#host-node}

- **HP-020** The Host Pool Templates SHALL register every Host's kubelet
  with the label `battery.liquidmetal-x.dev/host` set to `true`, through the
  node labels of the kubeadm join configuration.
- **HP-021** The Host Pool Templates SHALL join every Host with the taint
  `battery.liquidmetal-x.dev/host=true:NoSchedule`, so that only pods that
  tolerate it run on a Host's own Node.

The Host label is what the Exec Agent's DaemonSet selects (EA-004). The Host
Image sets it too (HI-074). The kubelet merges repeated `--node-labels`
flags, so the two agree, and a Host keeps the label when its image does not
set it.

A Host's Node offers pods only the Host reserve (HI-061), so ordinary pods
belong elsewhere. The taint is the cluster's policy, not the image's, which
is why the join configuration sets it. The Exec Agent tolerates every taint.

## AWS access {#host-access}

- **HP-030** The Host Pool Templates SHALL NOT give a Host an AWS instance
  profile, and SHALL NOT fetch a Host's bootstrap data from AWS Secrets
  Manager.
- **HP-031** The Host Pool Templates SHALL require session tokens for the
  instance metadata service on every Host, with a response hop limit of 1.

A Host runs other people's code in its MicroVMs, and host-network pods such
as the Exec Agent. With no instance profile, nothing on a Host can get AWS
credentials from the instance metadata service. The guests cannot reach
that service at all (HI-033). CAPA fetches bootstrap data from Secrets
Manager through the instance profile, so the templates pass it in the
user-data instead. It holds a short-lived kubeadm join token and the Host
configuration file, which holds no secret (HI-052).
