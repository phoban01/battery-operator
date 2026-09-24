# Quint models

[Quint](https://quint-lang.org) models of the Operator's types and systems.
They check rules that are easy to state and easy to break across the gRPC
boundary to battery, by random simulation, before and alongside the Go
code. They do not replace Go tests. How they cite requirements is in
[docs/requirements/README.md](../../docs/requirements/README.md#model-citations).

| Module | Covers |
|--------|--------|
| `types.qnt` | The shared types: `Pool` and `MicroVMClaim` (spec, status, phase, conditions) and their places in the API server, battery's Leases, MicroVMs and Pools, Nodes, Node reports and Hosts |
| `claims.qnt` | The claim lifecycle against battery (CL-001 to CL-042; ADR 0001, consequences 2 to 4), with battery as `docs/requirements/10-battery.md` describes it (BA-*) |
| `claims_test.qnt` | Scenario tests for `claims.qnt`, one interleaving each |
| `certificates.qnt` | Certificate approval and signing (CT-001 to CT-020; EA-061, EA-062, EA-068; ADR 0003 and ADR 0004) |
| `certificates_test.qnt` | Scenario tests for `certificates.qnt` |
| `pools.qnt` | Pools, placement and inventory: the Pool Controller and the Inventory Controller against battery (PO-001 to PO-012, IN-001 to IN-012; ADR 0001, consequence 1) |
| `pools_test.qnt` | Scenario tests for `pools.qnt` |

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
  witnessReleasedAfterLostAnswer witnessUnsweptLease witnessLateHeartbeat
quint run specs/quint/certificates.qnt --invariant safety --max-steps 60 --max-samples 20000 \
  --witnesses witnessAgentFullyCertified witnessForeignApprovalFailed witnessForeignApprovalSigned \
  witnessAnotherHostDenied witnessSubjectIgnored witnessApprovedAwaitingCA
quint test specs/quint/pools_test.qnt
quint run specs/quint/pools.qnt --invariant safety --max-steps 60 --max-samples 20000 \
  --witnesses witnessFlapAbsorbed witnessRestart witnessBatchedRestart witnessTwoRestarts \
  witnessVMOnFormerHost witnessPlacementUpdated witnessNoEligibleHost \
  witnessPoolDeleted witnessUnknownHostPicked
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
  relay a pending renewal as `Heartbeat`, write battery's expiry with the
  relayed `renewTime`, expire a Bound claim when the Events stream reports
  its MicroVM deleted (CL-013) or, once no renewal is pending and after
  reading its Lease with `ListLeases`, when battery's expiry has passed or
  battery no longer lists the Lease (CL-012, CL-014, CL-016), release with `ReleaseVM` and then remove the
  finalizer, and recover on start and on reconnecting. Each step is one
  call to battery or one write to the API server.
- **battery:** replenish the Pool, answer the `Lease` calls, sweep the
  Leases nobody renewed, and report each deleted MicroVM on its `Events`
  stream. These steps cite `docs/requirements/10-battery.md`. A `Heartbeat`
  renews any Lease battery still holds, including one past its expiry
  (BA-010). The sweep is a step of its own: time can pass a Lease's expiry
  by up to one sweep interval (`SWEEP`) before a sweep deletes it (BA-020),
  and battery's first sweep after a start is one interval later (BA-022).
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
| `orphanLeaseBounded` | an orphan, from a crash or a `ClaimVM` answer lost in transit, is never renewed, and is gone within the expiry threshold plus one sweep interval, or one sweep interval after battery starts (ADR 0001, consequence 2; BA-020, BA-022) |
| `finalizerRemovedOnlyAfterRelease` | a claim is gone only after battery has released its Lease (CL-020) |
| `leaseOnlyUnderFinalizer` | a Lease is claimed and recorded only under the finalizer (CL-001) |
| `statusExpiryIsBatterys` | a claim's `leaseExpiresAt` is never later than battery's (CL-011; ADR 0001, consequence 3) |
| `onlyRecordedLeasesReleased` | the controller releases only Leases a claim recorded (CL-031) |
| `recordedOnlyFromOwnAnswer` | a claim records only a lease id from an answer to its own `ClaimVM`, so no orphan is adopted (CL-008) |
| `boundClaimIsComplete` | a Bound claim carries its lease id, MicroVM, node name, time of binding and a true `Bound` (CL-002, RS-024); its expiry comes later, from `ListLeases` (CL-016), since battery's answer to `ClaimVM` carries none |
| `idleMirrorsBattery` | once the controller is idle, every Bound claim's Lease is one battery holds and has not run out (ADR 0001, consequence 4; CL-013, CL-014) |
| `expiredOnlyAfterLeaseEnds` | a claim goes Expired only once battery no longer holds its Lease or the Lease has run out (CL-016, #57) |
| `pendingRenewalKeptWhileHeld` | a claim with a renewal the controller has not relayed goes Expired only once battery no longer holds its Lease, and so would refuse the `Heartbeat` (CL-010, CL-014, CL-016, #85) |

`idleMirrorsBattery` is the safety form of "an unrenewed Bound claim
eventually goes Expired", since `quint run` checks state invariants only.
It depends on CL-014: without it, a dropped event leaves the claim Bound
(#45).

`expiredOnlyAfterLeaseEnds` did not hold with CL-014 alone. After a crash
between a successful `Heartbeat` and its status write, or a `Heartbeat`
answer lost in transit, CL-014 expired a claim whose Lease battery had just
renewed (#57). The controller now writes the relayed `renewTime` with
battery's expiry in one write (CL-010), so after such a crash or lost
answer the renewal is still pending: CL-014 and CL-016 wait, and the
`Heartbeat` is sent again. The scenario tests
`renewedClaimKeptAfterCrashTest`, `renewedClaimRenewedAgainAfterCrashTest`
and `heartbeatAnswerLostTest` show the claim kept Bound with battery's
expiry.

`pendingRenewalKeptWhileHeld` did not hold while CL-014 expired a claim as
soon as its expiry passed: battery keeps a Lease past its expiry until the
next sweep, and a `Heartbeat` then still renews it (BA-010, BA-020), so a
renewal asked for in time and not yet relayed was lost (#85). CL-014 and
CL-016 now wait for a pending renewal, and `renewalNotLostToExpiryTest`,
`renewalKeptPastExpiryTest` and `renewalKeptAcrossRestartTest` show the
claim kept. A claim with no renewal pending still goes Expired before the
sweep (`unrenewedClaimExpiresBeforeSweepTest`): that is what `Expired`
means (02-claims.md, Renewal).

One property does not hold, and is kept out of `safety`, with a scenario
test that reaches the violation:

| Property | Checks | Finding |
|----------|--------|---------|
| `orphanGoneByExpiry` | an orphan is gone within the expiry threshold, as ADR 0001, consequence 2, says; 02-claims.md, Binding, now states `orphanLeaseBounded` | the sweep can come up to one interval later (#86; `orphanOutlivesExpiryTest`) |

The witnesses show that the simulation reaches the interleavings that
matter: an orphan (`witnessOrphan`), an orphan beside a claim bound after
the retry (`witnessOrphanBesideBound`), an expired claim, a released one,
a claim expired by each of CL-013 and CL-014, an orphan from a `ClaimVM`
answer lost in transit (`witnessOrphanFromLostAnswer`), and a retried
`ReleaseVM` that finds the Lease unknown after a lost answer
(`witnessReleasedAfterLostAnswer`), a Lease past its expiry waiting for
the sweep (`witnessUnsweptLease`), and a renewal the controller can relay
for such a Lease (`witnessLateHeartbeat`). `make quint` fails if a witness is
never reached. A renewal kept past its expiry needs a renewal late in the
Lease and time passing before it is relayed, which random simulation
reaches too rarely for a witness; the scenario tests above cover it.

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

## The pools and inventory model

`pools.qnt` has three Hosts, two Pools and battery v0.3.3 as a sidecar
that reads its Hosts from its configuration only when it starts (ADR 0001,
consequence 1). The steps are:

- **the Inventory Controller:** a Node is a Host while it exists, is
  schedulable and its Node report says ready (IN-001). A change in that
  settles for the settle time (IN-011). When a settled change is waiting,
  the controller opens a restart window; when the window closes, it writes
  every change settled by then to battery's configuration and restarts
  battery once (IN-010, IN-012). A window whose changes flapped back closes
  without a restart.
- **the Pool Controller:** add the finalizer, `CreatePool`, `UpdatePool` on
  a new generation or a new set of matching Hosts, the `NoEligibleHost`
  condition, and `DeletePool` under the finalizer (PO-001 to PO-003,
  PO-010 to PO-012). It resolves a selector against the Hosts battery runs
  with whose Nodes exist.
- **battery:** replenishes each Pool on the least loaded Host in its
  `flintlock_hosts`, as its `PickHost` does. It does not check
  `flintlock_hosts` against its Hosts, and provisioning on a Host it does
  not know fails.
- **the environment:** Nodes are labelled, cordoned, uncordoned, deleted
  and created again; Node reports flip; Pools are created, changed and
  deleted; leased MicroVMs are released; time passes. The controllers act
  promptly: time does not pass while a restart window is waiting to open
  or close, or while battery is restarting.

IN-012 defines the restart window this way (#74): restarts are at least a
window apart, and every change settled in a window goes into its restart.

Invariants, all in `safety`:

| Invariant | Checks |
|-----------|--------|
| `poolHostsMatchSelector` | once the Pool Controller is idle, battery holds every live Pool at its current spec, and its `flintlock_hosts` are exactly the Hosts battery runs with that its selector matches (PO-002, PO-010, PO-011) |
| `noEligibleHostWhenNoneMatch` | once the Pool Controller is idle, a Pool says `NoEligibleHost` exactly when its selector matches no Host (PO-012) |
| `finalizerRemovedOnlyAfterDelete` | a Pool whose finalizer the controller removed is not in battery (PO-003) |
| `hostsFollowNodes` | battery's Hosts are the Nodes that are Hosts, except for a change younger than the settle time plus one restart window (IN-001, IN-002, IN-010, IN-011) |
| `noNewVMOnFormerHost` | a cordoned, deleted or not-ready Host gets no new MicroVM once its change has settled and its window has closed (IN-001, IN-002) |
| `restartOnlyForSettledChanges` | a restart applies only changes that have held for the settle time, so a flapping report restarts nothing (IN-011) |
| `restartsOncePerWindow` | battery restarts at most once per restart window (IN-012) |

The bound in `hostsFollowNodes` and `noNewVMOnFormerHost`, the settle time
plus one restart window, is tight: one tick less fails. Until then a
cordoned Host still gets new MicroVMs, as 04-inventory.md says.

Two properties do not hold, and are not in `safety`; a scenario test in
`pools_test.qnt` reaches each:

- `goneLeavesNothing`: a Pool gone from the API server is not in battery.
  Nothing orders `CreatePool` after the finalizer, so a Pool deleted in
  between leaves its battery Pool behind (#72,
  `poolDeletedBeforeFinalizerTest`).
- `poolsNameKnownHosts`: every Pool in battery names only Hosts battery
  knows. After a restart that removes a Host, Pools name it until the Pool
  Controller's `UpdatePool`, and battery's `PickHost` keeps choosing it
  and failing, so the Pool does not replenish (#73,
  `removedHostStallsPoolTest`).

The witnesses show that the simulation reaches a flap absorbed within the
settle time, a restart, a restart that batches two Hosts, two restarts, a
MicroVM placed on a Host whose Node had stopped being a Host (within the
bound), an `UpdatePool` for a changed set of Hosts, a Pool with
`NoEligibleHost`, a Pool deleted under its finalizer, and battery picking a
Host it does not know.

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
