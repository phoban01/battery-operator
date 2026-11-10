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

package fakebattery

import (
	"testing"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"google.golang.org/grpc/codes"
)

// drainedHostB sets up a Pool of four MicroVMs on host-a and host-b, two on
// each, claims until one Lease is on host-b, and drains host-b. It returns
// the Lease on host-b and the uid of the available MicroVM host-b held,
// which the drain deletes.
func drainedHostB(t *testing.T) (h *harness, admin poolmgrv1.HostAdminClient, onB *claimed, drainedUID string) {
	t.Helper()
	h = newHarness(t, Config{}, hostA, hostB)
	admin = h.hostAdmin()
	spec := h.spec("pool", 4, hostA, hostB)
	spec.Replenishment = replenishment{Type: poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE}
	h.createPool(spec)
	if got := h.vmHosts(); got[hostA] != 2 || got[hostB] != 2 {
		t.Fatalf("placement = %v, want 2 on each Host", got)
	}
	// REPLACE_ON_DELETE: a claim is not replaced, so host-b keeps one
	// available MicroVM beside the leased one.
	for range 3 {
		c := h.claim("pool")
		if c.Host == hostB {
			onB = c
			break
		}
	}
	if onB == nil {
		t.Fatal("no claim landed on host-b")
	}
	for _, r := range h.b.VMs() {
		if r.Host == hostB && r.Phase == poolmgrv1.VMPhase_AVAILABLE {
			drainedUID = r.UID
		}
	}
	if drainedUID == "" {
		t.Fatalf("no available microvm on host-b before the drain: %v", h.b.VMs())
	}

	got, err := admin.DrainHost(h.ctx, &poolmgrv1.DrainHostRequest{Name: hostB, Reason: "scale-down"})
	if err != nil {
		t.Fatalf("DrainHost: %v", err)
	}
	if !got.GetCordoned() || !got.GetDraining() || got.GetDrainingAt() == nil || got.GetCordonedAt() == nil ||
		got.GetCordonedReason() != "scale-down" {
		t.Fatalf("DrainHost = %v, want cordoned and draining with the reason", got)
	}
	return h, admin, onB, drainedUID
}

//= docs/requirements/10-battery.md#hosts
//= type=test
//# When `DrainHost` names a Host battery holds, battery SHALL
//# cordon the Host, mark it draining, and set every MicroVM on the Host
//# whose phase is not `LEASED`, `PRE_LEASE_HOOK_RUNNING` or `DELETING` to
//# `DELETING`, all in one transaction, and SHALL then delete each of those
//# MicroVMs through `flintlockd` before it answers, leaving one `flintlockd`
//# does not confirm in `DELETING` for the sweep.

//= docs/requirements/10-battery.md#hosts
//= type=test
//# battery SHALL NOT delete a MicroVM in the phase `LEASED` or
//# `PRE_LEASE_HOOK_RUNNING` because its Host is draining; such a MicroVM
//# ends by release or expiry, as on a cordoned Host.

//= docs/requirements/10-battery.md#hosts
//= type=test
//# battery SHALL record `VM_DELETED_ON_HOST_DRAIN` once for each
//# MicroVM it sets to `DELETING` because its Host is draining, when it
//# sets the phase, and SHALL record no other `VM_DELETED_*` event for it.

// TestDrainHostDeletesTheUnleasedMicroVMs: the drain deletes host-b's
// available MicroVM, with one VM_DELETED_ON_HOST_DRAIN, and the Pool
// replaces it on host-a; the leased MicroVM stays until its release, after
// which nothing is counted against host-b, and the Host can be removed
// once the Pool no longer names it.
func TestDrainHostDeletesTheUnleasedMicroVMs(t *testing.T) {
	h, admin, onB, drainedUID := drainedHostB(t)
	b := h.stubs[hostB]

	got := h.collectUntil(poolmgrv1.EventType_VM_DELETED_ON_HOST_DRAIN)
	if e := got[len(got)-1]; e.VMUID != drainedUID {
		t.Fatalf("VM_DELETED_ON_HOST_DRAIN for %s, want %s", e.VMUID, drainedUID)
	}
	for _, e := range got[:len(got)-1] {
		if e.Type == poolmgrv1.EventType_VM_DELETED_ON_HOST_DRAIN {
			t.Fatalf("a second VM_DELETED_ON_HOST_DRAIN before the first's: %v", got)
		}
	}
	h.waitDeletion(b, drainedUID)
	// The Pool refills on host-a, the only Host it may still place on.
	h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
	if got := h.vmHosts(); got[hostA] != 3 || got[hostB] != 1 {
		t.Fatalf("placement after the drain = %v, want 3 on host-a and the leased one on host-b", got)
	}
	if !b.has(onB.VMUID) {
		t.Fatalf("the leased microvm %s was deleted by the drain", onB.VMUID)
	}
	st, err := admin.GetHost(h.ctx, &poolmgrv1.GetHostRequest{Name: hostB})
	if err != nil {
		t.Fatalf("GetHost: %v", err)
	}
	if st.GetVmCount() != 1 || !st.GetHost().GetDraining() {
		t.Fatalf("GetHost after the drain = %v, want vm_count 1 and draining", st)
	}

	// The release ends the Lease as on any Host: its own event, not the
	// drain's, and then nothing is counted against host-b.
	h.release(onB.LeaseID)
	if n := h.seen[poolmgrv1.EventType_VM_DELETED_ON_HOST_DRAIN]; n != 1 {
		t.Fatalf("saw %d VM_DELETED_ON_HOST_DRAIN, want 1", n)
	}
	st, err = admin.GetHost(h.ctx, &poolmgrv1.GetHostRequest{Name: hostB})
	if err != nil {
		t.Fatalf("GetHost: %v", err)
	}
	if st.GetVmCount() != 0 {
		t.Fatalf("GetHost after the release = %v, want vm_count 0", st)
	}
	if _, err := h.client.UpdatePool(h.ctx, h.spec("pool", 4, hostA)); err != nil {
		t.Fatalf("UpdatePool: %v", err)
	}
	if _, err := admin.RemoveHost(h.ctx, &poolmgrv1.RemoveHostRequest{Name: hostB}); err != nil {
		t.Fatalf("RemoveHost after the drain: %v", err)
	}
}

//= docs/requirements/10-battery.md#hosts
//= type=test
//# While a Host is draining, battery SHALL, in every sweep, set
//# every MicroVM on the Host whose phase is not `LEASED`,
//# `PRE_LEASE_HOOK_RUNNING` or `DELETING` to `DELETING` and delete it
//# through `flintlockd`.

// TestADrainKeepsDeletingOnEveryTick: a MicroVM that becomes unleased on
// a draining Host after the call, here a leased one put back to AVAILABLE
// as a claim that loses a request id race does, is deleted by the next
// tick with the drain's event.
func TestADrainKeepsDeletingOnEveryTick(t *testing.T) {
	h, _, onB, drainedUID := drainedHostB(t)
	b := h.stubs[hostB]
	h.waitEvent(poolmgrv1.EventType_VM_DELETED_ON_HOST_DRAIN)
	h.waitDeletion(b, drainedUID)

	h.setPhase(onB.VMUID, poolmgrv1.VMPhase_AVAILABLE)
	h.tick()
	if e := h.waitEvent(poolmgrv1.EventType_VM_DELETED_ON_HOST_DRAIN); e.VMUID != onB.VMUID {
		t.Fatalf("VM_DELETED_ON_HOST_DRAIN for %s, want %s", e.VMUID, onB.VMUID)
	}
	h.waitDeletion(b, onB.VMUID)
	h.waitUntil("host-b is empty", func() bool { return h.vmHosts()[hostB] == 0 })
}

//= docs/requirements/10-battery.md#hosts
//= type=test
//# battery SHALL end a Host's drain only in `UncordonHost`, which
//# SHALL also uncordon the Host; `CordonHost` SHALL keep a draining Host
//# draining, and `UpdateHost` SHALL keep a Host's drain state.

// TestUncordonHostEndsTheDrain: CordonHost and UpdateHost leave a draining
// Host draining; UncordonHost ends the drain with the cordon, and the
// Host takes MicroVMs again.
func TestUncordonHostEndsTheDrain(t *testing.T) {
	b := newTestHost(t, hostB)
	h := newHarness(t, Config{Dial: dialer(b)}, hostA)
	admin := h.hostAdmin()
	if _, err := admin.AddHost(h.ctx, &poolmgrv1.AddHostRequest{Host: hostRequest(hostB)}); err != nil {
		t.Fatalf("AddHost: %v", err)
	}
	if _, err := admin.DrainHost(h.ctx, &poolmgrv1.DrainHostRequest{Name: hostB, Reason: "drain"}); err != nil {
		t.Fatalf("DrainHost: %v", err)
	}

	got, err := admin.CordonHost(h.ctx, &poolmgrv1.CordonHostRequest{Name: hostB, Reason: "keep"})
	if err != nil {
		t.Fatalf("CordonHost: %v", err)
	}
	if !got.GetDraining() || got.GetDrainingAt() == nil || got.GetCordonedReason() != "keep" {
		t.Fatalf("CordonHost on a draining Host = %v, want it still draining with the new reason", got)
	}
	got, err = admin.UpdateHost(h.ctx, &poolmgrv1.UpdateHostRequest{Host: hostRequest(hostB)})
	if err != nil {
		t.Fatalf("UpdateHost: %v", err)
	}
	if !got.GetDraining() || !got.GetCordoned() {
		t.Fatalf("UpdateHost = %v, want the drain kept", got)
	}

	got, err = admin.UncordonHost(h.ctx, &poolmgrv1.UncordonHostRequest{Name: hostB})
	if err != nil {
		t.Fatalf("UncordonHost: %v", err)
	}
	if got.GetDraining() || got.GetDrainingAt() != nil || got.GetCordoned() {
		t.Fatalf("UncordonHost = %v, want neither draining nor cordoned", got)
	}
	h.createPool(h.spec("pool", 2, hostB))
	h.tick()
	if got := h.vmHosts(); got[hostB] != 2 {
		t.Fatalf("placement after the uncordon = %v, want 2 on host-b", got)
	}
}

//= docs/requirements/10-battery.md#hosts
//= type=test
//# If `DrainHost` names a draining Host, then battery SHALL keep the
//# Host's `draining_at` and `cordoned_at`, SHALL replace its
//# `cordoned_reason` only when the request gives one, and SHALL answer with
//# the Host and no error.

// TestDrainHostIsIdempotent: a second DrainHost keeps the times of the
// first and the reason unless it gives one.
func TestDrainHostIsIdempotent(t *testing.T) {
	h := newHarness(t, Config{}, hostA, hostB)
	admin := h.hostAdmin()
	first, err := admin.DrainHost(h.ctx, &poolmgrv1.DrainHostRequest{Name: hostB, Reason: "first"})
	if err != nil {
		t.Fatalf("DrainHost: %v", err)
	}
	h.advance(time.Minute)

	again, err := admin.DrainHost(h.ctx, &poolmgrv1.DrainHostRequest{Name: hostB})
	if err != nil {
		t.Fatalf("DrainHost again: %v", err)
	}
	if !again.GetDrainingAt().AsTime().Equal(first.GetDrainingAt().AsTime()) ||
		!again.GetCordonedAt().AsTime().Equal(first.GetCordonedAt().AsTime()) ||
		again.GetCordonedReason() != "first" {
		t.Fatalf("DrainHost again = %v, want the times and reason of the first, %v", again, first)
	}
	third, err := admin.DrainHost(h.ctx, &poolmgrv1.DrainHostRequest{Name: hostB, Reason: "third"})
	if err != nil {
		t.Fatalf("DrainHost with a reason: %v", err)
	}
	if !third.GetDrainingAt().AsTime().Equal(first.GetDrainingAt().AsTime()) || third.GetCordonedReason() != "third" {
		t.Fatalf("DrainHost with a reason = %v, want the first's times and the new reason", third)
	}
}

//= docs/requirements/10-battery.md#hosts
//= type=test
//# If `DrainHost` names a Host battery does not hold, then battery
//# SHALL answer `NOT_FOUND`.

func TestDrainHostOnAnUnknownHostIsNotFound(t *testing.T) {
	h := newHarness(t, Config{}, hostA)
	_, err := h.hostAdmin().DrainHost(h.ctx, &poolmgrv1.DrainHostRequest{Name: hostB})
	if statusCode(t, err) != codes.NotFound {
		t.Fatalf("DrainHost(%s) = %v, want NOT_FOUND", hostB, err)
	}
}
