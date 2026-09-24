# What the Operator assumes battery does

The Operator runs battery unchanged (ADR 0001), so it can only depend on what
battery already does. This document lists those assumptions, each with an
identifier and a pointer into battery's source at
[v0.3.3](https://github.com/liquidmetal-dev/battery/tree/v0.3.3). Unlike the
other documents in this directory, its subject is battery, not a system this
project builds: each statement says what battery v0.3.3 does, as read from
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

Links point at the tag, so line numbers stay valid. Where battery v0.1.0
differs, the section says so; see also [Changes since v0.1.0](#since-v010).

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

| ID | Source at v0.3.3 |
|----|------------------|
| BA-001 | [`internal/api/lease.go`, `LeaseServer.ClaimVM`, L107-L110](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/api/lease.go#L107-L110); [`internal/store/sqlite.go`, `ClaimAvailableVM`, L280-L287](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/store/sqlite.go#L280-L287) |
| BA-002 | [`internal/api/lease.go`, `LeaseServer.ClaimVM`, L99-L102](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/api/lease.go#L99-L102) |
| BA-003 | [`internal/api/lease.go`, `LeaseServer.ClaimVM`, L123-L147](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/api/lease.go#L123-L147) |
| BA-004 | [`internal/api/lease.go`, `LeaseServer.ClaimVM`, L144-L174](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/api/lease.go#L144-L174) |
| BA-005 | [`internal/store/sqlite.go`, `ClaimAvailableVM`, L280-L309](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/store/sqlite.go#L280-L309) |
| BA-006 | [`internal/reconciler/provision.go`, `Provisioner.Provision`, L242-L244](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/reconciler/provision.go#L242-L244); [`internal/store/sqlite.go`, `ClaimAvailableVM`, L292-L299](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/store/sqlite.go#L292-L299) |

The lease id is a random UUID (`uuid.NewString`), so battery never hands the
same one out twice. Nothing in `ClaimVMRequest` identifies the caller's
claim, which is why a `ClaimVM` whose answer is lost cannot be retried
safely (02-claims.md, Binding). BA-004 is battery's side of that window:
after `CreateLease` nothing fails the call, so an error the Operator sees
after the commit comes from the transport or the Operator's own deadline,
not from battery (#61). The network interfaces in the answer are best effort
and can be empty.

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

| ID | Source at v0.3.3 |
|----|------------------|
| BA-010 | [`internal/api/lease.go`, `LeaseServer.Heartbeat`, L234-L262](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/api/lease.go#L234-L262); [`internal/store/sqlite.go`, `UpdateLeaseHeartbeat`, L360-L369](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/store/sqlite.go#L360-L369) |
| BA-011 | [`internal/api/lease.go`, `LeaseServer.Heartbeat`, L237-L240 and L255-L258](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/api/lease.go#L237-L258) |

`Heartbeat` reads the Lease row and updates it by lease id alone; neither
step compares the stored expiry with the time. A Lease whose expiry has
passed but that the sweep has not deleted yet ([Expiry](#expiry)) is
therefore renewed like any other. There is no separate answer for an
expired Lease: battery either renews the Lease or answers `NOT_FOUND`. The
same `NOT_FOUND` comes back if the Lease's Pool is gone (L245-L248), which
cannot happen while the Lease exists, since `DeletePool` refuses while the
Pool owns any MicroVM
([`internal/api/pooladmin.go`, L272-L280](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/api/pooladmin.go#L272-L280)).

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

| ID | Source at v0.3.3 |
|----|------------------|
| BA-020 | [`internal/reconciler/sweeper.go`, `DefaultSweepInterval`, L15-L17](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/reconciler/sweeper.go#L15-L17), [`Sweeper.Run`, L91-L104](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/reconciler/sweeper.go#L91-L104), [`Sweeper.Tick`, L106-L146](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/reconciler/sweeper.go#L106-L146); [`internal/store/sqlite.go`, `ListExpiredLeases`, L379-L400](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/store/sqlite.go#L379-L400); [`internal/config/config.go`, `SweepInterval`, L30-L33](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/config/config.go#L30-L33) |
| BA-021 | [`internal/reconciler/sweeper.go`, `Sweeper.beginExpiry`, L176-L189](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/reconciler/sweeper.go#L176-L189); [`internal/store/sqlite.go`, `DeleteLeaseIfExpired`, L431-L472](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/store/sqlite.go#L431-L472) |
| BA-022 | [`internal/reconciler/sweeper.go`, `Sweeper.Run`, L92-L104](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/reconciler/sweeper.go#L92-L104); [`cmd/poolmgrd/main.go`, L104-L120](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/cmd/poolmgrd/main.go#L104-L120) |
| BA-023 | [`internal/reconciler/sweeper.go`, `Sweeper.beginExpiry`, L198-L217](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/reconciler/sweeper.go#L198-L217), [`Sweeper.retryPendingDeletions`, L148-L166](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/reconciler/sweeper.go#L148-L166); [`internal/reconciler/provision.go`, `EnsureVMDeleted`, L374-L406](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/reconciler/provision.go#L374-L406) |

`sweep_interval` is set in battery's configuration file and defaults to 10
seconds. The sweep runs on a `time.Ticker`, which first fires one interval
after `Run` starts. Together with BA-010 this means:

- a Lease can outlive its expiry by up to one `sweep_interval` while battery
  runs, and any `Heartbeat` in that time renews it;
- a Lease that expired while battery was stopped survives the restart by up
  to one `sweep_interval`, and can be renewed in that time too;
- a sweep that fails to read the store skips its turn
  (`Sweeper.Tick`, L115-L118), so the bound holds only while battery's
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

| ID | Source at v0.3.3 |
|----|------------------|
| BA-030 | [`internal/api/lease.go`, `LeaseServer.ReleaseVM`, L264-L303](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/api/lease.go#L264-L303); [`internal/reconciler/provision.go`, `FinishVMDeletion`, L408-L441](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/reconciler/provision.go#L408-L441) |
| BA-031 | [`internal/api/lease.go`, `LeaseServer.ReleaseVM`, L284-L287](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/api/lease.go#L284-L287); [`internal/reconciler/sweeper.go`, `Sweeper.retryPendingDeletions`, L148-L166](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/reconciler/sweeper.go#L148-L166); [`internal/reconciler/provision.go`, `FinishVMDeletion`, L425-L436](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/reconciler/provision.go#L425-L436) |
| BA-032 | [`internal/api/lease.go`, `LeaseServer.ReleaseVM`, L271-L274](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/api/lease.go#L271-L274) |

While a Lease is kept after an `UNAVAILABLE` `ReleaseVM`, it is an ordinary
Lease row: `Heartbeat` renews it (BA-010), `ListLeases` lists it (BA-040),
and a retried `ReleaseVM` tries the deletion again. If its expiry passes
first, the sweep deletes the row (BA-020) and then finishes the MicroVM's
deletion, reporting it as `VM_DELETED_DUE_TO_EXPIRY`; a `ReleaseVM` after
that is `NOT_FOUND`. A `ReleaseVM` for a Lease whose MicroVM record is
already gone deletes the Lease and succeeds (L296-L302), so a retry after a
lost answer is safe.

## Reading Leases {#list-leases}

- **BA-040** When battery receives a `ListLeases`, battery SHALL answer with
  every Lease it holds, or every Lease of the one Pool the request names,
  each with its lease id, MicroVM uid, Pool, claim time, last heartbeat time
  and expiry, without renewing any of them.
- **BA-041** battery SHALL include in the answer to `ListLeases` a Lease
  whose expiry has passed but that no sweep has deleted yet.

| ID | Source at v0.3.3 |
|----|------------------|
| BA-040 | [`internal/api/lease.go`, `LeaseServer.ListLeases`, L305-L313](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/api/lease.go#L305-L313); [`internal/store/sqlite.go`, `ListLeases`, L402-L429](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/store/sqlite.go#L402-L429); [`api/proto/poolmgr/v1alpha1/lease.proto`, L20-L21 and L61-L67](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/api/proto/poolmgr/v1alpha1/lease.proto#L20-L67) |
| BA-041 | [`internal/store/sqlite.go`, `ListLeases`, L402-L409](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/store/sqlite.go#L402-L409) |

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

| ID | Source at v0.3.3 |
|----|------------------|
| BA-050 | [`internal/api/events.go`, `EventsServer.Subscribe`, L49-L89](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/api/events.go#L49-L89) |
| BA-051 | [`internal/reconciler/provision.go`, `FinishVMDeletion`, L408-L441](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/reconciler/provision.go#L408-L441), called after `EnsureVMDeleted` succeeds in [`internal/api/lease.go`, L284-L292](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/api/lease.go#L284-L292) and [`internal/reconciler/sweeper.go`, L157-L164 and L214-L217](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/reconciler/sweeper.go#L157-L217) |

The outbox is battery's database, and nothing prunes it yet, so a
subscriber that reconnects, including after battery restarts, is sent the
whole history again. Recording an event is best effort
([`EmitEvent`, `internal/reconciler/provision.go`, L354-L365](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/reconciler/provision.go#L354-L365)):
if the write fails the event is lost for good. A subscriber can therefore
miss an event, but never because it was disconnected. battery never records
`VM_RELEASED`, although the proto defines it, and records
`VM_EXPIRING_SOON` once for each expiry a Lease has, within one
process-wide `warning_window` (30 seconds by default) of it, so a renewed
Lease is warned again
([`Sweeper.Tick`, L131-L135](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/reconciler/sweeper.go#L131-L135)).

## Restarts {#restarts}

- **BA-060** battery SHALL keep its Pools, MicroVMs and Leases across a
  restart, in its database.
- **BA-061** battery SHALL read the client certificate, key and certificate
  authority of each Host's TLS configuration only when it starts, and SHALL
  present that client certificate to the Host until it exits.

| ID | Source at v0.3.3 |
|----|------------------|
| BA-060 | [`cmd/poolmgrd/main.go`, L73-L77](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/cmd/poolmgrd/main.go#L73-L77); [`internal/store/sqlite.go`, `Open`, L26-L51](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/store/sqlite.go#L26-L51) |
| BA-061 | [`cmd/poolmgrd/main.go`, L64 and L84-L87](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/cmd/poolmgrd/main.go#L64-L87); [`internal/flintlockclient/pool.go`, `New`, L44-L82](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/flintlockclient/pool.go#L44-L82), [`dialCredentials`, L149-L175](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/flintlockclient/pool.go#L149-L175) |

Leases are not renewed or expired while battery is stopped; after the
restart they are swept as BA-022 says. Calls to battery fail while it is
stopped. The database lasts as long as the file `poolmgrd -db` names, so
whether Leases survive the Operator's pod being replaced, and not only
battery's container restarting, is up to the Manifests (06-deployment.md).

`flintlockclient.New` builds each Host's transport credentials once, at
startup, from a `tls.Config` whose `Certificates` hold the key pair read
then; nothing reads the files again. poolmgrd handles only SIGINT and
SIGTERM, both of which stop it, and watches no file, so a renewed
certificate reaches the Hosts only through a restart (06-deployment.md,
DP-007). The same holds for the certificate authority in `ca_file`.

## Deleting a Pool {#delete-pool}

- **BA-070** If a Pool owns a MicroVM in any phase, then battery SHALL
  answer `DeletePool` for it with `FAILED_PRECONDITION` and keep the Pool
  and its MicroVMs.
- **BA-071** If battery holds no Pool of the name and namespace a
  `DeletePool` names, then battery SHALL answer it with `NOT_FOUND`.
- **BA-072** While a Pool's replenishment strategy is `MIN_SIZE_THRESHOLD`
  and its size is 0, battery SHALL provision no MicroVM for it.
- **BA-073** battery SHALL NOT delete a MicroVM in the phase `QUARANTINED`.
- **BA-074** When `UpdatePool` succeeds, battery SHALL stop the Pool's
  reconciler, SHALL apply the hook failure policy of the Pool's previous
  spec to each MicroVM whose provisioning that stops, and SHALL start a
  reconciler with the new spec.

| ID | Source at v0.3.3 |
|----|------------------|
| BA-070 | [`internal/api/pooladmin.go`, `PoolAdminServer.DeletePool`, L250-L280](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/api/pooladmin.go#L250-L280) |
| BA-071 | [`internal/api/pooladmin.go`, `PoolAdminServer.DeletePool`, L263-L267](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/api/pooladmin.go#L263-L267) |
| BA-072 | [`internal/reconciler/strategy.go`, `minSizeThreshold`, L89-L113](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/reconciler/strategy.go#L89-L113); [`internal/reconciler/reconciler.go`, `Reconciler.Run`, L97-L126](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/reconciler/reconciler.go#L97-L126) |
| BA-073 | [`internal/reconciler/provision.go`, `ApplyHookFailurePolicy`, L333-L338](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/reconciler/provision.go#L333-L338); [`internal/reconciler/sweeper.go`, `Sweeper.retryPendingDeletions`, L148-L166](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/reconciler/sweeper.go#L148-L166), [`Sweeper.beginExpiry`, L176-L217](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/reconciler/sweeper.go#L176-L217); [`internal/api/lease.go`, `LeaseServer.ReleaseVM`, L270-L277](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/api/lease.go#L270-L277) |
| BA-074 | [`internal/api/pooladmin.go`, `PoolAdminServer.UpdatePool`, L228-L240](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/api/pooladmin.go#L228-L240); [`internal/poolmanager/manager.go`, `StopReconciler`, L141-L157](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/poolmanager/manager.go#L141-L157); [`internal/reconciler/provision.go`, `Provisioner.Provision`, L193-L262](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/reconciler/provision.go#L193-L262), [`ApplyHookFailurePolicy`, L307-L352](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/reconciler/provision.go#L306-L352) |

`DeletePool` lists the Pool's MicroVMs in every phase, `DELETING` and
`FAILED` included, and refuses if there is one; it deletes none of them.
battery v0.3.3 has no other call that deletes a Pool or an idle MicroVM,
and none of its replenishment strategies deletes a MicroVM when a Pool's
size drops: a strategy only decides how many to create. So a Pool that
has ever held a MicroVM can be deleted only once each of its MicroVMs has
gone some other way:

- a leased MicroVM goes when its Lease is released (BA-030) or expires
  (BA-023);
- an available MicroVM goes only by being claimed and then released;
- a MicroVM that is provisioning becomes available, or goes through the
  Pool's hook failure policy, which deletes it under `DELETE_AND_REPLACE`
  and quarantines it under `QUARANTINE`;
- a quarantined MicroVM never goes (BA-073).

battery v0.3.3 refuses a `MIN_SIZE_THRESHOLD` strategy without a positive
`min_size`, but not a size of 0, and a `MIN_SIZE_THRESHOLD` Pool of size 0
creates nothing: the tick's target is the size less every MicroVM already
there, and the strategy ignores claims and deletions (BA-072). The other
two strategies do not stop at a size of 0: `IMMEDIATE_ON_LEASE` creates a
MicroVM on every claim, and `REPLACE_ON_DELETE` one on every deletion,
whatever the size ([`internal/reconciler/strategy.go`, L86 and L127](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/reconciler/strategy.go#L74-L127)).
A Pool set to `MIN_SIZE_THRESHOLD`, size 0, can therefore be emptied of
its available MicroVMs by claiming each one and releasing it at once; with
no pre-lease commands and the policy `DELETE_AND_REPLACE`, such a claim
runs no hook, and one that fails deletes its MicroVM rather than
quarantining it.

A quarantined MicroVM has no Lease: `ApplyHookFailurePolicy` clears its
lease id, and a pre-lease hook that fails in `ClaimVM` creates none
([`internal/api/lease.go`, L119-L121](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/api/lease.go#L119-L121)).
`ReleaseVM` needs a Lease, the sweep deletes only the MicroVMs of expired
Leases and those already `DELETING`, and nothing else in battery deletes
a MicroVM. A Pool with a quarantined MicroVM can therefore not be deleted
through battery's API; only an edit of battery's database can.

`UpdatePool` restarts the Pool's reconciler, which cancels the context of
every `Provision` still running. A cancelled `Provision` goes through
`ApplyHookFailurePolicy` with the spec the old reconciler was started
with, whose policy may be `QUARANTINE`. A `Provision` cancelled before its
MicroVM has a record in battery leaves nothing in battery to delete. A
MicroVM whose record reached `PROVISIONING` or `CREATE_HOOK_RUNNING` when
poolmgrd itself stopped has no reconciler to finish it after the restart
([`internal/poolmanager/manager.go`, `Seed`, L78-L91](https://github.com/liquidmetal-dev/battery/blob/v0.3.3/internal/poolmanager/manager.go#L78-L91)),
and it too blocks `DeletePool` for good.

The Pool's status in battery counts only the available, leased,
provisioning and quarantined MicroVMs (`CountVMs`), so a Pool whose
counts are all 0 can still be refused while one of its MicroVMs is
`DELETING`.

## Where the stand-ins differ {#stand-ins}

The fake battery meets every assumption above except these, each cited
in its code as an exception:

- it sweeps once when it starts (BA-022), which is how a fresh Pool fills,
  and its state does not survive a restart (BA-060);
- a new `Events` subscriber is replayed only the last events of each Pool
  (BA-050);
- it reaches its Hosts over connections the test gives it, and reads no
  certificate (BA-061).

The pools model (`specs/quint/pools.qnt`) has no phases between created
and available, so no MicroVM is being provisioned when `UpdatePool` comes
(BA-074), and it has no quarantined MicroVM (BA-073).

The claim lifecycle model (`specs/quint/claims.qnt`) is coarser than
battery in these ways:

- its sweep can come at any time up to one sweep interval after an expiry
  or a start, rather than on a fixed period (BA-020, BA-022);
- it deletes a Lease and its MicroVM in one step (BA-023);
- its `Events` stream can drop any event, and loses the undelivered ones
  when battery stops, where battery replays its outbox (BA-050);
- an `UNAVAILABLE` `ReleaseVM` (BA-031) is a `ReleaseVM` that failed before
  battery acted.

## Changes since v0.1.0 {#since-v010}

Every assumption above holds for battery v0.1.0 too, except those about
`ListLeases`, which v0.1.0 does not have. Between the two versions:

- `ListLeases` (BA-040, BA-041) was added in v0.2.0, with a store index on
  a Lease's Pool.
- `Heartbeat`, `ClaimVM` and `ReleaseVM` in `internal/api/lease.go`, the
  sweeper in `internal/reconciler/sweeper.go` and the Events service in
  `internal/api/events.go` are unchanged, at the same lines.
- In `internal/store/sqlite.go`, `DeleteLeaseIfExpired` moved from L402 to
  L431 when `ListLeases` was inserted above it; its body is unchanged.
- `internal/reconciler/provision.go` gained logging and gives each
  provisioned MicroVM its own id, which moved `EnsureVMDeleted` and
  `FinishVMDeletion` down; their behaviour is unchanged.
- A Pool's reconciler now seeds an event-driven Pool once when it starts,
  and battery refuses to provision on a `flintlockd` older than v0.15.2.
  Neither changes an assumption here.
- `DeletePool`, `UpdatePool`, the replenishment strategies and
  `ApplyHookFailurePolicy` behave the same in v0.1.0, at other lines
  (BA-070 to BA-074).
