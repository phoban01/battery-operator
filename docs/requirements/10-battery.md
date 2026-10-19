# What the Operator assumes battery does

The Operator runs battery unchanged (ADR 0001), so it can only depend on what
battery already does. This document lists those assumptions, each with an
identifier and a pointer into battery's source at
[v0.4.0](https://github.com/liquidmetal-dev/battery/tree/v0.4.0). Unlike the
other documents in this directory, its subject is battery, not a system this
project builds: each statement says what battery v0.4.0 does, as read from
its source.

It is normative for the two things in this repository that stand in for
battery:

- the fake battery (`internal/fakebattery`) cites each assumption it
  implements, and tests them; it cites the ones it does not meet as
  exceptions, and lists them in its package documentation (TD-005);
- the Quint models (`specs/quint/`) cite each assumption their battery steps
  encode, with model citations.

A requirement elsewhere that depends on battery's behaviour should name the
assumption here. When battery changes, this document changes first, and the
fake battery and the models follow it.

Links point at the tag, so line numbers stay valid. Where battery v0.4.0
changed an assumption that held for v0.3.3, the section says so; see also
[Changes since v0.3.3](#since-v033).

## Claiming {#claiming}

- **BA-001** If a Pool has no MicroVM in the phase `AVAILABLE`, then battery
  SHALL answer `ClaimVM` for that Pool with `RESOURCE_EXHAUSTED` and create no
  Lease.
- **BA-002** If battery holds no Pool of the name and namespace a `ClaimVM`
  names, then battery SHALL answer it with `NOT_FOUND`.
- **BA-003** When `ClaimVM` succeeds, battery SHALL choose a new lease id,
  and set the Lease's expiry to the time of the claim plus the Pool's
  `heartbeat_expiry_threshold`.
- **BA-004** Once battery has committed a Lease in `ClaimVM`, battery SHALL
  answer that `ClaimVM` with success.
- **BA-005** battery SHALL move a MicroVM out of the phase `AVAILABLE` in
  the same transaction in which `ClaimVM` selects it, so that two `ClaimVM`
  calls never lease the same MicroVM.
- **BA-006** battery SHALL set a MicroVM's phase to `AVAILABLE` only when it
  finishes provisioning the MicroVM, so that a MicroVM that has been leased
  is never leased again.
- **BA-007** When `ClaimVM` carries a request id that no Lease battery
  holds was created with, and the call succeeds, battery SHALL store the
  request id on the Lease it creates, and SHALL report it in that Lease's
  record in `ListLeases`.
- **BA-008** When `ClaimVM` carries a request id, and battery holds a Lease
  created with that request id for the Pool the call names, whose MicroVM
  has a record and is not in the phase `DELETING`, battery SHALL answer
  with that Lease's id and MicroVM, create no Lease, and leave the Lease's
  expiry unchanged.
- **BA-009** If `ClaimVM` carries a request id, and battery holds a Lease
  created with that request id whose MicroVM is in the phase `DELETING` or
  has no record, then battery SHALL answer it with `ABORTED` and create no
  Lease.

| ID | Source at v0.4.0 |
|----|------------------|
| BA-001 | [`internal/api/lease.go`, `LeaseServer.ClaimVM`, L149-L152](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/api/lease.go#L149-L152); [`internal/store/sqlite.go`, `ClaimAvailableVM`, L478-L480 and L500-L502](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/store/sqlite.go#L478-L502) |
| BA-002 | [`internal/api/lease.go`, `LeaseServer.ClaimVM`, L131-L134](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/api/lease.go#L131-L134) |
| BA-003 | [`internal/api/lease.go`, `LeaseServer.ClaimVM`, L168-L170 and L176-L185](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/api/lease.go#L168-L185) |
| BA-004 | [`internal/api/lease.go`, `LeaseServer.ClaimVM`, L188-L203](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/api/lease.go#L188-L203) |
| BA-005 | [`internal/store/sqlite.go`, `ClaimAvailableVM`, L466-L518](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/store/sqlite.go#L466-L518) |
| BA-006 | [`internal/reconciler/provision.go`, `Provisioner.Provision`, L287-L289](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/provision.go#L287-L289); [`internal/store/sqlite.go`, `ClaimAvailableVM`, L489-L502](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/store/sqlite.go#L489-L502) |
| BA-007 | [`api/proto/poolmgr/v1alpha1/lease.proto`, `ClaimVMRequest.request_id`, L26-L33](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/api/proto/poolmgr/v1alpha1/lease.proto#L26-L33); [`internal/api/lease.go`, `LeaseServer.ClaimVM`, L139-L147 and L176-L185](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/api/lease.go#L139-L185); [`internal/store/sqlite.go`, `leaseColumns`, L523, `insertLease`, L565-L583](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/store/sqlite.go#L523-L583) |
| BA-008 | [`internal/api/lease.go`, `LeaseServer.ClaimVM`, L139-L147](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/api/lease.go#L139-L147), [`LeaseServer.replayClaim`, L210-L235](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/api/lease.go#L210-L235); [`internal/store/sqlite.go`, `GetLeaseByRequestID`, L628-L641](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/store/sqlite.go#L628-L641) |
| BA-009 | [`internal/api/lease.go`, `LeaseServer.replayClaim`, L216-L231](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/api/lease.go#L216-L231) |

The lease id is a random UUID (`uuid.NewString`), so battery never hands the
same one out twice. BA-004 is battery's side of the window between the
commit and the answer: after `LeaseVM` nothing fails the call, so an error
the Operator sees after the commit comes from the transport or the
Operator's own deadline, not from battery (#61). The network interfaces in
the answer are best effort and can be empty.

The request id is new in battery v0.4.0 (its ADR
`docs/adr/2026-09-24-idempotent-claims.md`, liquidmetal-dev/battery#98),
and it is what lets a `ClaimVM` whose answer was lost be retried
(02-claims.md, Retrying `ClaimVM`). An empty request id is the behaviour
before the field existed: the request is not retriable. The Lease row is
the record of the request: the request id is free again once the Lease
ends, by release or by expiry, and there is nothing else to sweep. A
replay fetches the MicroVM's network interfaces from `flintlockd` again,
as a fresh claim does. battery refuses a request id longer than 255 bytes,
and one that a Lease of another Pool was created with, each with
`INVALID_ARGUMENT`. Two `ClaimVM` calls with the same request id can both
pass the lookup and each take a MicroVM; the unique index on the request
id refuses the second Lease, and that call puts its MicroVM back to
`AVAILABLE` and answers with the winner's Lease, as a replay would
(`yieldClaim`, L245-L269). The pre-lease hooks ran on the MicroVM it put
back, which is what the hooks are for.

`ABORTED` is where BA-009 and the retry meet. A release deletes the
MicroVM's row before the Lease's, and marks the MicroVM `DELETING` first
(`EnsureVMDeleted`), so between those points a replay finds the Lease
ending and refuses; once the Lease row is gone the request id is free and
the retry claims afresh. An expiry deletes the Lease row first (BA-023),
so a replay after it claims afresh and never sees `ABORTED`. battery also
answers `ABORTED` when the MicroVM it had taken for the claim is deleted
during the claim, which only a forced `DeletePool` does to a MicroVM
being claimed (`claimAborted`, L112-L114).

`ClaimAvailableVM` selects an `AVAILABLE` MicroVM and moves it to `LEASED`
with an update guarded by `phase = AVAILABLE`, inside one transaction, and
fails if the guard matched no row. The only other write of `AVAILABLE` is
the last step of provisioning a new MicroVM, which has a uid of its own. A
MicroVM once leased goes on to `DELETING` and is removed (BA-023, BA-030),
or is quarantined or deleted after a failed hook, and never comes back. So
battery leases each MicroVM to one Lease at most, once, which the claim
lifecycle model checks as `vmLeasedToAtMostOneClaim` and
`releasedVMNeverReused`.

## Heartbeat {#heartbeat}

- **BA-010** When battery receives a `Heartbeat` for a Lease it still holds,
  battery SHALL set the Lease's expiry to the time of the `Heartbeat` plus
  the Pool's `heartbeat_expiry_threshold` and answer with that expiry,
  whether or not the Lease's previous expiry has passed.
- **BA-011** If battery does not hold the Lease a `Heartbeat` names, then
  battery SHALL answer it with `NOT_FOUND`.

| ID | Source at v0.4.0 |
|----|------------------|
| BA-010 | [`internal/api/lease.go`, `LeaseServer.Heartbeat`, L364-L390](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/api/lease.go#L364-L390); [`internal/store/sqlite.go`, `UpdateLeaseHeartbeat`, L643-L652](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/store/sqlite.go#L643-L652) |
| BA-011 | [`internal/api/lease.go`, `LeaseServer.Heartbeat`, L365-L368 and L383-L386](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/api/lease.go#L365-L386) |

`Heartbeat` reads the Lease row and updates it by lease id alone; neither
step compares the stored expiry with the time. A Lease whose expiry has
passed but that the sweep has not deleted yet ([Expiry](#expiry)) is
therefore renewed like any other. There is no separate answer for an
expired Lease: battery either renews the Lease or answers `NOT_FOUND`. The
same `NOT_FOUND` comes back if the Lease's Pool is gone ([L373-L376](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/api/lease.go#L373-L376)), which
cannot happen while the Lease exists: `DeletePool` refuses while the Pool
owns a leased MicroVM
([`internal/api/pooladmin.go`, L340-L352](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/api/pooladmin.go#L340-L352)),
and with `force` it deletes the Pool's Leases in the same transaction as
the Pool
([`internal/store/sqlite.go`, L261-L263](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/store/sqlite.go#L261-L263)).

## Expiry {#expiry}

- **BA-020** battery SHALL delete an expired Lease only in a sweep, which
  runs once every `sweep_interval` while battery runs, and which deletes
  every Lease whose expiry is at or before the time of the sweep.
- **BA-021** If a `Heartbeat` renews a Lease after a sweep has listed it as
  expired and before the sweep deletes it, then battery SHALL keep the Lease
  with its renewed expiry.
- **BA-022** battery SHALL run its first sweep one `sweep_interval` after it
  starts, not at start.
- **BA-023** When a sweep deletes a Lease, battery SHALL delete the Lease's
  MicroVM through `flintlockd`, and SHALL retry the deletion in every later
  sweep until `flintlockd` confirms it.

| ID | Source at v0.4.0 |
|----|------------------|
| BA-020 | [`internal/reconciler/sweeper.go`, `DefaultSweepInterval`, L15-L17](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/sweeper.go#L15-L17), [`Sweeper.Run`, L92-L104](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/sweeper.go#L92-L104), [`Sweeper.Tick`, L109-L146](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/sweeper.go#L109-L146); [`internal/store/sqlite.go`, `ListExpiredLeases`, L662-L666](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/store/sqlite.go#L662-L666); [`internal/config/config.go`, `SweepInterval`, L30-L33](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/config/config.go#L30-L33) |
| BA-021 | [`internal/reconciler/sweeper.go`, `Sweeper.beginExpiry`, L185-L189](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/sweeper.go#L185-L189); [`internal/store/sqlite.go`, `DeleteLeaseIfExpired`, L680-L719](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/store/sqlite.go#L680-L719) |
| BA-022 | [`internal/reconciler/sweeper.go`, `Sweeper.Run`, L92-L104](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/sweeper.go#L92-L104); [`cmd/poolmgrd/main.go`, L121-L137](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/cmd/poolmgrd/main.go#L121-L137) |
| BA-023 | [`internal/reconciler/sweeper.go`, `Sweeper.beginExpiry`, L198-L217](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/sweeper.go#L198-L217), [`Sweeper.retryPendingDeletions`, L151-L166](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/sweeper.go#L151-L166); [`internal/reconciler/provision.go`, `EnsureVMDeleted`, L425-L428, `EnsureVMDeletedCheckingPool`, L434-L462](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/provision.go#L425-L462) |

`sweep_interval` is set in battery's configuration file and defaults to 10
seconds. The sweep runs on a `time.Ticker`, which first fires one interval
after `Run` starts. Together with BA-010 this means:

- a Lease can outlive its expiry by up to one `sweep_interval` while battery
  runs, and any `Heartbeat` in that time renews it;
- a Lease that expired while battery was stopped survives the restart by up
  to one `sweep_interval`, and can be renewed in that time too;
- a sweep that fails to read the store skips its turn
  ([`Sweeper.Tick`, L115-L118](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/sweeper.go#L115-L118)), so the bound holds only while battery's
  database answers.

The claim requirements allow for the first: a claim with a renewal the
Claim Controller has not yet relayed is not expired while battery still
holds its Lease (02-claims.md, Renewal, #85). The orphan bound in
02-claims.md, Binding, allows for all three (#86).

The Lease row goes before the MicroVM: `DeleteLeaseIfExpired` deletes the
row, then the sweep deletes the MicroVM. Once the Lease is gone, a
`Heartbeat` for it is `NOT_FOUND` (BA-011) even while the MicroVM is still
being deleted.

## Release {#release}

- **BA-030** When battery receives a `ReleaseVM` for a Lease it holds,
  battery SHALL delete the Lease's MicroVM through `flintlockd` and answer
  with success only once `flintlockd` has confirmed the deletion and the
  Lease is deleted.
- **BA-031** If `flintlockd` does not confirm the deletion of a released
  Lease's MicroVM, then battery SHALL answer the `ReleaseVM` with
  `UNAVAILABLE`, keep the Lease, and retry the deletion in every later sweep
  until `flintlockd` confirms it, deleting the Lease then.
- **BA-032** If battery does not hold the Lease a `ReleaseVM` names, then
  battery SHALL answer it with `NOT_FOUND`.

| ID | Source at v0.4.0 |
|----|------------------|
| BA-030 | [`internal/api/lease.go`, `LeaseServer.ReleaseVM`, L402-L458](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/api/lease.go#L402-L458); [`internal/reconciler/provision.go`, `EnsureVMDeletedCheckingPool`, L434-L462](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/provision.go#L434-L462), [`FinishVMRelease`, L501-L511](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/provision.go#L501-L511) |
| BA-031 | [`internal/api/lease.go`, `LeaseServer.ReleaseVM`, L434-L435](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/api/lease.go#L434-L435); [`internal/reconciler/sweeper.go`, `Sweeper.retryPendingDeletions`, L151-L166](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/sweeper.go#L151-L166); [`internal/reconciler/provision.go`, `FinishVMDeletion`, L482-L487](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/provision.go#L482-L487), [`FinishVMRelease`, L501-L511](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/provision.go#L501-L511) |
| BA-032 | [`internal/api/lease.go`, `LeaseServer.ReleaseVM`, L403-L406](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/api/lease.go#L403-L406) |

While a Lease is kept after an `UNAVAILABLE` `ReleaseVM`, it is an ordinary
Lease row: `Heartbeat` renews it (BA-010), `ListLeases` lists it (BA-040),
and a retried `ReleaseVM` tries the deletion again. If its expiry passes
first, the sweep deletes the row (BA-020) and then finishes the MicroVM's
deletion, reporting it as `VM_DELETED_DUE_TO_EXPIRY`; a `ReleaseVM` after
that is `NOT_FOUND`. A `ReleaseVM` for a Lease whose MicroVM record is
already gone deletes the Lease and succeeds ([L450-L457](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/api/lease.go#L450-L457)), so a retry after a
lost answer is safe.

## Reading Leases {#list-leases}

- **BA-040** When battery receives a `ListLeases`, battery SHALL answer with
  every Lease it holds, or every Lease of the one Pool the request names,
  each with its lease id, MicroVM uid, Pool, claim time, last heartbeat time
  and expiry, without renewing any of them.
- **BA-041** battery SHALL include in the answer to `ListLeases` a Lease
  whose expiry has passed but that no sweep has deleted yet.

| ID | Source at v0.4.0 |
|----|------------------|
| BA-040 | [`internal/api/lease.go`, `LeaseServer.ListLeases`, L462-L468](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/api/lease.go#L462-L468); [`internal/store/sqlite.go`, `ListLeases`, L668-L678](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/store/sqlite.go#L668-L678); [`api/proto/poolmgr/v1alpha1/lease.proto`, L20-L21 and L69-L75](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/api/proto/poolmgr/v1alpha1/lease.proto#L20-L75) |
| BA-041 | [`internal/store/sqlite.go`, `ListLeases`, L668-L678](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/store/sqlite.go#L668-L678) |

`ListLeases` is new in battery v0.2.0 (liquidmetal-dev/battery@8eca269).
It is the only call that reads a Lease without changing it. Its query has no
filter on the expiry, so a caller has to compare `expires_at` with the time
itself to tell a live Lease from one waiting for the sweep. A Lease it does
not list is one battery does not hold, and since lease ids are never reused
(BA-003), battery will not hold it again.

## Events {#events}

- **BA-050** When a client subscribes to battery's `Events` service, battery
  SHALL send every event its outbox holds for the Pool the subscription
  names, or for every Pool, and then each new event as battery records it.
- **BA-051** battery SHALL record `VM_DELETED_DUE_TO_EXPIRY` or
  `VM_DELETED_ON_RELEASE` for a leased MicroVM only after `flintlockd` has
  confirmed its deletion.

| ID | Source at v0.4.0 |
|----|------------------|
| BA-050 | [`internal/api/events.go`, `EventsServer.Subscribe`, L57-L89](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/api/events.go#L57-L89) |
| BA-051 | [`internal/reconciler/provision.go`, `FinishVMDeletion`, L481-L492, `FinishVMRelease`, L501-L511](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/provision.go#L481-L511), called after `EnsureVMDeleted` succeeds in [`internal/api/lease.go`, L417-L443](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/api/lease.go#L417-L443) and [`internal/reconciler/sweeper.go`, L157-L164 and L214-L217](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/sweeper.go#L157-L217) |

The outbox is battery's database, and nothing prunes it yet, so a
subscriber that reconnects, including after battery restarts, is sent the
whole history again. Recording an event is best effort
([`EmitEvent`, `internal/reconciler/provision.go`, L403-L411](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/provision.go#L403-L411)):
if the write fails the event is lost for good. A subscriber can therefore
miss an event, but never because it was disconnected. battery never records
`VM_RELEASED`, although the proto defines it, and records
`VM_EXPIRING_SOON` once for each expiry a Lease has, within one
process-wide `warning_window` (30 seconds by default) of it, so a renewed
Lease is warned again
([`Sweeper.Tick`, L131-L135](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/sweeper.go#L131-L135)).

## Restarts {#restarts}

- **BA-060** battery SHALL keep its Pools, MicroVMs and Leases across a
  restart, in its database.
- **BA-061** battery SHALL read the client certificate, key and certificate
  authority of each Host's TLS configuration only when it starts, and SHALL
  present that client certificate to the Host until it exits.
- **BA-062** When battery starts on a database that battery v0.3.3 wrote,
  battery SHALL migrate the database's schema before it serves, and SHALL
  keep the Pools, MicroVMs and Leases in it.

| ID | Source at v0.4.0 |
|----|------------------|
| BA-060 | [`cmd/poolmgrd/main.go`, L74-L77](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/cmd/poolmgrd/main.go#L74-L77); [`internal/store/sqlite.go`, `Open`, L28-L57](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/store/sqlite.go#L28-L57) |
| BA-061 | [`cmd/poolmgrd/main.go`, L65 and L101-L104](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/cmd/poolmgrd/main.go#L65-L104); [`internal/flintlockclient/pool.go`, `New`, L44-L82](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/flintlockclient/pool.go#L44-L82), [`dialCredentials`, L149-L175](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/flintlockclient/pool.go#L149-L175) |
| BA-062 | [`internal/store/sqlite.go`, `Open`, L28-L57, `migrations`, L59-L70, `migrate`, L72-L91](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/store/sqlite.go#L28-L91); [`internal/store/schema.sql`, L1-L3](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/store/schema.sql#L1-L3) |

Leases are not renewed or expired while battery is stopped; after the
restart they are swept as BA-022 says. Calls to battery fail while it is
stopped. The database lasts as long as the file `poolmgrd -db` names, so
whether Leases survive the Operator's pod being replaced, and not only
battery's container restarting, is up to the Manifests (06-deployment.md).

battery v0.4.0 is the first release whose schema differs from an earlier
one. `store.Open` applies `schema.sql`, the version-0 baseline, with
`CREATE TABLE IF NOT EXISTS`, and then the numbered migrations above the
database's `PRAGMA user_version`, each in its own transaction with the
version bump. A v0.3.3 database is at version 0, and migration 1 adds the
Lease's `request_id` column and its unique index; nothing else in the
file changes, so every Pool, MicroVM and Lease comes through. battery
refuses a database whose version is newer than its own migrations list,
so a later battery that adds a migration cannot be rolled back over a
migrated database; v0.3.3 reads no version at all, and its queries name
their columns, so a rollback from v0.4.0 to v0.3.3 works. The e2e suite
upgrades a running Operator from the v0.3.3 sidecar to v0.4.0 with a
claim Bound (test/e2e).

`flintlockclient.New` builds each Host's transport credentials once, at
startup, from a `tls.Config` whose `Certificates` hold the key pair read
then; nothing reads the files again. poolmgrd handles only SIGINT and
SIGTERM, both of which stop it, and watches no file, so a renewed
certificate reaches the Hosts only through a restart (06-deployment.md,
DP-007). The same holds for the certificate authority in `ca_file`.

## Deleting a Pool {#delete-pool}

- **BA-070** If a Pool owns a MicroVM in the phase `LEASED` or
  `PRE_LEASE_HOOK_RUNNING`, then battery SHALL answer a `DeletePool`
  without `force` for it with `FAILED_PRECONDITION` and keep the Pool and
  its MicroVMs.
- **BA-071** If battery holds no Pool of the name and namespace a
  `DeletePool` names, then battery SHALL answer it with `NOT_FOUND`.
- **BA-072** While a Pool's replenishment strategy is `MIN_SIZE_THRESHOLD`
  and its size is 0, battery SHALL provision no MicroVM for it.
- **BA-073** battery SHALL NOT delete a MicroVM in the phase `QUARANTINED`
  other than in a `DeletePool` of its Pool.
- **BA-074** When `UpdatePool` succeeds, battery SHALL stop the Pool's
  reconciler, SHALL apply the hook failure policy of the Pool's previous
  spec to each MicroVM whose provisioning that stops, and SHALL start a
  reconciler with the new spec.

| ID | Source at v0.4.0 |
|----|------------------|
| BA-070 | [`internal/api/pooladmin.go`, `PoolAdminServer.DeletePool`, L340-L352 and L360-L366](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/api/pooladmin.go#L340-L366); [`internal/store/sqlite.go`, `DeletePoolAndMarkVMs`, L240-L252](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/store/sqlite.go#L240-L252) |
| BA-071 | [`internal/api/pooladmin.go`, `PoolAdminServer.DeletePool`, L330-L335 and L367-L369](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/api/pooladmin.go#L330-L369) |
| BA-072 | [`internal/reconciler/strategy.go`, `minSizeThreshold`, L107-L122](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/strategy.go#L107-L122); [`internal/reconciler/reconciler.go`, `Reconciler.Run`, L98-L121](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/reconciler.go#L98-L121) |
| BA-073 | [`internal/reconciler/provision.go`, `ApplyHookFailurePolicy`, L379-L384](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/provision.go#L379-L384); [`internal/reconciler/sweeper.go`, `Sweeper.retryPendingDeletions`, L151-L166](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/sweeper.go#L151-L166); [`internal/reconciler/reconciler.go`, `RecoverAbandonedClaims`, L276-L310](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/reconciler.go#L276-L310); [`internal/store/sqlite.go`, `DeletePoolAndMarkVMs`, L254-L259](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/store/sqlite.go#L254-L259); [`internal/api/lease.go`, `LeaseServer.ReleaseVM`, L403-L406](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/api/lease.go#L403-L406) |
| BA-074 | [`internal/api/pooladmin.go`, `PoolAdminServer.UpdatePool`, L282-L299](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/api/pooladmin.go#L282-L299); [`internal/poolmanager/manager.go`, `StopReconciler`, L154-L156](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/poolmanager/manager.go#L154-L156); [`internal/reconciler/provision.go`, `Provisioner.Provision`, L238-L289](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/provision.go#L238-L289), [`ApplyHookFailurePolicy`, L372-L398](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/provision.go#L352-L398) |

battery v0.4.0's `DeletePool` drains the Pool it deletes
(liquidmetal-dev/battery#83): it counts the Pool's MicroVMs in the phases
`LEASED` and `PRE_LEASE_HOOK_RUNNING`, refuses if there is one unless the
request sets `force`, and otherwise stops the Pool's reconciler, deletes
the Pool row and marks every MicroVM of the Pool `DELETING` in one
transaction, and then deletes each MicroVM through `flintlockd`,
recording `VM_DELETED_ON_POOL_DELETE` for it; a MicroVM whose Host does
not answer is left `DELETING` for the sweep. With `force` it deletes the
leased MicroVMs and their Leases too. battery v0.3.3 refused while the
Pool owned a MicroVM in any phase, and deleted none of them; the Pool
Controller's drain (03-pools.md, PO-030 to PO-034) was written for that,
still works against v0.4.0, since the refusal it reacts to is the one
v0.4.0 still gives, and is replaced by battery's own draining in #199.
None of battery's replenishment strategies deletes a MicroVM when a
Pool's size drops: a strategy only decides how many to create. So, short
of deleting the Pool, a MicroVM goes only these ways:

- a leased MicroVM goes when its Lease is released (BA-030) or expires
  (BA-023);
- an available MicroVM goes only by being claimed and then released;
- a MicroVM that is provisioning becomes available, or goes through the
  Pool's hook failure policy, which deletes it under `DELETE_AND_REPLACE`
  and quarantines it under `QUARANTINE`;
- a quarantined MicroVM never goes (BA-073), until its Pool is deleted.

battery v0.4.0 refuses a `MIN_SIZE_THRESHOLD` strategy without a positive
`min_size`, but not a size of 0, and a `MIN_SIZE_THRESHOLD` Pool of size 0
creates nothing: the tick's target is the size less every MicroVM already
there, and the strategy ignores claims and deletions (BA-072). In battery
v0.4.0 the other two strategies stop at a size of 0 too: a claim or a
deletion provisions the shortfall, which is nothing at size 0
([`internal/reconciler/strategy.go`, L87-L97 and L130-L142](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/strategy.go#L87-L142));
in v0.3.3 they created one MicroVM on every claim or deletion, whatever
the size, which is why the drained spec is a `MIN_SIZE_THRESHOLD` Pool.
A Pool set to `MIN_SIZE_THRESHOLD`, size 0, can be emptied of
its available MicroVMs by claiming each one and releasing it at once; with
no pre-lease commands and the policy `DELETE_AND_REPLACE`, such a claim
runs no hook, and one that fails deletes its MicroVM rather than
quarantining it.

A quarantined MicroVM has no Lease: `ApplyHookFailurePolicy` clears its
lease id, and a pre-lease hook that fails in `ClaimVM` creates none
([`internal/api/lease.go`, L160-L166](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/api/lease.go#L160-L166)).
`ReleaseVM` needs a Lease, the sweep deletes only the MicroVMs of expired
Leases and those already `DELETING`, and at start battery marks
`DELETING` only a MicroVM left `LEASED` or `PRE_LEASE_HOOK_RUNNING` with
no Lease row (`RecoverAbandonedClaims`). So a quarantined MicroVM goes
only with its Pool, which v0.4.0's `DeletePool` deletes whole; in v0.3.3
only an edit of battery's database could remove it.

`UpdatePool` restarts the Pool's reconciler, which cancels the context of
every `Provision` still running. A cancelled `Provision` goes through
`ApplyHookFailurePolicy` with the spec the old reconciler was started
with, whose policy may be `QUARANTINE`. A `Provision` cancelled before its
MicroVM has a record in battery leaves nothing in battery to delete. A
MicroVM whose record reached `PROVISIONING` or `CREATE_HOOK_RUNNING` when
poolmgrd itself stopped has no reconciler to finish it after the restart
([`internal/poolmanager/manager.go`, `Seed`, L88-L99](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/poolmanager/manager.go#L88-L99)),
and it blocked `DeletePool` for good in v0.3.3; v0.4.0's `DeletePool`
deletes it with the Pool.

The Pool's status in battery counts only the available, leased,
provisioning and quarantined MicroVMs (`CountVMs`); a MicroVM whose
pre-lease hooks are running, or that is `LEASED` with no Lease row yet,
is counted as claiming, which the status reports as leased. A `DELETING`
MicroVM counts nowhere, and no longer blocks `DeletePool`.

## Seeding a Pool {#seeding}

- **BA-075** When a Pool's reconciler starts, and on each of its ticks
  after that, battery SHALL provision for the Pool its size less its
  available, claiming and provisioning MicroVMs for `IMMEDIATE_ON_LEASE`,
  and its size less its available, claiming, leased and provisioning
  MicroVMs for `REPLACE_ON_DELETE`.
- **BA-076** When a MicroVM of a Pool is claimed, for `IMMEDIATE_ON_LEASE`,
  and when one is deleted, for `REPLACE_ON_DELETE`, battery SHALL provision
  the shortfall of BA-075 at once, without waiting for the tick.

| ID | Source at v0.4.0 |
|----|------------------|
| BA-075 | [`internal/reconciler/reconciler.go`, `Reconciler.Run`, L105-L114](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/reconciler.go#L105-L114), [`Reconciler.reconcile`, L126-L133](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/reconciler.go#L126-L133); [`internal/reconciler/strategy.go`, `shortfall` and `DesiredNewVMs`, L87-L93 and L130-L136](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/strategy.go#L87-L136) |
| BA-076 | [`internal/reconciler/strategy.go`, `immediateOnLease.OnVMClaimed`, L95-L97, `replaceOnDelete.OnVMDeleted`, L140-L142](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/strategy.go#L95-L142); [`internal/reconciler/reconciler.go`, `Reconciler.Run`, L115-L118](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/reconciler/reconciler.go#L115-L118) |

battery v0.3.3 seeded a Pool of these two strategies once, when its
reconciler started, marked it seeded whether or not the provisions
succeeded, and after that provisioned for it only on a claim or a
deletion, so a Pool whose seed failed stayed short until the next claim,
deletion or restart; the Pool Controller works around that (03-pools.md,
PO-035 to PO-038). battery v0.4.0's reconciler reconciles when it starts,
on every tick and on every claim and deletion notification, each time
provisioning the strategy's shortfall, so a failed provision is tried
again on the next tick for every strategy (liquidmetal-dev/battery
96ab0f8). A MicroVM counts as claiming while its claim's pre-lease hooks
run, and while it is `LEASED` with no Lease row yet (`CountVMs`); one
left that way by a battery that died mid-claim is deleted at the next
start (`RecoverAbandonedClaims`). A Pool's reconciler starts when battery
starts (`Manager.Seed`), on `CreatePool`, and on every `UpdatePool`,
which restarts it whether or not the spec changed (BA-074;
[`internal/api/pooladmin.go`, `PoolAdminServer.UpdatePool`, L292-L303](https://github.com/liquidmetal-dev/battery/blob/v0.4.0/internal/api/pooladmin.go#L292-L303)).
A Pool whose template sets a static address or MAC is kept to one live
MicroVM, and battery refuses such a template unless the Pool can never
hold two.

## Where the stand-ins differ {#stand-ins}

The fake battery meets every assumption above except these, each cited
in its code as an exception:

- it sweeps once when it starts (BA-022), which is how a fresh Pool fills,
  and its state does not survive a restart (BA-060), so it has no
  database to migrate (BA-062);
- a new `Events` subscriber is replayed only the last events of each Pool
  (BA-050);
- it reaches its Hosts over connections the test gives it, and reads no
  certificate (BA-061);
- where a test sets `SeedOnce` (TD-007), its tick tops up an
  event-driven Pool only on the first tick after its `CreatePool` or
  `UpdatePool`, as battery v0.3.3 seeded such a Pool, where battery
  v0.4.0 tops it up on every tick (BA-075);
- its `DeletePool` refuses while the Pool owns a MicroVM in any phase, as
  battery v0.3.3 did, where battery v0.4.0 refuses only for a leased or
  claiming MicroVM and deletes the rest with the Pool (BA-070); #199
  brings the fake and the Pool Controller to v0.4.0's draining.

The pools model (`specs/quint/pools.qnt`) has no phases between created
and available, so no MicroVM is being provisioned when `UpdatePool` comes
(BA-074), and it has no quarantined MicroVM (BA-073). It has no
replenishment strategies either: its battery tops a Pool up whenever the
Pool is below its size (BA-075, BA-076). Its `DeletePool` refuses while
the Pool owns any MicroVM, as the fake's does (BA-070, #199).

The claim lifecycle model (`specs/quint/claims.qnt`) is coarser than
battery in these ways:

- its sweep can come at any time up to one sweep interval after an expiry
  or a start, rather than on a fixed period (BA-020, BA-022);
- it deletes a Lease and its MicroVM in one step (BA-023), so a replay
  never finds a Lease whose MicroVM is deleting, and it never answers
  `ABORTED` (BA-009);
- its `Events` stream can drop any event, and loses the undelivered ones
  when battery stops, where battery replays its outbox (BA-050);
- an `UNAVAILABLE` `ReleaseVM` (BA-031) is a `ReleaseVM` that failed before
  battery acted.

## Changes since v0.3.3 {#since-v033}

This document described battery v0.3.3 until #197 moved it to v0.4.0.
Between the two versions (battery's release notes for v0.4.0):

- `ClaimVM` gained the request id (BA-007 to BA-009), and a store
  migration for it, the first (BA-062). `ClaimVM` can now answer
  `ABORTED`.
- `DeletePool` drains the Pool it deletes, and refuses only for a leased
  or claiming MicroVM unless `force` is set (BA-070). It records a new
  event, `VM_DELETED_ON_POOL_DELETE`, for each MicroVM it deletes, and
  deletes quarantined ones too (BA-073).
- A Pool's reconciler provisions the strategy's shortfall on every tick
  and every claim or deletion, where v0.3.3 seeded an event-driven Pool
  once (BA-075, BA-076). A MicroVM counts as leased only once its Lease
  row exists, and one left claimed with no Lease by a dead battery is
  deleted at the next start.
- battery reserves a placement before it creates a MicroVM, so that two
  provisions never over-fill a Host, and its `ListHosts` counts every
  MicroVM on a Host; a Host can be cordoned through a new `HostAdmin`
  service, which the Operator does not use yet (#198).
- Every other assumption holds as it did, at other lines.

The differences between v0.1.0 and v0.3.3 are in this document's history
before #197.
