# Test doubles

There is no KVM and no battery daemon in development, so the controllers,
the Exec Agent and the Client Library are tested against fakes and a
Kubernetes API server test environment (envtest). The fakes come from
flintlock-runner, where `10-test-doubles.md` specifies them.

## The fake battery {#fake-battery}

- **TD-001** The fake battery SHALL serve battery v0.1.0's `PoolAdmin`,
  `Lease` and `Events` services over gRPC with the generated server stubs.
- **TD-002** The fake battery SHALL create, place and delete MicroVMs only
  through the fake `flintlockd`.
- **TD-003** The fake battery SHALL replenish Pools, expire Leases that are
  not renewed within the Pool's expiry threshold, and answer `ClaimVM` on an
  empty Pool with `RESOURCE_EXHAUSTED`, as battery v0.1.0 does.
- **TD-004** The fake battery SHALL run on an injectable clock, so that a
  test can expire a Lease without waiting.
- **TD-005** The fake battery SHALL document every behaviour in which it
  deliberately differs from battery v0.1.0.

## The fake flintlockd {#fake-flintlockd}

- **TD-010** The fake `flintlockd` SHALL serve flintlock's MicroVM and
  `MicroVMExec` services over gRPC, with MicroVMs that exist only in memory.
- **TD-011** The fake `flintlockd` SHALL let a test script the output and
  exit status of an exec, and cut a stream before its exit status.

## Test environments {#test-environments}

- **TD-020** The controllers SHALL be tested against envtest serving the
  CRDs, and the fake battery.
- **TD-021** The Exec Agent SHALL be tested against envtest serving the
  CRDs, and the fake `flintlockd`.
- **TD-022** The Client Library SHALL be tested through a whole claim's life
  against envtest, the controllers, the fake battery, the Exec Agent and the
  fake `flintlockd`.
