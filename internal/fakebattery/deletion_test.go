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

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"google.golang.org/grpc/codes"
)

// drained is spec as the Pool Controller drains it (03-pools.md,
// Deletion): size 0, MIN_SIZE_THRESHOLD with a min_size of 1, no
// pre-lease commands, DELETE_AND_REPLACE.
func drained(spec poolSpec) poolSpec {
	one := int32(1)
	spec.Size = 0
	spec.Replenishment = replenishment{Type: poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD, MinSize: &one}
	spec.PreLeaseCommands = nil
	spec.HookFailurePolicy = poolmgrv1.HookFailurePolicy_DELETE_AND_REPLACE
	return spec
}

// TestDeletePoolRefusesWhileThePoolOwnsAMicroVM covers BA-070 and BA-071:
// DeletePool refuses a Pool with a MicroVM in any phase and deletes
// nothing, and deletes the Pool once its last MicroVM has gone.
func TestDeletePoolRefusesWhileThePoolOwnsAMicroVM(t *testing.T) {
	//= docs/requirements/10-battery.md#delete-pool
	//= type=test
	//# If a Pool owns a MicroVM in any phase, then battery SHALL
	//# answer `DeletePool` for it with `FAILED_PRECONDITION` and keep the Pool
	//# and its MicroVMs.

	//= docs/requirements/10-battery.md#delete-pool
	//= type=test
	//# If battery holds no Pool of the name and namespace a
	//# `DeletePool` names, then battery SHALL answer it with `NOT_FOUND`.
	refused := func(h *harness, what string) {
		t.Helper()
		before := len(h.b.VMs())
		if err := h.client.DeletePool(h.ctx, h.ref("pool")); statusCode(t, err) != codes.FailedPrecondition {
			t.Fatalf("DeletePool with %s = %v, want FAILED_PRECONDITION", what, err)
		}
		h.pool("pool")
		if after := len(h.b.VMs()); after != before {
			t.Fatalf("DeletePool with %s left %d microvms, want %d", what, after, before)
		}
	}

	t.Run("available and leased", func(t *testing.T) {
		h := newHarness(t, Config{}, hostA)
		host := h.stubs[hostA]
		spec := h.spec("pool", 2, hostA)
		spec.Replenishment = replenishment{Type: poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE}
		h.createPool(spec)
		refused(h, "two available microvms")

		claim := h.claim("pool")
		refused(h, "a lease held")
		if n := host.live(); n != 2 {
			t.Fatalf("%d microvms on the host after the refusals, want 2", n)
		}

		// Drain it: nothing replaces what the drain takes.
		if _, err := h.client.UpdatePool(h.ctx, drained(spec)); err != nil {
			t.Fatalf("UpdatePool: %v", err)
		}
		h.release(claim.LeaseID)
		h.release(h.claim("pool").LeaseID)
		if err := h.client.DeletePool(h.ctx, h.ref("pool")); err != nil {
			t.Fatalf("DeletePool of an empty pool: %v", err)
		}
		if err := h.client.DeletePool(h.ctx, h.ref("pool")); statusCode(t, err) != codes.NotFound {
			t.Fatalf("DeletePool of a deleted pool = %v, want NOT_FOUND", err)
		}
	})

	t.Run("provisioning", func(t *testing.T) {
		h := newHarness(t, Config{}, hostA)
		host := h.stubs[hostA]
		gate := make(chan struct{})
		host.set(func(s *testHost) { s.createGate = gate })
		if _, err := h.client.CreatePool(h.ctx, h.spec("pool", 1, hostA)); err != nil {
			t.Fatalf("CreatePool: %v", err)
		}
		<-host.createEntered
		refused(h, "a microvm being created")
		close(gate)
		h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
	})

	t.Run("deleting", func(t *testing.T) {
		h := newHarness(t, Config{}, hostA)
		host := h.stubs[hostA]
		spec := h.spec("pool", 1, hostA)
		h.createPool(spec)
		if _, err := h.client.UpdatePool(h.ctx, drained(spec)); err != nil {
			t.Fatalf("UpdatePool: %v", err)
		}
		claim := h.claim("pool")
		gate := make(chan struct{})
		host.set(func(s *testHost) { s.deleteGate = gate })
		released := make(chan error, 1)
		go func() { released <- h.client.ReleaseVM(h.ctx, claim.LeaseID) }()
		h.waitDeleteCall(host)
		if st := h.pool("pool").Status; st != (poolStatus{}) {
			t.Fatalf("status while the deletion is in flight = %+v, want every count 0", st)
		}
		refused(h, "a microvm being deleted")
		close(gate)
		if err := <-released; err != nil {
			t.Fatalf("ReleaseVM: %v", err)
		}
	})
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

// TestAQuarantinedMicroVMIsNeverDeleted covers BA-073: a quarantined
// MicroVM outlives ticks, a drain and every Lease expiry, and keeps its
// Pool from being deleted.
func TestAQuarantinedMicroVMIsNeverDeleted(t *testing.T) {
	//= docs/requirements/10-battery.md#delete-pool
	//= type=test
	//# battery SHALL NOT delete a MicroVM in the phase `QUARANTINED`.
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
	if err := h.client.DeletePool(h.ctx, h.ref("pool")); statusCode(t, err) != codes.FailedPrecondition {
		t.Fatalf("DeletePool with a quarantined microvm = %v, want FAILED_PRECONDITION", err)
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
