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

battery chooses the lease id, and `ClaimVM` carries only the Pool, so
`ClaimVM` cannot be retried safely. If the Operator stops between battery's
answer and the status write, the Lease is orphaned and the next reconcile
claims a second MicroVM. CL-002 keeps that window as short as it can be. The
orphan is bounded: nothing renews it, so battery expires it after the Pool's
`heartbeat_expiry_threshold` and deletes the MicroVM (ADR 0001,
consequence 2). A client-chosen lease id upstream would close the window.

`HostInfo.address` in battery's answer is where battery reaches the Host's
`flintlockd`, not where a consumer reaches the Exec Agent, which is why CL-005
takes the agent's address from the Node report instead.

## Renewal {#renewal}

- **CL-010** When the `spec.renewTime` of a Bound claim changes, the Claim
  Controller SHALL call battery's `Heartbeat` for the claim's Lease and write
  the expiry time battery returns to the claim's status.
- **CL-011** The Claim Controller SHALL take a claim's Lease expiry time only
  from battery's answer to `Heartbeat` or `ClaimVM`, and SHALL NOT compute it.
- **CL-012** If battery reports a claim's Lease as unknown or expired, then
  the Claim Controller SHALL set the claim's phase to `Expired` and its
  condition `Bound` false with the reason `LeaseExpired`.
- **CL-013** When battery's `Events` stream reports that the MicroVM of a
  Bound claim was deleted, the Claim Controller SHALL set the claim's phase
  to `Expired` and its condition `Bound` false with the reason
  `LeaseExpired`.
- **CL-014** When the Lease expiry time battery gave for a Bound claim has
  passed and no `Heartbeat` for that claim has succeeded since, the Claim
  Controller SHALL set the claim's phase to `Expired` and its condition
  `Bound` false with the reason `LeaseExpired`.

Renewal is relayed (ADR 0001, consequence 3): how long a Lease survives now
includes the time the Claim Controller takes to react to a changed
`renewTime`. A Holder renews well inside the Pool's heartbeat interval for
that reason, and CL-011 keeps the expiry a consumer reads the one battery
will enforce.

A Holder that stops renewing sends no `renewTime`, so CL-010 and CL-012 alone
would leave its claim `Bound` after battery has expired the Lease (#45, found
by the Quint model of the claim lifecycle). battery v0.1.0 has no call that
reads a Lease without renewing it, so the Claim Controller learns of the
expiry from battery's `Events` stream (CL-013), and from the expiry time
battery itself gave when the stream has missed it (CL-014). CL-014 compares
against battery's own time, so it does not break CL-011.

## Release {#release}

- **CL-020** When a claim that has a lease id is deleted, the Claim
  Controller SHALL call battery's `ReleaseVM` for the Lease, and SHALL remove
  the finalizer only once battery has released the Lease or reported it
  unknown.
- **CL-021** When a claim that has no lease id is deleted, the Claim
  Controller SHALL remove its finalizer without calling battery.

A released MicroVM is deleted by battery and never reused.

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
is unwanted.
