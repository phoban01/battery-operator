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
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
)

// PoolFinalizer holds a Pool until battery has deleted it.
const PoolFinalizer = "battery.liquidmetal-x.dev/pool"

// Reasons for the Pool's Ready condition set by the subreconcilers here.
const (
	// PoolReasonRejected: battery refused the Pool's spec (PO-004).
	PoolReasonRejected = "Rejected"
	// PoolReasonDeletionBlocked: battery refused to delete the Pool, which
	// still has MicroVMs; the message is battery's.
	PoolReasonDeletionBlocked = "DeletionBlocked"
)

// poolChain is the Pool Controller's chain, in order.
func poolChain() []poolSubreconciler {
	return []poolSubreconciler{
		poolDeletion{},
		poolFinalizer{},
		poolDeclaration{},
		poolUpdate{},
		poolRejection{},
	}
}

// poolDeletion deletes a deleted Pool from battery and then lets it go.
// For a Pool that is not being deleted it does nothing.
//
// battery refuses DeletePool with FAILED_PRECONDITION while the Pool still
// has MicroVMs (battery.ErrFailedPrecondition). The finalizer then stays,
// Ready turns false with the reason DeletionBlocked and battery's message,
// and the reconcile returns the error, so that the workqueue retries it
// with its exponential backoff (up to about 16 minutes between tries) until
// battery accepts. battery v0.1.0 has no call to drain a Pool, so the
// Operator does not try to empty it first.
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
	//# the Pool is deleted SHALL call `DeletePool` and remove the finalizer only
	//# once battery has deleted the Pool or reported it unknown.
	ref := poolRef(s.Pool)
	err := s.Battery.DeletePool(ctx, ref)
	switch {
	case err == nil, errors.Is(err, battery.ErrNotFound):
		controllerutil.RemoveFinalizer(s.Pool, PoolFinalizer)
		s.Log.Info("Deleted Pool from battery", "pool", ref.String())
		return poolStop, nil
	case errors.Is(err, battery.ErrFailedPrecondition):
		s.setCondition(batteryv1alpha1.PoolConditionReady, metav1.ConditionFalse,
			PoolReasonDeletionBlocked, batteryMessage(err, battery.ErrFailedPrecondition))
		return poolStop, fmt.Errorf("battery refused to delete Pool %s: %w", ref, err)
	default:
		return poolStop, fmt.Errorf("deleting Pool %s from battery: %w", ref, err)
	}
}

// poolFinalizer adds the finalizer to a Pool that lacks it, and ends the
// chain so that the Pool reaches battery only once the finalizer is stored:
// the patch that adds it brings the Pool back for the next reconcile.
type poolFinalizer struct{}

func (poolFinalizer) Reconcile(_ context.Context, s *poolScope) (poolNext, error) {
	//= docs/requirements/03-pools.md#declaration
	//# The Pool Controller SHALL add a finalizer to each Pool, and when
	//# the Pool is deleted SHALL call `DeletePool` and remove the finalizer only
	//# once battery has deleted the Pool or reported it unknown.
	if controllerutil.AddFinalizer(s.Pool, PoolFinalizer) {
		return poolStop, nil
	}
	return poolContinue, nil
}

// poolDeclaration creates the Pool in battery when battery does not hold
// it. The spec it creates is the Pool's current generation, which is then
// the observed one.
type poolDeclaration struct{}

func (poolDeclaration) Reconcile(ctx context.Context, s *poolScope) (poolNext, error) {
	ref := poolRef(s.Pool)
	_, err := s.Battery.GetPool(ctx, ref)
	switch {
	case err == nil:
		return poolContinue, nil
	case !errors.Is(err, battery.ErrNotFound):
		return poolStop, fmt.Errorf("getting Pool %s from battery: %w", ref, err)
	}

	//= docs/requirements/03-pools.md#declaration
	//# When a Pool exists that battery does not hold, the Pool
	//# Controller SHALL create it in battery with `CreatePool`, under the Pool's
	//# namespace and name.
	_, err = s.Battery.CreatePool(ctx, poolSpecToBattery(s.Pool))
	switch {
	case err == nil:
		s.Pool.Status.ObservedGeneration = s.Pool.Generation
		s.Log.Info("Created Pool in battery", "pool", ref.String(), "generation", s.Pool.Generation)
		return poolContinue, nil
	case errors.Is(err, battery.ErrAlreadyExists):
		// Created since GetPool; poolUpdate sends the spec.
		return poolContinue, nil
	case errors.Is(err, battery.ErrInvalid):
		s.refusal = err
		return poolContinue, nil
	default:
		return poolStop, fmt.Errorf("creating Pool %s in battery: %w", ref, err)
	}
}

// poolUpdate sends a Pool's spec to battery when its generation has moved
// past the one battery last accepted.
type poolUpdate struct{}

func (poolUpdate) Reconcile(ctx context.Context, s *poolScope) (poolNext, error) {
	if s.refusal != nil || s.Pool.Generation == s.Pool.Status.ObservedGeneration {
		return poolContinue, nil
	}

	//= docs/requirements/03-pools.md#declaration
	//# When a Pool's `metadata.generation` differs from its
	//# `status.observedGeneration`, the Pool Controller SHALL send the Pool's spec
	//# to battery with `UpdatePool` and then set `status.observedGeneration` to
	//# that generation.
	ref := poolRef(s.Pool)
	_, err := s.Battery.UpdatePool(ctx, poolSpecToBattery(s.Pool))
	switch {
	case err == nil:
		s.Pool.Status.ObservedGeneration = s.Pool.Generation
		s.Log.Info("Updated Pool in battery", "pool", ref.String(), "generation", s.Pool.Generation)
		return poolContinue, nil
	case errors.Is(err, battery.ErrInvalid):
		s.refusal = err
		return poolContinue, nil
	default:
		// ErrNotFound included: battery lost the Pool since poolDeclaration
		// looked, and the retry creates it again.
		return poolStop, fmt.Errorf("updating Pool %s in battery: %w", ref, err)
	}
}

// poolRejection reports battery's refusal of the Pool's spec on the Ready
// condition, and clears a refusal battery no longer makes. The Pool is not
// retried: a new spec, which is a new generation, is.
type poolRejection struct{}

func (poolRejection) Reconcile(_ context.Context, s *poolScope) (poolNext, error) {
	if s.refusal == nil {
		ready := meta.FindStatusCondition(s.Pool.Status.Conditions, batteryv1alpha1.PoolConditionReady)
		if ready != nil && ready.Reason == PoolReasonRejected {
			meta.RemoveStatusCondition(&s.Pool.Status.Conditions, batteryv1alpha1.PoolConditionReady)
		}
		return poolContinue, nil
	}

	//= docs/requirements/03-pools.md#declaration
	//# If battery refuses a Pool's spec, then the Pool Controller SHALL
	//# set the Pool's condition `Ready` false with the reason `Rejected` and
	//# battery's message.
	msg := batteryMessage(s.refusal, battery.ErrInvalid)
	s.setCondition(batteryv1alpha1.PoolConditionReady, metav1.ConditionFalse, PoolReasonRejected, msg)
	s.Log.Info("Battery refused Pool", "pool", poolRef(s.Pool).String(), "message", msg)
	return poolContinue, nil
}

// batteryMessage is battery's own message in an error the battery client
// returned wrapping sentinel.
func batteryMessage(err, sentinel error) string {
	return strings.TrimPrefix(err.Error(), sentinel.Error()+": ")
}
