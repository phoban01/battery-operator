# Test doubles

There is no KVM and no battery daemon in development, so the controllers,
the Exec Agent and the Client Library are tested in two layers: unit tests
against the fakes below and controller-runtime's fake client, and an e2e suite
on a kind cluster, with battery's real `poolmgrd` and the fake `flintlockd`
([test environments](#test-environments)). The fakes come from
flintlock-runner, where `10-test-doubles.md` specifies them.

## The fake battery {#fake-battery}

- **TD-001** The fake battery SHALL serve battery v0.3.3's `PoolAdmin`,
  `Lease` and `Events` services over gRPC with the generated server stubs.
- **TD-002** The fake battery SHALL create, place and delete MicroVMs only
  through the fake `flintlockd`.
- **TD-003** The fake battery SHALL replenish Pools, expire Leases that are
  not renewed within the Pool's expiry threshold, and answer `ClaimVM` on an
  empty Pool with `RESOURCE_EXHAUSTED`, as battery v0.3.3 does.
- **TD-004** The fake battery SHALL run on an injectable clock, so that a
  test can expire a Lease without waiting.
- **TD-005** The fake battery SHALL document every behaviour in which it
  deliberately differs from battery v0.3.3.
- **TD-006** The fake battery SHALL let a test make it unavailable for a
  period, delay its answers to `ClaimVM`, refuse heartbeats, fail a Pool's
  hooks, and drop its `Events` streams.

## The fake flintlockd {#fake-flintlockd}

- **TD-010** The fake `flintlockd` SHALL serve flintlock's MicroVM and
  `MicroVMExec` services over gRPC, with MicroVMs that exist only in memory.
- **TD-011** The fake `flintlockd` SHALL let a test script the output and
  exit status of an exec, and cut a stream before its exit status.
- **TD-012** Where a test gives it a client certificate authority, the fake
  `flintlockd` SHALL serve over TLS and refuse a client whose certificate
  that authority did not sign.

## Test environments {#test-environments}

- **TD-020** (withdrawn)
- **TD-021** (withdrawn)
- **TD-022** (withdrawn)
- **TD-023** The unit tests SHALL test each subreconciler, and the rest of
  the logic of the controllers and the Exec Agent, against the fake battery,
  the fake `flintlockd` and a fake Kubernetes client, without a Kubernetes
  API server.
- **TD-024** The e2e suite SHALL run the Operator with battery's `poolmgrd`
  as its sidecar, the Exec Agent, and the fake `flintlockd` as each Host's
  `flintlockd`, in a kind cluster, from the Manifests.
- **TD-025** The e2e suite SHALL test the behaviour that depends on the
  Kubernetes API server, including CRD validation, admission policies,
  TokenReview, TokenRequest and `CertificateSigningRequest`s, in the kind
  cluster.
- **TD-026** The e2e suite SHALL take a Client Library claim through its
  whole life: bind, run a command through the Exec Agent, renew, and
  release.
- **TD-027** The unit tests and the e2e suite SHALL use envtest only where
  neither a fake nor the kind cluster can exercise a behaviour, and SHALL
  say why beside that use.

The tests are in two layers ([ADR 0006](../adr/0006-unit-tests-and-kind-e2e.md)).
Logic is unit-tested with fakes, which the scope and subreconciler structure
of CLAUDE.md makes possible, and anything that needs a real API server is
tested end to end in kind, with the real battery. TD-020 to TD-022 required
envtest and are withdrawn; the envtest suites of the first waves have moved
onto the two layers.
