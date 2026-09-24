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
	"fmt"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
)

// syncedAfter runs Synced on a claim in phase, after a ClaimVM that ended
// with err, and returns the claim's Synced condition and phase.
func syncedAfter(t *testing.T, phase batteryv1alpha1.MicroVMClaimPhase, err error) (*metav1.Condition, batteryv1alpha1.MicroVMClaimPhase) {
	t.Helper()
	cl := aClaim(batteryv1alpha1.ReleaseFinalizer)
	cl.Status.Phase = phase
	s, _ := newScope(t, cl, answers(nil, nil))
	s.Called(methodClaimVM, err)
	if _, rerr := (Synced{}).Reconcile(context.Background(), s); rerr != nil {
		t.Fatal(rerr)
	}
	return meta.FindStatusCondition(s.Claim.Status.Conditions, batteryv1alpha1.ConditionSynced), s.Claim.Status.Phase
}

//= docs/requirements/02-claims.md#failed-calls
//= type=test
//# If a call to battery for a claim fails in transit, then the
//# Claim Controller SHALL set the claim's condition `Synced` false with the
//# reason `BatteryUnavailable`, and SHALL NOT change the claim's phase
//# because of that failure.

// TestSyncedFalseWhenACallFailsInTransit covers CL-040.
func TestSyncedFalseWhenACallFailsInTransit(t *testing.T) {
	for _, err := range []error{battery.ErrUnavailable, context.DeadlineExceeded, context.Canceled} {
		for _, phase := range []batteryv1alpha1.MicroVMClaimPhase{"", batteryv1alpha1.MicroVMClaimPending, batteryv1alpha1.MicroVMClaimBound} {
			cond, got := syncedAfter(t, phase, fmt.Errorf("ClaimVM: %w", err))
			if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != batteryv1alpha1.ReasonBatteryUnavailable {
				t.Errorf("%v in phase %q: Synced = %+v, want false with BatteryUnavailable", err, phase, cond)
			}
			if got != phase {
				t.Errorf("%v: phase = %q, want it left %q", err, got, phase)
			}
		}
	}
}

//= docs/requirements/02-claims.md#failed-calls
//= type=test
//# If a call to battery for a claim fails with an error that no
//# other requirement in this document names, then the Claim Controller SHALL
//# set the claim's condition `Synced` false with the reason `BatteryError`,
//# SHALL NOT change the claim's phase because of that failure, and SHALL
//# retry the call with backoff.

// TestSyncedFalseOnAnUnnamedError covers CL-041's condition and phase; its
// retry is TestBindReturnsOtherFailures.
func TestSyncedFalseOnAnUnnamedError(t *testing.T) {
	for _, err := range []error{battery.ErrInvalid, battery.ErrFailedPrecondition, fmt.Errorf("battery: Internal: oops")} {
		cond, got := syncedAfter(t, batteryv1alpha1.MicroVMClaimPending, err)
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != batteryv1alpha1.ReasonBatteryError {
			t.Errorf("%v: Synced = %+v, want false with BatteryError", err, cond)
		}
		if got != batteryv1alpha1.MicroVMClaimPending {
			t.Errorf("%v: phase = %q, want it left Pending", err, got)
		}
	}
}

//= docs/requirements/02-claims.md#failed-calls
//= type=test
//# When battery answers a call for a claim, the Claim Controller
//# SHALL set the claim's condition `Synced` true.

// TestSyncedTrueWhenBatteryAnswers covers CL-042: success, and the answers
// NOT_FOUND and RESOURCE_EXHAUSTED, all set Synced true, including after an
// earlier failure.
func TestSyncedTrueWhenBatteryAnswers(t *testing.T) {
	for _, err := range []error{nil, battery.ErrNotFound, battery.ErrExhausted} {
		cond, _ := syncedAfter(t, batteryv1alpha1.MicroVMClaimPending, err)
		if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != batteryv1alpha1.ReasonSynced {
			t.Errorf("%v: Synced = %+v, want true", err, cond)
		}
	}

	cl := aClaim(batteryv1alpha1.ReleaseFinalizer)
	cl.Status.Conditions = []metav1.Condition{{
		Type: batteryv1alpha1.ConditionSynced, Status: metav1.ConditionFalse,
		Reason: batteryv1alpha1.ReasonBatteryUnavailable, LastTransitionTime: metav1.NewTime(start.Add(-time.Minute)),
	}}
	s, _ := newScope(t, cl, answers(nil, nil))
	s.Called(methodClaimVM, nil)
	if _, err := (Synced{}).Reconcile(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if !meta.IsStatusConditionTrue(s.Claim.Status.Conditions, batteryv1alpha1.ConditionSynced) {
		t.Error("Synced stayed false after battery answered")
	}
}

// TestSyncedUnchangedWithoutACall: a reconcile that made no call to
// battery leaves Synced as it was.
func TestSyncedUnchangedWithoutACall(t *testing.T) {
	s, _ := newScope(t, aClaim(batteryv1alpha1.ReleaseFinalizer), answers(nil, nil))
	if _, err := (Synced{}).Reconcile(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if len(s.Claim.Status.Conditions) != 0 {
		t.Errorf("conditions = %+v, want none", s.Claim.Status.Conditions)
	}
}
