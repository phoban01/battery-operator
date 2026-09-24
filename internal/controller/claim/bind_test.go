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

package claim

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/controller/claimscope"
)

// leased is battery's answer to a successful ClaimVM in these tests.
var leased = &battery.Claim{
	LeaseID: "lease-1",
	VMUID:   "vm-1",
	Host:    battery.HostRef{Name: "node-a", Address: "10.0.0.1:9090"},
}

//= docs/requirements/02-claims.md#binding
//= type=test
//# When battery's `ClaimVM` succeeds for a claim, the Claim
//# Controller SHALL write the lease id, the MicroVM's uid, the Host's name as
//# the node name and the time of binding to the claim's status, set the phase
//# to `Bound` and set the condition `Bound` true, before it makes any other
//# call to battery for that claim.

// TestBindWritesTheLease covers CL-002: a successful ClaimVM on the claim's
// Pool fills the status, and the chain stops so that the status is written
// before anything else, battery included, is called.
func TestBindWritesTheLease(t *testing.T) {
	ctx := context.Background()
	b := answers(leased, nil)
	s, c := newScope(t, aClaim(batteryv1alpha1.ReleaseFinalizer), b)

	// The subreconciler after Bind must not run: it would be another call.
	after := claimscope.Func(func(context.Context, *claimscope.Scope) (claimscope.Result, error) {
		t.Error("the chain went on after Bind")
		return claimscope.Result{}, nil
	})
	if err := (claimscope.Chain{Steps: []claimscope.Subreconciler{Bind{}, after}}).Run(ctx, s); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !s.Result.Stop {
		t.Error("Bind did not stop the chain")
	}
	if calls := b.claimCalls(); len(calls) != 1 || calls[0] != (battery.PoolRef{Name: "small", Namespace: "ci"}) {
		t.Errorf("ClaimVM calls = %v, want one for ci/small", calls)
	}
	if err := s.Patch(ctx); err != nil {
		t.Fatalf("Patch: %v", err)
	}

	got := &batteryv1alpha1.MicroVMClaim{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(s.Claim), got); err != nil {
		t.Fatal(err)
	}
	st := got.Status
	if st.LeaseID != leased.LeaseID {
		t.Errorf("leaseID = %q, want lease-1", st.LeaseID)
	}
	if st.MicroVM == nil || st.MicroVM.UID != "vm-1" {
		t.Errorf("microVM = %+v, want uid vm-1", st.MicroVM)
	}
	if st.Host == nil || st.Host.NodeName != "node-a" {
		t.Errorf("host = %+v, want nodeName node-a", st.Host)
	}
	if st.Host != nil && st.Host.AgentAddress != "" {
		t.Errorf("host.agentAddress = %q, want it left to CL-005", st.Host.AgentAddress)
	}
	if st.BoundTime == nil || !st.BoundTime.Time.Equal(start) {
		t.Errorf("boundTime = %v, want %v", st.BoundTime, start)
	}
	if st.Phase != batteryv1alpha1.MicroVMClaimBound {
		t.Errorf("phase = %q, want Bound", st.Phase)
	}
	cond := meta.FindStatusCondition(st.Conditions, batteryv1alpha1.ConditionBound)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != batteryv1alpha1.ReasonBound {
		t.Errorf("Bound condition = %+v, want true with reason Bound", cond)
	}
	if cond != nil && cond.ObservedGeneration != got.Generation {
		t.Errorf("Bound observedGeneration = %d, want %d", cond.ObservedGeneration, got.Generation)
	}
}

// TestBindSkipsABoundClaim: a claim with a lease id is never claimed again.
func TestBindSkipsABoundClaim(t *testing.T) {
	cl := aClaim(batteryv1alpha1.ReleaseFinalizer)
	cl.Status.LeaseID = "lease-0"
	b := answers(leased, nil)
	s, _ := newScope(t, cl, b)

	res, err := Bind{}.Reconcile(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if res != (claimscope.Result{}) {
		t.Errorf("result = %+v, want the chain to continue", res)
	}
	if n := len(b.claimCalls()); n != 0 {
		t.Errorf("ClaimVM was called %d times for a bound claim", n)
	}
}

// TestBindTrustsTheAPIServerOverTheCache: a cached copy that has not seen
// an earlier bind's status does not make battery lease a second MicroVM.
func TestBindTrustsTheAPIServerOverTheCache(t *testing.T) {
	cl := aClaim(batteryv1alpha1.ReleaseFinalizer)
	cl.Status.LeaseID = "lease-0"
	b := answers(leased, nil)
	s, _ := newScope(t, cl, b)
	// The scope's copy is the stale cache's.
	s.Claim.Status.LeaseID = ""
	s.Original.Status.LeaseID = ""

	res, err := Bind{}.Reconcile(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stop {
		t.Error("Bind did not stop the chain on a stale claim")
	}
	if n := len(b.claimCalls()); n != 0 {
		t.Errorf("ClaimVM was called %d times for a claim the API server shows bound", n)
	}
}

//= docs/requirements/02-claims.md#binding
//= type=test
//# When a claim that has no lease id in its status is
//# reconciled, the Claim Controller SHALL add the finalizer
//# `battery.liquidmetal-x.dev/release` to the claim before it calls battery's
//# `ClaimVM` for the claim's Pool.

// TestBindWaitsForTheFinalizerToBeWritten covers CL-001 from Bind's side:
// a finalizer that is only in the scope's copy is not enough to call
// ClaimVM.
func TestBindWaitsForTheFinalizerToBeWritten(t *testing.T) {
	b := answers(leased, nil)
	s, _ := newScope(t, aClaim(), b)
	s.Claim.Finalizers = []string{batteryv1alpha1.ReleaseFinalizer}

	res, err := Bind{}.Reconcile(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stop {
		t.Error("Bind did not stop the chain")
	}
	if n := len(b.claimCalls()); n != 0 {
		t.Errorf("ClaimVM was called %d times before the finalizer was written", n)
	}
}

// TestBindLeavesPendingFailuresToPending: an exhausted or unknown Pool, or
// a ClaimVM that fails in transit, is recorded in the scope and the chain
// goes on; the status is Pending's to write.
func TestBindLeavesPendingFailuresToPending(t *testing.T) {
	for _, want := range []error{battery.ErrExhausted, battery.ErrNotFound, battery.ErrUnavailable, context.DeadlineExceeded} {
		t.Run(want.Error(), func(t *testing.T) {
			s, _ := newScope(t, aClaim(batteryv1alpha1.ReleaseFinalizer), answers(nil, fmt.Errorf("ClaimVM: %w", want)))

			res, err := Bind{}.Reconcile(context.Background(), s)
			if err != nil {
				t.Fatal(err)
			}
			if res != (claimscope.Result{}) {
				t.Errorf("result = %+v, want the chain to continue", res)
			}
			if call := s.BatteryCall; call == nil || call.Method != methodClaimVM || !errors.Is(call.Err, want) {
				t.Errorf("BatteryCall = %+v, want ClaimVM failing with %v", call, want)
			}
			if s.Claim.Status.LeaseID != "" || s.Claim.Status.Phase != "" {
				t.Errorf("status = %+v, want it untouched", s.Claim.Status)
			}
		})
	}
}

//= docs/requirements/02-claims.md#failed-calls
//= type=test
//# If a call to battery for a claim fails with an error that no
//# other requirement in this document names, then the Claim Controller SHALL
//# set the claim's condition `Synced` false with the reason `BatteryError`,
//# SHALL NOT change the claim's phase because of that failure, and SHALL
//# retry the call with backoff.

// TestBindReturnsOtherFailures covers CL-041 from Bind's side: an error no
// requirement names is returned, so the controller retries it with its
// rate limiter's backoff, and the phase is untouched.
func TestBindReturnsOtherFailures(t *testing.T) {
	cl := aClaim(batteryv1alpha1.ReleaseFinalizer)
	cl.Status.Phase = batteryv1alpha1.MicroVMClaimPending
	s, _ := newScope(t, cl, answers(nil, battery.ErrInvalid))

	_, err := Bind{}.Reconcile(context.Background(), s)
	if !errors.Is(err, battery.ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
	if call := s.BatteryCall; call == nil || !errors.Is(call.Err, battery.ErrInvalid) {
		t.Errorf("BatteryCall = %+v, want ClaimVM failing with ErrInvalid", call)
	}
	if s.Claim.Status.Phase != batteryv1alpha1.MicroVMClaimPending {
		t.Errorf("phase = %q, want it left Pending", s.Claim.Status.Phase)
	}
}

//= docs/requirements/02-claims.md#binding
//= type=test
//# The Claim Controller SHALL write to a claim's status only a
//# lease id that battery returned in its answer to a `ClaimVM` call for that
//# claim.

// TestBindWritesOnlyTheLeaseItsClaimVMReturned covers CL-008: a ClaimVM
// whose answer was lost leaves no lease id, and Bind makes no other call to
// battery to look for the lost Lease (any would panic in the stub); the
// retry writes the lease id of its own answer.
func TestBindWritesOnlyTheLeaseItsClaimVMReturned(t *testing.T) {
	ctx := context.Background()
	answersInTurn := []error{battery.ErrUnavailable, nil}
	second := &battery.Claim{LeaseID: "lease-2", VMUID: "vm-2", Host: battery.HostRef{Name: "node-b"}}
	b := &stubBattery{claimVM: func(battery.PoolRef) (*battery.Claim, error) {
		err := answersInTurn[0]
		answersInTurn = answersInTurn[1:]
		if err != nil {
			return nil, err
		}
		return second, nil
	}}
	c := newFakeClient(t, aClaim(batteryv1alpha1.ReleaseFinalizer))
	key := claimKey

	for range 2 {
		s := scopeFor(t, c, key, b)
		if err := pendingChain().Run(ctx, s); err != nil {
			t.Fatal(err)
		}
		if err := s.Patch(ctx); err != nil {
			t.Fatal(err)
		}
		if s.Claim.Status.LeaseID != "" && s.Claim.Status.LeaseID != second.LeaseID {
			t.Fatalf("leaseID = %q, want only lease-2, from ClaimVM's own answer", s.Claim.Status.LeaseID)
		}
		if len(b.claimCalls()) == 1 && s.Claim.Status.LeaseID != "" {
			t.Fatalf("leaseID = %q after a ClaimVM that failed in transit, want none", s.Claim.Status.LeaseID)
		}
	}
	got := &batteryv1alpha1.MicroVMClaim{}
	if err := c.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.LeaseID != second.LeaseID {
		t.Errorf("leaseID = %q, want lease-2", got.Status.LeaseID)
	}
}
