# 0001. A standalone operator in front of an unmodified battery

- **Status:** Proposed
- **Date:** 2026-09-23
- **Discussion:** [liquidmetal-dev/battery#46](https://github.com/liquidmetal-dev/battery/issues/46)

## Context

[battery](https://github.com/liquidmetal-dev/battery) keeps pools of
flintlock MicroVMs warm and leases them to clients. Its whole interface is
gRPC:

| Service | RPCs |
|---------|------|
| `PoolAdmin` | `CreatePool`, `UpdatePool`, `DeletePool`, `GetPool`, `ListPools` |
| `Lease` | `ClaimVM`, `Heartbeat`, `ReleaseVM` |
| `Events` | `Subscribe` (a stream) |

battery keeps its own state in SQLite. It learns the flintlock Hosts it may
use from its configuration file.

battery#46 asks for declarative pool management on Kubernetes. On
2026-09-22 we proposed two namespaced resources in that issue:

- `Pool`, which is `PoolSpec` as a resource;
- `MicroVMClaim`, which is `ClaimVM`, `Heartbeat` and `ReleaseVM` as a
  resource.

A `MicroVMClaim` names the ServiceAccount allowed to use its MicroVM. Each
claim carries its own bound token, which a per-Host Exec Agent checks before
it runs anything in the MicroVM. There is deliberately no `MicroVM`
resource. The full shapes and their reasoning are in
[the proposal](https://github.com/phoban01/flintlock-runner/blob/main/docs/proposals/battery-claim-resources.md).
On 2026-09-23 Richard Case (@richardcase) answered: "i think this looks
good".

What was left open is how to build it. Two options were discussed in the
issue:

- **A. A common core with two binaries**, in battery's repository. The
  operator reuses battery's code directly, and the battery daemon does not
  run at all.
- **B. A new operator that calls an unchanged battery over its gRPC API**,
  with battery as a sidecar or as its own Deployment. Richard said this was
  "probably the least destabilizing to the existing battery
  implementation".

The first client is [flintlock-runner](https://github.com/phoban01/flintlock-runner),
a GitLab runner that runs each CI job in a warm MicroVM. It already has an
Exec Agent, built against a provisional stand-in for `MicroVMClaim`, and it
cannot finish its cluster fleet until the real resources exist.

## Decision

1. **The operator is a standalone project**, `phoban01/battery-operator`,
   licensed Apache-2.0 as battery and flintlock are. It is offered to
   liquidmetal-dev for adoption once it works.
2. **The operator is a client of battery's gRPC API, and battery is not
   changed (option B).** The operator holds no MicroVM or lease state that
   battery does not also hold. battery stays the authority over VMs and
   leases, and the resources are the declaration plus a mirror of battery's
   answer.
3. **The API is the two resources from battery#46, as posted:** `Pool`,
   and `MicroVMClaim` with `spec.serviceAccountName` and a per-claim bound
   token. There is no `MicroVM` resource.
4. **There are four components:**
   - **The Pool controller** reconciles each `Pool` into `CreatePool`,
     `UpdatePool` and `DeletePool`, and writes `status` from `GetPool` and
     the `Events` stream.
   - **The MicroVMClaim controller** turns a new claim into `ClaimVM`, a
     changed `spec.renewTime` into `Heartbeat`, and deletion into
     `ReleaseVM` under a finalizer.
   - **The Inventory controller** decides which Nodes are flintlock Hosts
     and tells battery (see [Consequences](#consequences)).
   - **The Exec Agent** runs on every Host and lets a claim's holder, and
     only its holder, run commands in that claim's MicroVM.
5. **Code the operator needs moves here from flintlock-runner**, as listed
   [below](#code-moving-from-flintlock-runner). flintlock-runner then
   consumes this project instead of carrying the code itself.
6. **The API group is `battery.liquidmetal-x.dev` until adoption.**
   `battery.liquidmetal.dev` is liquidmetal-dev's domain, which this project
   does not own. The `-x` marks the group as experimental, in the way
   `x-k8s.io` marks Kubernetes SIG projects. On adoption the group becomes
   `battery.liquidmetal.dev`, which is a breaking rename, taken while the
   API is still `v1alpha1`.
7. **battery runs as a sidecar** in the operator's pod, listening on
   loopback:
   - battery is a single-writer SQLite service, so it is one replica
     either way;
   - its API is never on the network (consequence 5);
   - a restart for a Host change (consequence 1) stays inside one pod.

   The cost is that the operator's leader election and battery's lifecycle
   are coupled: the operator runs one replica too, and restarting battery
   restarts a container in the operator's pod. A separate Deployment stays
   possible later, behind mutual TLS.

## Consequences

### What option B buys

- battery is untouched. Its gRPC clients keep working, and if this project
  fails it can be dropped without harm to battery.
- The Exec Agent and the other pieces that aren't specific to CI get a home
  that isn't a CI runner.
- Adoption later is a repository move rather than a merge into battery's
  internals.

### What the gRPC boundary makes hard

These were found by reading battery v0.1.0. They are the price of option B,
and the first two are worth raising upstream.

1. **Hosts are static configuration.** battery reads its `hosts:` list once
   at startup. It has no reload and no RPC to add or remove a Host. The
   Inventory controller therefore cannot register a Host over gRPC. With
   battery unchanged it has to render battery's configuration and restart
   battery whenever a Host joins or leaves.

   Pools name their Hosts too, in `PoolSpec.flintlock_hosts`. The Pool
   controller resolves `spec.placement.nodeSelector` to Host names and calls
   `UpdatePool` when the answer changes.

   This is the largest cost of option B. A Host registration RPC, or a
   configuration reload, upstream would remove the restarts.
2. **`ClaimVM` is not idempotent.** battery chooses the lease id, and the
   request carries only the pool. Suppose the controller crashes after
   `ClaimVM` succeeds but before the lease id is written to the claim's
   status: the lease is orphaned, and the retry claims a second VM. The
   orphan is bounded, because nothing heartbeats it, so battery expires it
   after the pool's `heartbeat_expiry_threshold` and deletes the VM. The
   cost is one warm VM held for that long, not a leak.

   A client-supplied lease id upstream, so that the claim's own uid could
   be the lease id, would remove the window.
3. **Renewal is relayed.** A client renews by patching `spec.renewTime`,
   and the controller turns that into `Heartbeat`, so how long a lease
   survives now includes the controller's reaction time. The controller has
   to heartbeat promptly, and `status.leaseExpiresAt` has to be battery's
   `expires_at` rather than a time the controller works out itself.
4. **There are two stores.** battery's SQLite database is the truth about
   VMs and leases, and the resources mirror it. On start the controllers
   rebuild their view from battery. For example, a claim whose lease battery
   no longer knows becomes `Expired`. battery runs as one replica, with
   persistent storage for its database, because losing that database
   orphans VMs on the Hosts.
5. **battery's API has to be private to the operator.** A client that can
   reach battery's gRPC API directly can claim and use a VM without a
   `MicroVMClaim`, which bypasses `spec.serviceAccountName` and the Exec
   Agent's checks. battery v0.1.0 listens on TCP only, so a unix socket
   would take a battery change. The unchanged options are loopback
   (`127.0.0.1`) inside the operator's pod, which only containers of that
   pod can reach, or mTLS with a client certificate only the operator
   holds. With battery as a sidecar (decision 7), it is loopback. battery
   already supports mutual TLS on its API
   (`validate_client` with `client_ca_file`), and a basic-auth token.

## Open questions

1. **Who creates the per-claim token's Secret?** The proposal has the
   client create it. The operator could create it on bind instead, which
   would put the convention in one place. The operator would then need to
   create Secrets in every client namespace.
2. **The Host Image.** flintlock-runner's bootc Host Image mixes generic
   prerequisites (flintlockd, the containerd thin pool, KVM) with CI Host
   Services (buildkitd, a Go proxy, a registry mirror). Leaning: it stays in
   flintlock-runner for now, and this project documents the prerequisites a
   Host must meet.
3. **Upstream asks.** Raise a Host registration RPC or configuration reload
   (consequence 1) and an idempotent `ClaimVM` (consequence 2) with battery.
   Neither blocks `v1alpha1`.

## Code moving from flintlock-runner

| From flintlock-runner | Becomes | Notes |
|---|---|---|
| `internal/agent` | the Exec Agent | The provisional claim lookup (`dynclaims.go`, the test resource `claims.test.flintlock-runner.dev`) is replaced by `MicroVMClaim`. The creator-annotation check becomes the four checks in the proposal: token audience, `spec.serviceAccountName`, the token's binding to the claim's Secret, and a Bound, unexpired claim for this VM on this Host. Host Service annotations are CI's and are split out. |
| `internal/hostcheck` | Exec Agent readiness | As is. |
| `internal/clock` | shared | As is. |
| `internal/flintlock/fake` | Exec Agent tests | A fake flintlockd. |
| `internal/poolmgr/fake`, and the connection, TLS and keepalive parts of `internal/poolmgr` | the controllers' battery client and its tests | A fake battery. |
| `internal/kubelabels` | the Exec Agent's annotations only | Renamed to this project's prefix. |
| `deploy/agent` | the Exec Agent's manifests | |

**Staying in flintlock-runner:** the executor and scheduler, the GitLab
protocol, the Host Services, the bootc Host Image for now, and the
`agent-exec` transport. The transport will use a small client package from
this project, which creates a claim and its token, waits for `Bound`,
renews, and dials the Exec Agent. That package is what flintlock-runner
integrates against.

**Removed from flintlock-runner** once its claim backend runs on this
operator: the virtual-kubelet Pod Provider (`internal/kubelet`), the
ReplicaSet pool backend (`internal/poolmgr/kube`) and the `kube-exec`
transport.

## What happens next

1. Scaffold the project and write the `v1alpha1` types and CRDs from
   battery#46, in `battery.liquidmetal-x.dev`.
2. Build the MicroVMClaim controller against the fake battery, then the
   Pool controller.
3. Move the Exec Agent over and switch it to the real claim checks.
4. Build the Inventory controller, on a configuration-render-and-restart
   basis until battery can register Hosts itself.
5. Write the client package, then return to flintlock-runner to integrate
   against it.
