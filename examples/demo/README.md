# Demo

`demo.yaml` is what the demo on the [docs site](https://phoban01.github.io/battery-operator/)
applies: a namespace, a Holder, and a Pool of two warm MicroVMs.

| Object | What it is for |
|---|---|
| `Namespace` `demo` | Holds the Holder, the Pool and the claims. |
| `ServiceAccount` `demo-holder` | The Holder: the identity a Consumer claims a MicroVM as. |
| `Role` and `RoleBinding` `demo-holder` | What the Client Library does as the Holder: create, read and delete its claims, read the Pool, create the claim's Secret, and request the Holder's tokens. |
| `Pool` `demo` | Two warm MicroVMs, 1 vCPU and 1024 MiB each, with one tap interface, on the Nodes labelled `battery.liquidmetal-x.dev/host=true`. A create hook runs `uname -r` in each through its guest agent before it is warm. |

## Run it

On a cluster the real hosts trial brought up (`hack/real-hosts/README.md`).
The Pool names the published Guest Images. The trial's Host pulls its own
builds of them from its registry, so name those:

```sh
export KUBECONFIG=hack/real-hosts/.state/kubeconfig
sed -e 's|ghcr.io/phoban01/battery-operator/guest-kernel:v0.3.3|127.0.0.1:5000/bo-trial/kernel:6.1|' \
  -e 's|ghcr.io/phoban01/battery-operator/guest-rootfs:v0.3.3|127.0.0.1:5000/bo-trial/rootfs:24.04|' \
  examples/demo/demo.yaml | kubectl apply -f -
kubectl -n demo wait --for=jsonpath={.status.available}=2 pool/demo --timeout=10m
kubectl -n demo get pool demo
```

Then claim a MicroVM and run a command in it, with the Consumer the trial
builds (`hack/real-hosts/smoke`), on the Host:

```sh
limactl shell bo-host-1 sudo bo-trial-smoke -kubeconfig /etc/rancher/k3s/k3s.yaml \
  -namespace demo -pool demo -holder demo-holder -command 'uname -a'
```

A claim is also an ordinary resource. `claim.yaml` claims a MicroVM by hand,
for the Holder `demo-holder`:

```sh
kubectl apply -f examples/demo/claim.yaml
kubectl -n demo get microvmclaims
kubectl -n demo get microvmclaim build-1 -o jsonpath='{.status}' | jq .
```

The claim's Lease lasts the Pool's `lease.expiryThreshold`, 30s here. Renew
it by setting `spec.renewTime` to the current time:

```sh
kubectl -n demo patch microvmclaim build-1 --type merge \
  -p '{"spec":{"renewTime":"'$(date -u +%Y-%m-%dT%H:%M:%S.000000Z)'"}}'
```

Unrenewed, the claim goes `Expired` and battery deletes its MicroVM.
Deleting the claim releases the MicroVM at once. A Consumer normally lets
the Client Library create, renew and delete its claims.

`hack/real-hosts/demo.sh` runs all of this as the recorded demo.

## On other Hosts

The Pool names the Guest Images that a release publishes,
`ghcr.io/phoban01/battery-operator/guest-kernel` and `guest-rootfs`
([guestimage/README.md](../../guestimage/README.md)), from `v0.3.3`, the
first release that publishes them. A later release works too.
The Hosts need a way out to ghcr.io. The kernel boots on Firecracker, not
on Cloud Hypervisor. flintlock v0.16.0 needs at least 1024 MiB and one
network interface per MicroVM (RS-014 to RS-016).
