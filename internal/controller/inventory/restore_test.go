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

	corev1 "k8s.io/api/core/v1"

	"github.com/phoban01/battery-operator/internal/batterysidecar"
)

// applyManifests does to battery's ConfigMap what applying the Manifests
// again does: it puts back the configuration they ship, with no Hosts, and
// leaves the annotations they do not give alone.
func (h *harness) applyManifests() {
	h.t.Helper()
	ctx := context.Background()
	cm := &corev1.ConfigMap{}
	if err := h.c.Get(ctx, configMapKey, cm); err != nil {
		h.t.Fatal(err)
	}
	cm.Data = shippedConfigMap(h.t).Data
	if err := h.c.Update(ctx, cm); err != nil {
		h.t.Fatal(err)
	}
	if got := h.stored().Hosts; len(got) != 0 {
		h.t.Fatalf("after the apply, the ConfigMap names %v, want no Hosts", got)
	}
}

//= docs/requirements/04-inventory.md#keeping
//= type=test
//# If battery's configuration names Hosts other than those the
//# Inventory Controller last wrote to it, then the Inventory Controller SHALL
//# write those Hosts back to battery's configuration without restarting
//# battery.

// TestRestorePutsBackHostsAnApplyReset covers IN-014: once battery runs
// with a Host, an apply of the Manifests that resets its ConfigMap is
// undone on the next reconcile, with the configuration battery restarted
// with, and battery is not restarted, then or later.
func TestRestorePutsBackHostsAnApplyReset(t *testing.T) {
	h := newHarness(t, host(nodeA, addrA))
	joinAll(h)
	if n := h.restarter.count(); n != 1 {
		t.Fatalf("restarts = %d, want 1", n)
	}

	h.applyManifests()
	h.mustReconcile()
	stored := h.stored()
	if !stored.Hosts.Equal(Hosts{nodeA: addrA}) {
		t.Fatalf("after the reconcile, the ConfigMap names %v, want node-a", stored.Hosts)
	}
	if string(stored.Raw) != string(h.restarter.configs[0]) {
		t.Error("the configuration put back is not the one battery restarted with")
	}
	if stored.Pending {
		t.Error("the configuration put back is marked pending")
	}
	if got := h.published(); !got.Equal(Hosts{nodeA: addrA}) {
		t.Errorf("published %v, want node-a", got)
	}

	// Nothing settles or opens a window for it: battery is left alone.
	h.advance(settle)
	h.advance(10 * window)
	if n := h.restarter.count(); n != 1 {
		t.Errorf("restarts = %d after the apply, want 1", n)
	}
}

// TestRestoreAfterTheOperatorRestarts checks that the Hosts written are
// put back by an Operator that did not write them: the record is on the
// ConfigMap, not in memory.
func TestRestoreAfterTheOperatorRestarts(t *testing.T) {
	h := newHarness(t, host(nodeA, addrA), host(nodeB, addrB))
	joinAll(h)

	h.applyManifests()
	h.state, h.hosts = NewState(), NewHostSet()
	h.mustReconcile()
	want := Hosts{nodeA: addrA, nodeB: addrB}
	if got := h.stored().Hosts; !got.Equal(want) {
		t.Fatalf("the ConfigMap names %v, want %v", got, want)
	}
	if got := h.published(); !got.Equal(want) {
		t.Errorf("published %v, want %v", got, want)
	}
	h.advance(settle)
	h.advance(window)
	if n := h.restarter.count(); n != 1 {
		t.Errorf("restarts = %d, want 1", n)
	}
}

// TestRestoreRecordsTheHostsBatteryStartedWith checks that where no Hosts
// are recorded as written, the first reconcile records those the
// configuration names, without restarting battery.
func TestRestoreRecordsTheHostsBatteryStartedWith(t *testing.T) {
	h := newHarness(t)
	if got := h.stored().Written; got != nil {
		t.Fatalf("the shipped ConfigMap records %v as written", got)
	}
	h.mustReconcile()
	if got := h.stored().Written; got == nil || len(got) != 0 {
		t.Errorf("recorded %v as written, want no Hosts", got)
	}
	if n := h.restarter.count(); n != 0 {
		t.Errorf("restarts = %d, want none", n)
	}
}

// TestRestoreRecordsAgainOverAnUnreadableRecord checks that a record of
// the Hosts written that is not a JSON object is made again from the
// configuration rather than stopping the controller.
func TestRestoreRecordsAgainOverAnUnreadableRecord(t *testing.T) {
	h := newHarness(t)
	h.mustReconcile()
	ctx := context.Background()
	cm := &corev1.ConfigMap{}
	if err := h.c.Get(ctx, configMapKey, cm); err != nil {
		t.Fatal(err)
	}
	cm.Annotations[HostsAnnotation] = "not json"
	if err := h.c.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}
	h.mustReconcile()
	if err := h.c.Get(ctx, configMapKey, cm); err != nil {
		t.Fatal(err)
	}
	if got := cm.Annotations[HostsAnnotation]; got != "{}" {
		t.Errorf("%s = %q, want {}", HostsAnnotation, got)
	}
}

// TestRestoreFinishesAPendingRestartWithTheHostsPutBack checks that an
// apply that resets the ConfigMap while a restart is pending does not
// lose the restart's Hosts: Resume restarts battery with them.
func TestRestoreFinishesAPendingRestartWithTheHostsPutBack(t *testing.T) {
	h := newHarness(t, host(nodeA, addrA))
	h.restarter.err = errors.New("battery did not answer")
	h.mustReconcile()
	h.advance(settle)
	h.clock.Advance(window)
	if _, err := h.reconcile(); err == nil {
		t.Fatal("a failed restart did not fail the reconcile")
	}

	h.applyManifests()
	h.restarter.err = nil
	h.mustReconcile()
	if n := h.restarter.count(); n != 1 {
		t.Fatalf("restarts = %d, want the pending one finished", n)
	}
	if got := hostsOf(h.lastRestart()); !got.Equal(Hosts{nodeA: addrA}) {
		t.Errorf("battery restarted with %v, want node-a", got)
	}
	if stored := h.stored(); stored.Pending || !stored.Hosts.Equal(Hosts{nodeA: addrA}) {
		t.Errorf("stored %+v, want node-a and no restart pending", stored)
	}
}

// TestRestoreLeavesTheControllersOwnChanges checks that a change the
// Inventory Controller writes is not taken for one to undo: a Host leaving
// stays gone.
func TestRestoreLeavesTheControllersOwnChanges(t *testing.T) {
	h := newHarness(t, host(nodeA, addrA), host(nodeB, addrB))
	joinAll(h)
	h.update(nodeB, cordon)
	h.mustReconcile()
	h.advance(settle)
	h.advance(window)
	if n := h.restarter.count(); n != 2 {
		t.Fatalf("restarts = %d, want 2", n)
	}
	h.mustReconcile()
	stored := h.stored()
	if !stored.Hosts.Equal(Hosts{nodeA: addrA}) || !stored.Written.Equal(stored.Hosts) {
		t.Errorf("stored Hosts %v, written %v, want node-a for both", stored.Hosts, stored.Written)
	}
	f, err := batterysidecar.Parse(stored.Raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := hostsOf(f); !got.Equal(Hosts{nodeA: addrA}) {
		t.Errorf("the configuration names %v, want node-a", got)
	}
}
