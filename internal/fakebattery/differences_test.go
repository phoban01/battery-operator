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
//# The fake battery SHALL document every behaviour in which it
//# deliberately differs from battery v0.1.0.

// TestDocumentedDifferences pins each difference from battery that the
// package documentation lists, so that the list and the behaviour cannot
// drift apart: a change to one of them fails here until the documentation
// says so too.
func TestDocumentedDifferences(t *testing.T) {
	t.Run("the tick tops up every strategy", func(t *testing.T) {
		h := newHarness(t, Config{}, hostA)
		host := h.stubs[hostA]
		// A REPLACE_ON_DELETE Pool whose first create fails has no deletion
		// to replace; battery would leave it short, the fake's tick fills it.
		host.set(func(s *testHost) { s.createErr = errors.New("host busy") })
		spec := h.spec("pool", 1, hostA)
		spec.Replenishment = replenishment{Type: poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE}
		if _, err := h.client.CreatePool(h.ctx, spec); err != nil {
			t.Fatalf("CreatePool: %v", err)
		}
		h.waitEvent(poolmgrv1.EventType_POOL_REPLENISHING)
		// The refused create drops its reservation.
		for len(h.b.VMs()) != 0 {
			if h.ctx.Err() != nil {
				t.Fatalf("the reservation of the refused create was never dropped: %v", h.b.VMs())
			}
			time.Sleep(time.Millisecond)
		}
		host.set(func(s *testHost) { s.createErr = nil })
		h.tick()
		h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
		if st := h.pool("pool").Status; st.Available != 1 {
			t.Fatalf("status after the tick = %+v, want the pool filled", st)
		}
	})

	t.Run("DeletePool deletes idle microvms and refuses while a lease is held", func(t *testing.T) {
		h := newHarness(t, Config{}, hostA)
		host := h.stubs[hostA]
		spec := h.spec("pool", 2, hostA)
		spec.Replenishment = replenishment{Type: poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE}
		h.createPool(spec)

		claim := h.claim("pool")
		if err := h.client.DeletePool(h.ctx, h.ref("pool")); statusCode(t, err) != codes.FailedPrecondition {
			t.Fatalf("DeletePool with a lease held = %v, want FAILED_PRECONDITION", err)
		}
		if err := h.client.ReleaseVM(h.ctx, claim.LeaseID); err != nil {
			t.Fatalf("ReleaseVM: %v", err)
		}
		h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
		if n := host.live(); n != 2 {
			t.Fatalf("%d microvms on the host before DeletePool, want 2", n)
		}
		if err := h.client.DeletePool(h.ctx, h.ref("pool")); err != nil {
			t.Fatalf("DeletePool with only idle microvms: %v", err)
		}
		if n := host.live(); n != 0 {
			t.Fatalf("%d microvms on the host after DeletePool, want none", n)
		}
	})

	t.Run("VM_EXPIRING_SOON fires one heartbeat interval before the expiry", func(t *testing.T) {
		h := newHarness(t, Config{}, hostA)
		spec := h.spec("pool", 1, hostA)
		spec.Replenishment = replenishment{Type: poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE}
		h.createPool(spec)
		claim := h.claim("pool")

		// 11s before the expiry: outside the Pool's 10s heartbeat interval,
		// though inside battery's default 30s warning_window.
		h.advance(19 * time.Second)
		if n := h.warned("pool"); n != 0 {
			t.Fatalf("%d leases warned 11s before the expiry, want none", n)
		}
		h.advance(2 * time.Second)

		// Like battery, the warning comes again for every new expiry.
		for i := range 2 {
			if i > 0 {
				h.advance(21 * time.Second)
			}
			warning := h.waitEvent(poolmgrv1.EventType_VM_EXPIRING_SOON)
			if payloadOf(t, warning)["lease_id"] != claim.LeaseID {
				t.Fatalf("warning %d is for %v, want the lease %s", i, payloadOf(t, warning)["lease_id"], claim.LeaseID)
			}
			if _, err := h.client.Heartbeat(h.ctx, claim.LeaseID); err != nil {
				t.Fatalf("Heartbeat: %v", err)
			}
		}
	})

	t.Run("ReleaseVM emits VM_RELEASED before the deletion", func(t *testing.T) {
		h := newHarness(t, Config{}, hostA)
		h.createPool(h.spec("pool", 1, hostA))
		claim := h.claim("pool")
		if err := h.client.ReleaseVM(h.ctx, claim.LeaseID); err != nil {
			t.Fatalf("ReleaseVM: %v", err)
		}
		got := h.collectUntil(poolmgrv1.EventType_VM_DELETED_ON_RELEASE)
		released := eventOfType(t, got, poolmgrv1.EventType_VM_RELEASED)
		if released.VMUID != claim.VMUID {
			t.Fatalf("VM_RELEASED for %q, want the released microvm %q", released.VMUID, claim.VMUID)
		}
	})

	t.Run("event payloads are JSON objects", func(t *testing.T) {
		h := newHarness(t, Config{}, hostA)
		h.createPool(h.spec("pool", 1, hostA))
		claim := h.claim("pool")
		events := h.b.events
		events.mu.Lock()
		var claimed *poolmgrv1.Event
		for _, e := range events.recent[keyOf(h.ref("pool"))] {
			if e.GetType() == poolmgrv1.EventType_VM_CLAIMED {
				claimed = e
			}
		}
		events.mu.Unlock()
		if claimed == nil {
			t.Fatal("no VM_CLAIMED event was recorded")
		}
		if p := payloadOf(t, &event{Type: claimed.GetType(), Payload: []byte(claimed.GetPayloadJson())}); p["lease_id"] != claim.LeaseID || p["host"] != hostA {
			t.Fatalf("VM_CLAIMED payload = %v, want the lease id and host", p)
		}
	})

	t.Run("a heartbeat expiry threshold that is not positive is refused", func(t *testing.T) {
		h := newHarness(t, Config{}, hostA)
		spec := h.spec("pool", 1, hostA)
		spec.HeartbeatExpiryThreshold = 0
		if _, err := h.client.CreatePool(h.ctx, spec); statusCode(t, err) != codes.InvalidArgument {
			t.Fatalf("CreatePool with no expiry threshold = %v, want INVALID_ARGUMENT", err)
		}
	})
}
