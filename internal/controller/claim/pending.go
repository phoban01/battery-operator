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
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/controller/claimscope"
)

// Backoff is how long a Pending claim waits before the next ClaimVM.
//
// The wait is as long as the claim has already been Pending, between Min
// and Max, so the total wait doubles with every attempt, as an exponential
// backoff does. It is measured from the Bound condition's
// lastTransitionTime, which is in the claim's status, so it needs no
// attempt count in memory and survives a restart of the Operator.
type Backoff struct {
	// Min is the first wait, and the shortest.
	Min time.Duration
	// Max caps every wait.
	Max time.Duration
}

// DefaultBackoff is the Claim Controller's Backoff.
var DefaultBackoff = Backoff{Min: time.Second, Max: 30 * time.Second}

// after is the wait for a claim that has been Pending for pending.
func (b Backoff) after(pending time.Duration) time.Duration {
	return min(max(pending, b.Min), b.Max)
}

// Pending records why battery's ClaimVM could not bind a claim, from the
// scope's BatteryCall: the claim stays Pending, its Bound condition is
// false with the reason, and the chain asks for a retry after Backoff.
type Pending struct {
	Backoff Backoff
}

var _ claimscope.Subreconciler = Pending{}

// Reconcile implements claimscope.Subreconciler.
func (p Pending) Reconcile(_ context.Context, s *claimscope.Scope) (claimscope.Result, error) {
	call := s.BatteryCall
	if call == nil || call.Method != methodClaimVM || call.Err == nil {
		return claimscope.Result{}, nil
	}
	pool := s.Claim.Spec.PoolRef.Name
	var reason, message string
	switch err := call.Err; {
	//= docs/requirements/02-claims.md#binding
	//# If battery's `ClaimVM` fails because the Pool has no warm
	//# MicroVM, then the Claim Controller SHALL keep the claim in the phase
	//# `Pending` with the condition `Bound` false and the reason `PoolExhausted`,
	//# and SHALL retry with backoff.
	case errors.Is(err, battery.ErrExhausted):
		reason = batteryv1alpha1.ReasonPoolExhausted
		message = fmt.Sprintf("Pool %s has no available MicroVM", pool)
	//= docs/requirements/02-claims.md#binding
	//# If the claim's Pool does not exist, then the Claim Controller
	//# SHALL keep the claim in the phase `Pending` with the condition `Bound`
	//# false and the reason `PoolNotFound`, and SHALL retry with backoff.
	case errors.Is(err, battery.ErrNotFound):
		// battery is the authority over Pools (ADR 0001): a Pool
		// resource that battery does not know yet cannot be claimed from
		// either.
		reason = batteryv1alpha1.ReasonPoolNotFound
		message = fmt.Sprintf("battery has no Pool %s", pool)
	//= docs/requirements/02-claims.md#binding
	//# If battery's `ClaimVM` for a claim fails in transit, then the
	//# Claim Controller SHALL keep the claim in the phase `Pending` with the
	//# condition `Bound` false and the reason `BatteryUnavailable`, and SHALL
	//# retry `ClaimVM` with backoff.
	case failedInTransit(err):
		// battery may have leased a MicroVM whose answer was lost; that
		// Lease is an orphan, and the retry claims another (CL-008).
		reason = batteryv1alpha1.ReasonBatteryUnavailable
		message = fmt.Sprintf("ClaimVM on Pool %s got no answer from battery", pool)
	default:
		return claimscope.Result{}, nil
	}

	now := s.Clock.Now()
	st := &s.Claim.Status
	st.Phase = batteryv1alpha1.MicroVMClaimPending
	// The lastTransitionTime is kept while the condition stays false, so
	// it is when the claim started waiting.
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type:               batteryv1alpha1.ConditionBound,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: s.Claim.Generation,
		LastTransitionTime: metav1.NewTime(now),
	})
	since := meta.FindStatusCondition(st.Conditions, batteryv1alpha1.ConditionBound).LastTransitionTime.Time
	after := p.Backoff.after(now.Sub(since))
	s.Log.V(1).Info("Left MicroVMClaim Pending", "reason", reason, "retryAfter", after)
	return claimscope.Result{RequeueAfter: after}, nil
}
