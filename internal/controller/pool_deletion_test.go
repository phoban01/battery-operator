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
	"testing"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/fakebattery"
	"github.com/phoban01/battery-operator/internal/fakeflintlock"
	"github.com/phoban01/battery-operator/internal/reconcile"
)

// heldDeletedPool is deletedPool, with pre-lease hooks and the quarantine
// policy, held by b with st as its status. It returns the Pool and its ref.
func heldDeletedPool(b *stubBattery, st battery.PoolStatus) (*batteryv1alpha1.Pool, battery.PoolRef) {
	pool := deletedPool()
	pool.Spec.Hooks.PreLease = []string{"/opt/prepare.sh"}
	pool.Spec.Hooks.FailurePolicy = batteryv1alpha1.HookFailureQuarantine
	ref := poolRef(pool)
	b.pools[ref] = poolSpecToBattery(pool, []string{testHostA})
	b.status[ref] = st
	return pool, ref
}

// runDeletion runs the chain for a deleted Pool and returns its result and error.
func runDeletion(t *testing.T, s *poolScope) (reconcile.Result, error) {
	t.Helper()
	return poolChain().Run(context.Background(), s)
}

func wantReady(t *testing.T, s *poolScope, reason string) *metav1.Condition {
	t.Helper()
	ready := readyCondition(s.Object)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != reason {
		t.Fatalf("Ready = %+v, want False/%s", ready, reason)
	}
	return ready
}

// wantNoClaims fails if the Pool Controller claimed or released a MicroVM.
// The stub battery has neither call, so one would panic first; this says
// so where a test relies on it.
func wantNoClaims(t *testing.T, b *stubBattery) {
	t.Helper()
	for _, call := range b.Calls() {
		if strings.HasPrefix(call, "ClaimVM ") || strings.HasPrefix(call, "ReleaseVM ") {
			t.Errorf("the Pool Controller called %q for a deleted Pool", call)
		}
	}
}

//= docs/requirements/03-pools.md#declaration
//= type=test
//# The Pool Controller SHALL add a finalizer to each Pool, and when
//# the Pool is deleted SHALL call `DeletePool` until battery accepts it
//# (PO-042, PO-043), and remove the finalizer only once battery has
//# deleted the Pool or reported it unknown.

//= docs/requirements/03-pools.md#deletion
//= type=test
//# The Pool Controller SHALL NOT claim or release a MicroVM of a
//# deleted Pool, and SHALL leave the deletion of the Pool's MicroVMs to
//# battery's `DeletePool`.

// TestADeletedPoolGoesWithItsMicroVMs covers PO-003 and PO-044: a Pool no
// claim holds goes in one DeletePool, which deletes its available,
// provisioning and quarantined MicroVMs with it (BA-070, BA-073).
func TestADeletedPoolGoesWithItsMicroVMs(t *testing.T) {
	b := newStubBattery()
	pool, ref := heldDeletedPool(b, battery.PoolStatus{Available: 2, Provisioning: 1, Quarantined: 1})
	s := newTestPoolScope(pool, b)

	if _, err := runDeletion(t, s); err != nil {
		t.Fatalf("chain: %v", err)
	}
	wantCalls(t, b, "DeletePool ci/runners")
	wantNoClaims(t, b)
	if controllerutil.ContainsFinalizer(s.Object, PoolFinalizer) {
		t.Error("the finalizer is still on the Pool battery deleted")
	}
	if _, ok := b.spec(ref); ok {
		t.Error("battery still holds the Pool")
	}
	if ready := readyCondition(s.Object); ready != nil {
		t.Errorf("Ready = %+v, want none", ready)
	}
}

//= docs/requirements/03-pools.md#deletion
//= type=test
//# When battery refuses `DeletePool` for a deleted Pool and the
//# Pool's spec in battery is not its drained spec, the Pool Controller
//# SHALL send battery the Pool's drained spec with `UpdatePool`.

//= docs/requirements/03-pools.md#deletion
//= type=test
//# While battery refuses `DeletePool` for a deleted Pool, the Pool
//# Controller SHALL set the Pool's condition `Ready` false with the reason
//# `DeletionBlocked` and a message that gives the Pool's leased MicroVMs.

// TestADeletedPoolWaitsForItsClaims covers PO-042, PO-043 and PO-044:
// battery refuses while claims hold two MicroVMs, so the Pool Controller
// gives the Pool its drained spec, which keeps the Pool's template, Hosts
// and hooks, says DeletionBlocked with the counts, keeps the finalizer and
// takes no MicroVM itself. Once the claims end, the next reconcile deletes
// the Pool.
func TestADeletedPoolWaitsForItsClaims(t *testing.T) {
	b := newStubBattery()
	pool, ref := heldDeletedPool(b, battery.PoolStatus{Available: 1, Leased: 2, Quarantined: 1})
	before := b.pools[ref]
	s := newTestPoolScope(pool, b)

	_, err := runDeletion(t, s)
	if !errors.Is(err, battery.ErrFailedPrecondition) {
		t.Fatalf("chain error = %v, want the refusal, for the workqueue's backoff", err)
	}
	wantCalls(t, b, "DeletePool ci/runners", "GetPool ci/runners", "UpdatePool ci/runners")
	wantNoClaims(t, b)
	got, _ := b.spec(ref)
	if got.Size != 0 ||
		got.Replenishment.Type != poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD ||
		got.Replenishment.MinSize == nil || *got.Replenishment.MinSize != 1 {
		t.Errorf("drained spec = size %d, %v min %v; want size 0, MIN_SIZE_THRESHOLD min 1",
			got.Size, got.Replenishment.Type, got.Replenishment.MinSize)
	}
	if !proto.Equal(got.Template, before.Template) || !slices.Equal(got.FlintlockHosts, before.FlintlockHosts) ||
		!slices.Equal(got.PreLeaseCommands, before.PreLeaseCommands) || got.HookFailurePolicy != before.HookFailurePolicy ||
		got.HeartbeatExpiryThreshold != before.HeartbeatExpiryThreshold {
		t.Errorf("drained spec changed more than the size and strategy battery held: %+v", got)
	}
	ready := wantReady(t, s, PoolReasonDeletionBlocked)
	if !strings.Contains(ready.Message, "2 leased") {
		t.Errorf("Ready message = %q, want the leased count", ready.Message)
	}
	if !controllerutil.ContainsFinalizer(s.Object, PoolFinalizer) {
		t.Error("the finalizer went while battery still holds the Pool")
	}
	if st := s.Object.Status; st.Available != 1 || st.Leased != 2 || st.Quarantined != 1 {
		t.Errorf("status counts = %+v, want battery's", st)
	}

	// The claims end; the next reconcile deletes the Pool, quarantined
	// MicroVM and all.
	b.status[ref] = battery.PoolStatus{Available: 1, Quarantined: 1}
	b.calls = nil
	s = newTestPoolScope(s.Object, b)
	if _, err := runDeletion(t, s); err != nil {
		t.Fatalf("chain: %v", err)
	}
	wantCalls(t, b, "DeletePool ci/runners")
	if controllerutil.ContainsFinalizer(s.Object, PoolFinalizer) {
		t.Error("the finalizer is still on the Pool battery deleted")
	}
}

// TestADrainedPoolIsNotUpdatedAgain covers PO-042 for a Pool whose spec in
// battery is already drained: no UpdatePool, and the Pool still waits.
func TestADrainedPoolIsNotUpdatedAgain(t *testing.T) {
	b := newStubBattery()
	pool, ref := heldDeletedPool(b, battery.PoolStatus{Leased: 1})
	b.pools[ref] = drainedSpec(b.pools[ref])
	s := newTestPoolScope(pool, b)
	if _, err := runDeletion(t, s); !errors.Is(err, battery.ErrFailedPrecondition) {
		t.Fatalf("chain error = %v, want the refusal", err)
	}
	wantCalls(t, b, "DeletePool ci/runners", "GetPool ci/runners")
	wantReady(t, s, PoolReasonDeletionBlocked)
}

// TestAFailedDrainIsRetried: an UpdatePool that fails leaves the Pool
// DeletionBlocked, with the finalizer, and returns the failure so the
// workqueue tries again (PO-042, PO-043).
func TestAFailedDrainIsRetried(t *testing.T) {
	b := newStubBattery()
	pool, _ := heldDeletedPool(b, battery.PoolStatus{Available: 1, Leased: 1})
	b.updateErr = battery.ErrUnavailable
	s := newTestPoolScope(pool, b)
	_, err := runDeletion(t, s)
	if !errors.Is(err, battery.ErrUnavailable) {
		t.Fatalf("chain error = %v, want the failed UpdatePool", err)
	}
	wantReady(t, s, PoolReasonDeletionBlocked)
	if !controllerutil.ContainsFinalizer(s.Object, PoolFinalizer) {
		t.Error("the finalizer went while battery still holds the Pool")
	}
}

// TestADeletedPoolBatteryLosesIsLetGo: battery refused, then no longer
// knows the Pool when asked for it (PO-003's "reported it unknown").
func TestADeletedPoolBatteryLosesIsLetGo(t *testing.T) {
	b := newStubBattery()
	pool := deletedPool()
	b.deleteErr = battery.ErrFailedPrecondition
	s := newTestPoolScope(pool, b)
	if _, err := runDeletion(t, s); err != nil {
		t.Fatalf("chain: %v", err)
	}
	wantCalls(t, b, "DeletePool ci/runners", "GetPool ci/runners")
	if controllerutil.ContainsFinalizer(s.Object, PoolFinalizer) {
		t.Error("the finalizer is still on a Pool battery does not know")
	}
}

// TestPoolDeletionAgainstTheFakeBattery drives a Pool's deletion through
// the reconciler, the fake client and the fake battery, which drains a
// Pool in DeletePool as battery v0.4.0 does (BA-070): the Pool battery
// filled, with one MicroVM a claim holds, gets its drained spec and waits
// DeletionBlocked for the claim with its warm MicroVMs, and goes with them
// once the claim's Lease is released (PO-003, PO-042 to PO-044).
func TestPoolDeletionAgainstTheFakeBattery(t *testing.T) {
	ctx := context.Background()
	fl := fakeflintlock.New(fakeflintlock.Config{Name: testHostA})
	t.Cleanup(func() { _ = fl.Close() })
	flConn, err := fl.Conn()
	if err != nil {
		t.Fatalf("fake flintlockd: %v", err)
	}
	t.Cleanup(func() { _ = flConn.Close() })
	hosts := fakebattery.NewHosts()
	hosts.Add(testHostA, testHostA+":9090", flConn)
	bc, _ := startPoolFakeBatteryWith(t, fakebattery.Config{Hosts: hosts})

	pool := finalizedPool()
	k8s := newPoolFakeClient(t, pool, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: testHostA}})
	r := &PoolReconciler{Client: k8s, Battery: bc, Hosts: newStubHosts(testHostA), Clock: clock.NewFake(poolTestEpoch)}
	key := client.ObjectKeyFromObject(pool)
	ref := poolRef(pool)
	reconcileOnce := func() { _, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: key}) }
	available := func(n int32) func() (bool, string) {
		return func() (bool, string) {
			held, err := bc.GetPool(ctx, ref)
			if err != nil {
				return false, err.Error()
			}
			return held.Status.Available == n && held.Status.Provisioning == 0, fmt.Sprintf("%+v", held.Status)
		}
	}

	// Declare the Pool, let battery fill it, and have a claim hold one of
	// its MicroVMs; IMMEDIATE_ON_LEASE replaces it.
	reconcileOnce()
	eventually(t, "the Pool filled", available(3))
	claim, err := bc.ClaimVM(ctx, ref, "")
	if err != nil {
		t.Fatalf("ClaimVM: %v", err)
	}
	eventually(t, "the Pool refilled", available(3))

	if err := k8s.Delete(ctx, pool.DeepCopy()); err != nil {
		t.Fatalf("Delete Pool: %v", err)
	}
	eventually(t, "the Pool blocked by the claim", func() (bool, string) {
		reconcileOnce()
		got := &batteryv1alpha1.Pool{}
		if err := k8s.Get(ctx, key, got); err != nil {
			return false, err.Error()
		}
		ready := readyCondition(got)
		return ready != nil && ready.Reason == PoolReasonDeletionBlocked &&
				got.Status.Available == 3 && got.Status.Leased == 1,
			fmt.Sprintf("Ready %+v, status %+v", ready, got.Status)
	})
	held, err := bc.GetPool(ctx, ref)
	if err != nil {
		t.Fatalf("GetPool: %v", err)
	}
	if !isDrainedSpec(held.Spec) {
		t.Errorf("battery's spec for the deleted Pool = %+v, want its drained spec", held.Spec)
	}
	if n := len(fl.MicroVMs()); n != 4 {
		t.Fatalf("%d MicroVMs on the Host while the claim holds one, want 4", n)
	}

	if err := bc.ReleaseVM(ctx, claim.LeaseID); err != nil {
		t.Fatalf("ReleaseVM: %v", err)
	}
	eventually(t, "the Pool deleted", func() (bool, string) {
		reconcileOnce()
		err := k8s.Get(ctx, key, &batteryv1alpha1.Pool{})
		return apierrors.IsNotFound(err), fmt.Sprintf("Get Pool: %v", err)
	})
	if _, err := bc.GetPool(ctx, ref); !errors.Is(err, battery.ErrNotFound) {
		t.Errorf("battery GetPool after the deletion: %v, want not found", err)
	}
	if n := len(fl.MicroVMs()); n != 0 {
		t.Errorf("%d MicroVMs left on the Host, want none", n)
	}
}
