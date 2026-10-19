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
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/controller/claimscope"
	"github.com/phoban01/battery-operator/internal/reconcile"
)

var testBackoff = Backoff{Min: time.Second, Max: 8 * time.Second}

// pendingChain is binding without the finalizer step, which the claims in
// these tests do not need: they already carry the finalizer.
func pendingChain() reconcile.Chain[*claimscope.Scope] {
	return reconcile.Chain[*claimscope.Scope]{
		Steps:   []reconcile.SubReconciler[*claimscope.Scope]{Bind{}, Pending{Backoff: testBackoff}},
		Finally: []reconcile.SubReconciler[*claimscope.Scope]{Synced{}},
	}
}

// reconcileAt runs the chain on a fresh scope for the claim in c at time
// now, patches it, and returns the chain's result and the claim as stored.
func reconcileAt(t *testing.T, c client.Client, b battery.Client, now time.Time) (reconcile.Result, *batteryv1alpha1.MicroVMClaim) {
	t.Helper()
	ctx := context.Background()
	key := claimKey
	s := scopeFor(t, c, key, b)
	s.Clock = clock.NewFake(now)
	res, err := pendingChain().Run(ctx, s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := s.Patch(ctx); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	got := &batteryv1alpha1.MicroVMClaim{}
	if err := c.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	return res, got
}

// assertPending checks that the claim is Pending with Bound false for
// reason.
func assertPending(t *testing.T, got *batteryv1alpha1.MicroVMClaim, reason string) {
	t.Helper()
	if got.Status.Phase != batteryv1alpha1.MicroVMClaimPending {
		t.Errorf("phase = %q, want Pending", got.Status.Phase)
	}
	if got.Status.LeaseID != "" {
		t.Errorf("leaseID = %q, want none", got.Status.LeaseID)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, batteryv1alpha1.ConditionBound)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != reason {
		t.Errorf("Bound condition = %+v, want false with reason %s", cond, reason)
	}
}

//= docs/requirements/02-claims.md#binding
//= type=test
//# If battery's `ClaimVM` fails because the Pool has no warm
//# MicroVM, then the Claim Controller SHALL keep the claim in the phase
//# `Pending` with the condition `Bound` false and the reason `PoolExhausted`,
//# and SHALL retry with backoff.

// TestPendingOnAnExhaustedPoolBacksOffThenBinds covers CL-003: while the
// Pool is exhausted the claim stays Pending and each retry waits longer, up
// to the cap; once a MicroVM is available the claim binds.
func TestPendingOnAnExhaustedPoolBacksOffThenBinds(t *testing.T) {
	c := newFakeClient(t, aClaim(batteryv1alpha1.ReleaseFinalizer))
	exhausted := answers(nil, battery.ErrExhausted)

	// Each retry comes when the last one asked; the waits double to the cap.
	now := start
	for _, want := range []time.Duration{1, 1, 2, 4, 8, 8} {
		res, got := reconcileAt(t, c, exhausted, now)
		assertPending(t, got, batteryv1alpha1.ReasonPoolExhausted)
		if res.RequeueAfter != want*time.Second {
			t.Fatalf("at +%v: RequeueAfter = %v, want %v", now.Sub(start), res.RequeueAfter, want*time.Second)
		}
		now = now.Add(res.RequeueAfter)
	}

	res, got := reconcileAt(t, c, answers(leased, nil), now)
	if got.Status.Phase != batteryv1alpha1.MicroVMClaimBound || got.Status.LeaseID != leased.LeaseID {
		t.Errorf("status = %+v, want Bound on lease-1", got.Status)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v after binding, want none", res.RequeueAfter)
	}
	if !meta.IsStatusConditionTrue(got.Status.Conditions, batteryv1alpha1.ConditionBound) {
		t.Error("Bound is not true after binding")
	}
}

//= docs/requirements/02-claims.md#binding
//= type=test
//# If the claim's Pool does not exist, then the Claim Controller
//# SHALL keep the claim in the phase `Pending` with the condition `Bound`
//# false and the reason `PoolNotFound`, and SHALL retry with backoff.

// TestPendingOnAMissingPoolBacksOff covers CL-004: battery does not know
// the Pool, so the claim stays Pending with PoolNotFound and is retried
// with backoff, and binds once the Pool exists and has a MicroVM.
func TestPendingOnAMissingPoolBacksOff(t *testing.T) {
	c := newFakeClient(t, aClaim(batteryv1alpha1.ReleaseFinalizer))
	missing := answers(nil, battery.ErrNotFound)

	res, got := reconcileAt(t, c, missing, start)
	assertPending(t, got, batteryv1alpha1.ReasonPoolNotFound)
	if res.RequeueAfter != time.Second {
		t.Errorf("RequeueAfter = %v, want 1s", res.RequeueAfter)
	}

	res, got = reconcileAt(t, c, missing, start.Add(3*time.Second))
	assertPending(t, got, batteryv1alpha1.ReasonPoolNotFound)
	if res.RequeueAfter != 3*time.Second {
		t.Errorf("RequeueAfter = %v, want 3s", res.RequeueAfter)
	}

	// The Pool now exists but is exhausted: the reason changes and the
	// wait keeps counting from when the claim went Pending.
	res, got = reconcileAt(t, c, answers(nil, battery.ErrExhausted), start.Add(6*time.Second))
	assertPending(t, got, batteryv1alpha1.ReasonPoolExhausted)
	if res.RequeueAfter != 6*time.Second {
		t.Errorf("RequeueAfter = %v, want 6s", res.RequeueAfter)
	}

	_, got = reconcileAt(t, c, answers(leased, nil), start.Add(12*time.Second))
	if got.Status.Phase != batteryv1alpha1.MicroVMClaimBound {
		t.Errorf("phase = %q, want Bound", got.Status.Phase)
	}
}

//= docs/requirements/02-claims.md#binding
//= type=test
//# If the Pool of a claim that has no lease id in its status has
//# a deletion timestamp, then the Claim Controller SHALL NOT call battery's
//# `ClaimVM` for the claim, SHALL keep the claim in the phase `Pending` with
//# the condition `Bound` false and the reason `PoolDeleting`, and SHALL
//# retry with backoff.

// TestPendingOnADeletingPoolBacksOffWithoutClaimVM covers CL-063: while
// the claim's Pool is being deleted, the claim stays Pending with
// PoolDeleting, is retried with backoff, and battery gets no ClaimVM,
// though it still has a warm MicroVM to give. Synced is left as it was,
// since battery was not called. Once the Pool is gone, the claim is one
// whose Pool does not exist (CL-004).
func TestPendingOnADeletingPoolBacksOffWithoutClaimVM(t *testing.T) {
	ctx := context.Background()
	pool := deletingPool()
	c := newFakeClient(t, aClaim(batteryv1alpha1.ReleaseFinalizer), pool)
	warm := answers(leased, nil)

	now := start
	for _, want := range []time.Duration{1, 1, 2} {
		res, got := reconcileAt(t, c, warm, now)
		assertPending(t, got, batteryv1alpha1.ReasonPoolDeleting)
		if res.RequeueAfter != want*time.Second {
			t.Fatalf("at +%v: RequeueAfter = %v, want %v", now.Sub(start), res.RequeueAfter, want*time.Second)
		}
		if cond := meta.FindStatusCondition(got.Status.Conditions, batteryv1alpha1.ConditionSynced); cond != nil {
			t.Errorf("Synced = %+v, want it unset with no call to battery", cond)
		}
		now = now.Add(res.RequeueAfter)
	}
	if n := len(warm.claimCalls()); n != 0 {
		t.Fatalf("ClaimVM was called %d times for a claim on a deleting Pool", n)
	}

	// battery deletes the Pool, and the Pool Controller removes its
	// finalizer, so the Pool is gone.
	if err := c.Get(ctx, client.ObjectKeyFromObject(pool), pool); err != nil {
		t.Fatal(err)
	}
	pool.Finalizers = nil
	if err := c.Update(ctx, pool); err != nil {
		t.Fatal(err)
	}
	gone := answers(nil, battery.ErrNotFound)
	_, got := reconcileAt(t, c, gone, now)
	assertPending(t, got, batteryv1alpha1.ReasonPoolNotFound)
	if n := len(gone.claimCalls()); n != 1 {
		t.Errorf("ClaimVM was called %d times once the Pool was gone, want 1", n)
	}
}

// TestPendingIgnoresASuccessfulOrAbsentClaimVM: with no pending failure in
// the scope, Pending changes nothing.
func TestPendingIgnoresASuccessfulOrAbsentClaimVM(t *testing.T) {
	s, _ := newScope(t, aClaim(batteryv1alpha1.ReleaseFinalizer), answers(nil, nil))
	res, err := Pending{Backoff: testBackoff}.Reconcile(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if res != (reconcile.Result{}) || s.Object.Status.Phase != "" {
		t.Errorf("result = %+v, status = %+v, want neither changed", res, s.Object.Status)
	}
}

//= docs/requirements/02-claims.md#binding
//= type=test
//# If battery's `ClaimVM` for a claim fails in transit, then the
//# Claim Controller SHALL keep the claim in the phase `Pending` with the
//# condition `Bound` false and the reason `BatteryUnavailable`, and SHALL
//# retry `ClaimVM` with backoff.

// TestPendingWhenClaimVMFailsInTransitBacksOff covers CL-007: a ClaimVM
// that battery did not answer, whether the connection was down or the
// deadline passed, leaves the claim Pending with BatteryUnavailable, and
// ClaimVM is retried with backoff until battery answers.
func TestPendingWhenClaimVMFailsInTransitBacksOff(t *testing.T) {
	c := newFakeClient(t, aClaim(batteryv1alpha1.ReleaseFinalizer))

	res, got := reconcileAt(t, c, answers(nil, battery.ErrUnavailable), start)
	assertPending(t, got, batteryv1alpha1.ReasonBatteryUnavailable)
	if res.RequeueAfter != time.Second {
		t.Errorf("RequeueAfter = %v, want 1s", res.RequeueAfter)
	}

	res, got = reconcileAt(t, c, answers(nil, context.DeadlineExceeded), start.Add(2*time.Second))
	assertPending(t, got, batteryv1alpha1.ReasonBatteryUnavailable)
	if res.RequeueAfter != 2*time.Second {
		t.Errorf("RequeueAfter = %v, want 2s", res.RequeueAfter)
	}

	b := answers(leased, nil)
	_, got = reconcileAt(t, c, b, start.Add(4*time.Second))
	if got.Status.Phase != batteryv1alpha1.MicroVMClaimBound || len(b.claimCalls()) != 1 {
		t.Errorf("phase = %q after %d ClaimVM calls, want Bound after one retry", got.Status.Phase, len(b.claimCalls()))
	}
}

//= docs/requirements/02-claims.md#claim-retry
//= type=test
//# If battery answers a `ClaimVM` for a claim with `ABORTED`,
//# then the Claim Controller SHALL keep the claim in the phase `Pending`
//# with the condition `Bound` false and the reason `ClaimAborted`, and SHALL
//# retry `ClaimVM` with backoff.

// TestPendingWhenClaimVMIsAbortedBacksOff covers CL-062: battery's ABORTED
// leaves the claim Pending with ClaimAborted and Synced true, since
// battery answered, and ClaimVM is retried with backoff until it succeeds.
func TestPendingWhenClaimVMIsAbortedBacksOff(t *testing.T) {
	c := newFakeClient(t, aClaim(batteryv1alpha1.ReleaseFinalizer))

	res, got := reconcileAt(t, c, answers(nil, battery.ErrAborted), start)
	assertPending(t, got, batteryv1alpha1.ReasonClaimAborted)
	if res.RequeueAfter != time.Second {
		t.Errorf("RequeueAfter = %v, want 1s", res.RequeueAfter)
	}
	if !meta.IsStatusConditionTrue(got.Status.Conditions, batteryv1alpha1.ConditionSynced) {
		t.Error("Synced is not true after battery answered ABORTED")
	}

	res, got = reconcileAt(t, c, answers(nil, battery.ErrAborted), start.Add(2*time.Second))
	assertPending(t, got, batteryv1alpha1.ReasonClaimAborted)
	if res.RequeueAfter != 2*time.Second {
		t.Errorf("RequeueAfter = %v, want 2s", res.RequeueAfter)
	}

	b := answers(leased, nil)
	_, got = reconcileAt(t, c, b, start.Add(4*time.Second))
	if got.Status.Phase != batteryv1alpha1.MicroVMClaimBound || len(b.claimCalls()) != 1 {
		t.Errorf("phase = %q after %d ClaimVM calls, want Bound after one retry", got.Status.Phase, len(b.claimCalls()))
	}
}
