# The Claim Controller

The Claim Controller turns each `MicroVMClaim` into battery's `Lease` calls
and mirrors battery's answers into the claim's status (ADR 0001, decision 4).
battery is the authority over Leases; a claim's status is a copy of what
battery said (consequence 4).

## Binding {#binding}

- **CL-001** When a claim that has no lease id in its status is
  reconciled, the Claim Controller SHALL add the finalizer
  `battery.liquidmetal-x.dev/release` to the claim before it calls battery's
  `ClaimVM` for the claim's Pool.
- **CL-002** When battery's `ClaimVM` succeeds for a claim, the Claim
  Controller SHALL write the lease id, the MicroVM's uid, the Host's name as
  the node name and the time of binding to the claim's status, set the phase
  to `Bound` and set the condition `Bound` true, before it makes any other
  call to battery for that claim.
- **CL-003** If battery's `ClaimVM` fails because the Pool has no warm
  MicroVM, then the Claim Controller SHALL keep the claim in the phase
  `Pending` with the condition `Bound` false and the reason `PoolExhausted`,
  and SHALL retry with backoff.
- **CL-004** If the claim's Pool does not exist, then the Claim Controller
  SHALL keep the claim in the phase `Pending` with the condition `Bound`
  false and the reason `PoolNotFound`, and SHALL retry with backoff.
- **CL-005** When a claim is Bound, the Claim Controller SHALL set the claim's
  Exec Agent address from the Node report of the claim's Host.
- **CL-006** If the Node report of a Bound claim's Host carries no Exec Agent
  address, then the Claim Controller SHALL keep the claim Bound and set the
  condition `AgentAvailable` false with the reason `NoAgentAddress`.
- **CL-007** If battery's `ClaimVM` for a claim fails in transit, then the
  Claim Controller SHALL keep the claim in the phase `Pending` with the
  condition `Bound` false and the reason `BatteryUnavailable`, and SHALL
  retry `ClaimVM` with backoff.
- **CL-008** The Claim Controller SHALL write to a claim's status only a
  lease id that battery returned in its answer to a `ClaimVM` call for that
  claim.

battery chooses the lease id, and `ClaimVM` carries only the Pool, so
`ClaimVM` cannot be retried safely. If the Operator stops between battery's
answer and the status write, the Lease is orphaned and the next reconcile
claims a second MicroVM. CL-002 keeps that window as short as it can be. The
orphan is bounded: nothing renews it, so battery expires it after the Pool's
`heartbeat_expiry_threshold` and deletes the MicroVM (ADR 0001,
consequence 2). A client-chosen lease id upstream would close the window.

A `ClaimVM` that fails in transit opens the same window without a crash.
battery commits the Lease before it answers, so an answer lost after that
point orphans the Lease, and the retry of CL-007 claims a second MicroVM.
The orphan is bounded in the same way. battery v0.3.3's `ListLeases` shows
the Pool's Leases, but it cannot say which of them a lost answer carried: a
Lease record names no claim, and another claim's `ClaimVM` in the same Pool
may have been answered and not yet written (CL-002). A Lease picked out of
that list could end up recorded in two claims, so CL-008 rules out adopting
one, and the orphan is left to expire (CL-031). A deadline that passes
while battery runs the Pool's pre-lease hooks costs a MicroVM rather than
orphaning one, which is why `ClaimVM` has a deadline of its own (DP-013).

`HostInfo.address` in battery's answer is where battery reaches the Host's
`flintlockd`, not where a consumer reaches the Exec Agent, which is why CL-005
takes the agent's address from the Node report instead.

## Renewal {#renewal}

- **CL-010** While the `spec.renewTime` of a Bound claim differs from the
  last `renewTime` the Claim Controller relayed for it, the Claim Controller
  SHALL call battery's `Heartbeat` for the claim's Lease, and SHALL write
  the expiry time battery returns and the `renewTime` it relayed to the
  claim's status in one write.
- **CL-011** The Claim Controller SHALL take a claim's Lease expiry time only
  from battery's answer to `Heartbeat` or `ListLeases`, and SHALL NOT
  compute it.
- **CL-012** If battery answers a `Heartbeat` for a claim's Lease with
  `NOT_FOUND`, or leaves the claim's Lease out of its answer to
  `ListLeases`, then the Claim Controller SHALL set the claim's phase to
  `Expired` and its condition `Bound` false with the reason `LeaseExpired`.
- **CL-013** When battery's `Events` stream reports that the MicroVM of a
  Bound claim was deleted, the Claim Controller SHALL set the claim's phase
  to `Expired` and its condition `Bound` false with the reason
  `LeaseExpired`.
- **CL-014** When battery lists a Bound claim's Lease in its answer to
  `ListLeases` with an expiry time that has passed, and the claim has no
  pending renewal, the Claim Controller SHALL set the claim's phase to
  `Expired` and its condition `Bound` false with the reason `LeaseExpired`.
- **CL-015** If battery's `Heartbeat` for a Bound claim fails in transit,
  then the Claim Controller SHALL keep the claim `Bound` with the Lease
  expiry time already in its status, and SHALL retry the `Heartbeat` with
  backoff while the claim is Bound.
- **CL-016** When a Bound claim has no pending renewal, and its status has
  no Lease expiry time or one that has passed, the Claim Controller SHALL
  read the claim's Lease with battery's `ListLeases`, and SHALL keep the
  claim `Bound` and write the expiry time battery lists if that time has
  not passed.
- **CL-017** If the `ListLeases` call of CL-016 fails in transit, then the
  Claim Controller SHALL keep the claim `Bound` and retry the call with
  backoff.
- **CL-018** While battery has not yet answered `ClaimVM` calls for other
  claims, the Claim Controller SHALL still reconcile a Bound claim that has
  a pending renewal.

Renewal is relayed (ADR 0001, consequence 3): how long a Lease survives now
includes the time the Claim Controller takes to react to a changed
`renewTime`. A Holder renews well inside the Pool's heartbeat interval for
that reason, and CL-011 keeps the expiry a consumer reads the one battery
will enforce.

A claim has a pending renewal while its `spec.renewTime` differs from the
last one the Claim Controller relayed (glossary). The Claim Controller
records the relayed `renewTime` in the claim's status, as
`status.observedRenewTime`, in the same write as battery's expiry (CL-010),
so a crash cannot record one without the other, and a pending renewal
survives a restart of the Operator. Binding records the `renewTime` the
claim had when `ClaimVM` was called: a new Lease already runs from a time
later than any renewal before it.

battery v0.3.3's answer to `ClaimVM` carries no expiry time, so a newly
Bound claim has none in its status. CL-016 reads it with `ListLeases`
rather than work it out from the Pool's `heartbeat_expiry_threshold`,
which CL-011 forbids.

A Holder that stops renewing sends no `renewTime`, so CL-010 and CL-012 alone
would leave its claim `Bound` after battery has expired the Lease (#45, found
by the Quint model of the claim lifecycle). The Claim Controller learns of
the expiry from battery's `Events` stream (CL-013), and from the expiry time
battery itself lists when the stream has missed it (CL-014, CL-016). Both
compare against battery's own time, so neither breaks CL-011.

A `Heartbeat` that fails in transit may or may not have renewed the Lease.
CL-015 keeps the claim as it was rather than expire it on a network blip,
and the retry is safe: a second `Heartbeat` only moves the expiry again.

Either way, the expiry in a claim's status can be older than battery's. A
`Heartbeat` answer lost in transit, or lost in a crash before the status
write (#57), leaves the Lease renewed in battery and the old expiry in the
status. battery v0.3.3's `ListLeases` reads Leases without renewing them,
so CL-016 asks battery before anything expires the claim on time. A Lease
battery lists with an expiry still to come was renewed since, and the
claim keeps it. A Lease battery does not list is one battery no longer
holds and will never hold again (BA-040, BA-003), and CL-012 applies. One listed
with a passed expiry has run out, and CL-014 applies. While `ListLeases`
fails, the Claim Controller cannot tell, and CL-017 keeps the claim Bound
rather than expire a Lease battery may still hold. The claim's condition
`Synced` shows why (CL-040). battery cannot expire a Lease while it is
down, and its sweep removes the overdue ones once it runs again, so CL-030
settles such claims once battery is back.

A Lease whose expiry has passed is still held by battery until its next
sweep, up to one `sweep_interval` later, and a `Heartbeat` in that time
renews it (BA-010, BA-020). So a Holder whose renewal the Claim Controller
has not relayed yet when the expiry passes would lose a Lease battery was
still willing to keep (#85). CL-014 and CL-016 therefore wait while a
renewal is pending, and CL-010 relays it first: battery either renews the
Lease or answers `NOT_FOUND`, and CL-012 applies. A claim with no pending
renewal goes `Expired` as soon as battery lists its expiry as passed,
without waiting for the sweep: `Expired` means that the Lease has run out,
not that battery has deleted it, and the Exec Agent's check of an
unexpired claim needs no allowance for the sweep. A renewal that reaches
the Claim Controller only after the sweep is too late, which is why a
Holder renews well inside the heartbeat interval.

CL-018 is for the same Holder (#89). battery runs the Pool's pre-lease
hooks inside `ClaimVM`, so a `ClaimVM` can take up to its own deadline
(DP-013), much longer than any other call. The Claim Controller runs
several reconciles at once, a number the Operator's flag
`--claim-concurrent-reconciles` sets, and lets at most one fewer of them
call `ClaimVM` at a time; a claim that finds every such slot taken waits
and retries without calling battery. At least one reconcile is always free
for renewals, expiries and releases, and it never waits for battery's
answer to a `ClaimVM`. Reconciles of one claim never run at once, so
binding still re-reads the claim before its `ClaimVM` as before.

## Release {#release}

- **CL-020** When a claim that has a lease id is deleted, the Claim
  Controller SHALL call battery's `ReleaseVM` for the Lease, and SHALL remove
  the finalizer only once battery has released the Lease or reported it
  unknown.
- **CL-021** When a claim that has no lease id is deleted, the Claim
  Controller SHALL remove its finalizer without calling battery.
- **CL-022** If battery's `ReleaseVM` for a deleted claim fails in transit,
  then the Claim Controller SHALL keep the finalizer and retry `ReleaseVM`
  with backoff.

A released MicroVM is deleted by battery and never reused.

battery v0.3.3 answers `ReleaseVM` with `UNAVAILABLE`, and keeps the Lease,
while `flintlockd` has not confirmed the MicroVM's deletion
(`internal/api/lease.go`); its sweeper retries the deletion meanwhile. The
Operator cannot tell that answer from a lost one, and CL-022 retries both.
A retry after a release whose answer was lost finds the Lease unknown,
which CL-020 takes as released, and so does a retry after a crash between
`ReleaseVM` and the removal of the finalizer.

## Recovery {#recovery}

- **CL-030** When the Claim Controller starts, and when its connection to
  battery is restored, the Claim Controller SHALL reconcile every Bound claim against
  battery's Leases.
- **CL-031** The Claim Controller SHALL NOT release a Lease that battery
  holds and no claim records.

battery's database and the claims can disagree after the Operator or battery
restarts, the latter on every change of Hosts (ADR 0001, consequence 1).
CL-030 with CL-012 makes a claim whose Lease battery has lost show
`Expired`. The opposite case, a Lease with no claim, is the orphan of
[Binding](#binding); CL-031 leaves it to expire rather than guessing that it
is unwanted. battery v0.3.3's `ListLeases` gives CL-030 every Lease in one
call.

## Failed calls {#failed-calls}

- **CL-040** If a call to battery for a claim fails in transit, then the
  Claim Controller SHALL set the claim's condition `Synced` false with the
  reason `BatteryUnavailable`, and SHALL NOT change the claim's phase
  because of that failure.
- **CL-041** If a call to battery for a claim fails with an error that no
  other requirement in this document names, then the Claim Controller SHALL
  set the claim's condition `Synced` false with the reason `BatteryError`,
  SHALL NOT change the claim's phase because of that failure, and SHALL
  retry the call with backoff.
- **CL-042** When battery answers a call for a claim, the Claim Controller
  SHALL set the claim's condition `Synced` true.

battery is the authority over Leases, and a call that fails in transit (see
the glossary) says nothing about what battery did. So a failure never moves
a claim's phase: only an answer from battery, or battery's `Events` stream,
does (CL-012, CL-013, CL-014). The specific cases are CL-007, CL-015,
CL-017 and CL-022, which say what is retried. `Synced` shows a consumer
that the claim's status may be stale, and why, while the phase stays what
battery last said. An answer here is anything battery says about the
Lease or the Pool, `NotFound` and `RESOURCE_EXHAUSTED` included, and not
the `UNAVAILABLE` of a failure in transit. Nor is an error that no
requirement here names, such as `INVALID_ARGUMENT`: it says nothing about
the Lease or the Pool either, so CL-041 applies to it and CL-042 does not
(#89).

While the connection to battery is down, the Operator reports itself not
ready (DP-012), and every claim whose next step needs battery shows `Synced`
false once it tries. A Bound claim that needs no call keeps the status of
battery's last answer until the connection is restored and CL-030
reconciles it.
