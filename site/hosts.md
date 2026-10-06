---
layout: default
title: Hosts with Cluster API
---

[Home]({{ "/" | relative_url }}) · [Install]({{ "/install.html" | relative_url }}) · **Hosts**

# Hosts with Cluster API

This page makes Hosts on AWS with Cluster API, from the published Host
Image. At the end, battery-operator runs in the workload cluster, and a
Pool keeps MicroVMs warm on the Hosts.

The steps use these tools: `kind`, `kubectl`, `clusterctl`,
`clusterawsadm`, `kustomize`, `podman` and the `aws` command line. The
examples use release `v0.3.2` and the AWS region `eu-west-1`. Use the
release and region you want.

The templates and the AMI script have not run against AWS yet. CI renders
the templates and checks them against the Cluster API and CAPA schemas. A
local proof boots the Host Image with the same kubeadm configuration
([hack/kubeadm-host](https://github.com/phoban01/battery-operator/blob/v0.3.2/hack/kubeadm-host/README.md)).
If a step fails for you, please
[open an issue](https://github.com/phoban01/battery-operator/issues).

## What a Host is

A Host is a Kubernetes Node that runs MicroVMs. battery creates the
MicroVMs through the Host's `flintlockd`, and the Exec Agent on the Host
checks it and reports on its Node. The Operator gives a Node to battery as a
Host only while that report says ready. The
[Host prerequisites](https://github.com/phoban01/battery-operator/blob/v0.3.2/docs/host-prerequisites.md)
say what a Node needs.

The Host Image is a bootc image that a Node boots from to become a Host. It
gives the Host:

- containerd with a thin pool, which it makes on the instance-store disk at
  first boot;
- Firecracker, Cloud Hypervisor and `flintlockd`, which serves over mutual
  TLS on port 9090 with certificates from the Exec Agent;
- the guest bridge, with DHCP, NAT and a firewall between the MicroVMs and
  the cluster;
- the kubelet and `kubeadm`, cloud-init, and the Host label
  `battery.liquidmetal-x.dev/host=true`.

Each release publishes it for x86_64 and arm64:

```
ghcr.io/phoban01/battery-operator/host-image:<version>
```

The [Host Image README](https://github.com/phoban01/battery-operator/blob/v0.3.2/hostimage/README.md)
has the detail.

## The steps

1. Make an AMI from the Host Image.
2. Make a management cluster with Cluster API and CAPA.
3. Make a workload cluster, with a CNI and the AWS cloud controller
   manager.
4. Make a security group that lets battery reach `flintlockd`.
5. Fill in a Host pool, and apply it to the management cluster.
6. Check that the Hosts joined.
7. Install battery-operator in the workload cluster, and check that the
   Exec Agent reports the Hosts ready.
8. Make a Pool.

## Make an AMI

The Host pools boot the Host Image from an AMI in the region of the
cluster. The AMI is x86_64 only, so the Hosts are x86_64 instances.

`make host-image-ami` makes the AMI with bootc-image-builder. You need:

- a Linux machine with podman and sudo;
- the `aws` command line, and AWS credentials in the environment or in
  `~/.aws`;
- an S3 bucket in the region, and the `vmimport` service role, for the
  snapshot import. You make both once.

Get a checkout of the release, and run the target:

```sh
git clone --branch v0.3.2 --depth 1 https://github.com/phoban01/battery-operator.git
cd battery-operator
make host-image-ami HOST_IMG=ghcr.io/phoban01/battery-operator/host-image:v0.3.2 \
  HOST_IMAGE_AMI_ARGS="--bucket my-import-bucket --region eu-west-1"
```

The last line of the output is the AMI id, its name, the image digest and
the Kubernetes version, for example:

```
ami-0123456789abcdef0 battery-host-v1.36.3-<first 12 digits of the digest> sha256:<digest> v1.36.3
```

The script tags the AMI with the image digest and the Kubernetes version.
Keep the AMI id and the version for the Host pool. To read the tags later:

```sh
aws ec2 describe-images --region eu-west-1 --image-ids ami-0123456789abcdef0 \
  --query 'Images[0].Tags'
```

`--dry-run` in `HOST_IMAGE_AMI_ARGS` prints the commands and runs nothing.
[Publishing an AMI](https://github.com/phoban01/battery-operator/blob/v0.3.2/hostimage/README.md#publishing-an-ami)
has the detail.

## Make a management cluster

The management cluster runs Cluster API and its AWS provider, CAPA. The
Host pool templates use the `v1beta2` APIs, and the repository checks them
against Cluster API v1.13.6 and CAPA v2.13.0. Install those versions.

For a trial, a kind cluster is enough:

```sh
kind create cluster --name capi-management
```

Get `clusterctl` v1.13.6 from the
[Cluster API releases](https://github.com/kubernetes-sigs/cluster-api/releases/tag/v1.13.6),
and `clusterawsadm` v2.13.0 from the
[CAPA releases](https://github.com/kubernetes-sigs/cluster-api-provider-aws/releases/tag/v2.13.0).
Then, with your AWS credentials in the environment:

```sh
export AWS_REGION=eu-west-1
# Once per AWS account: the IAM roles and policies that CAPA uses.
clusterawsadm bootstrap iam create-cloudformation-stack
export AWS_B64ENCODED_CREDENTIALS=$(clusterawsadm bootstrap credentials encode-as-profile)

clusterctl init \
  --core cluster-api:v1.13.6 \
  --bootstrap kubeadm:v1.13.6 \
  --control-plane kubeadm:v1.13.6 \
  --infrastructure aws:v2.13.0
```

The [Cluster API quick start](https://cluster-api.sigs.k8s.io/user/quick-start)
has more on each step.

## Make a workload cluster

The workload cluster runs battery-operator and the Hosts. Give it the
Kubernetes version of the Host Image, so that the Hosts' kubelet is not
newer than the control plane. The AMI's Kubernetes version tag has it, and
so does this table, with the AWS cloud controller manager for that version:

| Release | Kubernetes | AWS cloud controller manager |
|---------|------------|------------------------------|
| `v0.2.0`, `v0.2.1` | `v1.35.8` | `v1.35.2` |
| `v0.3.0` to `v0.3.2` | `v1.36.3` | `v1.36.1` |

The examples on this page use `v0.3.2`, so they use `v1.36.3` and
`v1.36.1`. For another release, use the versions of its row. The Host
Image first came with `v0.2.0`.

It needs ordinary worker Nodes too. The Hosts have a taint that keeps
ordinary pods off them, so the Operator and cert-manager run on the
workers.

CAPA's default template gives a kubeadm control plane, one MachineDeployment
of workers, and a ClusterResourceSet that installs the AWS cloud controller
manager. The Hosts join with `--cloud-provider=external`, so they need the
cloud controller manager to initialize their Nodes. Set its version from
the table:

```sh
export AWS_REGION=eu-west-1
export AWS_SSH_KEY_NAME=""        # or the name of an EC2 key pair
export AWS_CONTROL_PLANE_MACHINE_TYPE=t3.large
export AWS_NODE_MACHINE_TYPE=t3.large
export KUBERNETES_AWS_CCM_VERSION=v1.36.1

clusterctl generate cluster battery \
  --infrastructure aws:v2.13.0 \
  --kubernetes-version v1.36.3 \
  --control-plane-machine-count 1 \
  --worker-machine-count 2 \
  > battery-cluster.yaml
kubectl apply -f battery-cluster.yaml
```

CAPA boots the control plane and the workers from public AMIs that it
finds by Kubernetes version. To see if there is one for your version and
region:

```sh
clusterawsadm ami list --kubernetes-version v1.36.3 --region eu-west-1
```

If there is none, build one with
[image-builder](https://image-builder.sigs.k8s.io), and set `ami.id` in the
two AWSMachineTemplates of `battery-cluster.yaml`.

Wait for the control plane, then get the workload cluster's kubeconfig:

```sh
clusterctl describe cluster battery
clusterctl get kubeconfig battery > battery.kubeconfig
```

Install a CNI. This page uses Calico, as the Cluster API quick start does:

```sh
kubectl --kubeconfig battery.kubeconfig apply \
  -f https://raw.githubusercontent.com/projectcalico/calico/v3.31.2/manifests/calico.yaml
kubectl --kubeconfig battery.kubeconfig get nodes
```

Calico v3.32.2 works too.

The Nodes go `Ready` when the CNI runs. The CNI and kube-proxy DaemonSets
must tolerate the Host taint, `battery.liquidmetal-x.dev/host=true:NoSchedule`.
Calico's and kube-proxy's tolerate every `NoSchedule` taint. On a Host,
SELinux is enforcing: a CNI pod that is not privileged and writes to host
paths needs its own `seLinuxOptions`. Calico's pods are privileged, so
they need nothing more. See
[Pods run confined](https://github.com/phoban01/battery-operator/blob/v0.3.2/hostimage/README.md#pods-run-confined-hi-066).

Calico's manifest encapsulates pod traffic between Nodes in IP-in-IP (IP
protocol 4) and peers over BGP (TCP port 179). CAPA's node security group
admits both. Calico takes a Node's address from the first interface it
finds that is not on its exclude list. A Host also has its guest bridge,
`virbr-battery`. The list has `^virbr.*`, so Calico skips the bridge and takes
the Host's own address. Calico's default needs no change.

Note the cluster's address ranges. You need them for the security group
and the Host pool. With CAPA's defaults they are:

| Range | Default | Where it is set |
|-------|---------|-----------------|
| Nodes (the VPC) | `10.0.0.0/16` | the AWSCluster's `spec.network.vpc.cidrBlock` |
| Pods | `192.168.0.0/16` | the Cluster's `spec.clusterNetwork.pods.cidrBlocks` |
| Services | `10.96.0.0/12` | kubeadm's default, when the Cluster sets no `services` |

## Make the security group

battery runs in the Operator's pod and calls each Host's `flintlockd` on
TCP port 9090. CAPA's node security group does not open that port. Make a
security group in the cluster's VPC that does.

The source is the Operator's pod network. If the CNI masquerades pod
traffic to other Nodes, the traffic comes from the Node's address instead,
so admit the node range too. Calico's default IP pool masquerades traffic
to addresses outside the pool, and a Host's address is outside it.

```sh
vpc=$(kubectl get awscluster battery -o jsonpath='{.spec.network.vpc.id}')
sg=$(aws ec2 create-security-group --region eu-west-1 --vpc-id "$vpc" \
  --group-name battery-flintlockd \
  --description "flintlockd from the battery-operator pods" \
  --query GroupId --output text)
aws ec2 authorize-security-group-ingress --region eu-west-1 --group-id "$sg" \
  --protocol tcp --port 9090 --cidr 192.168.0.0/16
aws ec2 authorize-security-group-ingress --region eu-west-1 --group-id "$sg" \
  --protocol tcp --port 9090 --cidr 10.0.0.0/16
echo "$sg"
```

The Host's own firewall admits only `FLINTLOCKD_CLIENT_CIDRS` from the
Host pool, below, so set the same ranges there.
[The security group](https://github.com/phoban01/battery-operator/blob/v0.3.2/config/capi/README.md#the-security-group)
has the detail.

## Fill in a Host pool

A Host pool is a set of Hosts with one instance type and one AMI. It is not
a `Pool`: a `Pool` holds MicroVMs, and a Host pool holds the machines they
run on.

The templates are in `config/capi` of the checkout of the release. Each
pool is a directory under `config/capi/pools/`, with its settings in a
ConfigMap in `host-pool.yaml`. A kustomize Component writes the settings
into the pool's MachineDeployment, AWSMachineTemplate,
KubeadmConfigTemplate and MachineHealthCheck. The ConfigMap is
`local-config`, so it never reaches a cluster. Put every value in
`host-pool.yaml`, and do not change `host-pool/` or `host-pool-params/`.

Pick a private subnet of the cluster for the Hosts:

```sh
kubectl get awscluster battery -o jsonpath='{range .spec.network.subnets[*]}{.resourceID}{"\t"}{.availabilityZone}{"\t"}{.isPublic}{"\n"}{end}'
```

Then, in the checkout:

1. In `config/capi/kustomization.yaml`, set `namespace` to the namespace of
   the `Cluster` object, here `default`.
2. In `config/capi/pools/example/host-pool.yaml`, replace every value that
   says `REPLACE`, and check the others. For the cluster above:

   ```yaml
   data:
     name: battery-hosts-example
     clusterName: battery
     replicas: "1"
     # A metal instance with an instance-store disk.
     instanceType: m6id.metal
     amiID: ami-0123456789abcdef0
     subnetID: subnet-0123456789abcdef0
     additionalSecurityGroups:
       - id: sg-0123456789abcdef0
     rootVolumeGiB: "100"
     # The AMI's battery.liquidmetal-x.dev/kubernetes-version tag.
     kubernetesVersion: v1.36.3
     nodeDrainTimeoutSeconds: "4500"
     unhealthySeconds: "300"
     host.conf: |
       GUEST_SUBNET=10.220.0.0/16
       PROTECTED_CIDRS=10.0.0.0/16,192.168.0.0/16,10.96.0.0/12
       FLINTLOCKD_CLIENT_CIDRS=192.168.0.0/16,10.0.0.0/16
       HOST_RESERVE_VCPU=2
       HOST_RESERVE_MEMORY_MB=4096
   ```

`host.conf` is the Host configuration file. Set these in every pool:

- `FLINTLOCKD_CLIENT_CIDRS`: the ranges that may reach `flintlockd`, the
  same as the security group's.
- `PROTECTED_CIDRS`: the node, pod and Service ranges, and any other range
  a MicroVM must not reach.
- `GUEST_SUBNET`: the MicroVMs' subnet. Keep it clear of the ranges above.

Use only the keys that the
[Host configuration file](https://github.com/phoban01/battery-operator/blob/v0.3.2/hostimage/README.md#host-configuration-file)
section names. The Host skips any other key without an error.

Look at what the pool renders, then apply it to the management cluster:

```sh
kustomize build config/capi
kustomize build config/capi | kubectl apply -f -
```

Use kustomize v5. `make kustomize` installs the version the repository
pins into `bin/`.

To change the number of Hosts later, change `replicas` and apply again. To
add a pool, copy `pools/example` to `pools/<name>`, give its `name` and its
ConfigMap names of their own, and add it to `resources` in
`config/capi/kustomization.yaml`.
[Cluster API host pools](https://github.com/phoban01/battery-operator/blob/v0.3.2/config/capi/README.md)
says what each key sets, and what the templates set for you.

## Check that the Hosts joined

In the management cluster, the pool's Machines go `Running`:

```sh
kubectl get machinedeployment battery-hosts-example
kubectl get machines -l battery.liquidmetal-x.dev/host-pool=battery-hosts-example
```

A metal instance takes some minutes to boot. The first boot also makes the
thin pool and pulls images. The MachineHealthCheck gives a Host 20 minutes
before it replaces the Machine.

In the workload cluster, each Host is a Node with the Host label and the
Host taint. The Host Image adds labels with its digest and the Firecracker
and Cloud Hypervisor versions:

```sh
kubectl --kubeconfig battery.kubeconfig get nodes -l battery.liquidmetal-x.dev/host=true \
  -L battery.liquidmetal-x.dev/host-image,battery.liquidmetal-x.dev/firecracker
kubectl --kubeconfig battery.kubeconfig get nodes -l battery.liquidmetal-x.dev/host=true \
  -o custom-columns='NAME:.metadata.name,TAINTS:.spec.taints[*].key,PROVIDER:.spec.providerID'
```

Each Host shows the taint `battery.liquidmetal-x.dev/host`, and a provider
id that starts with `aws://`. The cloud controller manager sets the
provider id. Until it does, the Node keeps the taint
`node.cloudprovider.kubernetes.io/uninitialized`.

`flintlockd` does not run yet. It starts when the Exec Agent writes its
certificates, and the Exec Agent comes with battery-operator.

## Install battery-operator

Install battery-operator in the workload cluster, with Flux or with
`kubectl`, as [Install]({{ "/install.html" | relative_url }}) says. Flux
deploys battery-operator only: the Host pools stay in the management
cluster, applied as above.

The Exec Agent runs on each Host, gets `flintlockd`'s certificates through
CertificateSigningRequests that the Operator signs, and reports on the
Host's Node:

```sh
kubectl --kubeconfig battery.kubeconfig -n battery-operator-system get pods -o wide \
  -l app.kubernetes.io/component=exec-agent
kubectl --kubeconfig battery.kubeconfig get node <node> -o jsonpath='{.metadata.annotations}'
```

| Annotation | Value |
|------------|-------|
| `battery.liquidmetal-x.dev/exec-agent-ready` | `true` when the Host is ready, else `false` |
| `battery.liquidmetal-x.dev/exec-agent-reason` | `Ready`, or the first missing prerequisite |
| `battery.liquidmetal-x.dev/exec-agent-message` | every missing prerequisite in words |
| `battery.liquidmetal-x.dev/flintlockd-address` | where battery reaches `flintlockd`: the Host's address, port 9090 |

When the report says ready, the Operator gives the Node to battery as a
Host. If it says `false`, the reason names the first missing prerequisite:

- `HostImageNotReady`: a unit of the Host Image failed, for example
  because the instance has no KVM or no free instance-store disk, or
  `host.conf` is not valid. The message says which. See
  [Not ready reasons](https://github.com/phoban01/battery-operator/blob/v0.3.2/hostimage/README.md#not-ready-reasons).
- `KVMUnavailable`: the instance type has no KVM. Use a metal instance.
- `ThinPoolMissing`: there is no thin pool.
- `FlintlockdNotReady`: the Exec Agent cannot reach `flintlockd` yet.

A Host that is not ready keeps a ready Node, so the MachineHealthCheck does
not replace it. Fix the cause in `host-pool.yaml`, apply it, and delete the
Machine so that Cluster API makes a new one.

## Make a Pool

A Pool places its MicroVMs on Hosts through its `placement.nodeSelector`.
Select the Host label. Then claim a MicroVM, as
[Make a Pool and claim a MicroVM]({{ "/install.html#make-a-pool-and-claim-a-microvm" | relative_url }})
says:

```yaml
  placement:
    nodeSelector:
      battery.liquidmetal-x.dev/host: "true"
```

`flintlockd` on the Hosts pulls the Pool's kernel and root filesystem
images, so the Hosts' subnet needs a way out to their registry, such as the
NAT gateway that CAPA makes for its private subnets.

## Remove the Hosts

Delete the Pools and claims first, so that battery deletes their MicroVMs.
Then delete the Host pool from the management cluster:

```sh
kustomize build config/capi | kubectl delete -f -
```

Cluster API drains each Host before it deletes the Machine. The Exec Agent
holds the drain open while claims are Bound on the Host, for up to an hour
by default.
