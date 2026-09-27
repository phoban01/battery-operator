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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
)

// Reasons for the Pool's Ready and Exhausted conditions set by the status
// subreconcilers (docs/requirements/03-pools.md#pool-status).
const (
	// PoolReasonAtSize: Ready is true; battery holds the Pool, a Host can
	// run it, and it has its size in available, leased and provisioning
	// MicroVMs (PO-022).
	PoolReasonAtSize = "AtSize"
	// PoolReasonBelowSize: battery holds the Pool with fewer available,
	// leased and provisioning MicroVMs than its size (PO-022).
	PoolReasonBelowSize = "BelowSize"
	// PoolReasonNotFilling: Ready is false; the Pool is below its size
	// and a reseed has not changed its shortfall (PO-039). It works around
	// battery v0.3.3, which does not report why a provision fails.
	PoolReasonNotFilling = "NotFilling"
	// PoolReasonNotInBattery: battery does not hold the Pool (PO-022).
	PoolReasonNotInBattery = "NotInBattery"
	// PoolReasonNoEligibleHost: the Pool's selector matches no Host
	// (PO-012, PO-022).
	PoolReasonNoEligibleHost = "NoEligibleHost"

	// PoolReasonNoneAvailable: Exhausted is true; battery reports no
	// available MicroVM in a Pool whose size is greater than zero (PO-021).
	PoolReasonNoneAvailable = "NoneAvailable"
	// PoolReasonAvailable: Exhausted is false; battery reports at least one
	// available MicroVM (PO-021).
	PoolReasonAvailable = "Available"
	// PoolReasonSizeZero: Exhausted is false for a Pool of size zero,
	// which is not meant to hold MicroVMs (PO-021).
	PoolReasonSizeZero = "SizeZero"
)

// poolCounts copies battery's PoolStatus into the Pool's status. The
// counts come from battery's answer in this reconcile and from nothing
// else: not from the Pool's claims, and not from the counts the Pool had.
// A Pool battery does not hold has no PoolStatus, and so counts of zero.
//
// poolDeclaration and poolUpdate leave battery's answer in the scope.
// When they have none (CreatePool answered that the Pool already exists),
// poolCounts asks battery with GetPool.
type poolCounts struct{}

func (poolCounts) Reconcile(ctx context.Context, s *poolScope) (poolNext, error) {
	if s.held == nil && s.refusal == nil {
		ref := poolRef(s.Pool)
		held, err := s.Battery.GetPool(ctx, ref)
		switch {
		case err == nil:
			s.held = held
		case !errors.Is(err, battery.ErrNotFound):
			return poolStop, fmt.Errorf("getting Pool %s from battery: %w", ref, err)
		}
	}

	//= docs/requirements/03-pools.md#pool-status
	//# The Pool Controller SHALL take a Pool's counts of available,
	//# leased, provisioning and quarantined MicroVMs from battery's `PoolStatus`
	//# for the Pool and from nothing else.
	var counts battery.PoolStatus
	if s.held != nil {
		counts = s.held.Status
	}
	st := &s.Pool.Status
	st.Available = counts.Available
	st.Leased = counts.Leased
	st.Provisioning = counts.Provisioning
	st.Quarantined = counts.Quarantined
	return poolContinue, nil
}

// poolExhaustion sets the Exhausted condition from the counts poolCounts
// took from battery.
type poolExhaustion struct{}

func (poolExhaustion) Reconcile(_ context.Context, s *poolScope) (poolNext, error) {
	//= docs/requirements/03-pools.md#pool-status
	//# The Pool Controller SHALL set a Pool's condition `Exhausted`
	//# true while battery reports no available MicroVM in a Pool whose size is
	//# greater than zero, and false otherwise.
	switch {
	case s.Pool.Spec.Size <= 0:
		s.setCondition(batteryv1alpha1.PoolConditionExhausted, metav1.ConditionFalse,
			PoolReasonSizeZero, "The Pool's size is zero")
	case s.Pool.Status.Available == 0:
		s.setCondition(batteryv1alpha1.PoolConditionExhausted, metav1.ConditionTrue,
			PoolReasonNoneAvailable, "battery reports no available MicroVM")
	default:
		s.setCondition(batteryv1alpha1.PoolConditionExhausted, metav1.ConditionFalse,
			PoolReasonAvailable, fmt.Sprintf("battery reports %d available MicroVMs", s.Pool.Status.Available))
	}
	return poolContinue, nil
}

// poolReadiness sets the Ready condition from the Hosts poolPlacement
// found, battery's answer and the counts, unless poolRejection has already
// set it to battery's refusal of the spec, which then stands, over
// NoEligibleHost too (03-pools.md, Placement).
//
// A selector that matches no Host is reported first, whether or not
// battery holds the Pool: it is what the Pool's owner has to change.
type poolReadiness struct{}

func (poolReadiness) Reconcile(_ context.Context, s *poolScope) (poolNext, error) {
	if s.refusal != nil {
		return poolContinue, nil
	}

	//= docs/requirements/03-pools.md#pool-status
	//# The Pool Controller SHALL set a Pool's condition `Ready` true
	//# while battery holds the Pool, its selector matches a Host, and the sum of
	//# its available, leased and provisioning MicroVMs is at least its size.
	st := s.Pool.Status
	total := st.Available + st.Leased + st.Provisioning
	switch {
	case len(s.hosts) == 0:
		//= docs/requirements/03-pools.md#placement
		//# While a Pool's selector matches no Host, the Pool Controller
		//# SHALL set the Pool's condition `Ready` false with the reason
		//# `NoEligibleHost`.
		s.setCondition(batteryv1alpha1.PoolConditionReady, metav1.ConditionFalse,
			PoolReasonNoEligibleHost, "The Pool's node selector matches no Host")
	case s.held == nil:
		s.setCondition(batteryv1alpha1.PoolConditionReady, metav1.ConditionFalse,
			PoolReasonNotInBattery, "battery does not hold the Pool")
	case total < s.Pool.Spec.Size && s.notFilling != nil:
		//= docs/requirements/03-pools.md#refill
		//# While a Pool's shortfall is unchanged since the Pool
		//# Controller saw the Pool stalled after an `UpdatePool` of PO-036, and the
		//# sum of its available, leased and provisioning MicroVMs is less than its
		//# size, the Pool Controller SHALL set the Pool's condition `Ready` false
		//# with the reason `NotFilling`.
		s.setCondition(batteryv1alpha1.PoolConditionReady, metav1.ConditionFalse, PoolReasonNotFilling,
			notFillingMessage(s.notFilling))
	case total < s.Pool.Spec.Size:
		s.setCondition(batteryv1alpha1.PoolConditionReady, metav1.ConditionFalse, PoolReasonBelowSize,
			fmt.Sprintf("%d of %d MicroVMs are available, leased or provisioning", total, s.Pool.Spec.Size))
	default:
		s.setCondition(batteryv1alpha1.PoolConditionReady, metav1.ConditionTrue, PoolReasonAtSize,
			fmt.Sprintf("%d of %d MicroVMs are available, leased or provisioning", total, s.Pool.Spec.Size))
	}
	return poolContinue, nil
}

// notFillingMessage is the message of Ready with the reason NotFilling.
// battery v0.3.3 gives the Operator no cause, so it points at the log
// that has one.
func notFillingMessage(nf *poolNotFilling) string {
	//= docs/requirements/03-pools.md#refill
	//# The Pool Controller SHALL give the condition `Ready` with the
	//# reason `NotFilling` a message that gives the Pool's shortfall, the number
	//# of `UpdatePool` calls of PO-036 since the shortfall last changed, the time
	//# of the next one while the Pool is stalled, and the log of the battery
	//# container as the place to find the cause.
	next := "battery is provisioning for the last one"
	if !nf.next.IsZero() {
		next = "the next is due at " + nf.next.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("The Pool is %s short after %s; %s. "+
		"battery does not report why provisioning fails: see the log of the battery container",
		countOf(int(nf.shortfall), "MicroVM"), countOf(nf.reseeds, "reseed"), next)
}

// countOf is n and the noun, plural unless n is one.
func countOf(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
