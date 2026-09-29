# Cluster API host pools

These templates make Hosts with [Cluster API](https://cluster-api.sigs.k8s.io)
on AWS. Each Host boots the [Host Image](../../hostimage/README.md) from an
AMI, and joins the workload cluster with kubeadm
([ADR 0007](../../docs/adr/0007-reference-host-image-and-cluster-api.md)).
Their requirements are in
[12-host-pool.md](../../docs/requirements/12-host-pool.md).

You fill in the templates once per cluster, and once per Host pool. A Host
pool is a set of Hosts with one instance type and one AMI. It is not a
`Pool`: a `Pool` holds MicroVMs, and a Host pool holds the machines they
run on.

## What you need

- A management cluster with Cluster API and the AWS provider (CAPA)
  installed. The templates use the `v1beta2` APIs. `make capi-check`
  validates them against Cluster API v1.13.6 and CAPA v2.13.0.
- A workload cluster that Cluster API manages, with a kubeadm control
  plane, a CNI and the AWS cloud controller manager. The Hosts join it with
  `--cloud-provider=external`.
- battery-operator in the workload cluster: `config/default` and
  `config/exec-agent`.
- The Host Image published as an AMI in the region of the cluster. See
  [Publishing an AMI](../../hostimage/README.md#publishing-an-ami).
- A subnet for the Hosts.
- A security group that lets battery reach `flintlockd` on each Host. See
  [The security group](#the-security-group).

## Layout

| Path | What it is |
|------|------------|
| `kustomization.yaml` | The entry point: the namespace, and the list of pools |
| `host-pool/` | The objects of one pool, with placeholders: MachineDeployment, AWSMachineTemplate, KubeadmConfigTemplate, MachineHealthCheck |
| `host-pool-params/` | A kustomize Component that writes a pool's settings into those objects |
| `pools/example/` | One pool: its `host-pool.yaml` and its kustomization. Copy it for each pool |

Do not change `host-pool/` or `host-pool-params/` to fill in a pool. Put
every value in the pool's `host-pool.yaml`.

## Fill in a pool

1. In `kustomization.yaml`, set `namespace` to the namespace of the
   workload cluster's `Cluster` object.
2. In `pools/example/host-pool.yaml`, replace every value that says
   `REPLACE`, and check the others:

   | Key | What it sets |
   |-----|--------------|
   | `name` | The name of the pool, and of each of its objects |
   | `clusterName` | The Cluster API `Cluster` the Hosts join |
   | `replicas` | The pool size: the number of Hosts |
   | `instanceType` | One EC2 instance type. A Host needs KVM, so a metal instance, and an instance-store disk for the thin pool |
   | `amiID` | The AMI of the Host Image, by id |
   | `subnetID` | The subnet of the Hosts, by id |
   | `additionalSecurityGroups` | A list of `id: sg-…` entries: security groups for the Hosts besides CAPA's own. See below |
   | `kubernetesVersion` | The Kubernetes version of that AMI: its `battery.liquidmetal-x.dev/kubernetes-version` tag |
   | `nodeDrainTimeoutSeconds` | How long Cluster API waits for a Host to drain. At least the Exec Agent's `--drain-timeout`, one hour by default |
   | `unhealthySeconds` | How long a Host's Node may stay not ready before Cluster API replaces the Machine |
   | `host.conf` | The Host's settings. See below |

3. Build and apply the objects to the management cluster:

   ```sh
   kustomize build config/capi | kubectl --context MANAGEMENT apply -f -
   ```

To add a pool, copy `pools/example` to `pools/<name>`. Change `name`, and
give the ConfigMap in `host-pool.yaml` a name of its own. Then add the
directory to `resources` in `kustomization.yaml`. To change the pool size
later, change `replicas` and apply again.

The ConfigMap in `host-pool.yaml` holds the settings only. Its
`config.kubernetes.io/local-config` annotation keeps it out of the output,
so it never reaches a cluster.

## The security group

battery runs in the Operator's pod and calls each Host's `flintlockd` on
TCP port 9090. CAPA's default node security group does not open that port.
So at least one group in `additionalSecurityGroups` needs this inbound
rule:

| Protocol | Port | Source |
|----------|------|--------|
| TCP | 9090 | The Operator's pod network: the same ranges as `FLINTLOCKD_CLIENT_CIDRS` |

If your CNI masquerades pod traffic between Nodes, the traffic arrives
from the source Node's address. Then add the node range as a source too,
and to `FLINTLOCKD_CLIENT_CIDRS`. The Host's own firewall still admits only
`FLINTLOCKD_CLIENT_CIDRS`.

`additionalSecurityGroups` is a list in `host-pool.yaml`, not a string.
The settings object is never applied, so kustomize copies the list into
the AWSMachineTemplate as it is.

## The Host's settings

`host.conf` is the Host configuration file, `/etc/battery/host.conf`. The
KubeadmConfigTemplate writes it through its `files`, and cloud-init writes
it on the Host at first boot. The Host Image reads it after that, before
`flintlockd` starts. The keys and their defaults are in
[the Host Image README](../../hostimage/README.md#host-configuration-file).
A key you leave out takes its default. Use only keys from that list:
`make test` fails on any other key, because the Host skips it without an
error.

Set these two in every pool. They depend on the cluster, so they have no
useful default:

- `FLINTLOCKD_CLIENT_CIDRS`: the pod network of the Operator, where battery
  runs. Only these addresses may reach `flintlockd` from off the Host. If
  it is empty, battery cannot reach the Host. If your CNI masquerades pod
  traffic to other Nodes, add the node CIDR too.
- `PROTECTED_CIDRS`: the cluster's node, pod and Service ranges, and any
  other range a MicroVM must not reach.

Also check `GUEST_SUBNET`, the subnet of the MicroVMs on each Host. Keep it
clear of the ranges above. Its default is `10.220.0.0/16`.

A consumer that runs services for its MicroVMs on the bridge gateway sets
`GATEWAY_SERVICE_PORTS` and `GATEWAY_SERVICE_UIDS` too. The example has
them commented out. Left out, guests reach only DHCP and DNS on the
gateway.

The file holds settings, never secrets. The bootstrap data goes to the
instance as user-data, not through AWS Secrets Manager, because a Host has
no instance profile.

## What the templates set for you

- The Host label `battery.liquidmetal-x.dev/host=true`, as a kubelet node
  label. The Exec Agent runs only on Nodes with this label. The Host Image
  sets it too.
- The taint `battery.liquidmetal-x.dev/host=true:NoSchedule`, which keeps
  ordinary pods off the Hosts. The Exec Agent tolerates it. Your CNI and
  kube-proxy DaemonSets must tolerate it too; most tolerate every taint.
- `--cloud-provider=external`, and the Node name from the instance's
  private DNS name, as CAPA expects.
- No instance profile, no SSH key, and IMDSv2 only, with a hop limit of 1.
- A MachineHealthCheck that replaces a Machine whose Node stays not ready,
  and stops when more than 40% of the pool is unhealthy.
- A first boot timeout of 20 minutes, because the first boot makes the
  thin pool and pulls images.

A Host that cannot run MicroVMs, for example without KVM, keeps a ready
Node. The Exec Agent reports the reason on the Node, and the Inventory
Controller does not give the Host to battery. The MachineHealthCheck does
not see this, so replace such a Machine by hand.

## Checks

- `make test` runs `internal/manifests/capi_test.go`. It renders
  `config/capi` and two filled-in pools in
  `internal/manifests/testdata/capi`, and checks them against the
  requirements.
- `make capi-check` renders the same, and validates every object against
  the CRDs of Cluster API and CAPA, strictly: an unknown field fails. It
  downloads the CRDs from the pinned releases once, and checks their
  checksums. CI runs it as `dagger call capi-check`.

Neither check applies the templates to a cluster, or runs anything on AWS.
