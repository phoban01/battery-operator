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
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
)

// declaredPool is finalizedPool as battery accepted it: its generation is
// observed, so the chain reads battery and sends nothing.
func declaredPool() *batteryv1alpha1.Pool {
	pool := finalizedPool()
	pool.Status.ObservedGeneration = pool.Generation
	return pool
}

// holdPool puts pool in b at its spec, on hosts, with the given counts.
func holdPool(b *stubBattery, pool *batteryv1alpha1.Pool, hosts []string, st battery.PoolStatus) {
	spec := poolSpecToBattery(pool, nil)
	spec.FlintlockHosts = hosts
	b.pools[spec.Ref] = spec
	b.status[spec.Ref] = st
}

func runStatusChain(t *testing.T, pool *batteryv1alpha1.Pool, b *stubBattery, hosts ...string) *poolScope {
	t.Helper()
	s := newTestPoolScope(pool, b, hosts...)
	if err := runPoolChain(context.Background(), s, poolChain()); err != nil {
		t.Fatalf("chain: %v", err)
	}
	return s
}

func wantCondition(t *testing.T, pool *batteryv1alpha1.Pool, condType string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	c := meta.FindStatusCondition(pool.Status.Conditions, condType)
	if c == nil || c.Status != status || c.Reason != reason {
		t.Errorf("%s = %+v, want %s with the reason %s", condType, c, status, reason)
	}
}

//= docs/requirements/03-pools.md#pool-status
//= type=test
//# The Pool Controller SHALL take a Pool's counts of available,
//# leased, provisioning and quarantined MicroVMs from battery's `PoolStatus`
//# for the Pool and from nothing else.

// TestPoolCountsComeFromBattery covers PO-020: the counts are battery's,
// whatever the Pool said before, and zero while battery holds no Pool.
func TestPoolCountsComeFromBattery(t *testing.T) {
	t.Run("battery holds the Pool", func(t *testing.T) {
		b := newStubBattery()
		pool := declaredPool()
		pool.Status.Available, pool.Status.Leased, pool.Status.Provisioning, pool.Status.Quarantined = 9, 9, 9, 9
		holdPool(b, pool, []string{testHostA}, battery.PoolStatus{Available: 2, Leased: 1, Provisioning: 3, Quarantined: 4})

		s := runStatusChain(t, pool, b, testHostA)
		st := s.Pool.Status
		if st.Available != 2 || st.Leased != 1 || st.Provisioning != 3 || st.Quarantined != 4 {
			t.Errorf("counts = %d available, %d leased, %d provisioning, %d quarantined; want 2, 1, 3, 4",
				st.Available, st.Leased, st.Provisioning, st.Quarantined)
		}
		wantCalls(t, b, "GetPool ci/runners")
	})
	t.Run("battery refuses the Pool", func(t *testing.T) {
		b := newStubBattery()
		b.createErr = fmt.Errorf("%w: spec.size must not be negative", battery.ErrInvalid)
		pool := finalizedPool()
		pool.Status.Available, pool.Status.Leased = 5, 1

		s := runStatusChain(t, pool, b)
		st := s.Pool.Status
		if st.Available != 0 || st.Leased != 0 || st.Provisioning != 0 || st.Quarantined != 0 {
			t.Errorf("counts = %+v, want zero while battery holds no Pool", st)
		}
	})
	t.Run("CreatePool finds the Pool already there", func(t *testing.T) {
		b := &racingStubBattery{stubBattery: newStubBattery()}
		pool := declaredPool()
		b.created = poolSpecToBattery(pool, nil)
		b.status[poolRef(pool)] = battery.PoolStatus{Available: 1}

		s := newTestPoolScope(pool, b)
		if err := runPoolChain(context.Background(), s, poolChain()); err != nil {
			t.Fatalf("chain: %v", err)
		}
		if s.Pool.Status.Available != 1 {
			t.Errorf("available = %d, want 1 from GetPool", s.Pool.Status.Available)
		}
		wantCalls(t, b.stubBattery, "GetPool ci/runners", "CreatePool ci/runners", "GetPool ci/runners")
	})
}

// racingStubBattery answers the first GetPool NotFound and then holds the
// Pool, as if another writer created it in between, so that CreatePool
// finds it already there.
type racingStubBattery struct {
	*stubBattery
	created battery.PoolSpec
}

func (b *racingStubBattery) CreatePool(ctx context.Context, spec battery.PoolSpec) (*battery.Pool, error) {
	b.mu.Lock()
	b.pools[b.created.Ref] = b.created
	b.mu.Unlock()
	return b.stubBattery.CreatePool(ctx, spec)
}

//= docs/requirements/03-pools.md#pool-status
//= type=test
//# The Pool Controller SHALL set a Pool's condition `Exhausted`
//# true while battery reports no available MicroVM in a Pool whose size is
//# greater than zero, and false otherwise.

// TestPoolExhaustedFollowsAvailable covers PO-021.
func TestPoolExhaustedFollowsAvailable(t *testing.T) {
	for _, tc := range []struct {
		name       string
		size       int32
		available  int32
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{name: "none available", size: 3, available: 0, wantStatus: metav1.ConditionTrue, wantReason: PoolReasonNoneAvailable},
		{name: "one available", size: 3, available: 1, wantStatus: metav1.ConditionFalse, wantReason: PoolReasonAvailable},
		{name: "size zero", size: 0, available: 0, wantStatus: metav1.ConditionFalse, wantReason: PoolReasonSizeZero},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newStubBattery()
			pool := declaredPool()
			pool.Spec.Size = tc.size
			holdPool(b, pool, []string{testHostA}, battery.PoolStatus{Available: tc.available, Leased: 3})

			s := runStatusChain(t, pool, b, testHostA)
			wantCondition(t, s.Pool, batteryv1alpha1.PoolConditionExhausted, tc.wantStatus, tc.wantReason)
		})
	}
}

//= docs/requirements/03-pools.md#pool-status
//= type=test
//# The Pool Controller SHALL set a Pool's condition `Ready` true
//# while battery holds the Pool, its selector matches a Host, and the sum of
//# its available, leased and provisioning MicroVMs is at least its size.

// TestPoolReadyNeedsBatteryAHostAndItsSize covers PO-022, each clause on
// its own. "Its selector matches a Host" is what poolPlacement resolved.
func TestPoolReadyNeedsBatteryAHostAndItsSize(t *testing.T) {
	for _, tc := range []struct {
		name       string
		hosts      []string
		counts     battery.PoolStatus
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{name: "at its size", hosts: []string{testHostA}, counts: battery.PoolStatus{Available: 1, Leased: 1, Provisioning: 1},
			wantStatus: metav1.ConditionTrue, wantReason: PoolReasonAtSize},
		{name: "above its size", hosts: []string{testHostA}, counts: battery.PoolStatus{Available: 3, Leased: 2},
			wantStatus: metav1.ConditionTrue, wantReason: PoolReasonAtSize},
		{name: "quarantined MicroVMs do not count", hosts: []string{testHostA}, counts: battery.PoolStatus{Available: 2, Quarantined: 5},
			wantStatus: metav1.ConditionFalse, wantReason: PoolReasonBelowSize},
		{name: "no Host", hosts: nil, counts: battery.PoolStatus{Available: 3},
			wantStatus: metav1.ConditionFalse, wantReason: PoolReasonNoEligibleHost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newStubBattery()
			pool := declaredPool()
			holdPool(b, pool, tc.hosts, tc.counts)

			s := runStatusChain(t, pool, b, tc.hosts...)
			wantCondition(t, s.Pool, batteryv1alpha1.PoolConditionReady, tc.wantStatus, tc.wantReason)
		})
	}

	t.Run("battery does not hold the Pool", func(t *testing.T) {
		s := newTestPoolScope(declaredPool(), newStubBattery(), testHostA)
		s.hosts = []string{testHostA}
		if _, err := (poolReadiness{}).Reconcile(context.Background(), s); err != nil {
			t.Fatal(err)
		}
		wantCondition(t, s.Pool, batteryv1alpha1.PoolConditionReady, metav1.ConditionFalse, PoolReasonNotInBattery)
	})

	t.Run("battery's refusal stands", func(t *testing.T) {
		b := newStubBattery()
		b.createErr = fmt.Errorf("%w: spec.size must not be negative", battery.ErrInvalid)
		s := runStatusChain(t, finalizedPool(), b)
		wantCondition(t, s.Pool, batteryv1alpha1.PoolConditionReady, metav1.ConditionFalse, PoolReasonRejected)
	})
}
