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
	"errors"
	"slices"
	"testing"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"google.golang.org/grpc/codes"
)

// drained is spec as the Pool Controller drains a deleted Pool that battery
// refuses to delete (03-pools.md, Deletion): size 0 and MIN_SIZE_THRESHOLD
// with a min_size of 1.
func drained(spec poolSpec) poolSpec {
	one := int32(1)
	spec.Size = 0
	spec.Replenishment = replenishment{Type: poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD, MinSize: &one}
	return spec
}

// setPhase puts the MicroVM uid in phase, for a state no RPC reaches on
// its own, such as a claim whose pre-lease hooks are running.
func (h *harness) setPhase(uid string, phase VMPhase) {
	h.t.Helper()
	h.b.mu.Lock()
	defer h.b.mu.Unlock()
	vm := h.b.vmByUID[uid]
	if vm == nil {
		h.t.Fatalf("no microvm %s", uid)
	}
	h.b.setPhaseLocked(vm, phase)
}

// waitUntil polls cond until it holds, or fails the test with what.
func (h *harness) waitUntil(what string, cond func() bool) {
	h.t.Helper()
	for !cond() {
		if h.ctx.Err() != nil {
			h.t.Fatalf("waiting until %s: %v", what, h.ctx.Err())
		}
		time.Sleep(time.Millisecond)
	}
}

// phases lists each MicroVM's uid and phase, for comparing the fake's state
// before and after a call.
func phases(vms []VMRecord) []string {
	out := make([]string, 0, len(vms))
	for _, vm := range vms {
		out = append(out, vm.UID+"="+vm.Phase.String())
	}
	return out
}

// TestDeletePoolRefusesWhileAMicroVMIsLeased covers BA-070 and BA-071:
// DeletePool refuses a Pool with a leased or claiming MicroVM and changes
// nothing, deletes the Pool once the Lease has gone, and then does not
// know it.
func TestDeletePoolRefusesWhileAMicroVMIsLeased(t *testing.T) {
	//= docs/requirements/10-battery.md#delete-pool
	//= type=test
	//# If a Pool owns a MicroVM in the phase `LEASED` or
	//# `PRE_LEASE_HOOK_RUNNING`, then battery SHALL answer a `DeletePool`
	//# without `force` for it with `FAILED_PRECONDITION` and keep the Pool and
	//# its MicroVMs.

	//= docs/requirements/10-battery.md#delete-pool
	//= type=test
	//# If battery holds no Pool of the name and namespace a
	//# `DeletePool` names, then battery SHALL answer it with `NOT_FOUND`.
	h := newHarness(t, Config{}, hostA)
	host := h.stubs[hostA]
	spec := h.spec("pool", 2, hostA)
	// REPLACE_ON_DELETE provisions nothing on a claim, so the state
	// compared below is still.
	spec.Replenishment = replenishment{Type: poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE}
	h.createPool(spec)
	refused := func(what string) {
		t.Helper()
		before := phases(h.b.VMs())
		if err := h.client.DeletePool(h.ctx, h.ref("pool")); statusCode(t, err) != codes.FailedPrecondition {
			t.Fatalf("DeletePool with %s = %v, want FAILED_PRECONDITION", what, err)
		}
		h.pool("pool")
		if after := phases(h.b.VMs()); !slices.Equal(after, before) {
			t.Fatalf("DeletePool with %s changed the microvms from %v to %v", what, before, after)
		}
		if n := host.live(); n != 2 {
			t.Fatalf("%d microvms on the host after DeletePool with %s, want 2", n, what)
		}
	}

	claim := h.claim("pool")
	refused("a leased microvm")
	if _, err := h.client.Heartbeat(h.ctx, claim.LeaseID); err != nil {
		t.Fatalf("Heartbeat after the refusal: %v, want the lease kept", err)
	}

	// The release deletes the leased MicroVM, and REPLACE_ON_DELETE
	// replaces it. Then one MicroVM is claimed with its pre-lease hooks
	// still running.
	h.release(claim.LeaseID)
	h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
	var other string
	for _, vm := range h.b.VMs() {
		if vm.Phase == poolmgrv1.VMPhase_AVAILABLE {
			other = vm.UID
		}
	}
	h.setPhase(other, poolmgrv1.VMPhase_PRE_LEASE_HOOK_RUNNING)
	before := phases(h.b.VMs())
	if err := h.client.DeletePool(h.ctx, h.ref("pool")); statusCode(t, err) != codes.FailedPrecondition {
		t.Fatalf("DeletePool with a claiming microvm = %v, want FAILED_PRECONDITION", err)
	}
	if after := phases(h.b.VMs()); !slices.Equal(after, before) {
		t.Fatalf("DeletePool with a claiming microvm changed the microvms from %v to %v", before, after)
	}
	h.setPhase(other, poolmgrv1.VMPhase_AVAILABLE)

	if err := h.client.DeletePool(h.ctx, h.ref("pool")); err != nil {
		t.Fatalf("DeletePool with no lease: %v", err)
	}
	if err := h.client.DeletePool(h.ctx, h.ref("pool")); statusCode(t, err) != codes.NotFound {
		t.Fatalf("DeletePool of a deleted pool = %v, want NOT_FOUND", err)
	}
}

// TestDeletePoolDrainsThePool covers BA-070's other half: with no leased
// MicroVM, DeletePool deletes the Pool with every MicroVM it owns,
// available, provisioning and quarantined alike, records
// VM_DELETED_ON_POOL_DELETE for each that reached its Host, and provisions
// nothing more for it.
func TestDeletePoolDrainsThePool(t *testing.T) {
	h := newHarness(t, Config{}, hostA)
	host := h.stubs[hostA]
	spec := h.spec("pool", 3, hostA)
	spec.HookFailurePolicy = poolmgrv1.HookFailurePolicy_QUARANTINE
	h.b.SetFaults(Faults{HookFailures: []HookFailure{{Hook: HookCreate, Remaining: 1}}})
	if _, err := h.client.CreatePool(h.ctx, spec); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	for h.seen[poolmgrv1.EventType_VM_AVAILABLE] < 2 || h.seen[poolmgrv1.EventType_VM_HOOK_FAILED] < 1 {
		e, err := h.events.Recv()
		if err != nil {
			t.Fatalf("waiting for the pool to fill: %v", err)
		}
		h.seen[e.Type]++
	}

	// Two more MicroVMs whose creates the Host holds.
	gate := make(chan struct{})
	host.set(func(s *testHost) { s.createGate = gate })
	spec.Size = 4
	if _, err := h.client.UpdatePool(h.ctx, spec); err != nil {
		t.Fatalf("UpdatePool: %v", err)
	}
	<-host.createEntered
	st := h.pool("pool").Status
	if st.Available != 2 || st.Quarantined != 1 || st.Provisioning == 0 {
		t.Fatalf("status before DeletePool = %+v, want 2 available, 1 quarantined and some provisioning", st)
	}
	onHost := h.uids()

	if err := h.client.DeletePool(h.ctx, h.ref("pool")); err != nil {
		t.Fatalf("DeletePool with no lease: %v", err)
	}
	if _, err := h.client.GetPool(h.ctx, h.ref("pool")); statusCode(t, err) != codes.NotFound {
		t.Fatalf("GetPool after DeletePool = %v, want NOT_FOUND", err)
	}
	var deleted []string
	for len(deleted) < len(onHost) {
		deleted = append(deleted, h.waitEvent(poolmgrv1.EventType_VM_DELETED_ON_POOL_DELETE).VMUID)
	}
	slices.Sort(deleted)
	slices.Sort(onHost)
	if !slices.Equal(deleted, onHost) {
		t.Fatalf("VM_DELETED_ON_POOL_DELETE for %v, want one for each microvm on the host, %v", deleted, onHost)
	}
	for _, uid := range onHost {
		if host.has(uid) {
			t.Errorf("microvm %s is still on the host after DeletePool", uid)
		}
	}

	// The held creates finish, and their MicroVMs go too.
	close(gate)
	h.waitUntil("the held creates are deleted", func() bool { return host.live() == 0 && len(h.b.VMs()) == 0 })
	h.advance(testInterval)
	if n := host.live(); n != 0 {
		t.Fatalf("%d microvms on the host after a tick, want none for a deleted pool", n)
	}
}

// TestDeletePoolLeavesARefusedDeletionToTheSweep: a MicroVM whose Host
// refuses the deletion stays DELETING after DeletePool, and the next tick
// deletes it, as battery's sweep does.
func TestDeletePoolLeavesARefusedDeletionToTheSweep(t *testing.T) {
	h := newHarness(t, Config{}, hostA)
	host := h.stubs[hostA]
	h.createPool(h.spec("pool", 1, hostA))
	host.set(func(s *testHost) { s.deleteErr = errors.New("flintlockd is down") })
	if err := h.client.DeletePool(h.ctx, h.ref("pool")); err != nil {
		t.Fatalf("DeletePool: %v", err)
	}
	vms := h.b.VMs()
	if len(vms) != 1 || vms[0].Phase != poolmgrv1.VMPhase_DELETING || host.live() != 1 {
		t.Fatalf("microvms = %v, %d on the host; want the one left DELETING", vms, host.live())
	}
	host.set(func(s *testHost) { s.deleteErr = nil })
	h.advance(testInterval)
	h.waitUntil("the sweep deletes the microvm", func() bool { return host.live() == 0 && len(h.b.VMs()) == 0 })
}

// TestForcedDeletePoolTakesTheLeases: with force, DeletePool deletes a
// Pool with a leased MicroVM, and the Lease with it.
func TestForcedDeletePoolTakesTheLeases(t *testing.T) {
	h := newHarness(t, Config{}, hostA)
	host := h.stubs[hostA]
	spec := h.spec("pool", 2, hostA)
	spec.Replenishment = replenishment{Type: poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE}
	h.createPool(spec)
	claim := h.claim("pool")
	if err := h.client.ForceDeletePool(h.ctx, h.ref("pool")); err != nil {
		t.Fatalf("DeletePool with force: %v", err)
	}
	if _, err := h.client.Heartbeat(h.ctx, claim.LeaseID); statusCode(t, err) != codes.NotFound {
		t.Fatalf("Heartbeat after a forced DeletePool = %v, want NOT_FOUND", err)
	}
	if n := host.live(); n != 0 {
		t.Fatalf("%d microvms on the host after a forced DeletePool, want none", n)
	}
}

// TestADrainedPoolIsNotReplenished covers BA-072: a MIN_SIZE_THRESHOLD
// Pool of size 0 gets no new MicroVM from a tick, a claim or a deletion.
func TestADrainedPoolIsNotReplenished(t *testing.T) {
	//= docs/requirements/10-battery.md#delete-pool
	//= type=test
	//# While a Pool's replenishment strategy is `MIN_SIZE_THRESHOLD`
	//# and its size is 0, battery SHALL provision no MicroVM for it.
	for _, typ := range []poolmgrv1.ReplenishmentStrategyType{
		poolmgrv1.ReplenishmentStrategyType_IMMEDIATE_ON_LEASE,
		poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE,
	} {
		t.Run("from "+typ.String(), func(t *testing.T) {
			h := newHarness(t, Config{}, hostA)
			host := h.stubs[hostA]
			spec := h.spec("pool", 2, hostA)
			spec.Replenishment = replenishment{Type: typ}
			h.createPool(spec)
			if _, err := h.client.UpdatePool(h.ctx, drained(spec)); err != nil {
				t.Fatalf("UpdatePool: %v", err)
			}
			h.release(h.claim("pool").LeaseID)
			h.release(h.claim("pool").LeaseID)
			for range 3 {
				h.advance(testInterval)
			}
			if vms := h.b.VMs(); len(vms) != 0 {
				t.Fatalf("microvms after the drain = %v, want none", vms)
			}
			if n := host.live(); n != 0 {
				t.Fatalf("%d microvms on the host after the drain, want none", n)
			}
			if n := h.seen[poolmgrv1.EventType_POOL_REPLENISHING]; n != 1 {
				t.Fatalf("%d POOL_REPLENISHING events, want only the one that filled the pool", n)
			}
		})
	}
}

// TestAQuarantinedMicroVMGoesOnlyWithItsPool covers BA-073: a quarantined
// MicroVM outlives ticks, a drained spec and every Lease expiry, and goes
// when DeletePool deletes its Pool.
func TestAQuarantinedMicroVMGoesOnlyWithItsPool(t *testing.T) {
	//= docs/requirements/10-battery.md#delete-pool
	//= type=test
	//# battery SHALL NOT delete a MicroVM in the phase `QUARANTINED`
	//# other than in a `DeletePool` of its Pool.
	h := newHarness(t, Config{}, hostA)
	host := h.stubs[hostA]
	spec := h.spec("pool", 1, hostA)
	spec.HookFailurePolicy = poolmgrv1.HookFailurePolicy_QUARANTINE
	h.b.SetFaults(Faults{HookFailures: []HookFailure{{Hook: HookCreate, Remaining: 1}}})
	if _, err := h.client.CreatePool(h.ctx, spec); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	failed := h.waitEvent(poolmgrv1.EventType_VM_HOOK_FAILED)
	if _, err := h.client.UpdatePool(h.ctx, drained(spec)); err != nil {
		t.Fatalf("UpdatePool: %v", err)
	}
	h.advance(spec.HeartbeatExpiryThreshold * 2)
	for range 3 {
		h.advance(testInterval)
	}
	if st := h.pool("pool").Status; st.Quarantined != 1 {
		t.Fatalf("status = %+v, want the quarantined microvm kept", st)
	}
	if !host.has(failed.VMUID) {
		t.Fatalf("quarantined microvm %s was deleted from its host", failed.VMUID)
	}
	if err := h.client.DeletePool(h.ctx, h.ref("pool")); err != nil {
		t.Fatalf("DeletePool with a quarantined microvm = %v, want it deleted with the pool", err)
	}
	if deleted := h.waitEvent(poolmgrv1.EventType_VM_DELETED_ON_POOL_DELETE); deleted.VMUID != failed.VMUID {
		t.Fatalf("VM_DELETED_ON_POOL_DELETE names %q, want the quarantined microvm %q", deleted.VMUID, failed.VMUID)
	}
	if host.has(failed.VMUID) {
		t.Fatalf("quarantined microvm %s is still on its host after DeletePool", failed.VMUID)
	}
}

// TestUpdatePoolStopsThePoolsProvisioning covers BA-074: a MicroVM being
// provisioned when UpdatePool comes goes through the hook failure policy of
// the spec it was provisioned under, not the new one, and the Pool
// provisions under the new spec afterwards.
func TestUpdatePoolStopsThePoolsProvisioning(t *testing.T) {
	//= docs/requirements/10-battery.md#delete-pool
	//= type=test
	//# When `UpdatePool` succeeds, battery SHALL stop the Pool's
	//# reconciler, SHALL apply the hook failure policy of the Pool's previous
	//# spec to each MicroVM whose provisioning that stops, and SHALL start a
	//# reconciler with the new spec.
	for _, tc := range []struct {
		old, updated    poolmgrv1.HookFailurePolicy
		wantQuarantined bool
	}{
		{old: poolmgrv1.HookFailurePolicy_QUARANTINE, updated: poolmgrv1.HookFailurePolicy_DELETE_AND_REPLACE, wantQuarantined: true},
		{old: poolmgrv1.HookFailurePolicy_DELETE_AND_REPLACE, updated: poolmgrv1.HookFailurePolicy_QUARANTINE},
	} {
		t.Run("from "+tc.old.String(), func(t *testing.T) {
			h := newHarness(t, Config{}, hostA)
			host := h.stubs[hostA]
			gate := make(chan struct{})
			host.set(func(s *testHost) { s.createGate = gate })
			spec := h.spec("pool", 1, hostA)
			spec.HookFailurePolicy = tc.old
			if _, err := h.client.CreatePool(h.ctx, spec); err != nil {
				t.Fatalf("CreatePool: %v", err)
			}
			<-host.createEntered
			spec.HookFailurePolicy = tc.updated
			if _, err := h.client.UpdatePool(h.ctx, spec); err != nil {
				t.Fatalf("UpdatePool: %v", err)
			}
			host.set(func(s *testHost) { s.createGate = nil })
			close(gate)

			failed := h.waitEvent(poolmgrv1.EventType_VM_HOOK_FAILED)
			if p := payloadOf(t, failed); p["policy"] != tc.old.String() {
				t.Fatalf("VM_HOOK_FAILED payload = %v, want the old policy %s", p, tc.old)
			}
			if !tc.wantQuarantined {
				h.waitDeletion(host, failed.VMUID)
			}
			// The new reconciler fills the Pool under the new spec.
			h.advance(testInterval)
			h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
			st := h.pool("pool").Status
			switch {
			case tc.wantQuarantined && st.Quarantined != 1:
				t.Fatalf("status = %+v, want the cancelled microvm quarantined", st)
			case !tc.wantQuarantined && (st.Quarantined != 0 || host.has(failed.VMUID)):
				t.Fatalf("status = %+v, want the cancelled microvm deleted", st)
			}
			if st.Available != 1 {
				t.Fatalf("status = %+v, want a microvm available under the new spec", st)
			}
		})
	}
}
