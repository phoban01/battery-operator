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
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
)

// finalizedPool is testPool with the Pool Controller's finalizer.
func finalizedPool() *batteryv1alpha1.Pool {
	pool := testPool()
	pool.Finalizers = []string{PoolFinalizer}
	return pool
}

// deletedPool is finalizedPool being deleted.
func deletedPool() *batteryv1alpha1.Pool {
	pool := finalizedPool()
	now := metav1.NewTime(poolTestEpoch)
	pool.DeletionTimestamp = &now
	return pool
}

func wantCalls(t *testing.T, b *stubBattery, want ...string) {
	t.Helper()
	if got := b.Calls(); !slices.Equal(got, want) {
		t.Errorf("battery calls = %q, want %q", got, want)
	}
}

func readyCondition(pool *batteryv1alpha1.Pool) *metav1.Condition {
	return meta.FindStatusCondition(pool.Status.Conditions, batteryv1alpha1.PoolConditionReady)
}

//= docs/requirements/03-pools.md#declaration
//= type=test
//# The Pool Controller SHALL add a finalizer to each Pool, and when
//# the Pool is deleted SHALL call `DeletePool`, drain the Pool while battery
//# refuses it (PO-030, PO-031), and remove the finalizer only once battery
//# has deleted the Pool or reported it unknown.

// TestPoolFinalizerIsAddedBeforeBatteryHearsOfThePool covers PO-003: the
// chain stops once it adds the finalizer, so battery is not called until
// the finalizer is stored.
func TestPoolFinalizerIsAddedBeforeBatteryHearsOfThePool(t *testing.T) {
	b := newStubBattery()
	s := newTestPoolScope(testPool(), b)
	if err := runPoolChain(context.Background(), s, poolChain()); err != nil {
		t.Fatalf("chain: %v", err)
	}
	if !controllerutil.ContainsFinalizer(s.Pool, PoolFinalizer) {
		t.Errorf("finalizers = %v, want %q", s.Pool.Finalizers, PoolFinalizer)
	}
	wantCalls(t, b)

	next, err := poolFinalizer{}.Reconcile(context.Background(), s)
	if err != nil || next != poolContinue {
		t.Errorf("with the finalizer present: next = %v, err = %v; want continue", next, err)
	}
}

//= docs/requirements/03-pools.md#declaration
//= type=test
//# The Pool Controller SHALL add a finalizer to each Pool, and when
//# the Pool is deleted SHALL call `DeletePool`, drain the Pool while battery
//# refuses it (PO-030, PO-031), and remove the finalizer only once battery
//# has deleted the Pool or reported it unknown.

// TestDeletedPoolLeavesBatteryBeforeItsFinalizer covers PO-003 for each
// answer battery can give DeletePool but a refusal, which
// pool_deletion_test.go covers.
func TestDeletedPoolLeavesBatteryBeforeItsFinalizer(t *testing.T) {
	for _, tc := range []struct {
		name          string
		held          bool
		deleteErr     error
		wantFinalizer bool
		wantErr       error
	}{
		{name: "battery deletes it", held: true},
		{name: "battery does not know it", held: false},
		{name: "battery is unavailable", held: true, deleteErr: battery.ErrUnavailable,
			wantFinalizer: true, wantErr: battery.ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newStubBattery()
			pool := deletedPool()
			if tc.held {
				b.pools[poolRef(pool)] = poolSpecToBattery(pool, nil)
			}
			b.deleteErr = tc.deleteErr
			s := newTestPoolScope(pool, b)

			err := runPoolChain(context.Background(), s, poolChain())
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil) != (err == nil) {
				t.Fatalf("chain error = %v, want %v", err, tc.wantErr)
			}
			wantCalls(t, b, "DeletePool ci/runners")
			if got := controllerutil.ContainsFinalizer(s.Pool, PoolFinalizer); got != tc.wantFinalizer {
				t.Errorf("finalizer present = %v, want %v", got, tc.wantFinalizer)
			}
			if ready := readyCondition(s.Pool); ready != nil {
				t.Errorf("Ready = %+v, want none", ready)
			}
		})
	}
}

// TestDeletedPoolWithoutFinalizerIsLeftAlone: a Pool deleted without the
// finalizer never reached battery, so battery is not called.
func TestDeletedPoolWithoutFinalizerIsLeftAlone(t *testing.T) {
	b := newStubBattery()
	pool := deletedPool()
	pool.Finalizers = []string{"example.com/other"}
	s := newTestPoolScope(pool, b)
	if err := runPoolChain(context.Background(), s, poolChain()); err != nil {
		t.Fatalf("chain: %v", err)
	}
	wantCalls(t, b)
}

//= docs/requirements/03-pools.md#declaration
//= type=test
//# When a Pool exists that battery does not hold, the Pool
//# Controller SHALL add the finalizer `battery.liquidmetal-x.dev/pool` to the
//# Pool before it creates it in battery with `CreatePool`, under the Pool's
//# namespace and name.

// TestPoolBatteryDoesNotHoldIsCreated covers PO-001 for a Pool read with
// its finalizer; TestPoolIsNotCreatedBeforeItsFinalizerIsStored covers the
// order.
func TestPoolBatteryDoesNotHoldIsCreated(t *testing.T) {
	b := newStubBattery()
	s := newTestPoolScope(finalizedPool(), b)
	if err := runPoolChain(context.Background(), s, poolChain()); err != nil {
		t.Fatalf("chain: %v", err)
	}
	wantCalls(t, b, "GetPool ci/runners", "CreatePool ci/runners")
	spec, ok := b.spec(battery.PoolRef{Name: testPoolName, Namespace: testPoolNamespace})
	if !ok {
		t.Fatal("battery does not hold ci/runners")
	}
	if spec.Size != 3 {
		t.Errorf("battery's size = %d, want 3", spec.Size)
	}
	if s.Pool.Status.ObservedGeneration != 1 {
		t.Errorf("observedGeneration = %d, want 1", s.Pool.Status.ObservedGeneration)
	}
}

// TestPoolBatteryHoldsIsNotCreatedAgain: a Pool battery holds at the
// observed generation costs one GetPool.
func TestPoolBatteryHoldsIsNotCreatedAgain(t *testing.T) {
	b := newStubBattery()
	pool := finalizedPool()
	pool.Status.ObservedGeneration = 1
	b.pools[poolRef(pool)] = poolSpecToBattery(pool, nil)
	s := newTestPoolScope(pool, b)
	if err := runPoolChain(context.Background(), s, poolChain()); err != nil {
		t.Fatalf("chain: %v", err)
	}
	wantCalls(t, b, "GetPool ci/runners")
}

// TestPoolIsNotCreatedWhileBatteryIsUnavailable: an unanswered GetPool is
// retried, and CreatePool is not guessed at.
func TestPoolIsNotCreatedWhileBatteryIsUnavailable(t *testing.T) {
	b := newStubBattery()
	b.getErr = battery.ErrUnavailable
	s := newTestPoolScope(finalizedPool(), b)
	if err := runPoolChain(context.Background(), s, poolChain()); !errors.Is(err, battery.ErrUnavailable) {
		t.Fatalf("chain error = %v, want ErrUnavailable", err)
	}
	wantCalls(t, b, "GetPool ci/runners")
	if s.Pool.Status.ObservedGeneration != 0 {
		t.Errorf("observedGeneration = %d, want 0", s.Pool.Status.ObservedGeneration)
	}
}

//= docs/requirements/03-pools.md#declaration
//= type=test
//# When a Pool's `metadata.generation` differs from its
//# `status.observedGeneration`, the Pool Controller SHALL send the Pool's spec
//# to battery with `UpdatePool` and then set `status.observedGeneration` to
//# that generation.

// TestPoolWhoseGenerationMovedIsUpdated covers PO-002.
func TestPoolWhoseGenerationMovedIsUpdated(t *testing.T) {
	b := newStubBattery()
	pool := finalizedPool()
	b.pools[poolRef(pool)] = poolSpecToBattery(pool, nil)
	pool.Status.ObservedGeneration = 1
	pool.Generation = 2
	pool.Spec.Size = 5
	s := newTestPoolScope(pool, b)
	if err := runPoolChain(context.Background(), s, poolChain()); err != nil {
		t.Fatalf("chain: %v", err)
	}
	wantCalls(t, b, "GetPool ci/runners", "UpdatePool ci/runners")
	if spec, _ := b.spec(poolRef(pool)); spec.Size != 5 {
		t.Errorf("battery's size = %d, want 5", spec.Size)
	}
	if s.Pool.Status.ObservedGeneration != 2 {
		t.Errorf("observedGeneration = %d, want 2", s.Pool.Status.ObservedGeneration)
	}
}

// TestFailedUpdateLeavesObservedGeneration: observedGeneration moves only
// once battery has the spec.
func TestFailedUpdateLeavesObservedGeneration(t *testing.T) {
	b := newStubBattery()
	pool := finalizedPool()
	b.pools[poolRef(pool)] = poolSpecToBattery(pool, nil)
	pool.Status.ObservedGeneration = 1
	pool.Generation = 2
	b.updateErr = battery.ErrUnavailable
	s := newTestPoolScope(pool, b)
	if err := runPoolChain(context.Background(), s, poolChain()); !errors.Is(err, battery.ErrUnavailable) {
		t.Fatalf("chain error = %v, want ErrUnavailable", err)
	}
	if s.Pool.Status.ObservedGeneration != 1 {
		t.Errorf("observedGeneration = %d, want 1", s.Pool.Status.ObservedGeneration)
	}
}

//= docs/requirements/03-pools.md#declaration
//= type=test
//# If battery refuses a Pool's spec, then the Pool Controller SHALL
//# set the Pool's condition `Ready` false with the reason `Rejected` and
//# battery's message.

// TestPoolBatteryRefusesIsRejected covers PO-004, for a refused create and
// a refused update.
func TestPoolBatteryRefusesIsRejected(t *testing.T) {
	const msg = "spec.heartbeat_expiry_threshold must be positive"
	refusal := fmt.Errorf("%w: %s", battery.ErrInvalid, msg)
	for _, tc := range []struct {
		name     string
		held     bool
		wantCall string
	}{
		{name: "CreatePool", wantCall: "CreatePool ci/runners"},
		{name: "UpdatePool", held: true, wantCall: "UpdatePool ci/runners"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newStubBattery()
			pool := finalizedPool()
			if tc.held {
				b.pools[poolRef(pool)] = poolSpecToBattery(pool, nil)
				pool.Status.ObservedGeneration = 1
				pool.Generation = 2
			}
			b.createErr, b.updateErr = refusal, refusal
			s := newTestPoolScope(pool, b)
			wantGen := pool.Status.ObservedGeneration

			if err := runPoolChain(context.Background(), s, poolChain()); err != nil {
				t.Fatalf("chain: %v, want a refusal reported on the Pool, not returned", err)
			}
			wantCalls(t, b, "GetPool ci/runners", tc.wantCall)
			ready := readyCondition(s.Pool)
			if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != PoolReasonRejected || ready.Message != msg {
				t.Errorf("Ready = %+v, want False/Rejected/%q", ready, msg)
			}
			if ready != nil && ready.ObservedGeneration != pool.Generation {
				t.Errorf("Ready.observedGeneration = %d, want %d", ready.ObservedGeneration, pool.Generation)
			}
			if s.Pool.Status.ObservedGeneration != wantGen {
				t.Errorf("observedGeneration = %d, want %d: battery did not accept the spec", s.Pool.Status.ObservedGeneration, wantGen)
			}
		})
	}
}

// TestAcceptedPoolClearsItsRejection: once battery accepts a new spec, the
// Rejected condition goes, and Ready is poolReadiness's to say (PO-022).
func TestAcceptedPoolClearsItsRejection(t *testing.T) {
	b := newStubBattery()
	pool := finalizedPool()
	b.pools[poolRef(pool)] = poolSpecToBattery(pool, nil)
	pool.Status.ObservedGeneration = 1
	pool.Generation = 2
	meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type: batteryv1alpha1.PoolConditionReady, Status: metav1.ConditionFalse, Reason: PoolReasonRejected, Message: "no",
	})
	s := newTestPoolScope(pool, b)
	if err := runPoolChain(context.Background(), s, poolChain()); err != nil {
		t.Fatalf("chain: %v", err)
	}
	if ready := readyCondition(s.Pool); ready == nil || ready.Reason == PoolReasonRejected {
		t.Errorf("Ready = %+v, want the rejection cleared and Ready set from battery's answer", ready)
	}
}

//= docs/requirements/03-pools.md#declaration
//= type=test
//# When a Pool exists that battery does not hold, the Pool
//# Controller SHALL add the finalizer `battery.liquidmetal-x.dev/pool` to the
//# Pool before it creates it in battery with `CreatePool`, under the Pool's
//# namespace and name.

// TestPoolDeclarationWaitsForTheStoredFinalizer covers PO-001's order in
// poolDeclaration alone: a finalizer the chain has added but the API
// server has not yet stored does not let the Pool into battery.
func TestPoolDeclarationWaitsForTheStoredFinalizer(t *testing.T) {
	b := newStubBattery()
	s := newTestPoolScope(testPool(), b)
	controllerutil.AddFinalizer(s.Pool, PoolFinalizer)

	next, err := poolDeclaration{}.Reconcile(context.Background(), s)
	if err != nil || next != poolStop {
		t.Errorf("next = %v, err = %v; want stop", next, err)
	}
	wantCalls(t, b, "GetPool ci/runners")
	if _, ok := b.spec(poolRef(s.Pool)); ok {
		t.Error("battery holds a Pool whose finalizer is not stored")
	}
}
