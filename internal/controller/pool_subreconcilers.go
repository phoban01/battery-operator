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
)

// poolChain is the Pool Controller's chain, in order.
func poolChain() []poolSubreconciler {
	return []poolSubreconciler{
		poolDeletion{},
		poolFinalizer{},
		poolPlacement{},
		poolDeclaration{},
		poolUpdate{},
		poolRejection{},
		poolCounts{},
		poolExhaustion{},
		poolReadiness{},
	}
}

// poolFinalizer adds the finalizer to a Pool that lacks it, and ends the
// chain so that the Pool reaches battery only once the finalizer is stored:
// the patch that adds it brings the Pool back for the next reconcile.
type poolFinalizer struct{}

func (poolFinalizer) Reconcile(_ context.Context, s *poolScope) (poolNext, error) {
	//= docs/requirements/03-pools.md#declaration
	//# The Pool Controller SHALL add a finalizer to each Pool, and when
	//# the Pool is deleted SHALL call `DeletePool`, drain the Pool while battery
	//# refuses it (PO-030, PO-031), and remove the finalizer only once battery
	//# has deleted the Pool or reported it unknown.
	//
	//= docs/requirements/03-pools.md#declaration
	//# When a Pool exists that battery does not hold, the Pool
	//# Controller SHALL add the finalizer `battery.liquidmetal-x.dev/pool` to the
	//# Pool before it creates it in battery with `CreatePool`, under the Pool's
	//# namespace and name.
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
	held, err := s.Battery.GetPool(ctx, ref)
	switch {
	case err == nil:
		s.held = held
		return poolContinue, nil
	case !errors.Is(err, battery.ErrNotFound):
		return poolStop, fmt.Errorf("getting Pool %s from battery: %w", ref, err)
	}

	//= docs/requirements/03-pools.md#declaration
	//# When a Pool exists that battery does not hold, the Pool
	//# Controller SHALL add the finalizer `battery.liquidmetal-x.dev/pool` to the
	//# Pool before it creates it in battery with `CreatePool`, under the Pool's
	//# namespace and name.
	//
	// The finalizer counts once the API server has stored it, which is when
	// the Pool is read back with it. poolFinalizer ends the chain when it
	// adds one; this looks at the Pool as fetched, not as the chain has
	// changed it, so that no order of the chain can create in battery a Pool
	// the API server could still delete at once.
	if !controllerutil.ContainsFinalizer(s.fetched, PoolFinalizer) {
		return poolStop, nil
	}
	held, err = s.Battery.CreatePool(ctx, poolSpecToBattery(s.Pool, s.hosts))
	switch {
	case err == nil:
		s.held = held
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
// past the one battery last accepted, or when the Hosts its selector
// matches are no longer the ones battery's Pool names.
type poolUpdate struct{}

func (poolUpdate) Reconcile(ctx context.Context, s *poolScope) (poolNext, error) {
	if s.refusal != nil {
		return poolContinue, nil
	}
	ref := poolRef(s.Pool)
	if s.held == nil {
		// CreatePool found the Pool already there: see what battery holds.
		held, err := s.Battery.GetPool(ctx, ref)
		switch {
		case err == nil:
			s.held = held
		case errors.Is(err, battery.ErrNotFound):
			return poolContinue, nil
		default:
			return poolStop, fmt.Errorf("getting Pool %s from battery: %w", ref, err)
		}
	}

	//= docs/requirements/03-pools.md#declaration
	//# When a Pool's `metadata.generation` differs from its
	//# `status.observedGeneration`, the Pool Controller SHALL send the Pool's spec
	//# to battery with `UpdatePool` and then set `status.observedGeneration` to
	//# that generation.
	moved := s.Pool.Generation != s.Pool.Status.ObservedGeneration

	//= docs/requirements/03-pools.md#placement
	//# When the set of Hosts a Pool's selector matches changes, the
	//# Pool Controller SHALL update the Pool in battery with `UpdatePool`.
	placed := slices.Equal(slices.Sorted(slices.Values(s.held.Spec.FlintlockHosts)), s.hosts)
	if !moved && placed {
		return poolContinue, nil
	}

	held, err := s.Battery.UpdatePool(ctx, poolSpecToBattery(s.Pool, s.hosts))
	switch {
	case err == nil:
		s.held = held
		s.Pool.Status.ObservedGeneration = s.Pool.Generation
		s.Log.Info("Updated Pool in battery", "pool", ref.String(), "generation", s.Pool.Generation, "hosts", s.hosts)
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
