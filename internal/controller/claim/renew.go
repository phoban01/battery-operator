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

// holdsLease reports whether c is a Bound claim with a Lease that is not
// being deleted: the claims that renewal and expiry are about.
func holdsLease(c *batteryv1alpha1.MicroVMClaim) bool {
	return c.Status.Phase == batteryv1alpha1.MicroVMClaimBound &&
		c.Status.LeaseID != "" &&
		c.DeletionTimestamp.IsZero()
}

// pendingRenewal reports whether c has a pending renewal (glossary): its
// spec.renewTime differs from the one the controller last relayed, which
// the status records as observedRenewTime.
func pendingRenewal(c *batteryv1alpha1.MicroVMClaim) bool {
	return !c.Spec.RenewTime.Equal(c.Status.ObservedRenewTime)
}

// expire sets a claim's phase to Expired and its Bound condition false
// with the reason LeaseExpired. The rest of the status is kept: the lease
// id stays, so that the release of a deleted claim can still name it, and
// observedRenewTime stays as it was.
func expire(s *claimscope.Scope, why string) {
	st := &s.Claim.Status
	st.Phase = batteryv1alpha1.MicroVMClaimExpired
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type:               batteryv1alpha1.ConditionBound,
		Status:             metav1.ConditionFalse,
		Reason:             batteryv1alpha1.ReasonLeaseExpired,
		Message:            why,
		ObservedGeneration: s.Claim.Generation,
		LastTransitionTime: metav1.NewTime(s.Clock.Now()),
	})
	s.Log.Info("Expired MicroVMClaim", "lease", st.LeaseID, "why", why)
}

// retryAfter is how long a Bound claim waits before a call to battery that
// failed is made again. As with Pending's Backoff, the wait is as long as
// the claim has been out of sync with battery, between Min and Max, so it
// doubles with every attempt. It is measured from the Synced condition's
// lastTransitionTime, which is in the status and survives a restart.
func retryAfter(s *claimscope.Scope, b Backoff) time.Duration {
	now := s.Clock.Now()
	since := now
	if c := meta.FindStatusCondition(s.Claim.Status.Conditions, batteryv1alpha1.ConditionSynced); c != nil &&
		c.Status == metav1.ConditionFalse {
		since = c.LastTransitionTime.Time
	}
	return b.after(now.Sub(since))
}

// Renew relays a Bound claim's pending renewal to battery as a Heartbeat,
// and records battery's answer.
//
// Renewal is relayed (ADR 0001, consequence 3): the Holder patches
// spec.renewTime, and the watch on the claim reconciles it at once, so the
// Heartbeat follows the patch by the time the work queue takes, and Renew
// runs before any expiry check. The Holder renews every heartbeat interval
// of the Pool (spec.lease.heartbeatInterval) and battery expires the Lease
// its expiry threshold (spec.lease.expiryThreshold) after the last
// Heartbeat, so the controller's reaction time, from the Holder's patch to
// battery's answer, has to fit in the threshold less the interval: 20
// seconds for an interval of 10 and a threshold of 30. A Lease past its
// expiry but
// not yet swept is still renewed (BA-010), which adds up to one
// sweep_interval. The controller runs more than one reconcile at a time,
// and a ClaimVM never holds them all (CL-018), so a slow ClaimVM of
// another claim does not eat into it.
//
// A Heartbeat that battery answers moves the claim's expiry and records
// the renewTime it relayed, in one status write. Until that write lands,
// the renewal is still pending, so an answer lost in transit or in a crash
// only makes Renew call Heartbeat again, which moves the expiry again: no
// expiry check runs meanwhile (CL-014, CL-016; #57).
type Renew struct {
	// Backoff is how a Heartbeat that failed is retried.
	Backoff Backoff
}

var _ claimscope.Subreconciler = Renew{}

// Reconcile implements claimscope.Subreconciler.
func (r Renew) Reconcile(ctx context.Context, s *claimscope.Scope) (claimscope.Result, error) {
	c := s.Claim
	if !holdsLease(c) || !pendingRenewal(c) {
		return claimscope.Result{}, nil
	}
	// The renewTime relayed is the one this reconcile read: a later patch
	// by the Holder leaves the claim with a pending renewal again.
	relayed := c.Spec.RenewTime.DeepCopy()

	//= docs/requirements/02-claims.md#renewal
	//# While the `spec.renewTime` of a Bound claim differs from the
	//# last `renewTime` the Claim Controller relayed for it, the Claim Controller
	//# SHALL call battery's `Heartbeat` for the claim's Lease, and SHALL write
	//# the expiry time battery returns and the `renewTime` it relayed to the
	//# claim's status in one write.
	expiresAt, err := s.Battery.Heartbeat(ctx, c.Status.LeaseID)
	s.Called(methodHeartbeat, err)
	switch {
	//= docs/requirements/02-claims.md#renewal
	//# If battery answers a `Heartbeat` for a claim's Lease with
	//# `NOT_FOUND`, or leaves the claim's Lease out of its answer to
	//# `ListLeases`, then the Claim Controller SHALL set the claim's phase to
	//# `Expired` and its condition `Bound` false with the reason `LeaseExpired`.
	case errors.Is(err, battery.ErrNotFound):
		// battery renews any Lease it still holds, expired or not
		// (BA-010), so NOT_FOUND means the sweep or a release has ended
		// it, and it will never be held again.
		expire(s, fmt.Sprintf("battery no longer holds Lease %s", c.Status.LeaseID))
		return claimscope.Result{Stop: true}, nil
	//= docs/requirements/02-claims.md#renewal
	//# If battery's `Heartbeat` for a Bound claim fails in transit,
	//# then the Claim Controller SHALL keep the claim `Bound` with the Lease
	//# expiry time already in its status, and SHALL retry the `Heartbeat` with
	//# backoff while the claim is Bound.
	case failedInTransit(err):
		// battery may have renewed the Lease. The status keeps its
		// expiry and observedRenewTime, so the renewal stays pending and
		// no expiry check runs until a Heartbeat is answered.
		after := retryAfter(s, r.Backoff)
		s.Log.V(1).Info("Kept MicroVMClaim Bound after Heartbeat failed in transit", "retryAfter", after)
		return claimscope.Result{Stop: true, RequeueAfter: after}, nil
	case err != nil:
		// CL-041: Synced records it, and the controller's rate limiter
		// retries it with backoff.
		return claimscope.Result{Stop: true}, fmt.Errorf("renewing Lease %s: %w", c.Status.LeaseID, err)
	}

	//= docs/requirements/02-claims.md#renewal
	//# The Claim Controller SHALL take a claim's Lease expiry time only
	//# from battery's answer to `Heartbeat` or `ListLeases`, and SHALL NOT
	//# compute it.
	c.Status.LeaseExpiresAt = &metav1.Time{Time: expiresAt}
	c.Status.ObservedRenewTime = relayed
	s.Log.V(1).Info("Renewed MicroVMClaim", "lease", c.Status.LeaseID, "expiresAt", expiresAt)
	return claimscope.Result{Stop: true, RequeueAfter: until(s, expiresAt)}, nil
}

// until is how long from now until t, at least a second, for a requeue
// that checks an expiry once it has passed.
func until(s *claimscope.Scope, t time.Time) time.Duration {
	return max(t.Sub(s.Clock.Now()), time.Second)
}
