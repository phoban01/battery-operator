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

package inventory

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/phoban01/battery-operator/internal/batterysidecar"
	"github.com/phoban01/battery-operator/internal/execagent"
)

// TestAnnotationsAreTheExecAgents checks that the Node report the
// Inventory Controller reads is the one the Exec Agent writes.
func TestAnnotationsAreTheExecAgents(t *testing.T) {
	if AnnotationReady != execagent.AnnotationReady {
		t.Errorf("AnnotationReady = %q, the Exec Agent writes %q", AnnotationReady, execagent.AnnotationReady)
	}
	if AnnotationFlintlockdAddress != execagent.AnnotationFlintlockdAddress {
		t.Errorf("AnnotationFlintlockdAddress = %q, the Exec Agent writes %q",
			AnnotationFlintlockdAddress, execagent.AnnotationFlintlockdAddress)
	}
}

//= docs/requirements/04-inventory.md#admission
//= type=test
//# The Inventory Controller SHALL treat a Node as a Host only while
//# the Node's Node report says the Host is ready and the Node is schedulable.

//= docs/requirements/04-inventory.md#admission
//= type=test
//# The Inventory Controller SHALL give battery each Host under the
//# Node's name, at the `flintlockd` address the Host's Node report gives.

// TestAdmit covers IN-001 and IN-003: only a ready, schedulable Node with a
// flintlockd address is a Host, under its Node's name at that address.
func TestAdmit(t *testing.T) {
	deleting := host("deleting", "10.0.0.9:9090")
	deleting.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	deleting.Finalizers = []string{"example.com/hold"}
	noAddress := host("no-address", "")
	unreported := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "unreported"}}
	nodes := []*corev1.Node{
		host(nodeA, addrA),
		with(host("cordoned", "10.0.0.3:9090"), cordon),
		with(host("not-ready", "10.0.0.4:9090"), notReady),
		with(host("ready-yes", "10.0.0.5:9090"), func(n *corev1.Node) { n.Annotations[AnnotationReady] = "yes" }),
		noAddress,
		unreported,
		deleting,
		host(nodeB, addrB),
	}
	s := &Scope{}
	for _, n := range nodes {
		s.Nodes = append(s.Nodes, *n)
	}
	if _, err := (Admit{}).Reconcile(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if want := (Hosts{nodeA: addrA, nodeB: addrB}); !s.Observed.Equal(want) {
		t.Errorf("Observed = %v, want %v", s.Observed, want)
	}
}

func with(n *corev1.Node, change func(*corev1.Node)) *corev1.Node {
	change(n)
	return n
}

//= docs/requirements/04-inventory.md#applying
//= type=test
//# The Inventory Controller SHALL act on a change in whether a Node
//# is a Host only after the change has held for the configured settle time.

// TestSettleWaitsForTheSettleTime covers IN-011: a Node that becomes a
// Host is left out of the Hosts battery should have until the change has
// held for the settle time, and a change that flaps starts to settle again.
func TestSettleWaitsForTheSettleTime(t *testing.T) {
	h := newHarness(t)
	sub := Settle{Time: settle}
	run := func(observed Hosts) (*Scope, Result) {
		t.Helper()
		s := h.scope()
		s.Observed = observed
		r, err := sub.Reconcile(context.Background(), s)
		if err != nil {
			t.Fatal(err)
		}
		return s, r
	}

	s, r := run(Hosts{nodeA: addrA})
	if len(s.Desired) != 0 {
		t.Errorf("a change not yet settled is desired: %v", s.Desired)
	}
	if r.RequeueAfter != settle {
		t.Errorf("RequeueAfter = %s, want the settle time %s", r.RequeueAfter, settle)
	}

	h.clock.Advance(settle - time.Second)
	if s, r = run(Hosts{nodeA: addrA}); len(s.Desired) != 0 || r.RequeueAfter != time.Second {
		t.Errorf("a second before settling: Desired %v, RequeueAfter %s", s.Desired, r.RequeueAfter)
	}

	// It flaps back and forth: the change starts to settle again.
	run(Hosts{})
	h.clock.Advance(time.Second)
	if s, r = run(Hosts{nodeA: addrA}); len(s.Desired) != 0 || r.RequeueAfter != settle {
		t.Errorf("after a flap: Desired %v, RequeueAfter %s, want nothing and %s", s.Desired, r.RequeueAfter, settle)
	}

	h.clock.Advance(settle)
	if s, _ = run(Hosts{nodeA: addrA}); !s.Desired.Equal(Hosts{nodeA: addrA}) {
		t.Errorf("once settled: Desired %v, want node-a", s.Desired)
	}
}

// TestSettleMovesAnAddressOnceSettled checks that a Host whose flintlockd
// address moves keeps its old address until the move has settled.
func TestSettleMovesAnAddressOnceSettled(t *testing.T) {
	h := newHarness(t)
	s := h.scope()
	s.Config.Hosts = Hosts{nodeA: addrA}
	s.Observed = Hosts{nodeA: addrB}
	sub := Settle{Time: settle}
	if _, err := sub.Reconcile(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if !s.Desired.Equal(Hosts{nodeA: addrA}) {
		t.Errorf("before settling: Desired %v, want the old address", s.Desired)
	}
	h.clock.Advance(settle)
	if _, err := sub.Reconcile(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if !s.Desired.Equal(Hosts{nodeA: addrB}) {
		t.Errorf("once settled: Desired %v, want the new address", s.Desired)
	}
}

//= docs/requirements/04-inventory.md#admission
//= type=test
//# When a Node that is a Host is cordoned or deleted, or its Node
//# report says the Host is not ready, the Inventory Controller SHALL remove
//# the Host from battery's Hosts.

// TestHostLeaves covers IN-002: a Host that is cordoned, deleted or
// reported not ready leaves battery's Hosts once the change has settled
// and its window has closed.
func TestHostLeaves(t *testing.T) {
	for name, leave := range map[string]func(*harness){
		"cordoned":  func(h *harness) { h.update(nodeA, cordon) },
		"not ready": func(h *harness) { h.update(nodeA, notReady) },
		"deleted": func(h *harness) {
			if err := h.c.Delete(context.Background(), host(nodeA, addrA)); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, host(nodeA, addrA), host(nodeB, addrB))
			joinAll(h)
			before := h.restarter.count()

			leave(h)
			h.mustReconcile()
			h.advance(settle)
			if h.restarter.count() != before {
				t.Fatal("battery restarted before the window closed")
			}
			h.advance(window)
			if h.restarter.count() != before+1 {
				t.Fatalf("restarts = %d, want one more than %d", h.restarter.count(), before)
			}
			if got := hostsOf(h.lastRestart()); !got.Equal(Hosts{nodeB: addrB}) {
				t.Errorf("battery restarted with %v, want node-b alone", got)
			}
			if got := h.published(); !got.Equal(Hosts{nodeB: addrB}) {
				t.Errorf("published %v, want node-b alone", got)
			}
		})
	}
}

// TestLastHostLeavesForThePlaceholder checks that battery keeps its
// placeholder Host when no Host is left, since poolmgrd refuses to start
// with none, and that the placeholder is never published as a Host.
func TestLastHostLeavesForThePlaceholder(t *testing.T) {
	h := newHarness(t, host(nodeA, addrA))
	joinAll(h)
	h.update(nodeA, cordon)
	h.mustReconcile()
	h.advance(settle)
	h.advance(window)
	f := h.lastRestart()
	if len(f.Hosts) != 1 || f.Hosts[0].Name != batterysidecar.PlaceholderHostName {
		t.Errorf("battery restarted with %+v, want the placeholder alone", f.Hosts)
	}
	if got := h.published(); got == nil || len(got) != 0 {
		t.Errorf("published %v, want no Hosts", got)
	}
}

// joinAll runs the harness until every Node that is a Host has joined
// battery: one settle time and one window.
func joinAll(h *harness) {
	h.t.Helper()
	h.mustReconcile()
	h.advance(settle)
	h.advance(window)
}

//= docs/requirements/04-inventory.md#applying
//= type=test
//# When the set of Hosts changes, the Inventory Controller SHALL
//# write the new list to battery's configuration and restart battery.

//= docs/requirements/04-inventory.md#admission
//= type=test
//# The Inventory Controller SHALL configure battery to reach every
//# Host's `flintlockd` over TLS, verifying the serving certificate against
//# the configured certificate authority and presenting the Operator's client
//# certificate for `flintlockd`.

// TestReadyHostJoins covers IN-010 and IN-004: a ready Node joins battery's
// configuration, battery restarts once with exactly what was written, and
// every Host is reached over TLS with the client certificate and the
// serving CA. The Hosts are published only once battery has restarted.
func TestReadyHostJoins(t *testing.T) {
	h := newHarness(t, host(nodeA, addrA))
	h.restarter.during = func() {
		if got := h.published(); got == nil || len(got) != 0 {
			t.Errorf("during the restart, published %v, want the Hosts battery ran with before", got)
		}
		if !h.stored().Pending {
			t.Error("during the restart, the configuration is not marked pending")
		}
	}
	h.mustReconcile()
	if !h.hosts.Synced() {
		t.Error("the Hosts battery starts with were not published")
	}
	joinAll(h)

	if n := h.restarter.count(); n != 1 {
		t.Fatalf("restarts = %d, want 1", n)
	}
	stored := h.stored()
	if string(stored.Raw) != string(h.restarter.configs[0]) {
		t.Error("battery restarted with something other than its ConfigMap holds")
	}
	if stored.Pending {
		t.Error("the configuration is still pending after the restart")
	}
	f := h.lastRestart()
	if got := hostsOf(f); !got.Equal(Hosts{nodeA: addrA}) {
		t.Fatalf("battery restarted with %v, want node-a", got)
	}
	want := batterysidecar.HostTLS{
		CertFile: batterysidecar.ClientCertFile,
		KeyFile:  batterysidecar.ClientKeyFile,
		CAFile:   batterysidecar.ServingCAFile,
	}
	if f.Hosts[0].TLS != want {
		t.Errorf("TLS %+v, want %+v", f.Hosts[0].TLS, want)
	}
	if got := h.published(); !got.Equal(Hosts{nodeA: addrA}) {
		t.Errorf("published %v, want node-a", got)
	}

	// Nothing changes: battery is left alone.
	h.advance(10 * window)
	if n := h.restarter.count(); n != 1 {
		t.Errorf("restarts = %d with nothing changed, want 1", n)
	}
}

//= docs/requirements/04-inventory.md#applying
//= type=test
//# When a change has settled and no restart window is open, the
//# Inventory Controller SHALL open a restart window of the configured
//# length, and SHALL apply every change that has settled by the time the
//# window closes in a single restart of battery.

// TestWindowBatchesSettledChanges covers IN-012: two Nodes that settle at
// different times within one window join battery in one restart, at the
// window's close; a change still settling when it closes waits for a
// window of its own.
func TestWindowBatchesSettledChanges(t *testing.T) {
	h := newHarness(t, host(nodeA, addrA))
	h.mustReconcile()
	r := h.advance(settle) // node-a settles: the window opens
	if _, open := h.state.WindowOpened(); !open {
		t.Fatal("no window opened when node-a settled")
	}
	if r.RequeueAfter != window {
		t.Errorf("RequeueAfter = %s, want the window %s", r.RequeueAfter, window)
	}

	h.clock.Advance(window - settle - 10*time.Second)
	h.create(host(nodeB, addrB)) // settles 10s before the window closes
	h.mustReconcile()
	h.advance(settle)
	if n := h.restarter.count(); n != 0 {
		t.Fatalf("restarts = %d before the window closed", n)
	}
	h.create(host("node-c", "10.0.0.3:9090")) // still settling at the close
	h.advance(10 * time.Second)               // the window closes
	if n := h.restarter.count(); n != 1 {
		t.Fatalf("restarts = %d at the window's close, want 1", n)
	}
	if got := hostsOf(h.lastRestart()); !got.Equal(Hosts{nodeA: addrA, nodeB: addrB}) {
		t.Errorf("battery restarted with %v, want node-a and node-b", got)
	}

	// node-c settles in a window of its own.
	h.advance(settle)
	h.advance(window)
	if n := h.restarter.count(); n != 2 {
		t.Fatalf("restarts = %d, want 2", n)
	}
	if gap := h.restarter.at[1].Sub(h.restarter.at[0]); gap < window {
		t.Errorf("restarts %s apart, want at least the window %s", gap, window)
	}
}

// TestWindowWhoseChangesFlappedBackCloses checks that a window whose
// changes all flapped back closes without restarting battery.
func TestWindowWhoseChangesFlappedBackCloses(t *testing.T) {
	h := newHarness(t, host(nodeA, addrA))
	h.mustReconcile()
	h.advance(settle)
	h.update(nodeA, notReady)
	h.mustReconcile()
	if _, open := h.state.WindowOpened(); open {
		t.Error("the window is still open with nothing to apply")
	}
	h.advance(window)
	if n := h.restarter.count(); n != 0 {
		t.Errorf("restarts = %d, want none", n)
	}
}

// TestFlappingHostRestartsAtMostOncePerWindow is the issue's debounce test:
// a Host whose report flaps, sometimes faster than the settle time and
// sometimes slower, restarts battery at most once per restart window.
func TestFlappingHostRestartsAtMostOncePerWindow(t *testing.T) {
	h := newHarness(t, host(nodeA, addrA), host(nodeB, addrB))
	joinAll(h)
	h.restarter.at, h.restarter.configs = nil, nil

	const tick = time.Second
	const duration = 30 * time.Minute
	start := h.clock.Now()
	// Flap node-b every 5s for five minutes, then every 100s, slower than
	// the settle time and the window, for the rest.
	for elapsed := time.Duration(0); elapsed < duration; elapsed += tick {
		period := 5 * time.Second
		if elapsed >= 5*time.Minute {
			period = 100 * time.Second
		}
		if elapsed%period == 0 {
			if (elapsed/period)%2 == 0 {
				h.update(nodeB, notReady)
			} else {
				h.update(nodeB, ready)
			}
		}
		h.advance(tick)
	}
	at := h.restarter.at
	if len(at) == 0 {
		t.Fatal("battery never restarted, though node-b's slow flaps settle")
	}
	if most := int(duration/window) + 1; len(at) > most {
		t.Errorf("restarts = %d in %s, want at most %d", len(at), duration, most)
	}
	for i := 1; i < len(at); i++ {
		if gap := at[i].Sub(at[i-1]); gap < window {
			t.Errorf("restarts %d and %d are %s apart, want at least %s", i-1, i, gap, window)
		}
	}
	for _, t0 := range at {
		if t0.Before(start.Add(5 * time.Minute)) {
			t.Errorf("battery restarted at %s, while node-b flapped faster than the settle time", t0)
		}
	}
}

// TestResumeFinishesAnInterruptedRestart checks that a configuration
// written and not restarted into, because the restart failed or the
// Operator stopped, is restarted into on the next reconcile, before
// anything else, and that its Hosts are published only then.
func TestResumeFinishesAnInterruptedRestart(t *testing.T) {
	h := newHarness(t, host(nodeA, addrA))
	h.restarter.err = errors.New("battery did not answer")
	h.mustReconcile()
	h.advance(settle)
	h.clock.Advance(window)
	if _, err := h.reconcile(); err == nil {
		t.Fatal("a failed restart did not fail the reconcile")
	}
	if !h.stored().Pending {
		t.Fatal("the configuration written is not marked pending")
	}
	if got := h.published(); got == nil || len(got) != 0 {
		t.Errorf("after a failed restart, published %v, want the Hosts battery ran with", got)
	}

	// The Operator restarts: its memory is gone, the ConfigMap is not.
	h.state, h.hosts = NewState(), NewHostSet()
	h.restarter.err = nil
	h.mustReconcile()
	if n := h.restarter.count(); n != 1 {
		t.Fatalf("restarts = %d, want the interrupted one finished", n)
	}
	if h.stored().Pending {
		t.Error("the configuration is still pending")
	}
	if got := h.published(); !got.Equal(Hosts{nodeA: addrA}) {
		t.Errorf("published %v, want node-a", got)
	}
}

func TestOptions(t *testing.T) {
	if err := (Options{SettleTime: -time.Second}).Validate(); err == nil {
		t.Error("a negative settle time is valid")
	}
	if err := (Options{RestartWindow: -time.Second}).Validate(); err == nil {
		t.Error("a negative restart window is valid")
	}
	if err := (Options{}).Validate(); err != nil {
		t.Errorf("zero timings: %v", err)
	}
}
