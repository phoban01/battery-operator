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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/reconcile"
)

// PoolReasonDeletionBlocked is the reason of the Ready condition of a
// deleted Pool that battery refuses to delete while claims hold some of
// its MicroVMs (PO-043).
const PoolReasonDeletionBlocked = "DeletionBlocked"

// poolDeletion deletes a deleted Pool from battery and then lets it go.
// For a Pool that is not being deleted it does nothing.
//
// battery v0.4.0's DeletePool drains the Pool it deletes: it refuses with
// FAILED_PRECONDITION while the Pool has a leased or claiming MicroVM, and
// otherwise deletes the Pool with every MicroVM it owns (BA-070, BA-073).
// So poolDeletion only asks. While battery refuses, it gives the Pool its
// drained spec, under which battery creates no MicroVM for it (BA-072),
// and says on the Ready condition that the Pool waits for its claims.
// battery's Events stream (PO-023) and the workqueue's backoff bring the
// Pool back to ask again.
type poolDeletion struct{}

func (poolDeletion) Reconcile(ctx context.Context, s *poolScope) (reconcile.Result, error) {
	if s.Object.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(s.Object, PoolFinalizer) {
		return reconcile.Result{Stop: true}, nil
	}

	//= docs/requirements/03-pools.md#declaration
	//# The Pool Controller SHALL add a finalizer to each Pool, and when
	//# the Pool is deleted SHALL call `DeletePool` until battery accepts it
	//# (PO-042, PO-043), and remove the finalizer only once battery has
	//# deleted the Pool or reported it unknown.

	//= docs/requirements/03-pools.md#deletion
	//# The Pool Controller SHALL NOT claim or release a MicroVM of a
	//# deleted Pool, and SHALL leave the deletion of the Pool's MicroVMs to
	//# battery's `DeletePool`.
	ref := poolRef(s.Object)
	err := s.Battery.DeletePool(ctx, ref)
	switch {
	case err == nil, errors.Is(err, battery.ErrNotFound):
		releasePool(s, ref)
		return reconcile.Result{Stop: true}, nil
	case !errors.Is(err, battery.ErrFailedPrecondition):
		return reconcile.Result{Stop: true}, fmt.Errorf("deleting Pool %s from battery: %w", ref, err)
	}

	held, err := s.Battery.GetPool(ctx, ref)
	switch {
	case errors.Is(err, battery.ErrNotFound):
		releasePool(s, ref)
		return reconcile.Result{Stop: true}, nil
	case err != nil:
		return reconcile.Result{Stop: true}, fmt.Errorf("getting Pool %s from battery: %w", ref, err)
	}

	if !isDrainedSpec(held.Spec) {
		//= docs/requirements/03-pools.md#deletion
		//# When battery refuses `DeletePool` for a deleted Pool and the
		//# Pool's spec in battery is not its drained spec, the Pool Controller
		//# SHALL send battery the Pool's drained spec with `UpdatePool`.
		updated, err := s.Battery.UpdatePool(ctx, drainedSpec(held.Spec))
		if err != nil {
			res, refusal := reportRefusal(s, ref, held.Status)
			return res, errors.Join(fmt.Errorf("sending Pool %s's drained spec to battery: %w", ref, err), refusal)
		}
		held = updated
		s.Log.Info("Sent Pool's drained spec to battery", "pool", ref.String())
	}
	return reportRefusal(s, ref, held.Status)
}

// reportRefusal shows on the Pool's Ready condition that battery refuses
// to delete it while claims hold its MicroVMs, with battery's counts. It
// returns an error, so that the workqueue retries the Pool with its
// exponential backoff; the Events stream brings it back sooner when a
// MicroVM goes (PO-023). It returns poolDeletion's result, which stops the
// chain.
func reportRefusal(s *poolScope, ref battery.PoolRef, st battery.PoolStatus) (reconcile.Result, error) {
	setDeletionCounts(s, st)

	//= docs/requirements/03-pools.md#deletion
	//# While battery refuses `DeletePool` for a deleted Pool, the Pool
	//# Controller SHALL set the Pool's condition `Ready` false with the reason
	//# `DeletionBlocked` and a message that gives the Pool's leased MicroVMs.
	msg := fmt.Sprintf("battery holds %d leased MicroVMs of the Pool, and deletes the Pool with its other MicroVMs "+
		"once they are gone: a leased MicroVM goes when its claim is released or its Lease expires", st.Leased)
	s.setCondition(batteryv1alpha1.PoolConditionReady, metav1.ConditionFalse, PoolReasonDeletionBlocked, msg)
	s.Log.Info("Battery refused to delete Pool", "pool", ref.String(), "leased", st.Leased)
	return reconcile.Result{Stop: true}, fmt.Errorf("battery refused to delete Pool %s: %d leased MicroVMs remain: %w",
		ref, st.Leased, battery.ErrFailedPrecondition)
}

// releasePool removes the finalizer of a Pool battery no longer holds.
func releasePool(s *poolScope, ref battery.PoolRef) {
	controllerutil.RemoveFinalizer(s.Object, PoolFinalizer)
	s.Log.Info("Deleted Pool from battery", "pool", ref.String())
}

// setDeletionCounts copies battery's counts into a Pool being deleted, as
// poolCounts does for the others (PO-020).
func setDeletionCounts(s *poolScope, counts battery.PoolStatus) {
	st := &s.Object.Status
	st.Available = counts.Available
	st.Leased = counts.Leased
	st.Provisioning = counts.Provisioning
	st.Quarantined = counts.Quarantined
}

// drainedMinSize is the min_size of a drained spec: battery refuses
// MIN_SIZE_THRESHOLD without a positive one.
const drainedMinSize = 1

// drainedSpec is spec, the one battery holds for a deleted Pool, as its
// drained spec (glossary): size 0 and MIN_SIZE_THRESHOLD with a min_size
// of 1, under which battery creates no MicroVM for it (BA-072). The rest
// of the spec stays, so that a claim that binds to one of the Pool's
// available MicroVMs meanwhile runs the Pool's own pre-lease hooks.
func drainedSpec(spec battery.PoolSpec) battery.PoolSpec {
	minSize := int32(drainedMinSize)
	spec.Size = 0
	spec.Replenishment = battery.ReplenishmentStrategy{
		Type:    poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD,
		MinSize: &minSize,
	}
	spec.FlintlockHosts = slices.Clone(spec.FlintlockHosts)
	spec.CreateCommands = slices.Clone(spec.CreateCommands)
	spec.PreLeaseCommands = slices.Clone(spec.PreLeaseCommands)
	return spec
}

// isDrainedSpec reports whether spec is already a drained spec.
func isDrainedSpec(spec battery.PoolSpec) bool {
	r := spec.Replenishment
	return spec.Size == 0 &&
		r.Type == poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD &&
		r.MinSize != nil && *r.MinSize == drainedMinSize
}
