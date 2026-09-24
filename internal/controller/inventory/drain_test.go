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
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// leaveAndCloseWindow has two Hosts join, a Pool name both, and then
// node-a be cordoned, up to the reconcile at which its window closes.
func leaveAndCloseWindow(t *testing.T) (*harness, Result) {
	t.Helper()
	h := newHarness(t, host(nodeA, addrA), host(nodeB, addrB))
	joinAll(h)
	h.pools.set("runners", nodeA, nodeB)
	h.update(nodeA, cordon)
	h.mustReconcile()
	h.advance(settle)
	return h, h.advance(window)
}

//= docs/requirements/04-inventory.md#applying
//= type=test
//# When a restart of battery would remove a Host, the Inventory
//# Controller SHALL first give the Pool Controller the Hosts that remain,
//# and SHALL restart battery only once no Pool in battery names the removed
//# Host in its `flintlock_hosts` or the configured drain timeout has passed.

// TestRestartThatRemovesAHostWaitsForThePools covers IN-013: once the
// window closes, the Hosts that remain are published and battery is not
// restarted while a Pool still names the leaving Host; it is restarted
// once no Pool does.
func TestRestartThatRemovesAHostWaitsForThePools(t *testing.T) {
	h, res := leaveAndCloseWindow(t)
	before := h.restarter.count()
	if got := h.published(); !got.Equal(Hosts{nodeB: addrB}) {
		t.Errorf("published %v, want node-b alone before the restart", got)
	}
	if h.stored().Hosts[nodeA] == "" {
		t.Error("battery's configuration lost node-a while a Pool still names it")
	}
	if res.RequeueAfter != drainPoll {
		t.Errorf("RequeueAfter = %s, want %s", res.RequeueAfter, drainPoll)
	}

	h.advance(drainPoll)
	if h.restarter.count() != before {
		t.Fatal("battery restarted while a Pool still names node-a")
	}

	// The Pool Controller drops node-a.
	h.pools.set("runners", nodeB)
	h.advance(drainPoll)
	if h.restarter.count() != before+1 {
		t.Fatalf("restarts = %d, want %d", h.restarter.count(), before+1)
	}
	if got := hostsOf(h.lastRestart()); !got.Equal(Hosts{nodeB: addrB}) {
		t.Errorf("battery restarted with %v, want node-b alone", got)
	}
}

// TestRestartThatRemovesAHostWaitsAtMostTheDrainTimeout: a Pool the Pool
// Controller cannot update holds the restart back for the drain timeout
// and no longer (IN-013).
func TestRestartThatRemovesAHostWaitsAtMostTheDrainTimeout(t *testing.T) {
	h, _ := leaveAndCloseWindow(t)
	before := h.restarter.count()
	var waited time.Duration
	for waited < drain-drainPoll {
		res := h.advance(drainPoll)
		waited += drainPoll
		if h.restarter.count() != before {
			t.Fatalf("battery restarted after %s, before the drain timeout", waited)
		}
		if res.RequeueAfter <= 0 {
			t.Fatalf("no requeue after %s", waited)
		}
	}
	h.advance(drainPoll)
	if h.restarter.count() != before+1 {
		t.Fatalf("restarts = %d after the drain timeout, want %d", h.restarter.count(), before+1)
	}
}

// TestDrainWaitsThroughBatteryErrors: while battery's Pools cannot be
// listed, the restart waits, for the drain timeout at most.
func TestDrainWaitsThroughBatteryErrors(t *testing.T) {
	h := newHarness(t, host(nodeA, addrA), host(nodeB, addrB))
	joinAll(h)
	h.pools.fail(errors.New("battery is unavailable"))
	h.update(nodeA, cordon)
	h.mustReconcile()
	h.advance(settle)
	h.advance(window)
	before := h.restarter.count()
	h.advance(drain - drainPoll)
	if h.restarter.count() != before {
		t.Fatal("battery restarted before the drain timeout while its Pools could not be listed")
	}
	h.advance(drainPoll)
	if h.restarter.count() != before+1 {
		t.Fatalf("restarts = %d, want %d", h.restarter.count(), before+1)
	}
}

// TestDrainForAChangeThatFlapsBackGivesTheHostBack: a Host cordoned and
// uncordoned while the restart waits for the Pools is published again,
// and battery is not restarted.
func TestDrainForAChangeThatFlapsBackGivesTheHostBack(t *testing.T) {
	h, _ := leaveAndCloseWindow(t)
	before := h.restarter.count()
	h.update(nodeA, func(n *corev1.Node) { n.Spec.Unschedulable = false })
	h.mustReconcile()
	if h.restarter.count() != before {
		t.Error("battery restarted for a change that flapped back")
	}
	if got := h.published(); !got.Equal(Hosts{nodeA: addrA, nodeB: addrB}) {
		t.Errorf("published %v, want node-a and node-b", got)
	}
}

// TestJoiningHostNeedsNoDrain: a restart that only adds Hosts does not
// look at battery's Pools, and the new Host is published only once battery
// has restarted with it.
func TestJoiningHostNeedsNoDrain(t *testing.T) {
	h := newHarness(t, host(nodeA, addrA))
	h.restarter.during = func() {
		if h.hosts.Has(nodeA) {
			t.Error("node-a was published before battery restarted with it")
		}
	}
	joinAll(h)
	if h.restarter.count() != 1 {
		t.Fatalf("restarts = %d, want 1", h.restarter.count())
	}
	if !h.hosts.Has(nodeA) {
		t.Error("node-a is not published after the restart")
	}
	if h.pools.lists != 0 {
		t.Errorf("battery's Pools were listed %d times, want none", h.pools.lists)
	}
}
