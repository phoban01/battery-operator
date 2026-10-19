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
	"strings"
	"testing"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"google.golang.org/grpc/codes"
)

// The request ids these tests claim with.
const (
	requestA = "claim-a"
	requestB = "claim-b"
)

// TestClaimVMReplaysByRequestID covers BA-007 and BA-008: a ClaimVM with
// a request id stores it on the Lease, which ListLeases reports, and a
// later ClaimVM with the same request id gets the same Lease back, with
// no new Lease, no new MicroVM leased, and the expiry as it was. Another
// request id claims another MicroVM, and a ClaimVM with no request id is
// never replayed.
func TestClaimVMReplaysByRequestID(t *testing.T) {
	//= docs/requirements/10-battery.md#claiming
	//= type=test
	//# When `ClaimVM` carries a request id that no Lease battery
	//# holds was created with, and the call succeeds, battery SHALL store the
	//# request id on the Lease it creates, and SHALL report it in that Lease's
	//# record in `ListLeases`.

	//= docs/requirements/10-battery.md#claiming
	//= type=test
	//# When `ClaimVM` carries a request id, and battery holds a Lease
	//# created with that request id for the Pool the call names, whose MicroVM
	//# has a record and is not in the phase `DELETING`, battery SHALL answer
	//# with that Lease's id and MicroVM, create no Lease, and leave the Lease's
	//# expiry unchanged.
	h := newHarness(t, Config{}, hostA)
	spec := h.spec("pool", 3, hostA)
	spec.Replenishment = replenishment{Type: poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE}
	h.createPool(spec)

	first, err := h.client.ClaimVM(h.ctx, h.ref("pool"), requestA)
	if err != nil {
		t.Fatalf("ClaimVM(%s): %v", requestA, err)
	}
	h.waitEvent(poolmgrv1.EventType_VM_CLAIMED)
	listed, err := h.rawLease().ListLeases(h.ctx, &poolmgrv1.ListLeasesRequest{})
	if err != nil {
		t.Fatalf("ListLeases: %v", err)
	}
	if got := listed.GetLeases(); len(got) != 1 || got[0].GetRequestId() != requestA || got[0].GetLeaseId() != first.LeaseID {
		t.Fatalf("ListLeases = %+v, want the one lease with request id %q", got, requestA)
	}
	held := h.b.Leases()
	if len(held) != 1 || held[0].RequestID != requestA {
		t.Fatalf("leases = %+v, want the one lease with request id %q", held, requestA)
	}
	expiry := held[0].ExpiresAt

	// A replay, after time has passed: the same Lease, the same MicroVM,
	// the expiry untouched, and nothing claimed or emitted.
	h.advance(5 * time.Second)
	again, err := h.client.ClaimVM(h.ctx, h.ref("pool"), requestA)
	if err != nil {
		t.Fatalf("ClaimVM(%s) again: %v", requestA, err)
	}
	if again.LeaseID != first.LeaseID || again.VMUID != first.VMUID || again.Host != first.Host {
		t.Fatalf("replayed claim = %+v, want the first %+v", again, first)
	}
	leases := h.b.Leases()
	if len(leases) != 1 || !leases[0].ExpiresAt.Equal(expiry) {
		t.Fatalf("leases after the replay = %+v, want the one lease expiring at %s", leases, expiry)
	}
	if st := h.pool("pool").Status; st.Leased != 1 {
		t.Fatalf("pool status after the replay = %+v, want one leased", st)
	}
	if n := h.seen[poolmgrv1.EventType_VM_CLAIMED]; n != 1 {
		t.Fatalf("%d VM_CLAIMED events after the replay, want 1", n)
	}

	// Another request id is another claim; none at all is never replayed.
	other, err := h.client.ClaimVM(h.ctx, h.ref("pool"), requestB)
	if err != nil {
		t.Fatalf("ClaimVM(%s): %v", requestB, err)
	}
	if other.LeaseID == first.LeaseID || other.VMUID == first.VMUID {
		t.Fatalf("ClaimVM(%s) = %+v, want another lease and MicroVM than %+v", requestB, other, first)
	}
	anon, err := h.client.ClaimVM(h.ctx, h.ref("pool"), "")
	if err != nil {
		t.Fatalf("ClaimVM with no request id: %v", err)
	}
	if anon.LeaseID == first.LeaseID || anon.LeaseID == other.LeaseID {
		t.Fatalf("ClaimVM with no request id = %+v, want a lease of its own", anon)
	}
	for _, l := range h.b.Leases() {
		if l.LeaseID == anon.LeaseID && l.RequestID != "" {
			t.Fatalf("lease %s has request id %q, want none", l.LeaseID, l.RequestID)
		}
	}
}

// TestClaimVMAbortsWhileTheLeaseIsReleasing covers BA-009: while a
// released Lease's MicroVM is DELETING, because the Host has not confirmed
// the deletion, a ClaimVM with the Lease's request id is ABORTED and
// creates nothing; once the Lease is gone, the same request id claims
// afresh.
func TestClaimVMAbortsWhileTheLeaseIsReleasing(t *testing.T) {
	//= docs/requirements/10-battery.md#claiming
	//= type=test
	//# If `ClaimVM` carries a request id, and battery holds a Lease
	//# created with that request id whose MicroVM is in the phase `DELETING` or
	//# has no record, then battery SHALL answer it with `ABORTED` and create no
	//# Lease.
	h := newHarness(t, Config{}, hostA)
	host := h.stubs[hostA]
	spec := h.spec("pool", 2, hostA)
	spec.Replenishment = replenishment{Type: poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE}
	h.createPool(spec)

	first, err := h.client.ClaimVM(h.ctx, h.ref("pool"), requestA)
	if err != nil {
		t.Fatalf("ClaimVM(%s): %v", requestA, err)
	}
	h.waitEvent(poolmgrv1.EventType_VM_CLAIMED)

	host.set(func(s *testHost) { s.deleteErr = errors.New("host busy") })
	if err := h.client.ReleaseVM(h.ctx, first.LeaseID); statusCode(t, err) != codes.Unavailable {
		t.Fatalf("ReleaseVM while the host refuses = %v, want UNAVAILABLE", err)
	}
	_, err = h.client.ClaimVM(h.ctx, h.ref("pool"), requestA)
	if statusCode(t, err) != codes.Aborted {
		t.Fatalf("ClaimVM(%s) while its lease's MicroVM is deleting = %v, want ABORTED", requestA, err)
	}
	if leases := h.b.Leases(); len(leases) != 1 || leases[0].LeaseID != first.LeaseID {
		t.Fatalf("leases after the aborted claim = %+v, want only the releasing lease", leases)
	}

	host.set(func(s *testHost) { s.deleteErr = nil })
	h.advance(testInterval)
	h.waitEvent(poolmgrv1.EventType_VM_DELETED_ON_RELEASE)
	if leases := h.b.Leases(); len(leases) != 0 {
		t.Fatalf("leases after the deletion = %+v, want none", leases)
	}
	fresh, err := h.client.ClaimVM(h.ctx, h.ref("pool"), requestA)
	if err != nil {
		t.Fatalf("ClaimVM(%s) after the lease ended: %v", requestA, err)
	}
	if fresh.LeaseID == first.LeaseID || fresh.VMUID == first.VMUID {
		t.Fatalf("ClaimVM(%s) after the lease ended = %+v, want a fresh lease and MicroVM", requestA, fresh)
	}
}

// TestClaimVMRefusesABadRequestID: a request id a Lease of another Pool
// was created with, and one longer than battery's limit, are
// INVALID_ARGUMENT, as in battery.
func TestClaimVMRefusesABadRequestID(t *testing.T) {
	h := newHarness(t, Config{}, hostA)
	h.createPool(h.spec("one", 1, hostA))
	h.createPool(h.spec("two", 1, hostA))

	if _, err := h.client.ClaimVM(h.ctx, h.ref("one"), requestA); err != nil {
		t.Fatalf("ClaimVM(one, %s): %v", requestA, err)
	}
	if _, err := h.client.ClaimVM(h.ctx, h.ref("two"), requestA); statusCode(t, err) != codes.InvalidArgument {
		t.Fatalf("ClaimVM(two, %s) with one's request id = %v, want INVALID_ARGUMENT", requestA, err)
	}
	long := strings.Repeat("x", maxRequestIDLength+1)
	if _, err := h.client.ClaimVM(h.ctx, h.ref("two"), long); statusCode(t, err) != codes.InvalidArgument {
		t.Fatalf("ClaimVM with a %d-byte request id = %v, want INVALID_ARGUMENT", len(long), err)
	}
	if st := h.pool("two").Status; st.Leased != 0 {
		t.Fatalf("pool two's status after the refused claims = %+v, want nothing leased", st)
	}
}
