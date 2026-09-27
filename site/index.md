---
layout: default
title: battery-operator
---

<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/asciinema-player@3.10.0/dist/bundle/asciinema-player.css">

battery-operator is a Kubernetes operator for
[battery](https://github.com/liquidmetal-dev/battery), which keeps warm
pools of [flintlock](https://github.com/liquidmetal-dev/flintlock)
MicroVMs. With it, you declare a pool of Firecracker MicroVMs and claim one
the way you use anything else in a cluster:

- A **Pool** says what battery keeps warm: how many MicroVMs, their kernel,
  root filesystem, CPUs and memory, and which Nodes they run on.
- A **MicroVMClaim** is one client's hold on one warm MicroVM. The Operator
  binds it within a second, and releases the MicroVM when the claim goes.
- The **Exec Agent** on each Host lets the holder of a claim, and only the
  holder, run commands in its MicroVM.

## See it work

This recording is one Kubernetes Node with KVM. A Pool fills with two
Firecracker MicroVMs. A Consumer claims one with the Client Library, and
runs a command in it through the Exec Agent. The Pool starts a new MicroVM
in place of the one claimed. Then a claim made with `kubectl` binds, is
renewed, and goes Expired when nobody renews it. Deleting the Pool deletes
its MicroVMs.

<div id="demo"></div>
<script src="https://cdn.jsdelivr.net/npm/asciinema-player@3.10.0/dist/bundle/asciinema-player.min.js"></script>
<script>
  AsciinemaPlayer.create('{{ "/demo.cast" | relative_url }}', document.getElementById('demo'), {
    cols: 110, rows: 30, idleTimeLimit: 2, fit: 'width', poster: 'npt:0:3'
  });
</script>

Nothing in it is a fake: the Host runs Firecracker, `flintlockd` and
battery's own `poolmgrd`, in a Lima VM with nested virtualization on a Mac.
The [real hosts trial](https://github.com/phoban01/battery-operator/tree/main/hack/real-hosts)
brings it up, and `hack/real-hosts/demo.sh --record` records it again.

## How it fits together

```
  Kubernetes API                     Operator pod                        each Host (a Node)
 ┌─────────────────┐     ┌──────────────────────────────────┐     ┌───────────────────────────┐
 │ Pool            │◀───▶│ Pool, Claim, Inventory           │     │ Exec Agent (DaemonSet)    │
 │ MicroVMClaim    │     │ Controllers, certificate signer  │     │   checks the claim, then  │
 │ Nodes, CSRs     │     │            │ gRPC, loopback      │     │   relays exec ───────┐    │
 └─────────────────┘     │            ▼                     │     │                      ▼    │
                         │ battery poolmgrd (sidecar) ──────┼─────┼─▶ flintlockd ──▶ Firecracker
                         └──────────────────────────────────┘ mTLS└───────────────────────────┘
```

- **battery is not changed.** It runs as a sidecar in the Operator's pod,
  on loopback, and the Operator is a client of its gRPC API.
- **The Operator** turns each Pool and claim into battery calls, and copies
  battery's answers into their status. It tells battery which Nodes are
  Hosts.
- **Hosts get their certificates from Kubernetes.** The Exec Agent requests
  them through CertificateSigningRequests that the Operator approves and
  signs. battery reaches `flintlockd` over mutual TLS with them.
- **Only the holder runs commands.** A claim carries a token for its
  holder. The Exec Agent checks the claim, the token and the MicroVM before
  it relays a command to `flintlockd`'s exec API.

## Try it

You need a Kubernetes cluster whose Hosts meet the
[Host prerequisites](https://github.com/phoban01/battery-operator/blob/main/docs/host-prerequisites.md):
KVM, containerd's thin pool, and `flintlockd` with its exec API. The
quickest way is the real hosts trial, which makes one Lima VM on a Mac, or
any Ubuntu machine with KVM, into such a cluster:

```sh
hack/real-hosts/up.sh                     # the Host, k3s, cert-manager and the Operator
export KUBECONFIG=hack/real-hosts/.state/kubeconfig
kubectl apply -f examples/demo/demo.yaml  # a namespace, a Holder and a Pool
kubectl -n demo wait --for=jsonpath={.status.available}=2 pool/demo
```

The [demo manifests](https://github.com/phoban01/battery-operator/tree/main/examples/demo)
say what each object is for.

## Claims are resources

Every claim is a `MicroVMClaim` in the cluster, so `kubectl get` shows which
MicroVMs are held, by whom, on which Node, and until when:

```yaml
apiVersion: battery.liquidmetal-x.dev/v1alpha1
kind: MicroVMClaim
metadata:
  name: build-1
  namespace: demo
spec:
  poolRef:
    name: demo
  serviceAccountName: demo-holder   # the Holder: only it can use the MicroVM
```

```console
$ kubectl -n demo get microvmclaim build-1 -o jsonpath='{.status}' | jq '{phase, microVM, host, leaseExpiresAt}'
{
  "phase": "Bound",
  "microVM": { "uid": "01M3J9T8R6826GNNC6PJ3TDTSC" },
  "host": { "agentAddress": "192.168.5.15:10270", "nodeName": "bo-host-1" },
  "leaseExpiresAt": "2026-09-27T20:46:09Z"
}
```

A claim's Lease lasts the Pool's `lease.expiryThreshold`. The holder renews
it by setting `spec.renewTime`, and the Client Library does this for its
claims. A claim nobody renews goes `Expired`, and battery deletes its
MicroVM. Deleting a claim releases its MicroVM at once.

## Claim a MicroVM from Go

A Consumer uses the Client Library,
[`pkg/claimclient`](https://github.com/phoban01/battery-operator/tree/main/pkg/claimclient):

```go
cl, err := claimclient.New(claimclient.Config{
	Client:    kubeClient, // a controller-runtime client, as the Holder
	Namespace: "demo",
	ServingCA: servingCA,  // the Operator's serving CA
})

// Claim blocks until the claim is Bound.
claim, err := cl.Claim(ctx, claimclient.Request{
	Pool:               "demo",
	ServiceAccountName: "demo-holder",
})
defer claim.Release(ctx)

// Dial reaches the Exec Agent on the MicroVM's Host.
conn, err := claim.Dial(ctx)
```

The demo's Consumer,
[`hack/real-hosts/smoke`](https://github.com/phoban01/battery-operator/tree/main/hack/real-hosts/smoke),
is a complete example.

## How it is built

- Every decision is an
  [architecture decision record](https://github.com/phoban01/battery-operator/tree/main/docs/adr).
- Every behaviour is an EARS
  [requirement](https://github.com/phoban01/battery-operator/tree/main/docs/requirements),
  traced to its code and its test with [duvet](https://github.com/awslabs/duvet).
  CI fails a change that leaves a requirement without either.
- The claim lifecycle, pools and certificates are modelled in
  [Quint](https://github.com/phoban01/battery-operator/tree/main/specs/quint).
  CI simulates the models and checks them with Apalache, and replays the
  claim model's traces against the Claim Controller.
- Unit tests run against fakes of battery and `flintlockd`. The e2e suite
  runs the Manifests on kind, with battery's real `poolmgrd`.

## Status

`v1alpha1`. The Operator, the Exec Agent and the Client Library work
end to end on a real Host. The project lives outside battery so it can be
proven without disturbing battery, with the aim of being adopted by
liquidmetal-dev. The design was discussed in
[liquidmetal-dev/battery#46](https://github.com/liquidmetal-dev/battery/issues/46).
