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
| `claims_replay.qnt` | The traces of `claims.qnt` that the Claim Controller's replay test replays: `claims.qnt`'s steps, weighted towards the controller's (#62) |
| `certificates.qnt` | Certificate approval and signing, and the address pins (CT-001 to CT-020; EA-061, EA-062, EA-068; ADR 0003 and ADR 0004) |
| `certificates_test.qnt` | Scenario tests for `certificates.qnt` |
| `pools.qnt` | Pools, placement and inventory: the Pool Controller and the Inventory Controller against battery (PO-001 to PO-004, PO-010 to PO-012, PO-030 to PO-032, IN-001 to IN-013, DP-007, DP-008, BA-061, BA-070, BA-072; ADR 0001, consequence 1) |
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
  witnessReleasedAfterLostAnswer witnessUnsweptLease witnessLateHeartbeat \
  witnessIdleWithBound witnessIdleWithBoundAfterRestart \
  witnessExpiryRecordedForHeldLease witnessExpiredWithPendingRenewal
quint run specs/quint/certificates.qnt --invariant safety --max-steps 60 --max-samples 20000 \
  --witnesses witnessAgentFullyCertified witnessForeignApprovalFailed witnessForeignApprovalSigned \
  witnessAnotherHostDenied witnessSubjectIgnored witnessApprovedAwaitingCA \
  witnessKubeletLieRefused witnessRepinned
quint test specs/quint/pools_test.qnt
quint run specs/quint/pools.qnt --invariant safety --max-steps 60 --max-samples 20000 \
  --witnesses witnessFlapAbsorbed witnessRestart witnessBatchedRestart witnessTwoRestarts \
  witnessRenewalRestart witnessRenewalWithHostChange \
  witnessVMOnFormerHost witnessPlacementUpdated witnessNoEligibleHost \
  witnessPoolDeleted witnessDrainedForPool witnessRejectedWithoutHost \
  witnessPoolDrained witnessDeletionWaitsForClaim
```

A violation prints the trace that breaks the invariant and the seed that
reproduces it (`--seed`). `--invariants a b c` in place of `--invariant
safety` names the invariant that broke. `QUINT_MAX_STEPS`,
`QUINT_MAX_SAMPLES` and `QUINT_SEED` size and fix the simulations that
`make quint` runs.

### Bounded checks

```sh
make quint-verify                                   # every check below; slow
make quint-verify CHECKS="claims:deletedClaimGone"  # one of them
```

`make quint` simulates: it samples behaviours at random, deep but not all
of them. `make quint-verify`
([hack/quint-verify.sh](../../hack/quint-verify.sh)) checks, with
[Apalache](https://apalache-mc.org), every behaviour up to a bound:

| Check | What |
|-------|------|
| `claims:safety`, `certificates:safety`, `pools:safety` | every behaviour of up to `QUINT_VERIFY_STEPS` steps keeps the model's `safety` |
| `claims:<property>` | a liveness property of `claims.qnt` ([below](#liveness)): no behaviour of up to `QUINT_VERIFY_LIVENESS_STEPS` steps that ends in a loop meets its assumptions and never gets there |
| `claims:witness<Property>` | the property's witness, which must be violated: some such behaviour meets the assumptions and reaches the property's left-hand side, so the property is not checked only where it holds vacuously |

The checks are slow, minutes each at small bounds, and each step more of
bound costs more than the one before. So they are not in `ci.yml` and not
a required check.
[.github/workflows/quint-verify.yml](../../.github/workflows/quint-verify.yml)
runs them, one per job, on a PR that changes the models or the checks,
weekly on `main`, and by hand. `dagger call quint-verify
--checks="claims:safety"` runs them locally the same way.

The bounds are small, `QUINT_VERIFY_STEPS=5` and
`QUINT_VERIFY_LIVENESS_STEPS=6`, and so is what they cover: a claim takes
five steps to be created, get its finalizer and bind. They find a mistake
in a model's first steps that simulation might miss, and show that the
liveness properties hold at all; the simulations' 60 steps do the rest.
On GitHub's runners, at these bounds, `claims:safety` takes about ten
minutes and `pools:safety` about twenty; each step more of bound costs
several times the step before.

Apalache needs Java 17 or later, which devbox does not install; `dagger
call quint-verify` has one. The script installs Apalache 0.56.1, the
release quint 0.32.0's `quint verify` runs, into `$QUINT_HOME`, checked
against its release checksum. It compiles each model with `quint compile
--target json` and hands that to Apalache's own command line, as `quint
verify` does through a gRPC server; with quint 0.32.0 that server can
leave `quint verify` waiting after Apalache has answered. A counterexample
is left in `_apalache-out/`, as TLA+ (`violation1.tla` lists its states)
and as an ITF trace.

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
| `vmLeasedToAtMostOneClaim` | a MicroVM is leased to at most one claim (BA-005, BA-003, CL-008) |
| `releasedVMNeverReused` | a released or expired MicroVM is never handed out again (BA-006) |
| `orphanLeaseBounded` | an orphan, from a crash or a `ClaimVM` answer lost in transit, is never renewed (CL-019), and is gone within the expiry threshold plus one sweep interval, or one sweep interval after battery starts (BA-003, BA-020, BA-022) |
| `finalizerRemovedOnlyAfterRelease` | a claim is gone only after battery has released its Lease (CL-020) |
| `leaseOnlyUnderFinalizer` | a Lease is claimed and recorded only under the finalizer (CL-001) |
| `statusExpiryIsBatterys` | a claim's `leaseExpiresAt` is never later than battery's (CL-011, BA-010) |
| `onlyRecordedLeasesReleased` | the controller releases only Leases a claim recorded (CL-031) |
| `recordedOnlyFromOwnAnswer` | a claim records only a lease id from an answer to its own `ClaimVM`, so no orphan is adopted (CL-008) |
| `boundClaimIsComplete` | a Bound claim carries its lease id, MicroVM, node name, time of binding and a true `Bound` (CL-002, RS-024); its expiry comes later, from `ListLeases` (CL-016), since battery's answer to `ClaimVM` carries none |
| `idleMirrorsBattery` | once the controller is idle, every Bound claim's Lease is one battery holds and has not run out (CL-032, CL-013, CL-014) |
| `expiredOnlyAfterLeaseEnds` | a claim goes Expired only once battery no longer holds its Lease or the Lease has run out (CL-016, #57) |
| `pendingRenewalKeptWhileHeld` | a claim with a renewal the controller has not relayed goes Expired only once battery no longer holds its Lease, and so would refuse the `Heartbeat` (CL-010, CL-014, CL-016, #85) |

`idleMirrorsBattery` is the safety form of "an unrenewed Bound claim
eventually goes Expired", since `quint run` checks state invariants only;
`unrenewedClaimExpires` ([Liveness](#liveness)) is the property itself.
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
| `orphanGoneByExpiry` | an orphan is gone within the expiry threshold, as ADR 0001, consequence 2, first said; 02-claims.md, Binding, and the ADR's correction note now state `orphanLeaseBounded` | the sweep can come up to one interval later (#86; `orphanOutlivesExpiryTest`) |

The witnesses show that the simulation reaches the interleavings that
matter: an orphan (`witnessOrphan`), an orphan beside a claim bound after
the retry (`witnessOrphanBesideBound`), an expired claim, a released one,
a claim expired by each of CL-013 and CL-014, an orphan from a `ClaimVM`
answer lost in transit (`witnessOrphanFromLostAnswer`), a retried
`ReleaseVM` that finds the Lease unknown after a lost answer
(`witnessReleasedAfterLostAnswer`), a Lease past its expiry waiting for
the sweep (`witnessUnsweptLease`), and a renewal the controller can relay
for such a Lease (`witnessLateHeartbeat`). Others make an invariant's
left-hand side true, so it is not checked only where it holds trivially:
the controller idle with a claim Bound, before and after a battery restart
(`witnessIdleWithBound`, `witnessIdleWithBoundAfterRestart`, for
`idleMirrorsBattery`), an expiry recorded for a Lease battery holds
(`witnessExpiryRecordedForHeldLease`, for `statusExpiryIsBatterys`), and
a claim Expired with a renewal pending
(`witnessExpiredWithPendingRenewal`, for `pendingRenewalKeptWhileHeld`).
The rest have theirs among the witnesses above. `make quint` fails if a
witness is never reached. A renewal kept past its expiry needs a renewal late in the
Lease and time passing before it is relayed, which random simulation
reaches too rarely for a witness; the scenario tests above cover it.

### Liveness

The invariants hold for a controller that does nothing: a finalizer that
is never removed satisfies `finalizerRemovedOnlyAfterRelease`. These
properties say that the controller, and battery, get somewhere. Each is
`assumptions implies property`, and takes only the assumptions it needs.
`make quint-verify` checks them ([Bounded checks](#bounded-checks)).

| Property | Checks | Assumes | From |
|----------|--------|---------|------|
| `deletedClaimGone` | a deleted claim is eventually gone, its finalizer removed, with a Lease or without (CL-020, CL-021) | battery eventually up; the controller eventually stable; the controller fair | `init` |
| `pendingClaimBinds` | a Pending claim eventually binds, is deleted, or battery has no MicroVM left to give it (CL-001 to CL-003) | those, and battery replenishing its Pool | `init` |
| `unrenewedClaimExpires` | a Bound claim with no renewal pending whose Lease has run out eventually goes Expired, unless its Holder renews or deletes it first (#45; CL-032, CL-014) | those of `deletedClaimGone` | `CLAIM_RUN_OUT` |
| `orphanEventuallyGone` | an orphaned Lease that has run out is eventually gone, whatever the controller does (ADR 0001, consequence 2; CL-019, BA-020) | battery eventually up; its sweep | `ORPHAN_RUN_OUT` |

The assumptions, each a `temporal` in `claims.qnt`:

- `batteryEventuallyUp`: battery eventually stays up. A battery that stops
  for ever, or again and again, fails every call.
- `controllerEventuallyStable`: the Operator eventually stops crashing, and
  battery restarting. A controller that crashes again and again between
  `ReleaseVM` and the finalizer write never removes the finalizer.
- `controllerFair`: weak fairness on each of the Claim Controller's steps
  that succeeds, for the claim, and on its recovery and receiving events.
  The failures in transit never disable those steps, so this says that a
  call battery would answer is eventually made and answered, however often
  it fails first. The environment gets no fairness: Holders, crashes,
  battery stopping, dropped events, failed calls and time may come, or
  not.
- `replenishFair`, `sweepFair`: weak fairness on battery replenishing its
  Pool, and on its sweep.

How Apalache checks them shapes how they are written. Each point is in
the model's comments too:

- It looks for a counterexample that ends in a loop back to a state it has
  been in, within the bound, and only at behaviours that go on for ever.
  The checks take `stepOrStutter`, a step or no change, so that a
  behaviour that gets stuck, with no step enabled, stutters for ever and
  counts, as in TLA+.
- It takes no `weakFair`. Each step given fairness is disabled once taken,
  or can be taken only finitely often, so weak fairness on it is the same
  as its guard being false again and again, `always(eventually(not(guard)))`,
  which it does take. The guards are written out beside the assumptions;
  keep them in step with the actions.
- It takes no quantifier over a temporal formula, nor a temporal operator
  with parameters. The properties are stated for one claim, `LIVE_CLAIM`,
  by symmetry. `orphanEventuallyGone` says that again and again no orphan
  that has run out is left, which is the same as each one going, since an
  orphan stays one until battery deletes it and new ones stop coming.
- Its cost grows steeply with the bound. `stepOrStutter` is
  `stepOf(Set(LIVE_CLAIM))` or no change: the claim's own steps and every
  step that is no claim's, so crashes, battery stopping and starting,
  failed calls and time all still come, but the other two claims stay
  unborn.
- Time is unbounded, so a loop is a stretch in which time stands still,
  and time passing cannot be assumed. The properties about a Lease running
  out ask about one that has run out already. Getting there takes a claim
  five steps to bind and three ticks, too many for an affordable bound, so
  those two start from a state where it has: `CLAIM_RUN_OUT`, the claim
  Bound at time 0 with time at its expiry, and `ORPHAN_RUN_OUT`, an orphan
  claimed at time 0 and time at its expiry. `claimRunOutReachableTest` and
  `orphanRunOutReachableTest` show each is reachable from `init`.
- Apalache 0.56.1 fails with a `ClassCastException` on a chain of three or
  more `and`s inside an `if` in a lambda, such as `ctlRecover`'s, once it
  checks a temporal property. `ctlRecover` and `ctlEventDeleted` write
  theirs as `and { }`, which it takes, and which means the same.

Each property has a witness, `witness<Property>`, that `make
quint-verify` requires to be violated: a behaviour within the bound that
meets the assumptions and reaches the property's left-hand side. Without
one, the property would hold at that bound only because no behaviour gets
there.

### Replaying its traces against the Claim Controller

`internal/controller/claim_replay_test.go` replays traces of `claims.qnt`
against the Claim Controller's subreconciler chain, with controller-runtime's
fake client, a scripted battery and a fake clock, and compares the claims,
battery's Leases and what the controller holds in memory with the trace
after every step (#62). `make claims-traces`
([hack/claims-traces.sh](../../hack/claims-traces.sh)) writes the traces to
`internal/controller/testdata/claims-traces`: it simulates
`claims_replay.qnt`, whose `replayStep` takes `claims.qnt`'s steps weighted
towards the controller's, with a fixed seed and quint's `--mbt` metadata,
which names each step and its picks, and keeps a few traces that between
them take every step. `make quint` fails if the committed traces are not
what the model writes now, so a change to `claims.qnt`'s steps or state
runs `make claims-traces` and commits the new traces, and `make test`
replays them.

Where the model and the controller are known to differ, the test's
`knownDivergences` names the difference and its issue, and the replay of a
trace stops at the step that reaches it.

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
  checks of CT-010 to CT-015, then deny it or approve and sign it; or, for
  a request already approved by anyone, mark it Failed or sign it (CT-006,
  CT-008).
  It sets the subject itself (CT-007) and signs with its signer name's CA
  (CT-002). Signing a serving certificate for a Node with no pin pins its
  address to the Node in the same step (CT-016), and nothing else changes
  a pin (CT-017).
- **the environment:** the CA Secrets can be unreadable, so an approved
  request waits for its signature. A compromised Host's kubelet lists any
  addresses on its own Node (`kubeletReportsAddresses`), and an
  administrator clears a pin (`adminClearsPin`).

Invariants, all in `safety`:

| Invariant | Checks |
|-----------|--------|
| `noCertForAnotherNode` | every certificate names only its requester's own Host's addresses and SPIFFE IDs, whatever a compromised kubelet lists (CT-010, CT-011, CT-014, CT-015 to CT-017; ADR 0003, consequence 1) |
| `onlyExecAgentsCertified` | only the Exec Agent's ServiceAccount, bound to a Node, obtains a certificate (CT-010, CT-011, CT-014) |
| `signedOnlyWhenApproved` | nothing is signed without `Approved` (CT-003) |
| `signedOnlyAfterChecks` | nothing is signed without passing every check, whoever approved it (CT-006) |
| `foreignApprovalNeverBypassesChecks` | a request someone else approved has a certificate only if it passed every check when signed (CT-006) |
| `pinsAreTheHostsOwn` | every pin is an address of its own Host, and no address is pinned to two Nodes (CT-015, CT-016) |
| `subjectIsRequesterNode` | every certificate's subject is the requester's Node (CT-007) |
| `identitiesInTrustDomain` | every certificate's SPIFFE ID is in the configured trust domain, and never battery's (CT-020) |
| `caAndUsagesMatchSigner` | the signer name's CA and key usages, and no DNS name (CT-002, CT-012) |
| `onlyOurSignerNames` | the Operator does nothing with another signer name (CT-001) |
| `refusalNamesTheCheck` | a `Denied` or `Failed` condition names the check the request failed when the Operator refused it (CT-013, CT-008) |
| `unapprovedFailingDenied` | once the Operator is idle, every request for its signer names that fails a check and is not approved is Denied (CT-013) |
| `approvedFailingFailed` | an approved, unsigned request for its signer names that fails a check is Failed once the Operator is idle (CT-008) |
| `idleSettled` | once the Operator is idle and can read its CAs, every request for its signer names is signed, Denied or Failed |

`noCertForAnotherNode` checks the addresses a Host really holds, not the
ones its Node lists. A compromised Host's kubelet can list another Host's
address on its own Node (#75); `kubeletReportsAddresses` does that in
`step`, and the pins keep the invariant: `compromisedKubeletTest` shows
the requests it leads to refused for `CheckAddressPinned`. Pinning is
trust on first use, and the model assumes it in two places: a kubelet
lists addresses that are not its Host's only once its Node has a pin, and
an administrator clears only an uncompromised Host's pin
(`kubeletLiesOnlyAfterFirstUseTest`, `noClearingACompromisedPinTest`).
Without them, a compromised Host could pin an address no Node has pinned
yet, as 09-certificates.md#approval says.

A Node's addresses and the pins change after the Operator decides on a
request, so the invariants about refusals and signatures use what the
review said when the Operator decided (the history's `signedFailing` and
`refusedFor`), not what it says now.

A request someone else approved can't be Denied, since the API server
lets nobody withdraw `Approved` or add `Denied` beside it. CT-013 denies
only a failing request that is not approved, and CT-008 marks an approved
one Failed (#76); `failedOrDeniedTest` shows both.

A Host whose Node is gone still obtains its client certificate: CT-011,
unlike CT-010 and CT-014, does not ask for an existing Node, and the code
agrees (`nodeNotFoundTest`).

Not modelled: CT-004 (durations), CT-005 (publishing the CAs), the
Manifests (CT-021 to CT-023, EA-067), and the Exec Agent's handling of what
it receives (EA-063 to EA-066). Only the Operator signs: it alone holds the
CA keys.

The witnesses show an Exec Agent with all three certificates, a request
someone else approved both Failed and signed, a compromised agent denied
for another Host's address or SPIFFE ID, a requested subject ignored, an
approved request waiting for the CA, an address a kubelet listed refused
by its Node's pin, and a Node pinned again after an administrator cleared
its pin.

## The pools and inventory model

`pools.qnt` has three Hosts, two Pools and battery v0.3.3 as a sidecar
that reads its Hosts from its configuration, and its client certificate,
only when it starts (ADR 0001, consequence 1; BA-061). The steps are:

- **the Inventory Controller:** a Node is a Host while it exists, is
  schedulable and its Node report says ready (IN-001). A change in that
  settles for the settle time (IN-011). When a settled change is waiting,
  the controller opens a restart window; when the window closes, it writes
  every change settled by then to battery's configuration and restarts
  battery once (IN-010, IN-012). A renewed client certificate opens a
  window too, or joins the one open, and goes into the same restart
  (DP-007, DP-008). A window whose changes flapped back, with nothing
  renewed, closes without a restart. Before a restart that removes Hosts,
  it publishes the Hosts that remain and restarts only once no Pool in
  battery names a leaving Host, or, standing for the drain timeout, once
  the Pool Controller has nothing left to do (IN-013); a restart for a
  renewal alone removes no Host and does not wait. It publishes a joining
  Host only once battery has restarted with it.
- **the Pool Controller:** add the finalizer, `CreatePool` once the
  finalizer is stored, `UpdatePool` on a new generation or a new set of
  matching Hosts, the `Rejected` condition for a spec battery refuses, the
  `NoEligibleHost` condition, and `DeletePool` under the finalizer (PO-001
  to PO-004, PO-010 to PO-012). While battery refuses to delete a Pool, it
  drains it: the drained spec, a size of 0 here, then a claim and release
  of each available MicroVM, never one a claim holds (PO-030 to PO-032).
  It resolves a selector against the Hosts the Inventory Controller has
  published whose Nodes exist.
- **battery:** replenishes each Pool up to its size on the least loaded
  Host in its `flintlock_hosts`, as its `PickHost` does, so a Pool of size
  0 gets none (BA-072). It does not check `flintlock_hosts` against its
  Hosts, and provisioning on a Host it does not know fails. It refuses
  `DeletePool` while the Pool owns a MicroVM (BA-070). It refuses a spec
  whose heartbeat expiry threshold is not positive, which stands for every
  spec it refuses (PO-004). The model has no replenishment strategies,
  hooks, provisioning phase or quarantine (10-battery.md, stand-ins).
  Its replenishment whenever a Pool is below its size stands for battery
  together with the Pool Controller's reseed of a stalled Pool (PO-035 to
  PO-038), which works around battery v0.3.3 seeding an event-driven Pool
  only once (BA-075, BA-076).
- **the environment:** Nodes are labelled, cordoned, uncordoned, deleted
  and created again; Node reports flip; Pools are created, changed (to a
  spec battery refuses, too) and deleted; claims lease available MicroVMs
  and release them; cert-manager renews battery's client certificate;
  time passes. The controllers act promptly: time does not pass while a
  restart window is waiting to open or close, or while battery is
  restarting.

IN-012 defines the restart window this way (#74): restarts are at least a
window apart, and every change settled in a window goes into its restart.

Invariants, all in `safety`:

| Invariant | Checks |
|-----------|--------|
| `poolHostsMatchSelector` | once the Pool Controller is idle, battery holds every live Pool whose spec it accepts at its current spec, and its `flintlock_hosts` are exactly the published Hosts that its selector matches (PO-002, PO-010, PO-011) |
| `noEligibleHostWhenNoneMatch` | once the Pool Controller is idle, a Pool whose spec battery accepts says `NoEligibleHost` exactly when its selector matches no Host (PO-012) |
| `rejectedStandsOverNoEligibleHost` | once the Pool Controller is idle, a Pool whose spec battery refuses says `Rejected`, whether or not its selector matches a Host (PO-004 over PO-012, #115) |
| `finalizerRemovedOnlyAfterDelete` | a Pool whose finalizer the controller removed is not in battery (PO-003) |
| `goneLeavesNothing` | a Pool gone from the API server is not in battery: `CreatePool` waits for the stored finalizer (PO-001, PO-003, #72) |
| `vmsBelongToPools` | every MicroVM battery holds belongs to a Pool battery holds: deleting a Pool never orphans its MicroVMs (BA-070, PO-003, #81) |
| `drainLeavesLeases` | the drain of a deleted Pool never takes a MicroVM a claim holds: battery holds exactly the MicroVMs leased and not yet released (PO-032) |
| `hostsFollowNodes` | battery's Hosts are the Nodes that are Hosts, except for a change younger than the settle time plus one restart window (IN-001, IN-002, IN-010, IN-011) |
| `noNewVMOnFormerHost` | a cordoned, deleted or not-ready Host gets no new MicroVM once its change has settled and its window has closed (IN-001, IN-002) |
| `restartOnlyForSettledChanges` | a restart applies only changes that have held for the settle time, or a renewed certificate, so a flapping report restarts nothing (IN-011, DP-007) |
| `restartsOncePerWindow` | battery restarts at most once per restart window, for Host changes and renewals alike (IN-012, DP-008) |
| `poolsNameKnownHosts` | while battery runs, every Pool in battery names only Hosts battery knows, so `PickHost` never picks one it cannot provision on; the one exception is a Pool the Pool Controller cannot update when the drain timeout passes, until it is next written (IN-013, #73) |
| `certificateFollowsSecret` | while battery runs, it presents the certificate its Secret holds, except for at most one restart window after a renewal (DP-007) |

The bound in `hostsFollowNodes` and `noNewVMOnFormerHost`, the settle time
plus one restart window, is tight: one tick less fails. Until then a
cordoned Host still gets new MicroVMs, as 04-inventory.md says.

`poolsNameKnownHosts` was a finding (#73): before IN-013, a restart that
removed a Host left the Pools naming it until the Pool Controller's
`UpdatePool`, and battery's `PickHost` kept choosing it and failing.
`removedHostDrainedTest` walks that scenario now, and
`restartWaitsForThePoolsTest` shows the restart refused while the Pool
still names the Host. The Go code stands the drain timeout's wait for a
Pool the Pool Controller cannot update; the model lets the timeout pass
only once the Pool Controller has nothing left to do, which leaves a Pool
whose spec battery refuses, since `UpdatePool` cannot carry its Hosts
without its spec (`refusedPastDrainTest`).

`goneLeavesNothing` was a finding (#72): before PO-001 ordered `CreatePool`
after the finalizer, a Pool deleted in between left its battery Pool
behind. `createBeforeFinalizerTest` shows `CreatePool` refused before the
finalizer, and `poolDeletedBeforeFinalizerTest` a Pool deleted then leaving
nothing in battery.

`vmsBelongToPools` holds because battery refuses to delete a Pool that owns
MicroVMs (BA-070, #81). The model used to delete a Pool's MicroVMs with it,
which battery v0.3.3 does not do, and so hid that a filled Pool could never
be deleted. `deleteFilledPoolTest` walks a deleted Pool through its
drained spec, the drain of its available MicroVM and the wait for the one
a claim holds; `deleteRefusedWhileFilledTest` and `drainTakesNoLeaseTest`
show the refusal and a drain refused a leased MicroVM.

The witnesses show that the simulation reaches a flap absorbed within the
settle time, a restart, a restart that batches two Hosts, two restarts, a
MicroVM placed on a Host whose Node had stopped being a Host (within the
bound), an `UpdatePool` for a changed set of Hosts, a Pool with
`NoEligibleHost`, a Pool deleted under its finalizer, a Pool that says
`Rejected` while its selector matches no Host, a Pool dropping a
leaving Host before the restart that removes it, a deleted Pool drained of
an available MicroVM, and a drained Pool waiting for a claim's.

## Conventions

- One module per system, and the shared types in `types.qnt`.
- A model's invariants are named for what they check. Each one names the
  requirement or ADR consequence it models in its comment.
- Every invariant CI checks is in the model's `safety`.
- An invariant that is an implication has a witness that makes its
  left-hand side true, listed in `hack/quint.sh`, and another for after a
  crash or a battery restart where that changes how it holds. Otherwise
  the simulation can check it only where it holds trivially and still
  pass.
- A liveness property is a `temporal` of the form `assumptions implies
  property`, states each assumption as a `temporal` of its own, and has a
  witness `witness<Property>` that `hack/quint-verify.sh` requires to be
  violated. Both are listed in `hack/quint-verify.sh` and
  `.github/workflows/quint-verify.yml`.
- A property the requirements do not yet guarantee is a finding. File it
  as an issue and keep it out of `safety`, with a scenario test that
  reaches the violation. Don't change the model to hide it.
- Scenario tests go in `<model>_test.qnt`, as `run` definitions whose names
  end in `Test`.
