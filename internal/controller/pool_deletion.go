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

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
)

// Reasons for the Ready condition of a Pool being deleted
// (docs/requirements/03-pools.md#deletion).
const (
	// PoolReasonDeletionBlocked: battery refuses to delete the Pool while
	// claims hold some of its MicroVMs or some are quarantined, which the
	// Pool Controller does not delete (PO-032, PO-033).
	PoolReasonDeletionBlocked = "DeletionBlocked"
	// PoolReasonDraining: the Pool Controller is emptying the Pool in
	// battery so that battery can delete it (PO-034).
	PoolReasonDraining = "Draining"
)

// poolDrainRequeue is how soon a Pool being drained is reconciled again
// without an event: battery's Events stream (PO-023) usually comes first.
const poolDrainRequeue = 5 * time.Second

// poolDeletion deletes a deleted Pool from battery and then lets it go.
// For a Pool that is not being deleted it does nothing.
//
// battery v0.3.3 refuses DeletePool with FAILED_PRECONDITION while the
// Pool owns any MicroVM (BA-070), and has no call that deletes a Pool with
// its MicroVMs. So while battery refuses, poolDeletion drains the Pool:
// it gives it its drained spec, under which battery creates no MicroVM
// for it (BA-072), claims each available MicroVM and releases it at once,
// which deletes it (BA-030), and asks again. A MicroVM a claim holds, or
// one battery has quarantined, stays; the Pool waits, and says why on its
// Ready condition.
type poolDeletion struct{}

func (poolDeletion) Reconcile(ctx context.Context, s *poolScope) (poolNext, error) {
	if s.Pool.DeletionTimestamp.IsZero() {
		return poolContinue, nil
	}
	if !controllerutil.ContainsFinalizer(s.Pool, PoolFinalizer) {
		return poolStop, nil
	}

	//= docs/requirements/03-pools.md#declaration
	//# The Pool Controller SHALL add a finalizer to each Pool, and when
	//# the Pool is deleted SHALL call `DeletePool`, drain the Pool while battery
	//# refuses it (PO-030, PO-031), and remove the finalizer only once battery
	//# has deleted the Pool or reported it unknown.
	ref := poolRef(s.Pool)
	if done, err := deletePoolFromBattery(ctx, s, ref); done || err != nil {
		return poolStop, err
	}

	held, err := s.Battery.GetPool(ctx, ref)
	switch {
	case errors.Is(err, battery.ErrNotFound):
		releasePool(s, ref)
		return poolStop, nil
	case err != nil:
		return poolStop, fmt.Errorf("getting Pool %s from battery: %w", ref, err)
	}

	if !isDrainedSpec(held.Spec) {
		if held.Status.Provisioning > 0 {
			// UpdatePool would cancel these under the Pool's old hook
			// failure policy (BA-074), which may quarantine them.
			return poolStop, reportRefusal(s, ref, held.Status)
		}

		//= docs/requirements/03-pools.md#deletion
		//# When battery refuses `DeletePool` for a deleted Pool because
		//# the Pool still has MicroVMs, the Pool's spec in battery is not its
		//# drained spec, and battery reports no provisioning MicroVM in the Pool,
		//# the Pool Controller SHALL send battery the Pool's drained spec with
		//# `UpdatePool`.
		if held, err = s.Battery.UpdatePool(ctx, drainedSpec(held.Spec)); err != nil {
			return poolStop, fmt.Errorf("draining Pool %s in battery: %w", ref, err)
		}
		s.Log.Info("Sent Pool's drained spec to battery", "pool", ref.String())
	}

	if err := drainAvailable(ctx, s, ref, held.Status.Available); err != nil {
		return poolStop, errors.Join(err, reportRefusal(s, ref, held.Status))
	}
	if done, err := deletePoolFromBattery(ctx, s, ref); done || err != nil {
		return poolStop, err
	}
	switch held, err = s.Battery.GetPool(ctx, ref); {
	case errors.Is(err, battery.ErrNotFound):
		releasePool(s, ref)
		return poolStop, nil
	case err != nil:
		return poolStop, fmt.Errorf("getting Pool %s from battery: %w", ref, err)
	}
	return poolStop, reportRefusal(s, ref, held.Status)
}

// reportRefusal shows on the Pool's Ready condition why battery still
// refuses to delete it, with battery's counts. A Pool the Pool Controller
// is still draining is asked for again soon; one that waits on leased or
// quarantined MicroVMs returns an error, so that the workqueue retries it
// with its exponential backoff, and the Events stream brings it back
// sooner when a MicroVM goes (PO-023).
func reportRefusal(s *poolScope, ref battery.PoolRef, st battery.PoolStatus) error {
	setDeletionCounts(s, st)
	if st.Leased == 0 && st.Quarantined == 0 {
		//= docs/requirements/03-pools.md#deletion
		//# While battery refuses `DeletePool` for a deleted Pool that has
		//# no leased or quarantined MicroVM, the Pool Controller SHALL set the Pool's
		//# condition `Ready` false with the reason `Draining`.
		msg := fmt.Sprintf("The Pool Controller is emptying the Pool in battery, which deletes it once it has no MicroVM: "+
			"%d available, %d provisioning", st.Available, st.Provisioning)
		s.setCondition(batteryv1alpha1.PoolConditionReady, metav1.ConditionFalse, PoolReasonDraining, msg)
		s.Result.RequeueAfter = poolDrainRequeue
		return nil
	}

	//= docs/requirements/03-pools.md#deletion
	//# While battery refuses `DeletePool` for a deleted Pool that has
	//# leased or quarantined MicroVMs, the Pool Controller SHALL set the Pool's
	//# condition `Ready` false with the reason `DeletionBlocked` and a message
	//# that gives those counts.
	msg := fmt.Sprintf("battery holds %d leased and %d quarantined MicroVMs of the Pool, and deletes it only once they are gone: "+
		"a leased MicroVM goes when its claim is released or its Lease expires; battery never deletes a quarantined one",
		st.Leased, st.Quarantined)
	s.setCondition(batteryv1alpha1.PoolConditionReady, metav1.ConditionFalse, PoolReasonDeletionBlocked, msg)
	s.Log.Info("Battery refused to delete Pool", "pool", ref.String(), "leased", st.Leased, "quarantined", st.Quarantined)
	return fmt.Errorf("battery refused to delete Pool %s: %d leased and %d quarantined MicroVMs remain: %w",
		ref, st.Leased, st.Quarantined, battery.ErrFailedPrecondition)
}

// deletePoolFromBattery calls DeletePool. It reports done, having removed
// the finalizer, when battery deleted the Pool or does not know it; not
// done and no error when battery refused because the Pool has MicroVMs;
// and an error otherwise.
func deletePoolFromBattery(ctx context.Context, s *poolScope, ref battery.PoolRef) (bool, error) {
	err := s.Battery.DeletePool(ctx, ref)
	switch {
	case err == nil, errors.Is(err, battery.ErrNotFound):
		releasePool(s, ref)
		return true, nil
	case errors.Is(err, battery.ErrFailedPrecondition):
		return false, nil
	default:
		return false, fmt.Errorf("deleting Pool %s from battery: %w", ref, err)
	}
}

// releasePool removes the finalizer of a Pool battery no longer holds.
func releasePool(s *poolScope, ref battery.PoolRef) {
	controllerutil.RemoveFinalizer(s.Pool, PoolFinalizer)
	s.Log.Info("Deleted Pool from battery", "pool", ref.String())
}

// drainAvailable claims up to n available MicroVMs of the Pool and
// releases each at once, until battery has none left to claim.
func drainAvailable(ctx context.Context, s *poolScope, ref battery.PoolRef, n int32) error {
	//= docs/requirements/03-pools.md#deletion
	//# When battery refuses `DeletePool` for a deleted Pool whose
	//# spec in battery is its drained spec, the Pool Controller SHALL claim each
	//# available MicroVM of the Pool with `ClaimVM`, release it at once with
	//# `ReleaseVM`, and then call `DeletePool` again.
	for range n {
		claim, err := s.Battery.ClaimVM(ctx, ref)
		switch {
		case errors.Is(err, battery.ErrExhausted):
			return nil
		case err != nil:
			return fmt.Errorf("claiming a MicroVM of Pool %s to drain it: %w", ref, err)
		}

		//= docs/requirements/03-pools.md#deletion
		//# The Pool Controller SHALL NOT release a Lease of a deleted
		//# Pool other than one it claimed itself under PO-031.
		//
		// The only Lease released here is the one ClaimVM just returned. If
		// its release fails, or the Operator stops before it, nothing renews
		// the Lease, and battery deletes it and its MicroVM once the Pool's
		// heartbeat_expiry_threshold has passed (BA-023).
		if err := s.Battery.ReleaseVM(ctx, claim.LeaseID); err != nil && !errors.Is(err, battery.ErrNotFound) {
			return fmt.Errorf("releasing MicroVM %s of Pool %s to drain it: %w", claim.VMUID, ref, err)
		}
		s.Log.Info("Drained MicroVM from Pool", "pool", ref.String(), "microvm", claim.VMUID)
	}
	return nil
}

// setDeletionCounts copies battery's counts into a Pool being deleted, as
// poolCounts does for the others (PO-020).
func setDeletionCounts(s *poolScope, counts battery.PoolStatus) {
	st := &s.Pool.Status
	st.Available = counts.Available
	st.Leased = counts.Leased
	st.Provisioning = counts.Provisioning
	st.Quarantined = counts.Quarantined
}

// drainedMinSize is the min_size of a drained spec: battery refuses
// MIN_SIZE_THRESHOLD without a positive one.
const drainedMinSize = 1

// drainedSpec is spec, the one battery holds for a deleted Pool, as its
// drained spec (glossary): size 0, MIN_SIZE_THRESHOLD with a min_size of
// 1, no pre-lease commands and DELETE_AND_REPLACE. battery creates no
// MicroVM for it (BA-072), and a drain claim runs no hook and deletes the
// MicroVM rather than quarantine it if it fails.
func drainedSpec(spec battery.PoolSpec) battery.PoolSpec {
	minSize := int32(drainedMinSize)
	spec.Size = 0
	spec.Replenishment = battery.ReplenishmentStrategy{
		Type:    poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD,
		MinSize: &minSize,
	}
	spec.PreLeaseCommands = nil
	spec.HookFailurePolicy = poolmgrv1.HookFailurePolicy_DELETE_AND_REPLACE
	spec.FlintlockHosts = slices.Clone(spec.FlintlockHosts)
	spec.CreateCommands = slices.Clone(spec.CreateCommands)
	return spec
}

// isDrainedSpec reports whether spec is already a drained spec.
func isDrainedSpec(spec battery.PoolSpec) bool {
	r := spec.Replenishment
	return spec.Size == 0 &&
		r.Type == poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD &&
		r.MinSize != nil && *r.MinSize == drainedMinSize &&
		len(spec.PreLeaseCommands) == 0 &&
		spec.HookFailurePolicy == poolmgrv1.HookFailurePolicy_DELETE_AND_REPLACE
}
