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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/controller/claimscope"
)

// methodClaimVM names ClaimVM in a claimscope.BatteryCall.
const methodClaimVM = "ClaimVM"

// failedInTransit reports whether a call to battery ended without an
// answer saying what battery did (the glossary's "fails in transit"): the
// Client reports a lost connection, battery's own UNAVAILABLE and a call
// deadline that passed as ErrUnavailable (DP-011), and a deadline or
// cancellation of the caller's own context as the context's error.
func failedInTransit(err error) bool {
	return errors.Is(err, battery.ErrUnavailable) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled)
}

// answered reports whether battery answered a call with something about
// the Lease or the Pool: success, NOT_FOUND or RESOURCE_EXHAUSTED.
func answered(err error) bool {
	return err == nil || errors.Is(err, battery.ErrNotFound) || errors.Is(err, battery.ErrExhausted)
}

// Synced sets the claim's Synced condition from the scope's BatteryCall,
// the last call to battery the chain made for the claim. It runs after the
// chain whatever the chain did, and it never changes the phase: a failure
// says nothing about what battery did, so only an answer from battery moves
// a claim's phase. A reconcile that made no call to battery leaves the
// condition as it was.
type Synced struct{}

var _ claimscope.Subreconciler = Synced{}

// Reconcile implements claimscope.Subreconciler.
func (Synced) Reconcile(_ context.Context, s *claimscope.Scope) (claimscope.Result, error) {
	call := s.BatteryCall
	if call == nil {
		return claimscope.Result{}, nil
	}

	cond := metav1.Condition{
		Type:               batteryv1alpha1.ConditionSynced,
		ObservedGeneration: s.Claim.Generation,
		LastTransitionTime: metav1.NewTime(s.Clock.Now()),
	}
	switch {
	//= docs/requirements/02-claims.md#failed-calls
	//# When battery answers a call for a claim, the Claim Controller
	//# SHALL set the claim's condition `Synced` true.
	case answered(call.Err):
		cond.Status = metav1.ConditionTrue
		cond.Reason = batteryv1alpha1.ReasonSynced
		cond.Message = "battery answered " + call.Method
	//= docs/requirements/02-claims.md#failed-calls
	//# If a call to battery for a claim fails in transit, then the
	//# Claim Controller SHALL set the claim's condition `Synced` false with the
	//# reason `BatteryUnavailable`, and SHALL NOT change the claim's phase
	//# because of that failure.
	case failedInTransit(call.Err):
		cond.Status = metav1.ConditionFalse
		cond.Reason = batteryv1alpha1.ReasonBatteryUnavailable
		cond.Message = call.Method + " got no answer from battery: " + call.Err.Error()
	//= docs/requirements/02-claims.md#failed-calls
	//# If a call to battery for a claim fails with an error that no
	//# other requirement in this document names, then the Claim Controller SHALL
	//# set the claim's condition `Synced` false with the reason `BatteryError`,
	//# SHALL NOT change the claim's phase because of that failure, and SHALL
	//# retry the call with backoff.
	default:
		cond.Status = metav1.ConditionFalse
		cond.Reason = batteryv1alpha1.ReasonBatteryError
		cond.Message = call.Method + " failed: " + call.Err.Error()
	}
	meta.SetStatusCondition(&s.Claim.Status.Conditions, cond)
	return claimscope.Result{}, nil
}
