# Quint models

[Quint](https://quint-lang.org) models of the Operator's types and systems.
They check rules that are easy to state and easy to break across the gRPC
boundary to battery, by random simulation, before and alongside the Go
code. They do not replace Go tests. How they cite requirements is in
[docs/requirements/README.md](../../docs/requirements/README.md#model-citations).

| Module | Covers |
|--------|--------|
| `types.qnt` | The shared types: `Pool` and `MicroVMClaim` (spec, status, phase, conditions), battery's Leases, MicroVMs and Pools, Nodes, Node reports and Hosts |
| `claims.qnt` | The claim lifecycle against battery (CL-001 to CL-042; ADR 0001, consequences 2 to 4) |
| `claims_test.qnt` | Scenario tests for `claims.qnt`, one interleaving each |
| `certificates.qnt` | Certificate approval and signing (CT-001 to CT-020; EA-061, EA-062, EA-068; ADR 0003 and ADR 0004) |
| `certificates_test.qnt` | Scenario tests for `certificates.qnt` |

Still to come: pools, placement and inventory (#47).

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
  witnessExpiredByEvent witnessExpiredByTime witnessOrphanFromLostAnswer \
  witnessReleasedAfterLostAnswer
quint run specs/quint/certificates.qnt --invariant safety --max-steps 60 --max-samples 20000 \
  --witnesses witnessAgentFullyCertified witnessForeignApprovalFailed witnessForeignApprovalSigned \
  witnessAnotherHostDenied witnessSubjectIgnored witnessApprovedAwaitingCA
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
  claim when the Events stream reports its MicroVM deleted (CL-013) or,
  after reading its Lease with `ListLeases`, when battery's expiry has
  passed (CL-014, CL-016), release with `ReleaseVM` and then remove the
  finalizer, and recover on start and on reconnecting. Each step is one
  call to battery or one write to the API server.
- **battery:** replenish the Pool, answer the `Lease` calls, expire
  Leases nobody renews, and report each deleted MicroVM on its `Events`
  stream.
- **the environment:** claims are created, renewed and deleted, and time
  passes. The controller can crash between any two of its steps, which
  loses the answer it held, as between `ClaimVM` and the status write.
  battery can stop and start again. The Events stream can drop any event,
  and loses them all when battery stops.
- **calls that fail in transit:** `ClaimVM`, `Heartbeat`, `ListLeases` and
  `ReleaseVM` can each fail with battery having acted and the answer lost,
  or without battery acting (battery down, or battery's own `UNAVAILABLE`
  from `ReleaseVM`). The claim keeps its phase, shows `Synced` false, and
  the call is retried (CL-007, CL-015, CL-017, CL-022, CL-040).

Invariants, all in `safety`:

| Invariant | Checks |
|-----------|--------|
| `vmLeasedToAtMostOneClaim` | a MicroVM is leased to at most one claim (glossary, Lease) |
| `releasedVMNeverReused` | a released or expired MicroVM is never handed out again (02-claims.md, Release) |
| `orphanLeaseBounded` | an orphan, from a crash or a `ClaimVM` answer lost in transit, is never renewed, and is gone within the expiry threshold (ADR 0001, consequence 2) |
| `finalizerRemovedOnlyAfterRelease` | a claim is gone only after battery has released its Lease (CL-020) |
| `leaseOnlyUnderFinalizer` | a Lease is claimed and recorded only under the finalizer (CL-001) |
| `statusExpiryIsBatterys` | a claim's `leaseExpiresAt` is never later than battery's (CL-011; ADR 0001, consequence 3) |
| `onlyRecordedLeasesReleased` | the controller releases only Leases a claim recorded (CL-031) |
| `recordedOnlyFromOwnAnswer` | a claim records only a lease id from an answer to its own `ClaimVM`, so no orphan is adopted (CL-008) |
| `boundClaimIsComplete` | a Bound claim carries its lease id, MicroVM, node name, times and a true `Bound` (CL-002, RS-024) |
| `idleMirrorsBattery` | once the controller is idle, every Bound claim's Lease is one battery holds and has not run out (ADR 0001, consequence 4; CL-013, CL-014) |
| `expiredOnlyAfterLeaseEnds` | a claim goes Expired only once battery no longer holds its Lease or the Lease has run out (CL-016, #57) |

`idleMirrorsBattery` is the safety form of "an unrenewed Bound claim
eventually goes Expired", since `quint run` checks state invariants only.
It depends on CL-014: without it, a dropped event leaves the claim Bound
(#45).

`expiredOnlyAfterLeaseEnds` did not hold with CL-014 alone. After a crash
between a successful `Heartbeat` and its status write, or a `Heartbeat`
answer lost in transit, CL-014 expired a claim whose Lease battery had just
renewed (#57). CL-016 reads the Lease with `ListLeases` first, and the
scenario tests `renewedClaimKeptAfterCrashTest`,
`renewedClaimListedAfterCrashTest` and `heartbeatAnswerLostTest` show the
claim kept Bound with battery's expiry.

The witnesses show that the simulation reaches the interleavings that
matter: an orphan (`witnessOrphan`), an orphan beside a claim bound after
the retry (`witnessOrphanBesideBound`), an expired claim, a released one,
a claim expired by each of CL-013 and CL-014, an orphan from a `ClaimVM`
answer lost in transit (`witnessOrphanFromLostAnswer`), and a retried
`ReleaseVM` that finds the Lease unknown after a lost answer
(`witnessReleasedAfterLostAnswer`). `make quint` fails if a witness is
never reached. A claim kept Bound by `ListLeases` (CL-016) needs a
renewal late in the Lease, a lost answer and time passing before the
retry, which random simulation reaches too rarely for a witness; the
scenario tests above cover it.

## The certificates model

`certificates.qnt` has three Hosts, n1, n2 (dual-stack) and n3, whose Node
object has been deleted, and the Operator as approver and signer of
`CertificateSigningRequest`s. The steps are:

- **the requesters:** each uncompromised Host's Exec Agent requests its
  three certificates under its own Node's identity (EA-061, EA-062,
  EA-068). A Host can be compromised; from then on its agent submits
  anything. Other identities submit anything too: another ServiceAccount, a
  cluster admin, and the Exec Agent's ServiceAccount with a token bound to
  no pod. A forged request starts from the request some Host's agent would
  make for some signer name and changes any of its subject, IP addresses,
  SPIFFE IDs (including another trust domain), DNS names, self-signature
  and key usages, so most forgeries are near misses. The requester cannot
  choose its username or Node, which the API server records.
- **the other approvers:** anyone granted `approve` on the signer names
  approves or denies any undecided request, for any signer name.
- **the Operator:** one step is one reconcile of
  `CertificateSigningRequestReconciler`: review the request against the
  checks of CT-010 to CT-014, then deny it or approve and sign it; or, for
  a request already approved by anyone, mark it Failed or sign it (CT-006).
  It sets the subject itself (CT-007) and signs with its signer name's CA
  (CT-002).
- **the environment:** the CA Secrets can be unreadable, so an approved
  request waits for its signature.

Invariants, all in `safety`:

| Invariant | Checks |
|-----------|--------|
| `noCertForAnotherNode` | every certificate names only its requester's own Host's addresses and SPIFFE IDs (CT-010, CT-011, CT-014; ADR 0003, consequence 1) |
| `onlyExecAgentsCertified` | only the Exec Agent's ServiceAccount, bound to a Node, obtains a certificate (CT-010, CT-011, CT-014) |
| `signedOnlyWhenApproved` | nothing is signed without `Approved` (CT-003) |
| `signedOnlyAfterChecks` | nothing is signed without passing every check, whoever approved it (CT-006) |
| `foreignApprovalNeverBypassesChecks` | a request someone else approved that fails a check never gets a certificate (CT-006) |
| `subjectIsRequesterNode` | every certificate's subject is the requester's Node (CT-007) |
| `identitiesInTrustDomain` | every certificate's SPIFFE ID is in the configured trust domain, and never battery's (CT-020) |
| `caAndUsagesMatchSigner` | the signer name's CA and key usages, and no DNS name (CT-002, CT-012) |
| `onlyOurSignerNames` | the Operator does nothing with another signer name (CT-001) |
| `refusalNamesTheCheck` | a `Denied` or `Failed` condition names a check the request fails (CT-013) |
| `idleSettled` | once the Operator is idle and can read its CAs, every request for its signer names is signed, Denied or Failed |

`noCertForAnotherNode` checks the addresses a Host really holds, not the
ones its Node lists. It holds only if a Node's internal addresses belong to
its Host, and the main simulation assumes they do. A compromised Host's
kubelet can list another Host's address on its own Node; the step
`kubeletReportsAddresses` does that, is not in `step`, and the scenario
test `compromisedKubeletTest` reaches the violation (#75).

`failingRequestsDenied` (CT-013 as written: every request that fails a
check is Denied) does not hold, and is not in `safety`. A request someone
else approved can't be Denied; the Operator marks it Failed instead, which
no requirement mentions (#76). `failedNotDeniedTest` reaches it.

A Host whose Node is gone still obtains its client certificate: CT-011,
unlike CT-010 and CT-014, does not ask for an existing Node, and the code
agrees (`nodeNotFoundTest`).

Not modelled: CT-004 (durations), CT-005 (publishing the CAs), the
Manifests (CT-021 to CT-023, EA-067), and the Exec Agent's handling of what
it receives (EA-063 to EA-066). Only the Operator signs: it alone holds the
CA keys.

The witnesses show an Exec Agent with all three certificates, a request
someone else approved both Failed and signed, a compromised agent denied
for another Host's address or SPIFFE ID, a requested subject ignored, and an
approved request waiting for the CA.

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
