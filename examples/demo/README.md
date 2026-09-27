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

On a cluster the real hosts trial brought up (`hack/real-hosts/README.md`):

```sh
export KUBECONFIG=hack/real-hosts/.state/kubeconfig
kubectl apply -f examples/demo/demo.yaml
kubectl -n demo wait --for=jsonpath={.status.available}=2 pool/demo --timeout=10m
kubectl -n demo get pool demo
```

Then claim a MicroVM and run a command in it, with the Consumer the trial
builds (`hack/real-hosts/smoke`), on the Host:

```sh
limactl shell bo-host-1 sudo bo-trial-smoke -kubeconfig /etc/rancher/k3s/k3s.yaml \
  -namespace demo -pool demo -holder demo-holder -command 'uname -a'
```

`hack/real-hosts/demo.sh` runs all of this as the recorded demo.

## On other Hosts

The Pool names the kernel and root filesystem images the trial pushes to
each Host's registry on `127.0.0.1:5000`. On other Hosts, name images that
`flintlockd` there can pull. flintlock v0.15.2 needs at least 1024 MiB and
one network interface per MicroVM (RS-014 to RS-016).
