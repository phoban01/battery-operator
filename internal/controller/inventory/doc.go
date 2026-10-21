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

// Package inventory holds the Inventory Controller's subreconcilers
// (docs/requirements/04-inventory.md), its per-reconcile scope, and the
// set of Hosts battery places on, which the Pool Controller reads.
//
// The Inventory Controller keeps battery's Hosts in step with the Nodes
// through battery's HostAdmin API (ADR 0008). It has one reconcile for the
// whole cluster: every Node event, and every change to the Secret holding
// battery's flintlockd client certificate, leads to it. The controller
// lists the Nodes and battery's Hosts, and runs, in order:
//
//   - Admit decides which Nodes are Hosts now, from their Node reports and
//     whether they are schedulable (IN-001, IN-003).
//   - Settle keeps a change in whether a Node is a Host, or in its
//     flintlockd address, out of battery until it has held for the settle
//     time (IN-011), and so works out the Hosts battery should place on.
//   - Withdraw takes from the Pool Controller every Host the next steps
//     cordon, remove or move, before they call battery (IN-018).
//   - Cordon cordons the Hosts battery should no longer place on: their
//     Nodes are cordoned, not ready or gone (IN-015).
//   - Register adds the Hosts battery should place on, or updates or
//     uncordons them, with the TLS settings of IN-004 (IN-017, IN-019).
//   - Secure gives every other Host battery holds those TLS settings
//     (IN-020): a Host battery v0.4 kept has none.
//   - Remove removes the cordoned Hosts whose Nodes are gone, and asks
//     again while battery refuses (IN-016).
//   - Reload makes battery read its client certificate again, once the
//     Operator's mount holds what the Secret holds, after a renewal and once
//     after the Operator starts (DP-035).
//
// and finally, however those ended:
//
//   - Publish gives the Pool Controller the Hosts battery holds uncordoned
//     and is to go on placing on (IN-018).
//   - Report fails the reconcile with every Host's failure, so that it is
//     retried with back-off. A step that fails for one Host goes on with
//     the others.
//
// # What survives the Operator's restart
//
// battery's Hosts are in battery's database, and every reconcile starts
// from ListHosts, so nothing about them is kept in the Operator. Which
// Nodes have been changing and since when, and the certificate battery
// last read, are kept in memory (State): a Node seen for the first time
// starts to settle then, and every Host is reloaded once after the
// Operator starts.
package inventory
