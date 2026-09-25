# The e2e suite

The system as deployed, on kind ([ADR 0006](../../docs/adr/0006-unit-tests-and-kind-e2e.md)):
the Manifests, with battery's real `poolmgrd` as the Operator's sidecar, the
Exec Agent on every Host, and the fake `flintlockd` (`cmd/fake-flintlockd`)
standing in for each Host's `flintlockd`. It is written with
[sigs.k8s.io/e2e-framework](https://github.com/kubernetes-sigs/e2e-framework):
plain `go test`, behind the `e2e` build tag.

```sh
make test-e2e                                    # build the images, run, clean up
E2E_KEEP_CLUSTER=true make test-e2e              # leave the cluster up; the next run reuses it
make test-e2e-only E2E_ARGS='-run TestCRDValidation'  # images already built
make cleanup-test-e2e                            # delete a kept cluster
```

It needs Docker, `kind`, `kubectl` and Dagger (devbox has all but Docker).

## What a run does

`TestMain` (`main_test.go`, `setup_test.go`):

1. creates the kind cluster from `kind-config.yaml`: a control plane, and
   two workers labelled `battery.liquidmetal-x.dev/host=true`, which are the
   Hosts;
2. loads the Operator's, the Exec Agent's and the fake `flintlockd`'s
   images from the local Docker (`make docker-build-e2e` builds them with
   Dagger, tagged `e2e`). The nodes pull `poolmgrd` themselves, at
   `POOLMGRD_IMG`, which the Makefile derives from `go.mod`;
3. installs cert-manager, at the version `setup_test.go` pins;
4. deploys the overlay in `config/`, through a kustomization it writes to
   `.run/` (not committed) that sets the images;
5. waits for the Operator, the Exec Agent and the fake `flintlockd`, runs
   the tests, and deletes the cluster.

## The overlay (`config/`)

`config/default` and `config/exec-agent` as they ship, plus what only kind
needs. Nothing test-only goes in `config/` at the top of the repository.

- `fake-host.yaml`: the fake `flintlockd` on every Host, in the node's
  network namespace, on port 9090. It waits for the certificate, key and
  client CA the Exec Agent writes to `/etc/battery/flintlockd` (EA-064), and
  reads them again for every connection, because kind has no Host Image
  unit to start and restart it. It reports flintlock v0.15.2, the oldest
  that battery v0.3.3 accepts. Its init container does what the Host Image
  would: creates that directory for user 65532, and a stand-in for
  containerd's thin pool.
- `exec-agent-host-prerequisites.yaml`: Host prerequisites a kind node can
  satisfy. `/dev/null` stands in for `/dev/kvm`, and the stand-in thin pool
  for containerd's.
- `manager-inventory-timing.yaml`: a settle time and a restart window of
  five seconds for the Inventory Controller, instead of the Manifests'
  30 seconds and a minute, so that the Hosts are admitted sooner.

battery starts with no Hosts, as the Manifests ship it. The Inventory
Controller gives it the kind workers once their Exec Agents report them
ready, and restarts it; `TestPoolPlacementAndClaim` waits for that before
it makes a Pool.

## CI

The `e2e` job in `.github/workflows/ci.yml` runs `make test-e2e` on the
GitHub runner. kind runs in the runner's own Docker rather than inside a
Dagger function (Docker in Dagger): kind needs a privileged Docker daemon,
which the runner has and which a Dagger function would have to run as a
nested service, and a failing run is easier to debug from the runner. The
images are still built by Dagger (ADR 0005) and exported to the runner's
Docker, as `make docker-build` does locally. The job uploads the cluster's
logs when it fails. It is not a required check yet.

## Writing a feature

One `Test*` function per area, each running `testenv.Test` with
`features.New(...)`: a namespace of its own when it creates namespaced
objects, `Assess` steps that cite the requirements they check
(`//= type=test`), and dry runs (`client.DryRunAll`) wherever the API
server's answer is the point. `createNamespace` and `deleteNamespace` set
up and tear down that namespace; `deleteNamespace` waits until it is gone,
which is once the Operator has deleted its Pools from battery and released
its claims, and fails the feature if that does not happen in time.

## A smaller cluster

`KIND_CONFIG=<file>` creates the cluster from another kind configuration,
for example a single control plane labelled
`battery.liquidmetal-x.dev/host=true`. The cases that need a second Host or
a Node that is no Host are then skipped. It helps where a multi-node kind
cluster cannot pass pod traffic between its nodes: a host firewall that
filters bridged traffic by reverse path (NixOS's default,
`networking.firewall.checkReversePath`) drops it, and the API server then
cannot reach cert-manager's webhook.

## Reaching the Exec Agent

`TestClientLibraryClaimLife` runs the Client Library (`pkg/claimclient`) in
the test process, as a Consumer would, and dials the Exec Agent at the
address in the claim's status. That address is the Host's internal address
and port 10270, where the agent serves in the node's network namespace.
The test process reaches it directly: a kind node's internal address is on
kind's Docker network, which the machine running Docker routes to, the
GitHub runner included. The suite does not run a test pod in the cluster.
On a machine that cannot route to the Docker network, such as one whose
Docker runs in a VM, this test fails to connect, and no other test depends
on it.
