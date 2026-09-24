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
// set of Hosts it has given battery, which the Pool Controller reads.
//
// The Inventory Controller has one reconcile for the whole cluster: every
// Node event, every change to battery's ConfigMap, and every change to the
// Secret holding battery's flintlockd client certificate, leads to it. It
// runs, in order:
//
//   - Restore writes back the Hosts the Inventory Controller last wrote to
//     battery's configuration, when something else, applying the Manifests
//     again, has changed them, and does not restart battery for it: battery
//     runs with them already (IN-014).
//   - Resume restarts battery if a previous reconcile wrote battery's
//     configuration and did not finish restarting it, and publishes the
//     Hosts battery runs with the first time round.
//   - Admit decides which Nodes are Hosts now, from their Node reports and
//     whether they are schedulable (IN-001, IN-003).
//   - Settle keeps a change in whether a Node is a Host, or in its
//     flintlockd address, out of battery until it has held for the settle
//     time (IN-011), and so works out the Hosts battery should have
//     (IN-002).
//   - Renew notices that cert-manager has renewed battery's client
//     certificate, which battery reads only when it starts (DP-007,
//     BA-061).
//   - Window batches the settled changes and a renewal: the first one
//     waiting opens a restart window, and nothing is applied until it
//     closes (IN-012, DP-008).
//   - Drain, before a restart that removes Hosts, publishes the Hosts that
//     remain and waits, for at most the drain timeout, until no Pool in
//     battery names a leaving Host (IN-013). A restart for a renewal alone
//     removes no Host, and passes straight through.
//   - Apply writes the Hosts to battery's configuration and restarts
//     battery through the one restart hook, which waits for the
//     configuration and the certificate to reach battery's files (IN-010,
//     IN-004; DP-006, DP-007).
//
// Every restart of battery goes through Apply or Resume, and so through
// the one Restarter: a renewal and a Host change never restart battery
// twice.
//
// # The restart window
//
// When a settled change is waiting and no window is open, Window opens one.
// When it closes, Apply writes every change that has settled by then and
// restarts battery once; a change still settling waits for a window of its
// own. If every change in the window flapped back, the window closes and
// battery is left alone. Restarts are therefore at least one window apart,
// and a change reaches battery between the settle time and the settle time
// plus one window after it happens. That is the reading of IN-012 the
// model in specs/quint/pools.qnt checks.
//
// A renewed client certificate is a change too, with no settle time: it
// opens a window, or joins the one open, and goes into the same restart
// as the Host changes settled by its close (DP-008). A renewal comes weeks
// before the old certificate expires, so a window's wait costs nothing.
//
// The kubelet restarts battery with a back-off that grows while battery
// keeps exiting within ten minutes of starting (10s, 20s, 40s, ... up to
// five minutes), so the window also keeps the back-off from growing on a
// burst of changes.
//
// # What survives the Operator's restart
//
// Which Nodes have been changing and since when, and the open window, are
// kept in memory (State): a Node seen for the first time starts to settle
// then. battery's configuration is the ConfigMap, which also records,
// in the annotation RestartPendingAnnotation, a configuration written and
// not yet restarted into, so that Resume finishes the restart; in
// ClientCertificateAnnotation, the digest of the client certificate battery
// last restarted with, so that a renewal while the Operator was down is
// still noticed; and, in HostsAnnotation, the Hosts last written, so that
// Restore can put them back after an apply of the Manifests, which leaves
// annotations it does not give alone. Resume restarts with the certificate
// the Secret holds when it runs, which may be newer than the one the
// interrupted restart waited for.
package inventory
