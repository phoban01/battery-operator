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
	"context"
	"testing"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"google.golang.org/grpc/codes"
)

// TestFaultClaimLatency holds a claim for exactly the injected latency on the
// fake clock: the call is still outstanding until the clock reaches the
// deadline, and it succeeds when it does. A client's claim deadline is
// exercised the same way, by cancelling instead of advancing.
func TestFaultClaimLatency(t *testing.T) {
	h := newHarness(t, Config{}, hostA)
	h.createPool(h.spec("pool", 2, hostA))
	h.b.SetFaults(Faults{ClaimLatency: 5 * time.Second})

	type result struct {
		claim *claimed
		err   error
	}
	done := make(chan result, 1)
	go func() {
		claim, err := h.client.ClaimVM(h.ctx, h.ref("pool"))
		done <- result{claim, err}
	}()

	// The claim arms a second timer on the fake clock, next to the control
	// loop's; until it fires the claim is outstanding.
	h.waitTimers(2)
	select {
	case got := <-done:
		t.Fatalf("ClaimVM returned %+v before the injected latency elapsed", got)
	default:
	}
	h.clk.Advance(5 * time.Second)
	got := <-done
	if got.err != nil {
		t.Fatalf("ClaimVM after the latency: %v", got.err)
	}
	if got.claim.LeaseID == "" {
		t.Fatalf("ClaimVM returned %+v, want a lease", got.claim)
	}
	if leases := h.b.Leases(); len(leases) != 1 || !leases[0].ClaimedAt.Equal(testEpoch.Add(5*time.Second)) {
		t.Fatalf("lease = %+v, want one claimed at the end of the latency", leases)
	}

	// A caller that gives up while the latency runs gets its own context
	// error, and no Lease is left behind.
	h.b.SetFaults(Faults{ClaimLatency: time.Hour})
	ctx, cancel := context.WithCancel(h.ctx)
	errc := make(chan error, 1)
	go func() {
		_, err := h.client.ClaimVM(ctx, h.ref("pool"))
		errc <- err
	}()
	h.waitTimers(2)
	cancel()
	if err := <-errc; statusCode(t, err) != codes.Canceled {
		t.Fatalf("cancelled ClaimVM = %v, want CANCELED", err)
	}
	if leases := h.b.Leases(); len(leases) != 1 {
		t.Fatalf("leases after the cancelled claim = %+v, want only the first one", leases)
	}
}

// TestFaultUnavailablePeriod makes every RPC fail with UNAVAILABLE for a
// period measured on the fake clock, which is how a client meets a battery
// that is briefly away, and checks that the fake serves normally again
// once the period is over.
func TestFaultUnavailablePeriod(t *testing.T) {
	h := newHarness(t, Config{}, hostA)
	h.createPool(h.spec("pool", 1, hostA))
	admin := poolmgrv1.NewPoolAdminClient(h.rawConn())
	ref := refToProto(h.ref("pool"))

	h.b.SetFaults(Faults{UnavailableFor: 30 * time.Second})
	if _, err := admin.GetPool(h.ctx, &poolmgrv1.GetPoolRequest{Ref: ref}); statusCode(t, err) != codes.Unavailable {
		t.Fatalf("GetPool while unavailable: code %v, want UNAVAILABLE", statusCode(t, err))
	}
	if _, err := h.rawLease().ClaimVM(h.ctx, &poolmgrv1.ClaimVMRequest{Pool: ref}); statusCode(t, err) != codes.Unavailable {
		t.Fatalf("ClaimVM while unavailable: code %v, want UNAVAILABLE", statusCode(t, err))
	}
	if _, err := h.client.GetPool(h.ctx, h.ref("pool")); statusCode(t, err) != codes.Unavailable {
		t.Fatalf("GetPool through the client = %v, want ErrUnavailable", err)
	}

	// Halfway through it is still unavailable; at the end it is not.
	h.advance(20 * time.Second)
	if _, err := admin.GetPool(h.ctx, &poolmgrv1.GetPoolRequest{Ref: ref}); statusCode(t, err) != codes.Unavailable {
		t.Fatalf("GetPool 20s into a 30s outage: code %v, want UNAVAILABLE", statusCode(t, err))
	}
	h.advance(10 * time.Second)
	if _, err := admin.GetPool(h.ctx, &poolmgrv1.GetPoolRequest{Ref: ref}); err != nil {
		t.Fatalf("GetPool after the outage: %v", err)
	}
	if _, err := h.client.ClaimVM(h.ctx, h.ref("pool")); err != nil {
		t.Fatalf("ClaimVM after the outage: %v", err)
	}
}

// TestFaultHookFailure fails a create hook and a pre-lease hook, once each,
// and checks that the Pool's hook failure policy is what decides the
// MicroVM's fate: DELETE_AND_REPLACE returns it to its Host, QUARANTINE
// keeps it for inspection. Both emit VM_HOOK_FAILED, and the injection is
// consumed, so the retry that follows succeeds.
func TestFaultHookFailure(t *testing.T) {
	t.Run("create hook, delete and replace", func(t *testing.T) {
		h := newHarness(t, Config{}, hostA)
		host := h.stubs[hostA]
		h.b.SetFaults(Faults{HookFailures: []HookFailure{{Hook: HookCreate, Remaining: 1}}})
		if _, err := h.client.CreatePool(h.ctx, h.spec("pool", 1, hostA)); err != nil {
			t.Fatalf("CreatePool: %v", err)
		}

		failed := h.waitEvent(poolmgrv1.EventType_VM_HOOK_FAILED)
		if p := payloadOf(t, failed); p["hook"] != string(HookCreate) {
			t.Fatalf("VM_HOOK_FAILED payload = %v, want the create hook", p)
		}
		h.waitDeletion(host, failed.VMUID)
		assertDeleted(t, host, failed.VMUID)

		// The injection was consumed, so the tick's replacement comes up.
		h.advance(testInterval)
		available := h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
		if available.VMUID == failed.VMUID {
			t.Fatalf("VM_AVAILABLE for %q, the microvm whose hook failed", available.VMUID)
		}
		if st := h.pool("pool").Status; st.Available != 1 || st.Quarantined != 0 {
			t.Fatalf("status after the replacement = %+v, want one available and none quarantined", st)
		}
	})

	t.Run("create hook, quarantine", func(t *testing.T) {
		h := newHarness(t, Config{}, hostA)
		host := h.stubs[hostA]
		spec := h.spec("pool", 1, hostA)
		spec.HookFailurePolicy = poolmgrv1.HookFailurePolicy_QUARANTINE
		h.b.SetFaults(Faults{HookFailures: []HookFailure{{Hook: HookCreate, Remaining: 1}}})
		if _, err := h.client.CreatePool(h.ctx, spec); err != nil {
			t.Fatalf("CreatePool: %v", err)
		}

		failed := h.waitEvent(poolmgrv1.EventType_VM_HOOK_FAILED)
		if p := payloadOf(t, failed); p["policy"] != poolmgrv1.HookFailurePolicy_QUARANTINE.String() {
			t.Fatalf("VM_HOOK_FAILED payload = %v, want the quarantine policy", p)
		}
		h.advance(testInterval)
		h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
		if st := h.pool("pool").Status; st.Quarantined != 1 || st.Available != 1 {
			t.Fatalf("status = %+v, want the quarantined microvm kept and a replacement available", st)
		}
		kept := host.has(failed.VMUID)
		if !kept {
			t.Fatalf("quarantined microvm %s was deleted from its host", failed.VMUID)
		}
	})

	t.Run("pre-lease hook", func(t *testing.T) {
		h := newHarness(t, Config{}, hostA)
		host := h.stubs[hostA]
		h.createPool(h.spec("pool", 1, hostA))
		h.b.SetFaults(Faults{HookFailures: []HookFailure{{
			Pool: h.ref("pool"), Hook: HookPreLease, Remaining: 1,
		}}})

		_, err := h.rawLease().ClaimVM(h.ctx, &poolmgrv1.ClaimVMRequest{Pool: refToProto(h.ref("pool"))})
		if code := statusCode(t, err); code != codes.Internal {
			t.Fatalf("ClaimVM with a failing pre-lease hook: code %v, want INTERNAL", code)
		}
		failed := h.waitEvent(poolmgrv1.EventType_VM_HOOK_FAILED)
		if p := payloadOf(t, failed); p["hook"] != string(HookPreLease) {
			t.Fatalf("VM_HOOK_FAILED payload = %v, want the pre-lease hook", p)
		}
		if leases := h.b.Leases(); len(leases) != 0 {
			t.Fatalf("leases after the failed claim = %+v, want none", leases)
		}
		h.waitDeletion(host, failed.VMUID)

		// The next claim, on the replacement, works.
		h.advance(testInterval)
		h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
		if _, err := h.client.ClaimVM(h.ctx, h.ref("pool")); err != nil {
			t.Fatalf("ClaimVM after the injection was consumed: %v", err)
		}
	})
}

// TestFaultDropEventsStream drops every open Events stream once. Each
// subscriber sees the stream end with UNAVAILABLE, which is what makes the
// client fall back to polling and re-subscribe, and a new subscription
// made afterwards works and is replayed the events it missed.
func TestFaultDropEventsStream(t *testing.T) {
	h := newHarness(t, Config{}, hostA)
	h.createPool(h.spec("pool", 1, hostA))

	second, err := h.client.Subscribe(h.ctx, nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer second.Close()
	// Drain the replay so that the next Recv is the drop itself.
	for range 3 {
		if _, err := second.Recv(); err != nil {
			t.Fatalf("Recv of the replay: %v", err)
		}
	}

	if open := h.b.events.open(); open != 2 {
		t.Fatalf("%d open Events streams, want the harness's and this test's", open)
	}
	h.b.SetFaults(Faults{DropEventsStream: true})
	for name, stream := range map[string]*eventStream{"harness": h.events, "second": second} {
		_, err := stream.Recv()
		if statusCode(t, err) != codes.Unavailable {
			t.Fatalf("Recv on the %s stream = %v, want ErrUnavailable", name, err)
		}
		// The stream stays ended; it does not silently resume.
		if _, err := stream.Recv(); statusCode(t, err) != codes.Unavailable {
			t.Fatalf("second Recv on the %s stream = %v, want ErrUnavailable again", name, err)
		}
	}
	// The switch clears itself, so the re-subscription survives.
	if got := h.b.Faults(); got.DropEventsStream {
		t.Fatalf("Faults().DropEventsStream = true, want it cleared after the drop")
	}

	claim, err := h.client.ClaimVM(h.ctx, h.ref("pool"))
	if err != nil {
		t.Fatalf("ClaimVM: %v", err)
	}
	resubscribed, err := h.client.Subscribe(h.ctx, nil)
	if err != nil {
		t.Fatalf("Subscribe after the drop: %v", err)
	}
	defer resubscribed.Close()
	h.recvUntilClaim(resubscribed, claim.VMUID)
}

// TestFaultRefuseHeartbeats exercises Faults.RefuseHeartbeats, the switch
// that loses a Lease under its holder without waiting the expiry
// threshold out on a real clock. While it is set every
// Heartbeat is NOT_FOUND, the Lease itself lives on until the threshold
// passes, and the control loop then expires it and deletes the MicroVM;
// clearing the switch puts heartbeats back.
func TestFaultRefuseHeartbeats(t *testing.T) {
	h := newHarness(t, Config{}, hostA)
	spec := h.spec("pool", 1, hostA)
	h.createPool(spec)
	claim := h.claim("pool")

	if _, err := h.client.Heartbeat(h.ctx, claim.LeaseID); err != nil {
		t.Fatalf("Heartbeat before the fault: %v", err)
	}

	h.b.SetFaults(Faults{RefuseHeartbeats: true})
	if _, err := h.client.Heartbeat(h.ctx, claim.LeaseID); statusCode(t, err) != codes.NotFound {
		t.Fatalf("Heartbeat while heartbeats are refused = %v, want ErrNotFound", err)
	}
	if leases := h.b.Leases(); len(leases) != 1 {
		t.Fatalf("leases while heartbeats are refused = %+v, want the one that cannot renew", leases)
	}

	// Clearing the switch restores it, which is how a test hands the Lease
	// back after the failure it wanted.
	h.b.SetFaults(Faults{})
	if _, err := h.client.Heartbeat(h.ctx, claim.LeaseID); err != nil {
		t.Fatalf("Heartbeat after the fault was cleared: %v", err)
	}

	// Refused again and left alone, the Lease reaches its threshold and the
	// loop expires it.
	h.b.SetFaults(Faults{RefuseHeartbeats: true})
	h.advance(spec.HeartbeatExpiryThreshold)
	expired := h.waitEvent(poolmgrv1.EventType_VM_DELETED_DUE_TO_EXPIRY)
	if expired.VMUID != claim.VMUID {
		t.Fatalf("VM_DELETED_DUE_TO_EXPIRY for %q, want the leased microvm %q", expired.VMUID, claim.VMUID)
	}
	if leases := h.b.Leases(); len(leases) != 0 {
		t.Fatalf("leases after the expiry = %+v, want none", leases)
	}
}

// recvUntilClaim reads stream until it carries VM_CLAIMED for uid, which the
// replay a new subscriber is given already holds.
func (h *harness) recvUntilClaim(stream *eventStream, uid string) {
	h.t.Helper()
	for {
		e, err := stream.Recv()
		if err != nil {
			h.t.Fatalf("waiting for the claim of %s on the re-subscribed stream: %v", uid, err)
		}
		if e.Type == poolmgrv1.EventType_VM_CLAIMED && e.VMUID == uid {
			return
		}
	}
}
