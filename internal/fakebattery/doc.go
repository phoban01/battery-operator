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
// pool manager on battery's protos. It serves the poolmgr.v1alpha1
// PoolAdmin, Lease, Events and HostAdmin services over gRPC with the
// generated server stubs, and creates, places and deletes MicroVMs only through the
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
// Behaviour follows battery v0.4.0 where the two overlap: least-VM-count
// placement per Pool, RESOURCE_EXHAUSTED from ClaimVM on a Pool with
// nothing AVAILABLE, the request id on ClaimVM and the replay of a claim
// by it, the host field on ClaimVMResponse, ListLeases, the MicroVM ids,
// the three replenishment strategies topped up on every tick, the hook
// failure policies, the event types, UpdatePool cancelling the Pool's
// provisioning under the old spec's hook failure policy, and DeletePool
// refusing a Pool with a leased MicroVM and otherwise deleting it with
// every MicroVM it owns. Its HostAdmin follows the Host calls of
// battery v0.5.0 (TD-028): a Pool may name only a
// Host the fake holds, RemoveHost refuses while a Pool names the Host or
// any MicroVM is on it, and no new MicroVM is placed on a cordoned Host.
//
// What the Operator assumes battery does is listed in
// docs/requirements/10-battery.md (BA-*), from battery v0.4.0's source; the
// fake cites each assumption where it implements it, with a test, and cites
// the ones it does not meet as exceptions. In particular, as in battery, a
// Lease past its expiry is held until the next tick sweeps it, and a
// heartbeat in that time renews it (BA-010, BA-020); ReleaseVM answers
// UNAVAILABLE and keeps the Lease until the Host confirms the deletion
// (BA-031); a ClaimVM whose request id a Lease was created with gets that
// Lease back, or ABORTED while its MicroVM is deleting (BA-007 to
// BA-009); and VM_EXPIRING_SOON comes again after every heartbeat that
// moves a Lease's expiry, since battery warns once per expiry.
//
// The fake deliberately differs from battery in these ways:
//
//   - With Config.SeedOnce, the control loop's tick tops up an
//     event-driven Pool only on the first tick after its CreatePool or
//     UpdatePool, as battery v0.3.3 seeded such a Pool, so that a test
//     can reach a Pool that stays short (TD-007). Without it the tick
//     tops every Pool up to its target, as battery v0.4.0 does (BA-075).
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
//   - Hosts are connections the caller gives it. The Hosts in Config.Hosts
//     (Hosts.Add) are held from the start, uncordoned and with no TLS
//     settings, as if added already. AddHost and UpdateHost reach a Host
//     through Config.Dial, which stands in for battery connecting to
//     flintlockd and checking its version (BA-080). The fake reads no
//     certificate, where battery reads a Host's certificate each time it
//     connects to it (BA-081), and GetHost reports no flintlock_version.
//     There is no database or configuration file for Hosts to come from
//     (BA-086, BA-087).
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
//# deliberately differs from battery v0.4.0.

//= docs/requirements/10-battery.md#restarts
//= type=exception
//= reason=The fake keeps its state in memory and loses it when Run returns, so it has no database to migrate; the e2e suite upgraded battery's real poolmgrd from v0.3.3 until #198, and now upgrades it from v0.4.0 (test/e2e/upgrade_test.go).
//# When battery starts on a database that battery v0.3.3 wrote,
//# battery SHALL migrate the database's schema before it serves, and SHALL
//# keep the Pools, MicroVMs and Leases in it.
