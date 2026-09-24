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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/controller/claimscope"
)

// Bind leases a MicroVM for a claim that has no Lease, with battery's
// ClaimVM, and records the Lease in the claim's status.
//
// ClaimVM cannot be retried safely: battery chooses the lease id and the
// request carries only the Pool, so a second call leases a second MicroVM.
// Bind therefore calls it only for a claim that the API server itself, not
// the manager's cache, shows with no lease id and with the release
// finalizer written. A cache that has not yet seen the status of an
// earlier bind cannot make it claim twice.
//
// Bind records the call in the scope's BatteryCall, for Pending and
// Synced. A ClaimVM that fails because the Pool is exhausted or unknown, or
// that fails in transit, leaves the claim to Pending, and the chain goes
// on. Any other failure is returned, so the claim is retried with the
// controller's backoff and its phase is left as it was.
//
// A ClaimVM that fails in transit may have leased a MicroVM whose answer
// was lost. Bind never looks for that Lease, in battery or anywhere else:
// it writes only the lease id of the answer it got, and the lost Lease is
// an orphan like any other.
//
// With Slots set, Bind calls ClaimVM only while it holds one of their
// slots, so that a slow ClaimVM never holds up every reconcile (CL-018).
type Bind struct {
	// Slots bounds the ClaimVM calls in flight; nil does not bound them.
	Slots *BindSlots
	// SlotWait is how long a claim that found no slot waits; zero is
	// DefaultSlotWait.
	SlotWait time.Duration
}

var _ claimscope.Subreconciler = Bind{}

// Reconcile implements claimscope.Subreconciler.
func (b Bind) Reconcile(ctx context.Context, s *claimscope.Scope) (claimscope.Result, error) {
	if s.Claim.Status.LeaseID != "" {
		return claimscope.Result{}, nil
	}

	key := client.ObjectKeyFromObject(s.Claim)
	fresh := &batteryv1alpha1.MicroVMClaim{}
	if err := s.APIReader.Get(ctx, key, fresh); err != nil {
		if apierrors.IsNotFound(err) {
			return claimscope.Result{Stop: true}, nil
		}
		return claimscope.Result{}, fmt.Errorf("reading MicroVMClaim %s before claiming a MicroVM: %w", key, err)
	}
	switch {
	case fresh.Status.LeaseID != "":
		// The cache is behind an earlier bind; its update will reconcile
		// the claim again.
		s.Log.V(1).Info("Skipped ClaimVM for a MicroVMClaim the cache shows unbound", "lease", fresh.Status.LeaseID)
		return claimscope.Result{Stop: true}, nil
	case !fresh.DeletionTimestamp.IsZero():
		return claimscope.Result{Stop: true}, nil
	//= docs/requirements/02-claims.md#binding
	//# When a claim that has no lease id in its status is
	//# reconciled, the Claim Controller SHALL add the finalizer
	//# `battery.liquidmetal-x.dev/release` to the claim before it calls battery's
	//# `ClaimVM` for the claim's Pool.
	case !controllerutil.ContainsFinalizer(fresh, batteryv1alpha1.ReleaseFinalizer):
		// EnsureFinalizer added it, but it is not written yet.
		return claimscope.Result{Stop: true}, nil
	}

	//= docs/requirements/02-claims.md#renewal
	//# While battery has not yet answered `ClaimVM` calls for other
	//# claims, the Claim Controller SHALL still reconcile a Bound claim that has
	//# a pending renewal.
	if b.Slots != nil {
		if !b.Slots.tryAcquire() {
			wait := b.SlotWait
			if wait <= 0 {
				wait = DefaultSlotWait
			}
			s.Log.V(1).Info("Deferred ClaimVM for MicroVMClaim while other ClaimVM calls are in flight", "retryAfter", wait)
			return claimscope.Result{Stop: true, RequeueAfter: wait}, nil
		}
		defer b.Slots.release()
	}

	// The renewTime the claim has now is recorded as relayed: the new
	// Lease runs from later than any renewal before it (02-claims.md,
	// Renewal).
	renewTime := s.Claim.Spec.RenewTime.DeepCopy()
	pool := battery.PoolRef{Name: s.Claim.Spec.PoolRef.Name, Namespace: s.Claim.Namespace}
	claimed, err := s.Battery.ClaimVM(ctx, pool)
	s.Called(methodClaimVM, err)
	switch {
	case errors.Is(err, battery.ErrExhausted), errors.Is(err, battery.ErrNotFound), failedInTransit(err):
		return claimscope.Result{}, nil
	//= docs/requirements/02-claims.md#failed-calls
	//# If a call to battery for a claim fails with an error that no
	//# other requirement in this document names, then the Claim Controller SHALL
	//# set the claim's condition `Synced` false with the reason `BatteryError`,
	//# SHALL NOT change the claim's phase because of that failure, and SHALL
	//# retry the call with backoff.
	case err != nil:
		// The controller's rate limiter retries it with backoff; Synced
		// records the failure.
		return claimscope.Result{}, fmt.Errorf("claiming a MicroVM from Pool %s: %w", pool, err)
	}

	// The orphan window (ADR 0001, consequence 2). battery now holds a
	// Lease that only this reconcile knows about. Until the controller's
	// patch writes the status below, a crash, a lost API server or a
	// failed status write orphans that Lease, and the next reconcile
	// claims a second MicroVM. The window is kept as short as it can be:
	// Bind stops the chain, so the status is the next thing written and
	// no other call to battery comes first; the Synced step that runs
	// after the chain only reads the scope. The status patch carries no
	// optimistic lock, so no conflict can lose it. An orphaned Lease is
	// bounded: nothing renews it, so battery expires it after the Pool's
	// heartbeat_expiry_threshold and deletes its MicroVM. Only a
	// client-chosen lease id upstream would close the window.

	//= docs/requirements/02-claims.md#binding
	//# The Claim Controller SHALL write to a claim's status only a
	//# lease id that battery returned in its answer to a `ClaimVM` call for that
	//# claim.

	//= docs/requirements/02-claims.md#binding
	//# When battery's `ClaimVM` succeeds for a claim, the Claim
	//# Controller SHALL write the lease id, the MicroVM's uid, the Host's name as
	//# the node name and the time of binding to the claim's status, set the phase
	//# to `Bound` and set the condition `Bound` true, before it makes any other
	//# call to battery for that claim.
	now := metav1.NewTime(s.Clock.Now())
	st := &s.Claim.Status
	st.LeaseID = claimed.LeaseID
	st.MicroVM = &batteryv1alpha1.MicroVMReference{UID: claimed.VMUID}
	// The Exec Agent's address is not battery's to give (CL-005).
	st.Host = &batteryv1alpha1.HostReference{NodeName: claimed.Host.Name}
	st.BoundTime = &now
	// battery's answer carries no expiry; CheckExpiry reads it with
	// ListLeases once the binding is written (CL-016).
	st.LeaseExpiresAt = nil
	st.ObservedRenewTime = renewTime
	st.Phase = batteryv1alpha1.MicroVMClaimBound
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type:               batteryv1alpha1.ConditionBound,
		Status:             metav1.ConditionTrue,
		Reason:             batteryv1alpha1.ReasonBound,
		Message:            fmt.Sprintf("Leased MicroVM %s on Node %s", claimed.VMUID, claimed.Host.Name),
		ObservedGeneration: s.Claim.Generation,
		LastTransitionTime: now,
	})
	s.Log.Info("Bound MicroVMClaim", "lease", claimed.LeaseID, "microVM", claimed.VMUID, "node", claimed.Host.Name)
	return claimscope.Result{Stop: true}, nil
}
