---
layout: default
title: Install battery-operator
---

[Home]({{ "/" | relative_url }}) · **Install** · Hosts (coming with
[#192](https://github.com/phoban01/battery-operator/issues/192))

# Install battery-operator

This page installs battery-operator from a release, with Flux or with
`kubectl`. Then it makes a Pool and claims a MicroVM from it.

## What a release gives you

Each release publishes the Manifests as an OCI artifact:

```
ghcr.io/phoban01/battery-operator/manifests:<version>
```

The artifact is also tagged with the full SHA of the release's commit. It
holds one file, `install.yaml`. That file holds:

- the CRDs for `Pool` and `MicroVMClaim`;
- the Operator's Deployment, with battery's `poolmgrd` as a sidecar;
- the Exec Agent's DaemonSet, which runs on each Host;
- their RBAC, the Exec Agent's admission policy, and battery's volume;
- cert-manager Issuers and Certificates for the Operator's CAs and
  battery's client certificate.

Everything goes in the namespace `battery-operator-system`. The images are
pinned to the release: the Operator's and the Exec Agent's by tag and
digest, and battery's by the battery version the Operator is built for. You
do not set any image yourself.

The artifact has the media types that `flux push artifact` gives, so Flux
pulls it with no `layerSelector`.

## Before you start

You need:

- Kubernetes 1.30 or later. The Exec Agent's admission policy uses
  `admissionregistration.k8s.io/v1`. CI tests on 1.34.
- [cert-manager](https://cert-manager.io). The Manifests hold cert-manager
  objects, so cert-manager and its CRDs must be ready first. CI tests with
  cert-manager v1.20.2.
- Flux v2.6 or later, for the Flux path. That is the first release with
  `source.toolkit.fluxcd.io/v1` `OCIRepository`.
- One or more Hosts: Nodes that run `flintlockd` and have KVM. The
  [Host prerequisites](https://github.com/phoban01/battery-operator/blob/main/docs/host-prerequisites.md)
  say what a Host needs. The
  [Host Image](https://github.com/phoban01/battery-operator/tree/main/hostimage)
  is one way to build a Host. A page about Hosts comes with
  [#192](https://github.com/phoban01/battery-operator/issues/192).

You can install the Operator before you have Hosts. It runs, and it waits
for Hosts.

## Install with Flux

The examples use `v0.1.0`. Use the release you want from the
[releases page](https://github.com/phoban01/battery-operator/releases).

### cert-manager

battery-operator must wait for cert-manager, so cert-manager needs a Flux
`Kustomization` of its own. A `Kustomization` can depend only on another
`Kustomization`. If you install cert-manager with a `HelmRelease`, put the
`HelmRelease` in a `Kustomization`, and set `wait: true` on it.

For example, in the Git repository that Flux syncs, add
`infrastructure/cert-manager/cert-manager.yaml`:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: cert-manager
---
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: cert-manager
  namespace: cert-manager
spec:
  interval: 1h
  url: oci://quay.io/jetstack/charts/cert-manager
  ref:
    tag: v1.20.2
  layerSelector:
    mediaType: application/vnd.cncf.helm.chart.content.v1.tar+gzip
    operation: copy
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: cert-manager
  namespace: cert-manager
spec:
  interval: 1h
  chartRef:
    kind: OCIRepository
    name: cert-manager
  values:
    crds:
      enabled: true
      keep: true
```

Then add the `Kustomization` that applies it, next to Flux's own, for
example `clusters/<cluster>/cert-manager.yaml`:

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: cert-manager
  namespace: flux-system
spec:
  interval: 1h
  path: ./infrastructure/cert-manager
  prune: true
  # Ready only when cert-manager's Deployments are ready.
  wait: true
  timeout: 5m
  sourceRef:
    kind: GitRepository
    name: flux-system
```

If cert-manager is already in the cluster from another `Kustomization`,
use that `Kustomization`'s name in `dependsOn` below.

### battery-operator

Add `clusters/<cluster>/battery-operator.yaml`:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: battery-operator
  namespace: flux-system
spec:
  interval: 10m
  url: oci://ghcr.io/phoban01/battery-operator/manifests
  ref:
    tag: v0.1.0
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: battery-operator
  namespace: flux-system
spec:
  interval: 1h
  retryInterval: 2m
  timeout: 5m
  sourceRef:
    kind: OCIRepository
    name: battery-operator
  path: ./
  # Delete what a later release no longer has.
  prune: true
  # Ready only when every object is ready: the Operator's Deployment, the
  # Exec Agent's DaemonSet, the Certificates and the CRDs.
  wait: true
  dependsOn:
    - name: cert-manager
```

Commit and push. Flux applies cert-manager first, then battery-operator.

Notes:

- `ref.tag` pins one release. To take patch releases as they come, use
  `ref.semver` in its place, for example `semver: "0.1.x"`. Flux then
  ignores the commit SHA tags, because they are not versions.
- `interval` on the `OCIRepository` is how often Flux looks for a new
  artifact. `interval` on the `Kustomization` is how often Flux corrects
  drift in the cluster.
- Do not set `targetNamespace`. The Manifests set their own namespace, and
  they hold cluster-wide objects.
- With `wait: true`, Flux ignores `healthChecks`. To wait for the Operator
  alone, set `wait: false` and give the checks:

  ```yaml
    wait: false
    healthChecks:
      - apiVersion: apps/v1
        kind: Deployment
        name: battery-operator-controller-manager
        namespace: battery-operator-system
      - apiVersion: apps/v1
        kind: DaemonSet
        name: battery-operator-exec-agent
        namespace: battery-operator-system
  ```

- To install without a Git repository, apply the same two objects with
  `kubectl apply -f`, or create them with the flux CLI:

  ```sh
  flux create source oci battery-operator \
    --url=oci://ghcr.io/phoban01/battery-operator/manifests --tag=v0.1.0 --interval=10m
  flux create kustomization battery-operator \
    --source=OCIRepository/battery-operator --path=./ --prune=true --wait=true \
    --depends-on=cert-manager --interval=1h
  ```

### Check that it is up

```sh
flux get sources oci battery-operator
flux get kustomizations battery-operator
```

The source shows the revision it pulled, for example
`v0.1.0@sha256:...`. The `Kustomization` is `Ready` when every object is
ready. If it is not, `flux events --for Kustomization/battery-operator`
says why.

Then check the objects themselves:

```sh
kubectl -n battery-operator-system rollout status deployment/battery-operator-controller-manager
kubectl -n battery-operator-system get pods
kubectl -n battery-operator-system get certificates
kubectl get crd pools.battery.liquidmetal-x.dev microvmclaims.battery.liquidmetal-x.dev
```

The Operator's pod has two containers, `manager` and `battery`, and both
are ready. Each Certificate is `Ready`.

## Install with kubectl

Get `install.yaml` from the artifact. The flux CLI does this without a
cluster:

```sh
flux pull artifact oci://ghcr.io/phoban01/battery-operator/manifests:v0.1.0 \
  --output ./battery-operator
```

Or use [crane](https://github.com/google/go-containerregistry/tree/main/cmd/crane)
and `jq`:

```sh
ref=ghcr.io/phoban01/battery-operator/manifests
layer=$(crane manifest "$ref:v0.1.0" | jq -r '.layers[0].digest')
mkdir -p battery-operator
crane blob "$ref@$layer" | tar -xzf - -C battery-operator
```

Install cert-manager, and wait for it:

```sh
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.20.2/cert-manager.yaml
kubectl -n cert-manager rollout status deployment/cert-manager-webhook
```

Apply the Manifests:

```sh
kubectl apply --server-side -f battery-operator/install.yaml
```

cert-manager's webhook can refuse the Certificates for a short time after
it starts. If the apply fails on a Certificate or an Issuer, run it again.
Then check that it is up, as above, from `kubectl ... rollout status`.

To upgrade, pull the new release and apply it the same way. To remove
objects that a new release no longer has, delete them yourself, or use Flux.

## Make Nodes into Hosts

The Exec Agent runs only on Nodes with the label
`battery.liquidmetal-x.dev/host=true`. Label each Node that meets the Host
prerequisites:

```sh
kubectl label node <node> battery.liquidmetal-x.dev/host=true
```

The Exec Agent on the Node checks it, and reports on the Node. When the
report says ready, the Operator gives the Node to battery as a Host:

```sh
kubectl get node <node> \
  -o jsonpath='{.metadata.annotations.battery\.liquidmetal-x\.dev/exec-agent-ready}{"\n"}'
```

If it prints `false`, the annotation `battery.liquidmetal-x.dev/exec-agent-reason`
says which prerequisite is missing.

## Make a Pool and claim a MicroVM

The [demo manifests](https://github.com/phoban01/battery-operator/tree/main/examples/demo)
make a namespace, a Holder and a Pool. Get `demo.yaml`:

```sh
curl -fsSLO https://raw.githubusercontent.com/phoban01/battery-operator/main/examples/demo/demo.yaml
```

Edit the Pool in it. Change `kernel.image` and `rootVolume.containerSource`
to images that `flintlockd` on your Hosts can pull. flintlock v0.15.2 needs
at least 1024 MiB and one network interface per MicroVM. The Pool in it
is:

```yaml
apiVersion: battery.liquidmetal-x.dev/v1alpha1
kind: Pool
metadata:
  name: demo
  namespace: demo
spec:
  size: 2
  template:
    vcpu: 1
    memoryInMb: 1024
    kernel:
      image: <your kernel image>
      filename: boot/vmlinux
    rootVolume:
      containerSource: <your root filesystem image>
    interfaces:
      - deviceId: eth1
        type: Tap
  placement:
    nodeSelector:
      battery.liquidmetal-x.dev/host: "true"
  replenishment:
    type: ImmediateOnLease
  hooks:
    # Runs in each new MicroVM, through its guest agent, before it is warm.
    create: ["uname -r"]
    failurePolicy: DeleteAndReplace
  lease:
    heartbeatInterval: 10s
    expiryThreshold: 30s
```

Apply it, and wait until two MicroVMs are warm:

```sh
kubectl apply -f demo.yaml
kubectl -n demo wait --for=jsonpath={.status.available}=2 pool/demo --timeout=10m
kubectl -n demo get pool demo
```

Claim one MicroVM for the Holder `demo-holder`:

```yaml
apiVersion: battery.liquidmetal-x.dev/v1alpha1
kind: MicroVMClaim
metadata:
  name: build-1
  namespace: demo
spec:
  poolRef:
    name: demo
  serviceAccountName: demo-holder
```

```sh
kubectl apply -f https://raw.githubusercontent.com/phoban01/battery-operator/main/examples/demo/claim.yaml
kubectl -n demo get microvmclaims
```

The claim goes `Bound` within a second, and shows its Node and when its
Lease expires. The Lease lasts the Pool's `lease.expiryThreshold`, 30s
here. Renew it by setting `spec.renewTime` to the current time:

```sh
kubectl -n demo patch microvmclaim build-1 --type merge \
  -p '{"spec":{"renewTime":"'$(date -u +%Y-%m-%dT%H:%M:%S.000000Z)'"}}'
```

A claim that nobody renews goes `Expired`, and battery deletes its
MicroVM. Delete the claim to release its MicroVM at once:

```sh
kubectl -n demo delete microvmclaim build-1
```

A Consumer normally uses the
[Client Library](https://github.com/phoban01/battery-operator/tree/main/pkg/claimclient),
which creates, renews and deletes its claims, and runs commands in the
MicroVM through the Exec Agent.

## Uninstall

Delete the Pools and claims first, so that battery deletes their MicroVMs:

```sh
kubectl delete microvmclaims,pools --all -A
```

Then, with Flux, delete the `battery-operator` `Kustomization`. With
`prune: true`, Flux deletes every object it applied. Without Flux:

```sh
kubectl delete -f battery-operator/install.yaml
```

Deleting the CRDs deletes every Pool and claim that is left.
