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
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/controller/claimscope"
)

// HoldsLease reports whether c is a Bound claim that records a Lease and
// is not being deleted: the claims recovery reconciles (CL-030).
func HoldsLease(c *batteryv1alpha1.MicroVMClaim) bool { return holdsLease(c) }

// RecoveredLeases is what the Claim Controller's recovery learnt from
// battery (CL-030): for each Lease a Bound claim recorded when recovery
// began, battery's record of it, or that battery no longer holds it. The
// Operator's recovery fills it with Record, and Recover reads it. It is
// safe for concurrent use.
//
// Only Leases a claim recorded before the ListLeases are kept. Such a
// Lease was committed by battery before that call, since battery answered
// its ClaimVM before the claim's status was written (CL-002), so a Lease
// the call leaves out has ended and battery will never hold it again
// (BA-040, BA-003). A Lease bound after the call began may be missing from
// it and says nothing, which is why the list of claims comes first.
type RecoveredLeases struct {
	mu sync.Mutex
	// leases maps a lease id to battery's record, or to nil when battery
	// did not list it.
	leases map[string]*battery.LeaseRecord
}

// Record replaces what the last recovery learnt. recorded is the lease ids
// the Bound claims recorded before battery was asked, and listed is
// battery's answer to an unfiltered ListLeases.
func (r *RecoveredLeases) Record(recorded []string, listed []*battery.LeaseRecord) {
	byID := make(map[string]*battery.LeaseRecord, len(listed))
	for _, l := range listed {
		byID[l.LeaseID] = l
	}
	leases := make(map[string]*battery.LeaseRecord, len(recorded))
	//= docs/requirements/02-claims.md#recovery
	//# The Claim Controller SHALL NOT release a Lease that battery
	//# holds and no claim records.
	for _, id := range recorded {
		// A listed Lease that no claim records, such as the orphan of a
		// ClaimVM answer lost in a crash, is never looked at: nothing here
		// or in Recover releases a Lease, and battery expires the orphan
		// (02-claims.md, Binding).
		leases[id] = byID[id]
	}
	r.mu.Lock()
	r.leases = leases
	r.mu.Unlock()
}

// lookup returns what the last recovery learnt about the Lease id: known
// is false when it learnt nothing, and rec is nil when battery did not
// list the Lease. A record battery listed is handed out once, so that a
// later reconcile of the claim does not mistake it for a fresh answer; that
// battery no longer holds a Lease stays true, and is kept.
func (r *RecoveredLeases) lookup(id string) (rec *battery.LeaseRecord, known bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, known = r.leases[id]
	if rec != nil {
		delete(r.leases, id)
	}
	return rec, known
}

// Recover reconciles a Bound claim against what the last recovery read
// from battery (CL-030), and calls nothing itself: the recovery's
// ListLeases is battery's answer.
//
//   - A Lease battery did not list has ended, and the claim goes Expired
//     (CL-012), whatever its status says of the expiry.
//   - A Lease battery listed keeps the claim Bound. If battery's expiry is
//     later than the status's, as after a Heartbeat answer lost in a crash
//     (#57), the claim takes battery's (CL-011). An earlier one is older
//     than the status, which came from a later answer, and is ignored.
//     Whether an expiry that has passed ends the claim is left to
//     CheckExpiry, which reads the Lease again, and to Renew before it for
//     a claim with a pending renewal (#85).
//
// A claim bound after the recovery began is not in it and is left to the
// steps after this one.
type Recover struct {
	// Leases is what the Operator's recovery fills; nil recovers nothing.
	Leases *RecoveredLeases
}

var _ claimscope.Subreconciler = Recover{}

// Reconcile implements claimscope.Subreconciler.
func (r Recover) Reconcile(_ context.Context, s *claimscope.Scope) (claimscope.Result, error) {
	c := s.Claim
	if r.Leases == nil || !holdsLease(c) {
		return claimscope.Result{}, nil
	}
	rec, known := r.Leases.lookup(c.Status.LeaseID)
	if !known {
		return claimscope.Result{}, nil
	}
	//= docs/requirements/02-claims.md#recovery
	//# When the Claim Controller starts, and when its connection to
	//# battery is restored, the Claim Controller SHALL reconcile every Bound claim against
	//# battery's Leases.
	s.Called(methodListLeases, nil)

	//= docs/requirements/02-claims.md#renewal
	//# If battery answers a `Heartbeat` for a claim's Lease with
	//# `NOT_FOUND`, or leaves the claim's Lease out of its answer to
	//# `ListLeases`, then the Claim Controller SHALL set the claim's phase to
	//# `Expired` and its condition `Bound` false with the reason `LeaseExpired`.
	if rec == nil {
		expire(s, fmt.Sprintf("battery no longer held Lease %s when the Claim Controller recovered", c.Status.LeaseID))
		return claimscope.Result{Stop: true}, nil
	}

	//= docs/requirements/02-claims.md#renewal
	//# The Claim Controller SHALL take a claim's Lease expiry time only
	//# from battery's answer to `Heartbeat` or `ListLeases`, and SHALL NOT
	//# compute it.
	if exp := c.Status.LeaseExpiresAt; exp == nil || rec.ExpiresAt.After(exp.Time) {
		c.Status.LeaseExpiresAt = &metav1.Time{Time: rec.ExpiresAt}
		s.Log.V(1).Info("Recovered the Lease expiry of MicroVMClaim from battery", "lease", rec.LeaseID, "expiresAt", rec.ExpiresAt)
	}
	return claimscope.Result{}, nil
}
