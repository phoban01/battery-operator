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
// Node event leads to it. It runs, in order:
//
//   - Resume restarts battery if a previous reconcile wrote battery's
//     configuration and did not finish restarting it, and publishes the
//     Hosts battery runs with the first time round.
//   - Admit decides which Nodes are Hosts now, from their Node reports and
//     whether they are schedulable (IN-001, IN-003).
//   - Settle keeps a change in whether a Node is a Host, or in its
//     flintlockd address, out of battery until it has held for the settle
//     time (IN-011), and so works out the Hosts battery should have
//     (IN-002).
//   - Window batches the settled changes: the first one waiting opens a
//     restart window, and nothing is applied until it closes (IN-012).
//   - Apply writes the Hosts to battery's configuration and restarts
//     battery through the one restart hook (IN-010, IN-004; DP-006).
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
// not yet restarted into, so that Resume finishes the restart.
package inventory
