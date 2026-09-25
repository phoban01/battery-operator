/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package fakebattery is the fake battery
// (docs/requirements/08-test-doubles.md#fake-battery): a minimal but real
// pool manager on battery v0.3.3's protos. It serves the poolmgr.v1alpha1
// PoolAdmin, Lease and Events services over gRPC with the generated server
// stubs, and creates, places and deletes MicroVMs only through the
// flintlock MicroVM and MicroVMExec clients of the Hosts it is given, which
// in tests are fake flintlockds (package fakeflintlock). Conn dials it in
// process over the same handlers, so tests exercise the wire path without a
// network; Serve adds a TCP listener.
//
// State is held in memory and is lost on shutdown; because nothing
// persists, Run deletes every MicroVM it created before returning, so a
// stopped fake leaves no MicroVMs on its Hosts. The control loop (Run or
// Serve) drives replenishment, Lease expiry and deferred deletions on the
// configured clock; RPCs work before Run starts, but provisioning waits for
// it.
//
// Behaviour follows battery v0.3.3 where the two overlap: least-VM-count
// placement per Pool, RESOURCE_EXHAUSTED from ClaimVM on a Pool with
// nothing AVAILABLE, the host field on ClaimVMResponse, ListLeases, the
// MicroVM ids, the three replenishment strategies, the hook failure
// policies, the event types, DeletePool's refusal while a Pool owns any
// MicroVM, and UpdatePool cancelling the Pool's provisioning under the old
// spec's hook failure policy.
//
// What the Operator assumes battery does is listed in
// docs/requirements/10-battery.md (BA-*), from battery v0.3.3's source; the
// fake cites each assumption where it implements it, with a test, and cites
// the ones it does not meet as exceptions. In particular, as in battery, a
// Lease past its expiry is held until the next tick sweeps it, and a
// heartbeat in that time renews it (BA-010, BA-020); ReleaseVM answers
// UNAVAILABLE and keeps the Lease until the Host confirms the deletion
// (BA-031); and VM_EXPIRING_SOON comes again after every heartbeat that
// moves a Lease's expiry, since battery warns once per expiry.
//
// The fake deliberately differs from battery in these ways:
//
//   - The control loop's tick tops every Pool up to its target, whatever
//     its replenishment strategy. battery's tick does that only for
//     MIN_SIZE_THRESHOLD; for the event-driven strategies battery tops a
//     Pool up once, when its reconciler starts (at startup, CreatePool and
//     UpdatePool), and otherwise relies on the strategy's events. So the
//     fake recovers from a failed create or a quarantined MicroVM on the
//     next tick, where battery waits for the next event or restart.
//   - The fake does not check a Host's flintlock version; battery refuses
//     to create a MicroVM on a Host whose ServerInfo reports a version
//     older than v0.15.2.
//   - VM_EXPIRING_SOON fires one Pool heartbeat_interval before a Lease's
//     expiry, or half its heartbeat_expiry_threshold when the Pool sets no
//     interval; battery uses one process-wide warning_window, 30 seconds by
//     default.
//   - ReleaseVM emits VM_RELEASED before the deletion; battery never emits
//     it, although the proto defines it.
//   - The control loop ticks, and so sweeps, once when Run starts; battery's
//     first sweep is one sweep_interval after it starts (BA-022). The fake's
//     interval is Config.ReconcileInterval, one second by default, where
//     battery's sweep_interval defaults to ten seconds.
//   - State is lost when Run returns (above), where battery keeps its
//     database across a restart (BA-060). SetFaults with UnavailableFor
//     stands in for a restart.
//   - Hosts are connections the caller gives it (Hosts.Add), and it reads
//     no certificate; battery reads each Host's client certificate once,
//     when it starts (BA-061).
//   - A new Events subscriber is replayed the last Config.EventReplay events
//     of each Pool; battery replays its whole outbox (BA-050).
//   - Event payload_json carries a JSON object with the counts, lease id
//     and host of the transition, which battery does not promise.
//   - UpdatePool and CreatePool refuse a heartbeat_expiry_threshold that is
//     not positive, which would expire every Lease on the next tick.
//   - Fault injection (SetFaults) and inspection (Leases, VMs) exist only in
//     the fake.
package fakebattery

//= docs/requirements/08-test-doubles.md#fake-battery
//# The fake battery SHALL document every behaviour in which it
//# deliberately differs from battery v0.3.3.
