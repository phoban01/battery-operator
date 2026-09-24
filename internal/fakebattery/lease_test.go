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
	//= docs/requirements/10-battery.md#claiming
	//= type=test
	//# When `ClaimVM` succeeds, battery SHALL choose a new lease id,
	//# and set the Lease's expiry to the time of the claim plus the Pool's
	//# `heartbeat_expiry_threshold`.

	//= docs/requirements/10-battery.md#heartbeat
	//= type=test
	//# If battery does not hold the Lease a `Heartbeat` names, then
	//# battery SHALL answer it with `NOT_FOUND`.

	//= docs/requirements/10-battery.md#expiry
	//= type=test
	//# When a sweep deletes a Lease, battery SHALL delete the Lease's
	//# MicroVM through `flintlockd`

	//= docs/requirements/10-battery.md#events
	//= type=test
	//# battery SHALL record `VM_DELETED_DUE_TO_EXPIRY` or
	//# `VM_DELETED_ON_RELEASE` for a leased MicroVM only after `flintlockd` has
	//# confirmed its deletion.
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
	//= docs/requirements/10-battery.md#claiming
	//= type=test
	//# If a Pool has no MicroVM in the phase `AVAILABLE`, then battery
	//# SHALL answer `ClaimVM` for that Pool with `RESOURCE_EXHAUSTED` and create no
	//# Lease.

	//= docs/requirements/10-battery.md#claiming
	//= type=test
	//# If battery holds no Pool of the name and namespace a `ClaimVM`
	//# names, then battery SHALL answer it with `NOT_FOUND`.
	h := newHarness(t, Config{}, hostA)
	lease := h.rawLease()

	// A Pool with no MicroVM at all.
	empty := h.spec("empty", 0, hostA)
	h.createPool(empty)
	_, err := lease.ClaimVM(h.ctx, &poolmgrv1.ClaimVMRequest{Pool: refToProto(h.ref("empty"))})
	if code := statusCode(t, err); code != codes.ResourceExhausted {
		t.Fatalf("ClaimVM on an empty pool: code %v, want RESOURCE_EXHAUSTED", code)
	}
	if leases := h.b.Leases(); len(leases) != 0 {
		t.Fatalf("leases after ClaimVM on an empty pool = %+v, want none", leases)
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

// TestLateHeartbeatRenewsAnUnsweptLease holds the control loop's tick off
// with an hour-long interval, so that a Lease passes its expiry with no
// sweep. The Lease is still held and a heartbeat renews it, as in battery;
// only the sweep ends it.
func TestLateHeartbeatRenewsAnUnsweptLease(t *testing.T) {
	//= docs/requirements/10-battery.md#heartbeat
	//= type=test
	//# When battery receives a `Heartbeat` for a Lease it still holds,
	//# battery SHALL set the Lease's expiry to the time of the `Heartbeat` plus
	//# the Pool's `heartbeat_expiry_threshold` and answer with that expiry,
	//# whether or not the Lease's previous expiry has passed.

	//= docs/requirements/10-battery.md#expiry
	//= type=test
	//# battery SHALL delete an expired Lease only in a sweep, which
	//# runs once every `sweep_interval` while battery runs, and which deletes
	//# every Lease whose expiry is at or before the time of the sweep.
	const interval = time.Hour
	h := newHarness(t, Config{ReconcileInterval: interval}, hostA)
	spec := h.spec("pool", 1, hostA)
	spec.Replenishment = replenishment{Type: poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE}
	h.createPool(spec)
	claim := h.claim("pool")

	// Ten seconds past the 30s expiry, with no tick since.
	h.advance(40 * time.Second)
	leases := h.b.Leases()
	if len(leases) != 1 || leases[0].ExpiresAt.After(h.clk.Now()) {
		t.Fatalf("leases 10s past the expiry, before a sweep = %+v, want the expired lease still held", leases)
	}
	expires, err := h.client.Heartbeat(h.ctx, claim.LeaseID)
	if err != nil {
		t.Fatalf("Heartbeat on the expired, unswept lease: %v", err)
	}
	if want := testEpoch.Add(70 * time.Second); !expires.Equal(want) {
		t.Fatalf("Heartbeat expiry = %s, want %s", expires, want)
	}

	// Past the renewed expiry, the Lease still waits for the sweep; the
	// tick at the hour ends it.
	h.advance(40 * time.Second)
	if leases := h.b.Leases(); len(leases) != 1 {
		t.Fatalf("leases past the renewed expiry, before a sweep = %+v, want the lease still held", leases)
	}
	h.advance(interval)
	expired := h.waitEvent(poolmgrv1.EventType_VM_DELETED_DUE_TO_EXPIRY)
	if expired.VMUID != claim.VMUID {
		t.Fatalf("VM_DELETED_DUE_TO_EXPIRY for %q, want the leased microvm %q", expired.VMUID, claim.VMUID)
	}
	if _, err := h.client.Heartbeat(h.ctx, claim.LeaseID); statusCode(t, err) != codes.NotFound {
		t.Fatalf("Heartbeat after the sweep = %v, want NOT_FOUND", err)
	}
}

// TestReleaseKeepsTheLeaseUntilTheHostConfirms releases a Lease while the
// Host refuses deletions. The fake answers UNAVAILABLE and keeps the Lease,
// which a heartbeat still renews; once the Host takes the deletion, the
// loop finishes it and the Lease is gone, so a second release is
// NOT_FOUND.
func TestReleaseKeepsTheLeaseUntilTheHostConfirms(t *testing.T) {
	//= docs/requirements/10-battery.md#release
	//= type=test
	//# When battery receives a `ReleaseVM` for a Lease it holds,
	//# battery SHALL delete the Lease's MicroVM through `flintlockd` and answer
	//# with success only once `flintlockd` has confirmed the deletion and the
	//# Lease is deleted.

	//= docs/requirements/10-battery.md#release
	//= type=test
	//# If `flintlockd` does not confirm the deletion of a released
	//# Lease's MicroVM, then battery SHALL answer the `ReleaseVM` with
	//# `UNAVAILABLE`, keep the Lease, and retry the deletion in every later sweep
	//# until `flintlockd` confirms it, deleting the Lease then.

	//= docs/requirements/10-battery.md#release
	//= type=test
	//# If battery does not hold the Lease a `ReleaseVM` names, then
	//# battery SHALL answer it with `NOT_FOUND`.
	h := newHarness(t, Config{}, hostA)
	host := h.stubs[hostA]
	spec := h.spec("pool", 1, hostA)
	spec.Replenishment = replenishment{Type: poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE}
	h.createPool(spec)
	claim := h.claim("pool")

	host.set(func(s *testHost) { s.deleteErr = errors.New("host busy") })
	if err := h.client.ReleaseVM(h.ctx, claim.LeaseID); statusCode(t, err) != codes.Unavailable {
		t.Fatalf("ReleaseVM while the host refuses = %v, want UNAVAILABLE", err)
	}
	if leases := h.b.Leases(); len(leases) != 1 || leases[0].LeaseID != claim.LeaseID {
		t.Fatalf("leases after the refused release = %+v, want the lease kept", leases)
	}
	if _, err := h.client.Heartbeat(h.ctx, claim.LeaseID); err != nil {
		t.Fatalf("Heartbeat on the kept lease: %v", err)
	}

	host.set(func(s *testHost) { s.deleteErr = nil })
	h.advance(testInterval)
	h.waitEvent(poolmgrv1.EventType_VM_DELETED_ON_RELEASE)
	assertDeleted(t, host, claim.VMUID)
	if leases := h.b.Leases(); len(leases) != 0 {
		t.Fatalf("leases after the host took the deletion = %+v, want none", leases)
	}
	if err := h.client.ReleaseVM(h.ctx, claim.LeaseID); statusCode(t, err) != codes.NotFound {
		t.Fatalf("ReleaseVM of the released lease = %v, want NOT_FOUND", err)
	}
}

// TestListLeases reads Leases through the generated Lease client: every
// Pool's or one Pool's, ordered by lease id, without renewing any. As in
// battery, a Lease past its expiry is listed until the control loop sweeps
// it, and a Heartbeat before then renews it. An unknown Pool lists none
// without an error, a released Lease is gone, and RefuseHeartbeats hides
// every Lease as it makes Heartbeat NOT_FOUND.
func TestListLeases(t *testing.T) {
	// No timer tick within the test: the fake fills Pools and handles
	// claims on its kicks, and the Leases are never swept.
	h := newHarness(t, Config{ReconcileInterval: time.Hour}, hostA)
	for _, name := range []string{"one", "two"} {
		spec := h.spec(name, 1, hostA)
		spec.Replenishment = replenishment{Type: poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE}
		h.createPool(spec)
	}
	claims := map[string]*claimed{"one": h.claim("one"), "two": h.claim("two")}

	list := func(pool *PoolRef) []*poolmgrv1.LeaseRecord {
		t.Helper()
		req := &poolmgrv1.ListLeasesRequest{}
		if pool != nil {
			req.PoolRef = refToProto(*pool)
		}
		resp, err := h.rawLease().ListLeases(h.ctx, req)
		if err != nil {
			t.Fatalf("ListLeases(%v): %v", pool, err)
		}
		return resp.GetLeases()
	}

	all := list(nil)
	if len(all) != 2 || all[0].GetLeaseId() >= all[1].GetLeaseId() {
		t.Fatalf("ListLeases of every Pool = %v, want both Leases ordered by lease id", all)
	}
	for name, c := range claims {
		ref := h.ref(name)
		got := list(&ref)
		if len(got) != 1 {
			t.Fatalf("ListLeases(%s) = %v, want one Lease", ref, got)
		}
		l := got[0]
		if l.GetLeaseId() != c.LeaseID || l.GetVmUid() != c.VMUID ||
			l.GetPoolName() != name || l.GetPoolNamespace() != testNamespace ||
			!l.GetClaimedAt().AsTime().Equal(testEpoch) || !l.GetLastHeartbeatAt().AsTime().Equal(testEpoch) ||
			!l.GetExpiresAt().AsTime().Equal(testEpoch.Add(30*time.Second)) {
			t.Fatalf("ListLeases(%s) = %v, want lease %s of %s claimed at %s, expiring 30s later",
				ref, l, c.LeaseID, c.VMUID, testEpoch)
		}
	}
	unknown := h.ref("nope")
	if got := list(&unknown); len(got) != 0 {
		t.Fatalf("ListLeases of an unknown Pool = %v, want none", got)
	}

	// Past the expiry, with no sweep yet: still listed, and unrenewed.
	h.clk.Advance(40 * time.Second)
	one := h.ref("one")
	if got := list(&one); len(got) != 1 || !got[0].GetExpiresAt().AsTime().Equal(testEpoch.Add(30*time.Second)) {
		t.Fatalf("ListLeases past the expiry, before a sweep = %v, want the Lease with its old expiry", got)
	}
	expires, err := h.client.Heartbeat(h.ctx, claims["one"].LeaseID)
	if err != nil {
		t.Fatalf("Heartbeat of an expired Lease before a sweep: %v", err)
	}
	if got := list(&one); len(got) != 1 || !got[0].GetExpiresAt().AsTime().Equal(expires) {
		t.Fatalf("ListLeases after the Heartbeat = %v, want expiry %s", got, expires)
	}

	h.b.SetFaults(Faults{RefuseHeartbeats: true})
	if got := list(nil); len(got) != 0 {
		t.Fatalf("ListLeases with heartbeats refused = %v, want none", got)
	}
	h.b.SetFaults(Faults{})

	h.release(claims["one"].LeaseID)
	if got := list(nil); len(got) != 1 || got[0].GetLeaseId() != claims["two"].LeaseID {
		t.Fatalf("ListLeases after releasing one = %v, want only %s", got, claims["two"].LeaseID)
	}
}
