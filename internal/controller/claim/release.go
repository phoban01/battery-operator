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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/controller/claimscope"
)

// methodReleaseVM names ReleaseVM in a claimscope.BatteryCall.
const methodReleaseVM = "ReleaseVM"

// Release ends the Lease of a claim that is being deleted, with battery's
// ReleaseVM, and then removes the release finalizer so that the claim goes
// away. It runs first in the chain, and stops the chain for every claim
// being deleted: nothing else happens to a deleted claim.
//
// Release records the call in the scope's BatteryCall, for Synced. A
// ReleaseVM that fails in transit, battery's UNAVAILABLE included, keeps
// the finalizer and asks for a retry after Backoff. Any other failure is
// returned, so the claim is retried with the controller's backoff.
//
// The lease id Release reads may come from a cache that is behind an
// earlier bind. Removing the finalizer is safe even so: the controller's
// metadata patch carries an optimistic lock, and the bind's status write
// changed the claim's resourceVersion, so a patch from a copy without the
// lease id conflicts and the claim is reconciled again from a fresh copy.
type Release struct {
	Backoff Backoff
}

var _ claimscope.Subreconciler = Release{}

// Reconcile implements claimscope.Subreconciler.
func (r Release) Reconcile(ctx context.Context, s *claimscope.Scope) (claimscope.Result, error) {
	if s.Claim.DeletionTimestamp.IsZero() {
		return claimscope.Result{}, nil
	}
	stop := claimscope.Result{Stop: true}
	if !controllerutil.ContainsFinalizer(s.Claim, batteryv1alpha1.ReleaseFinalizer) {
		return stop, nil
	}
	leaseID := s.Claim.Status.LeaseID

	//= docs/requirements/02-claims.md#release
	//# When a claim that has no lease id is deleted, the Claim
	//# Controller SHALL remove its finalizer without calling battery.
	if leaseID == "" {
		// A claim that never bound holds no Lease. A ClaimVM whose answer
		// was lost may have leased one, but no claim records it, so it is
		// an orphan that battery expires (CL-008, CL-031).
		controllerutil.RemoveFinalizer(s.Claim, batteryv1alpha1.ReleaseFinalizer)
		s.Log.Info("Removed the release finalizer from an unbound MicroVMClaim")
		return stop, nil
	}

	//= docs/requirements/02-claims.md#release
	//# When a claim that has a lease id is deleted, the Claim
	//# Controller SHALL call battery's `ReleaseVM` for the Lease, and SHALL remove
	//# the finalizer only once battery has released the Lease or reported it
	//# unknown.
	err := s.Battery.ReleaseVM(ctx, leaseID)
	s.Called(methodReleaseVM, err)
	switch {
	case err == nil, errors.Is(err, battery.ErrNotFound):
		// NOT_FOUND is a Lease that has already ended: an earlier release
		// whose answer was lost, a crash before the finalizer was removed,
		// or a Lease battery expired (BA-032).
		controllerutil.RemoveFinalizer(s.Claim, batteryv1alpha1.ReleaseFinalizer)
		s.Log.Info("Released the Lease of MicroVMClaim", "lease", leaseID, "unknown", err != nil)
		return stop, nil

	//= docs/requirements/02-claims.md#release
	//# If battery's `ReleaseVM` for a deleted claim fails in transit,
	//# then the Claim Controller SHALL keep the finalizer and retry `ReleaseVM`
	//# with backoff.
	case failedInTransit(err):
		// battery answers UNAVAILABLE, and keeps the Lease, until
		// flintlockd confirms the MicroVM's deletion (BA-031); that cannot
		// be told from a lost answer, and both are retried. A retry after a
		// release whose answer was lost finds the Lease unknown.
		after := r.Backoff.after(s.Clock.Now().Sub(unavailableSince(s)))
		s.Log.V(1).Info("Kept the release finalizer of MicroVMClaim", "lease", leaseID, "retryAfter", after, "error", err.Error())
		return claimscope.Result{Stop: true, RequeueAfter: after}, nil

	default:
		// CL-041: the controller's rate limiter retries it with backoff;
		// Synced records the failure, and the finalizer stays.
		return stop, fmt.Errorf("releasing Lease %s: %w", leaseID, err)
	}
}

// unavailableSince is when battery started failing the claim's calls in
// transit: the lastTransitionTime of a Synced condition that is false with
// BatteryUnavailable, or now if it is not. The wait before the next
// ReleaseVM is as long as that has lasted, so the waits double as the
// failures go on, as Pending's do, without an attempt count in memory.
func unavailableSince(s *claimscope.Scope) time.Time {
	c := meta.FindStatusCondition(s.Claim.Status.Conditions, batteryv1alpha1.ConditionSynced)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != batteryv1alpha1.ReasonBatteryUnavailable {
		return s.Clock.Now()
	}
	return c.LastTransitionTime.Time
}
