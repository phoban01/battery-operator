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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/controller/claimscope"
)

// CheckExpiry asks battery about a Bound claim's Lease once the expiry in
// its status has passed, or when it has none, as a newly Bound claim does:
// battery's answer to ClaimVM carries no expiry. It reads the Lease with
// ListLeases, which renews nothing, and
//
//   - keeps the claim Bound with battery's expiry if that is still to come
//     (CL-016): the claim was just bound, or a Heartbeat answer was lost;
//   - expires the claim if battery lists the Lease with a passed expiry
//     (CL-014), or does not list it (CL-012).
//
// While the expiry in the status is still to come, CheckExpiry calls
// nothing and asks for a reconcile when it passes.
//
// A claim with a pending renewal is left to Renew, which runs first: a
// Lease past its expiry is still held until battery's next sweep, and a
// Heartbeat then renews it (BA-010, #85). Expiring the claim meanwhile
// would drop a renewal the Holder asked for in time.
type CheckExpiry struct {
	// Backoff is how a ListLeases that failed is retried.
	Backoff Backoff
}

var _ claimscope.Subreconciler = CheckExpiry{}

// Reconcile implements claimscope.Subreconciler.
func (e CheckExpiry) Reconcile(ctx context.Context, s *claimscope.Scope) (claimscope.Result, error) {
	c := s.Claim
	if !holdsLease(c) || pendingRenewal(c) {
		return claimscope.Result{}, nil
	}
	now := s.Clock.Now()
	if exp := c.Status.LeaseExpiresAt; exp != nil && exp.After(now) {
		return claimscope.Result{RequeueAfter: until(s, exp.Time)}, nil
	}

	//= docs/requirements/02-claims.md#renewal
	//# When a Bound claim has no pending renewal, and its status has
	//# no Lease expiry time or one that has passed, the Claim Controller SHALL
	//# read the claim's Lease with battery's `ListLeases`, and SHALL keep the
	//# claim `Bound` and write the expiry time battery lists if that time has
	//# not passed.
	pool := battery.PoolRef{Name: c.Spec.PoolRef.Name, Namespace: c.Namespace}
	leases, err := s.Battery.ListLeases(ctx, &pool)
	s.Called(methodListLeases, err)
	switch {
	//= docs/requirements/02-claims.md#renewal
	//# If the `ListLeases` call of CL-016 fails in transit, then the
	//# Claim Controller SHALL keep the claim `Bound` and retry the call with
	//# backoff.
	case failedInTransit(err):
		// battery cannot sweep the Lease while it cannot be reached
		// either, so nothing is lost by waiting.
		after := retryAfter(s, e.Backoff)
		s.Log.V(1).Info("Kept MicroVMClaim Bound after ListLeases failed in transit", "retryAfter", after)
		return claimscope.Result{Stop: true, RequeueAfter: after}, nil
	case err != nil:
		// CL-041: Synced records it, and the controller's rate limiter
		// retries it with backoff.
		return claimscope.Result{Stop: true}, fmt.Errorf("listing the Leases of Pool %s: %w", pool, err)
	}

	var rec *battery.LeaseRecord
	for _, l := range leases {
		if l.LeaseID == c.Status.LeaseID {
			rec = l
			break
		}
	}
	switch {
	//= docs/requirements/02-claims.md#renewal
	//# If battery answers a `Heartbeat` for a claim's Lease with
	//# `NOT_FOUND`, or leaves the claim's Lease out of its answer to
	//# `ListLeases`, then the Claim Controller SHALL set the claim's phase to
	//# `Expired` and its condition `Bound` false with the reason `LeaseExpired`.
	case rec == nil:
		// A Lease battery does not list has been swept or released, and
		// will never be held again (BA-040, BA-003).
		expire(s, fmt.Sprintf("battery no longer holds Lease %s", c.Status.LeaseID))
		return claimscope.Result{Stop: true}, nil
	//= docs/requirements/02-claims.md#renewal
	//# When battery lists a Bound claim's Lease in its answer to
	//# `ListLeases` with an expiry time that has passed, and the claim has no
	//# pending renewal, the Claim Controller SHALL set the claim's phase to
	//# `Expired` and its condition `Bound` false with the reason `LeaseExpired`.
	case !rec.ExpiresAt.After(now):
		// Expired means the Lease has run out, not that the sweep has
		// deleted it: battery's own expiry has passed and no renewal is
		// waiting to be relayed (02-claims.md, Renewal).
		expire(s, fmt.Sprintf("Lease %s expired at %s", c.Status.LeaseID, rec.ExpiresAt.UTC().Format(time.RFC3339)))
		return claimscope.Result{Stop: true}, nil
	}

	//= docs/requirements/02-claims.md#renewal
	//# The Claim Controller SHALL take a claim's Lease expiry time only
	//# from battery's answer to `Heartbeat` or `ListLeases`, and SHALL NOT
	//# compute it.
	c.Status.LeaseExpiresAt = &metav1.Time{Time: rec.ExpiresAt}
	s.Log.V(1).Info("Read the Lease expiry of MicroVMClaim from battery", "lease", rec.LeaseID, "expiresAt", rec.ExpiresAt)
	return claimscope.Result{Stop: true, RequeueAfter: until(s, rec.ExpiresAt)}, nil
}
