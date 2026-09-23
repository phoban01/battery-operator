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
// pool manager on battery v0.1.0's protos. It serves the poolmgr.v1alpha1
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
// Behaviour follows battery v0.1.0 where the two overlap: least-VM-count
// placement per Pool, RESOURCE_EXHAUSTED from ClaimVM on a Pool with
// nothing AVAILABLE, the host field on ClaimVMResponse, the three
// replenishment strategies, the hook failure policies and the event types.
// The fake deliberately differs from battery in these ways:
//
//   - The control loop's tick tops every Pool up to its target, whatever
//     its replenishment strategy; battery's tick does that only for
//     MIN_SIZE_THRESHOLD and otherwise relies on the strategy's events.
//     That is how a fresh Pool fills and how a Pool recovers from a failed
//     create or a quarantined MicroVM.
//   - DeletePool deletes the Pool's idle MicroVMs and refuses only while a
//     Lease is outstanding; battery refuses while the Pool owns any
//     MicroVM.
//   - VM_EXPIRING_SOON is emitted again after every heartbeat that moves a
//     Lease's expiry, so a long-held Lease sees one warning per heartbeat
//     where the proto describes one per Lease. A test that counts the
//     warnings is counting the fake's behaviour; wait for the first one
//     instead.
//   - Event payload_json carries a JSON object with the counts, lease id
//     and host of the transition, which battery does not promise.
//   - UpdatePool and CreatePool refuse a heartbeat_expiry_threshold that is
//     not positive, which would expire every Lease on the next tick.
//   - Fault injection (SetFaults) and inspection (Leases, VMs) exist only in
//     the fake.
package fakebattery

//= docs/requirements/08-test-doubles.md#fake-battery
//# The fake battery SHALL document every behaviour in which it
//# deliberately differs from battery v0.1.0.
