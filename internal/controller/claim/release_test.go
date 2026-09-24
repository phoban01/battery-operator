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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/controller/claimscope"
)

// testLease is the Lease the deleted claims in these tests are Bound on.
const testLease = "lease-1"

// releaseStub is a battery.Client whose ReleaseVM gives the next of
// answers, the last one again once they run out, and records the lease ids
// it was called with. Every other method panics through the nil embedded
// Client: a deleted claim must not be claimed or renewed.
type releaseStub struct {
	battery.Client
	answers []error
	leases  []string
}

func (b *releaseStub) ReleaseVM(_ context.Context, leaseID string) error {
	b.leases = append(b.leases, leaseID)
	err := b.answers[0]
	if len(b.answers) > 1 {
		b.answers = b.answers[1:]
	}
	return err
}

// releaseChain is the controller's chain, as the claims in these tests
// see it.
func releaseChain() claimscope.Chain {
	return claimscope.Chain{
		Steps: []claimscope.Subreconciler{
			Release{Backoff: testBackoff},
			EnsureFinalizer{},
			Bind{},
			Pending{Backoff: testBackoff},
		},
		Finally: []claimscope.Subreconciler{Synced{}},
	}
}

// aDeletedClaim is aClaim with finalizers, deleted, and Bound on leaseID
// unless leaseID is empty.
func aDeletedClaim(leaseID string, finalizers ...string) *batteryv1alpha1.MicroVMClaim {
	cl := aClaim(finalizers...)
	deleted := metav1.NewTime(start)
	cl.DeletionTimestamp = &deleted
	if leaseID != "" {
		bound := metav1.NewTime(start.Add(-time.Hour))
		cl.Status = batteryv1alpha1.MicroVMClaimStatus{
			Phase:     batteryv1alpha1.MicroVMClaimBound,
			LeaseID:   leaseID,
			BoundTime: &bound,
			Conditions: []metav1.Condition{
				{Type: batteryv1alpha1.ConditionBound, Status: metav1.ConditionTrue, Reason: batteryv1alpha1.ReasonBound, LastTransitionTime: bound},
				{Type: batteryv1alpha1.ConditionSynced, Status: metav1.ConditionTrue, Reason: batteryv1alpha1.ReasonSynced, LastTransitionTime: bound},
			},
		}
	}
	return cl
}

// reconcileDeletedAt runs releaseChain on a fresh scope for the claim in c
// at now and patches it. It returns the chain's result, the claim as
// stored, or nil once it is gone, and the chain's error.
func reconcileDeletedAt(t *testing.T, c client.Client, b battery.Client, now time.Time) (claimscope.Result, *batteryv1alpha1.MicroVMClaim, error) {
	t.Helper()
	ctx := context.Background()
	s := scopeFor(t, c, claimKey, b)
	s.Clock = clock.NewFake(now)
	runErr := releaseChain().Run(ctx, s)
	if err := s.Patch(ctx); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	got := &batteryv1alpha1.MicroVMClaim{}
	if err := c.Get(ctx, claimKey, got); err != nil {
		if apierrors.IsNotFound(err) {
			return s.Result, nil, runErr
		}
		t.Fatal(err)
	}
	return s.Result, got, runErr
}

//= docs/requirements/02-claims.md#release
//= type=test
//# When a claim that has a lease id is deleted, the Claim
//# Controller SHALL call battery's `ReleaseVM` for the Lease, and SHALL remove
//# the finalizer only once battery has released the Lease or reported it
//# unknown.

// TestReleaseReleasesTheLeaseOfADeletedClaim covers CL-020: a deleted
// Bound claim has its Lease released, and then goes away; so does one
// whose Lease battery reports unknown.
func TestReleaseReleasesTheLeaseOfADeletedClaim(t *testing.T) {
	for name, answer := range map[string]error{
		"released": nil,
		"unknown":  fmt.Errorf("ReleaseVM: %w", battery.ErrNotFound),
	} {
		t.Run(name, func(t *testing.T) {
			c := newFakeClient(t, aDeletedClaim(testLease, batteryv1alpha1.ReleaseFinalizer))
			b := &releaseStub{answers: []error{answer}}
			res, got, err := reconcileDeletedAt(t, c, b, start)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(b.leases) != 1 || b.leases[0] != testLease {
				t.Errorf("ReleaseVM calls = %v, want one for lease-1", b.leases)
			}
			if got != nil {
				t.Errorf("claim = %+v, want it gone", got.ObjectMeta)
			}
			if res.RequeueAfter != 0 {
				t.Errorf("RequeueAfter = %v, want none", res.RequeueAfter)
			}
		})
	}
}

// TestReleaseKeepsAnotherControllersFinalizer: a released claim loses only
// the release finalizer, and the chain records battery's answer in Synced.
func TestReleaseKeepsAnotherControllersFinalizer(t *testing.T) {
	c := newFakeClient(t, aDeletedClaim(testLease, batteryv1alpha1.ReleaseFinalizer, "example.com/other"))
	b := &releaseStub{answers: []error{nil}}
	_, got, err := reconcileDeletedAt(t, c, b, start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got == nil {
		t.Fatal("the claim is gone, want it held by example.com/other")
	}
	if controllerutil.ContainsFinalizer(got, batteryv1alpha1.ReleaseFinalizer) || !controllerutil.ContainsFinalizer(got, "example.com/other") {
		t.Errorf("finalizers = %v, want only example.com/other", got.Finalizers)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, batteryv1alpha1.ConditionSynced)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Message != "battery answered ReleaseVM" {
		t.Errorf("Synced = %+v, want true after ReleaseVM", cond)
	}
}

//= docs/requirements/02-claims.md#release
//= type=test
//# When a claim that has no lease id is deleted, the Claim
//# Controller SHALL remove its finalizer without calling battery.

// TestReleaseOfAnUnboundClaimCallsNoBattery covers CL-021: a claim deleted
// while Pending goes away, and battery is not called (any call would panic
// in the stub).
func TestReleaseOfAnUnboundClaimCallsNoBattery(t *testing.T) {
	cl := aDeletedClaim("", batteryv1alpha1.ReleaseFinalizer)
	cl.Status.Phase = batteryv1alpha1.MicroVMClaimPending
	c := newFakeClient(t, cl)
	b := &releaseStub{}
	res, got, err := reconcileDeletedAt(t, c, b, start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(b.leases) != 0 {
		t.Errorf("ReleaseVM calls = %v, want none", b.leases)
	}
	if got != nil {
		t.Errorf("claim = %+v, want it gone", got.ObjectMeta)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want none", res.RequeueAfter)
	}
}

//= docs/requirements/02-claims.md#release
//= type=test
//# If battery's `ReleaseVM` for a deleted claim fails in transit,
//# then the Claim Controller SHALL keep the finalizer and retry `ReleaseVM`
//# with backoff.

//= docs/requirements/02-claims.md#failed-calls
//= type=test
//# If a call to battery for a claim fails in transit, then the
//# Claim Controller SHALL set the claim's condition `Synced` false with the
//# reason `BatteryUnavailable`, and SHALL NOT change the claim's phase
//# because of that failure.

// TestReleaseRetriesWithBackoff covers CL-022 and CL-040 for ReleaseVM:
// while ReleaseVM fails in transit, or battery answers UNAVAILABLE because
// flintlockd has not confirmed the deletion, the claim keeps its finalizer
// and its phase, Synced is false, and each retry waits longer, up to the
// cap. The retry that finds the Lease released or unknown lets the claim
// go.
func TestReleaseRetriesWithBackoff(t *testing.T) {
	for name, last := range map[string]error{
		"released": nil,
		// The answer to an earlier ReleaseVM was lost after battery
		// released the Lease.
		"unknown": battery.ErrNotFound,
	} {
		t.Run(name, func(t *testing.T) {
			c := newFakeClient(t, aDeletedClaim(testLease, batteryv1alpha1.ReleaseFinalizer))
			unavailable := fmt.Errorf("ReleaseVM: %w", battery.ErrUnavailable)
			b := &releaseStub{answers: []error{
				unavailable, context.DeadlineExceeded, unavailable, unavailable, unavailable, unavailable, last,
			}}
			now := start
			for i, want := range []time.Duration{1, 1, 2, 4, 8, 8} {
				res, got, err := reconcileDeletedAt(t, c, b, now)
				if err != nil {
					t.Fatalf("attempt %d: Run: %v", i, err)
				}
				if got == nil {
					t.Fatalf("attempt %d: the claim is gone before battery released its Lease", i)
				}
				if !controllerutil.ContainsFinalizer(got, batteryv1alpha1.ReleaseFinalizer) {
					t.Fatalf("attempt %d: finalizers = %v, want the release finalizer kept", i, got.Finalizers)
				}
				if got.Status.Phase != batteryv1alpha1.MicroVMClaimBound || got.Status.LeaseID != testLease {
					t.Errorf("attempt %d: status = %+v, want it left Bound on lease-1", i, got.Status)
				}
				cond := meta.FindStatusCondition(got.Status.Conditions, batteryv1alpha1.ConditionSynced)
				if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != batteryv1alpha1.ReasonBatteryUnavailable {
					t.Errorf("attempt %d: Synced = %+v, want false with BatteryUnavailable", i, cond)
				}
				if res.RequeueAfter != want*time.Second {
					t.Fatalf("attempt %d at +%v: RequeueAfter = %v, want %v", i, now.Sub(start), res.RequeueAfter, want*time.Second)
				}
				now = now.Add(res.RequeueAfter)
			}
			_, got, err := reconcileDeletedAt(t, c, b, now)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got != nil {
				t.Errorf("claim = %+v, want it gone once battery released the Lease", got.ObjectMeta)
			}
			if len(b.leases) != 7 {
				t.Errorf("ReleaseVM calls = %d, want 7", len(b.leases))
			}
			for _, l := range b.leases {
				if l != testLease {
					t.Errorf("ReleaseVM for %q, want lease-1", l)
				}
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

// TestReleaseReturnsOtherFailures covers CL-041 for ReleaseVM: an error no
// requirement names is returned, so the controller retries it with its
// rate limiter's backoff; the finalizer and phase stay, and Synced says
// why.
func TestReleaseReturnsOtherFailures(t *testing.T) {
	c := newFakeClient(t, aDeletedClaim(testLease, batteryv1alpha1.ReleaseFinalizer))
	b := &releaseStub{answers: []error{battery.ErrInvalid}}
	_, got, err := reconcileDeletedAt(t, c, b, start)
	if !errors.Is(err, battery.ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
	if got == nil || !controllerutil.ContainsFinalizer(got, batteryv1alpha1.ReleaseFinalizer) {
		t.Fatalf("claim = %v, want it kept with the release finalizer", got)
	}
	if got.Status.Phase != batteryv1alpha1.MicroVMClaimBound {
		t.Errorf("phase = %q, want it left Bound", got.Status.Phase)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, batteryv1alpha1.ConditionSynced)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != batteryv1alpha1.ReasonBatteryError {
		t.Errorf("Synced = %+v, want false with BatteryError", cond)
	}
}

// TestReleaseContinuesForALiveClaim: a claim that is not being deleted
// goes on to the rest of the chain, and battery is not called.
func TestReleaseContinuesForALiveClaim(t *testing.T) {
	cl := aClaim(batteryv1alpha1.ReleaseFinalizer)
	cl.Status.LeaseID = testLease
	b := &releaseStub{}
	s, _ := newScope(t, cl, b)
	res, err := Release{Backoff: testBackoff}.Reconcile(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if res != (claimscope.Result{}) || s.BatteryCall != nil {
		t.Errorf("result = %+v, call = %+v; want the chain to continue with no call", res, s.BatteryCall)
	}
}

// TestReleaseStopsWithoutItsFinalizer: a deleted claim that no longer
// carries the release finalizer is left alone.
func TestReleaseStopsWithoutItsFinalizer(t *testing.T) {
	b := &releaseStub{}
	s, _ := newScope(t, aDeletedClaim(testLease, "example.com/other"), b)
	res, err := Release{Backoff: testBackoff}.Reconcile(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stop || s.BatteryCall != nil {
		t.Errorf("result = %+v, call = %+v; want the chain stopped with no call", res, s.BatteryCall)
	}
}
