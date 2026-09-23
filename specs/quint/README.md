# Quint models

[Quint](https://quint-lang.org) models of the Operator's types and systems.
They check rules that are easy to state and easy to break across the gRPC
boundary to battery, by random simulation, before and alongside the Go
code. They do not replace Go tests. How they cite requirements is in
[docs/requirements/README.md](../../docs/requirements/README.md#model-citations).

| Module | Covers |
|--------|--------|
| `types.qnt` | The shared types: `Pool` and `MicroVMClaim` (spec, status, phase, conditions), battery's Leases, MicroVMs and Pools, Nodes, Node reports and Hosts |
| `claims.qnt` | The claim lifecycle against battery (CL-001 to CL-031; ADR 0001, consequences 2 to 4) |
| `claims_test.qnt` | Scenario tests for `claims.qnt`, one interleaving each |

Still to come: pools, placement and inventory (#47), and certificate
approval (#48).

## Running them

```sh
make quint        # everything CI runs: typecheck, test, and simulate with the invariants
```

`make quint` runs [hack/quint.sh](../../hack/quint.sh), and CI runs it as
`dagger call quint`. By hand, from the repository root:

```sh
quint typecheck specs/quint/claims.qnt
quint test specs/quint/claims_test.qnt
quint run specs/quint/claims.qnt --invariant safety --max-steps 60 --max-samples 20000 \
  --witnesses witnessOrphan witnessOrphanBesideBound witnessExpired witnessReleased \
  witnessExpiredByEvent witnessExpiredByTime
```

A violation prints the trace that breaks the invariant and the seed that
reproduces it (`--seed`). `--invariants a b c` in place of `--invariant
safety` names the invariant that broke. `QUINT_MAX_STEPS`,
`QUINT_MAX_SAMPLES` and `QUINT_SEED` size and fix the simulations that
`make quint` runs.

`quint verify` (Apalache, exhaustive up to a bound) is not run in CI.

## The claim lifecycle model

`claims.qnt` has one Pool, three claims and six MicroVMs over two Hosts.
The steps are:

- **the Claim Controller:** add the finalizer, `ClaimVM`, write the binding,
  relay a renewal as `Heartbeat`, write battery's expiry, expire a Bound
  claim when the Events stream reports its MicroVM deleted (CL-013) or when
  battery's expiry has passed (CL-014), release under the finalizer, and
  recover on start and on reconnecting. Each step is one call to battery or
  one write to the API server.
- **battery:** replenish the Pool, answer the three `Lease` calls, expire
  Leases nobody renews, and report each deleted MicroVM on its `Events`
  stream.
- **the environment:** claims are created, renewed and deleted, and time
  passes. The controller can crash between any two of its steps, which
  loses the answer it held, as between `ClaimVM` and the status write.
  battery can stop and start again. The Events stream can drop any event,
  and loses them all when battery stops.

Invariants, all in `safety`:

| Invariant | Checks |
|-----------|--------|
| `vmLeasedToAtMostOneClaim` | a MicroVM is leased to at most one claim (glossary, Lease) |
| `releasedVMNeverReused` | a released or expired MicroVM is never handed out again (02-claims.md, Release) |
| `orphanLeaseBounded` | a crash-window orphan is never renewed, and is gone within the expiry threshold (ADR 0001, consequence 2) |
| `finalizerRemovedOnlyAfterRelease` | a claim is gone only after battery has released its Lease (CL-020) |
| `leaseOnlyUnderFinalizer` | a Lease is claimed and recorded only under the finalizer (CL-001) |
| `statusExpiryIsBatterys` | a claim's `leaseExpiresAt` is never later than battery's (CL-011; ADR 0001, consequence 3) |
| `onlyRecordedLeasesReleased` | the controller releases only Leases a claim recorded (CL-031) |
| `boundClaimIsComplete` | a Bound claim carries its lease id, MicroVM, node name, times and a true `Bound` (CL-002, RS-024) |
| `idleMirrorsBattery` | once the controller is idle, every Bound claim's Lease is one battery holds and has not run out (ADR 0001, consequence 4; CL-013, CL-014) |

`idleMirrorsBattery` is the safety form of "an unrenewed Bound claim
eventually goes Expired", since `quint run` checks state invariants only.
It depends on CL-014: without it, a dropped event leaves the claim Bound
(#45).

`expiredOnlyAfterLeaseEnds` (an Expired claim's Lease is gone or has run
out) does not hold, and is not in `safety`. After a crash between a
successful `Heartbeat` and its status write, CL-014 expires a claim whose
Lease battery has just renewed (#57). The scenario test
`renewedClaimExpiredAfterCrashTest` reaches it.

The witnesses show that the simulation reaches the interleavings that
matter: an orphan (`witnessOrphan`), an orphan beside a claim bound after
the retry (`witnessOrphanBesideBound`), an expired claim, a released one,
and a claim expired by each of CL-013 and CL-014. `make quint` fails if a
witness is never reached.

## Conventions

- One module per system, and the shared types in `types.qnt`.
- A model's invariants are named for what they check. Each one names the
  requirement or ADR consequence it models in its comment.
- Every invariant CI checks is in the model's `safety`.
- A property the requirements do not yet guarantee is a finding. File it
  as an issue and keep it out of `safety`, with a scenario test that
  reaches the violation. Don't change the model to hide it.
- Scenario tests go in `<model>_test.qnt`, as `run` definitions whose names
  end in `Test`.
