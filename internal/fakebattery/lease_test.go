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

//= docs/requirements/08-test-doubles.md#fake-battery
//= type=test
//# The fake battery SHALL replenish Pools, expire Leases that are
//# not renewed within the Pool's expiry threshold, and answer `ClaimVM` on an
//# empty Pool with `RESOURCE_EXHAUSTED`, as battery v0.1.0 does.

//= docs/requirements/08-test-doubles.md#fake-battery
//= type=test
//# The fake battery SHALL run on an injectable clock, so that a
//# test can expire a Lease without waiting.

// TestLeaseExpiryDeletesTheMicroVM holds a Lease across the expiry threshold
// on the fake clock, so the test never waits. A heartbeat inside the
// threshold keeps the Lease and its MicroVM; once the last heartbeat is
// older than the threshold the fake drops the Lease and deletes the MicroVM
// from the Host it was placed on.
func TestLeaseExpiryDeletesTheMicroVM(t *testing.T) {
	h := newHarness(t, Config{}, hostA)
	spec := h.spec("pool", 1, hostA)
	spec.Replenishment = replenishment{Type: poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE}
	h.createPool(spec)
	host := h.stubs[hostA]

	claim := h.claim("pool")
	if got := h.b.Leases(); len(got) != 1 || !got[0].ExpiresAt.Equal(testEpoch.Add(30*time.Second)) {
		t.Fatalf("leases after the claim = %+v, want one expiring at the threshold", got)
	}

	// Well inside the threshold: a heartbeat moves the expiry out.
	h.advance(25 * time.Second)
	expires, err := h.client.Heartbeat(h.ctx, claim.LeaseID)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if want := testEpoch.Add(55 * time.Second); !expires.Equal(want) {
		t.Fatalf("Heartbeat expiry = %s, want %s", expires, want)
	}
	h.advance(20 * time.Second)
	if got := h.b.Leases(); len(got) != 1 {
		t.Fatalf("leases 45s in, 20s after a heartbeat = %+v, want the lease still held", got)
	}
	if _, deleted := host.counts(); deleted != 0 {
		t.Fatalf("%d microvms deleted while the lease was alive, want 0", deleted)
	}

	// Past the extended expiry with no further heartbeat.
	h.advance(20 * time.Second)
	expired := h.waitEvent(poolmgrv1.EventType_VM_DELETED_DUE_TO_EXPIRY)
	if expired.VMUID != claim.VMUID {
		t.Fatalf("VM_DELETED_DUE_TO_EXPIRY for %q, want the leased microvm %q", expired.VMUID, claim.VMUID)
	}
	if got := payloadOf(t, expired)["lease_id"]; got != claim.LeaseID {
		t.Fatalf("VM_DELETED_DUE_TO_EXPIRY lease_id = %v, want %q", got, claim.LeaseID)
	}
	if got := h.b.Leases(); len(got) != 0 {
		t.Fatalf("leases after expiry = %+v, want none", got)
	}
	if _, err := h.client.Heartbeat(h.ctx, claim.LeaseID); statusCode(t, err) != codes.NotFound {
		t.Fatalf("Heartbeat on the expired lease = %v, want NOT_FOUND", err)
	}
	assertDeleted(t, host, claim.VMUID)
}

//= docs/requirements/08-test-doubles.md#fake-battery
//= type=test
//# The fake battery SHALL replenish Pools, expire Leases that are
//# not renewed within the Pool's expiry threshold, and answer `ClaimVM` on an
//# empty Pool with `RESOURCE_EXHAUSTED`, as battery v0.1.0 does.

// TestClaimOnEmptyPoolIsResourceExhausted asserts the gRPC status code on the
// wire, because a client's exhaustion path keys on the code, and checks the
// two ways a Pool can be empty: never filled, and every MicroVM already
// leased.
func TestClaimOnEmptyPoolIsResourceExhausted(t *testing.T) {
	h := newHarness(t, Config{}, hostA)
	lease := h.rawLease()

	// A Pool with no MicroVM at all.
	empty := h.spec("empty", 0, hostA)
	h.createPool(empty)
	_, err := lease.ClaimVM(h.ctx, &poolmgrv1.ClaimVMRequest{Pool: refToProto(h.ref("empty"))})
	if code := statusCode(t, err); code != codes.ResourceExhausted {
		t.Fatalf("ClaimVM on an empty pool: code %v, want RESOURCE_EXHAUSTED", code)
	}

	// A Pool whose only MicroVM is leased. REPLACE_ON_DELETE does not
	// replenish on a claim, so the Pool stays empty until the release.
	spec := h.spec("one", 1, hostA)
	spec.Replenishment = replenishment{Type: poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE}
	h.createPool(spec)
	claim := h.claim("one")
	if st := h.pool("one").Status; st.Available != 0 || st.Leased != 1 {
		t.Fatalf("status after the claim = %+v, want nothing available", st)
	}
	_, err = lease.ClaimVM(h.ctx, &poolmgrv1.ClaimVMRequest{Pool: refToProto(h.ref("one"))})
	if code := statusCode(t, err); code != codes.ResourceExhausted {
		t.Fatalf("ClaimVM on a fully leased pool: code %v, want RESOURCE_EXHAUSTED", code)
	}

	// The replacement the release starts makes the Pool claimable again, so
	// the code really tracks the AVAILABLE phase and is not a constant.
	h.release(claim.LeaseID)
	h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
	if _, err := lease.ClaimVM(h.ctx, &poolmgrv1.ClaimVMRequest{Pool: refToProto(h.ref("one"))}); err != nil {
		t.Fatalf("ClaimVM after the pool refilled: %v", err)
	}

	// An unknown Pool is NOT_FOUND, not RESOURCE_EXHAUSTED.
	_, err = lease.ClaimVM(h.ctx, &poolmgrv1.ClaimVMRequest{Pool: refToProto(h.ref("nope"))})
	if code := statusCode(t, err); code != codes.NotFound {
		t.Fatalf("ClaimVM on an unknown pool: code %v, want NOT_FOUND", code)
	}
}

// TestHostOnClaimResponse claims through the generated Lease client so that
// the host field is observed on the wire: its name is the Host the MicroVM
// was placed on, and its address the one that Host was added with.
func TestHostOnClaimResponse(t *testing.T) {
	h := newHarness(t, Config{}, hostA)
	h.createPool(h.spec("pool", 1, hostA))

	resp, err := h.rawLease().ClaimVM(h.ctx, &poolmgrv1.ClaimVMRequest{Pool: refToProto(h.ref("pool"))})
	if err != nil {
		t.Fatalf("ClaimVM: %v", err)
	}
	if resp.GetHost().GetName() != hostA {
		t.Fatalf("ClaimVMResponse.host.name = %q, want %q", resp.GetHost().GetName(), hostA)
	}
	if got, want := resp.GetHost().GetAddress(), "host-a:9090"; got != want {
		t.Fatalf("ClaimVMResponse.host.address = %q, want %q", got, want)
	}
	for _, vm := range h.b.VMs() {
		if vm.UID == resp.GetVmUid() && vm.Host != resp.GetHost().GetName() {
			t.Fatalf("microvm %s is on %q but the claim reported %q", vm.UID, vm.Host, resp.GetHost().GetName())
		}
	}
}
