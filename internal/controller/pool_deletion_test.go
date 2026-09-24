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

// runDeletion runs the chain for a deleted Pool and returns its error.
func runDeletion(t *testing.T, s *poolScope) error {
	t.Helper()
	return runPoolChain(context.Background(), s, poolChain())
}

func wantReady(t *testing.T, s *poolScope, reason string) *metav1.Condition {
	t.Helper()
	ready := readyCondition(s.Pool)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != reason {
		t.Fatalf("Ready = %+v, want False/%s", ready, reason)
	}
	return ready
}

//= docs/requirements/03-pools.md#deletion
//= type=test
//# When battery refuses `DeletePool` for a deleted Pool because
//# the Pool still has MicroVMs, the Pool's spec in battery is not its
//# drained spec, and battery reports no provisioning MicroVM in the Pool,
//# the Pool Controller SHALL send battery the Pool's drained spec with
//# `UpdatePool`.

//= docs/requirements/03-pools.md#deletion
//= type=test
//# When battery refuses `DeletePool` for a deleted Pool whose
//# spec in battery is its drained spec, the Pool Controller SHALL claim each
//# available MicroVM of the Pool with `ClaimVM`, release it at once with
//# `ReleaseVM`, and then call `DeletePool` again.

//= docs/requirements/03-pools.md#declaration
//= type=test
//# The Pool Controller SHALL add a finalizer to each Pool, and when
//# the Pool is deleted SHALL call `DeletePool`, drain the Pool while battery
//# refuses it (PO-030, PO-031), and remove the finalizer only once battery
//# has deleted the Pool or reported it unknown.

// TestDeletedPoolIsDrainedThenDeleted covers PO-030, PO-031 and PO-003: a
// refused DeletePool gives battery the drained spec, which keeps the
// Pool's template and Hosts, then the Pool Controller claims and releases
// each available MicroVM and deletes the Pool, all in one reconcile.
func TestDeletedPoolIsDrainedThenDeleted(t *testing.T) {
	b := newStubBattery()
	pool, ref := heldDeletedPool(b, battery.PoolStatus{Available: 2})
	s := newTestPoolScope(pool, b)

	if err := runDeletion(t, s); err != nil {
		t.Fatalf("chain: %v", err)
	}
	wantCalls(t, b,
		"DeletePool ci/runners", "GetPool ci/runners", "UpdatePool ci/runners",
		"ClaimVM ci/runners", "ReleaseVM lease-1",
		"ClaimVM ci/runners", "ReleaseVM lease-2",
		"DeletePool ci/runners")
	if controllerutil.ContainsFinalizer(s.Pool, PoolFinalizer) {
		t.Error("the finalizer is still on the Pool battery deleted")
	}
	if _, ok := b.spec(ref); ok {
		t.Error("battery still holds the Pool")
	}

	// The spec the drain sent: the one battery held, drained.
	b2 := newStubBattery()
	pool2, _ := heldDeletedPool(b2, battery.PoolStatus{Available: 1, Leased: 1})
	before := b2.pools[ref]
	s2 := newTestPoolScope(pool2, b2)
	_ = runDeletion(t, s2)
	got, _ := b2.spec(ref)
	if got.Size != 0 ||
		got.Replenishment.Type != poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD ||
		got.Replenishment.MinSize == nil || *got.Replenishment.MinSize != 1 ||
		len(got.PreLeaseCommands) != 0 ||
		got.HookFailurePolicy != poolmgrv1.HookFailurePolicy_DELETE_AND_REPLACE {
		t.Errorf("drained spec = size %d, %v min %v, pre-lease %q, %v; want size 0, MIN_SIZE_THRESHOLD min 1, none, DELETE_AND_REPLACE",
			got.Size, got.Replenishment.Type, got.Replenishment.MinSize, got.PreLeaseCommands, got.HookFailurePolicy)
	}
	if !proto.Equal(got.Template, before.Template) || !slices.Equal(got.FlintlockHosts, before.FlintlockHosts) ||
		got.HeartbeatExpiryThreshold != before.HeartbeatExpiryThreshold {
		t.Errorf("drained spec changed the template, Hosts or expiry threshold battery held: %+v", got)
	}
}

// TestADrainedPoolIsNotUpdatedAgain covers PO-031 for a Pool whose spec in
// battery is already drained: no UpdatePool, straight to the claims.
func TestADrainedPoolIsNotUpdatedAgain(t *testing.T) {
	b := newStubBattery()
	pool, ref := heldDeletedPool(b, battery.PoolStatus{Available: 1})
	b.pools[ref] = drainedSpec(b.pools[ref])
	s := newTestPoolScope(pool, b)
	if err := runDeletion(t, s); err != nil {
		t.Fatalf("chain: %v", err)
	}
	wantCalls(t, b, "DeletePool ci/runners", "GetPool ci/runners",
		"ClaimVM ci/runners", "ReleaseVM lease-1", "DeletePool ci/runners")
}

// TestADeletedPoolIsNotDrainedWhileProvisioning covers PO-030's wait:
// while battery reports a provisioning MicroVM, UpdatePool would cancel
// it under the old policy (BA-074), so the Pool Controller waits, says
// Draining and asks again soon.
func TestADeletedPoolIsNotDrainedWhileProvisioning(t *testing.T) {
	b := newStubBattery()
	pool, _ := heldDeletedPool(b, battery.PoolStatus{Available: 1, Provisioning: 1})
	s := newTestPoolScope(pool, b)
	if err := runDeletion(t, s); err != nil {
		t.Fatalf("chain: %v", err)
	}
	wantCalls(t, b, "DeletePool ci/runners", "GetPool ci/runners")
	wantReady(t, s, PoolReasonDraining)
	if s.Result.RequeueAfter != poolDrainRequeue {
		t.Errorf("RequeueAfter = %v, want %v", s.Result.RequeueAfter, poolDrainRequeue)
	}
	if !controllerutil.ContainsFinalizer(s.Pool, PoolFinalizer) {
		t.Error("the finalizer went while battery still holds the Pool")
	}
	if st := s.Pool.Status; st.Available != 1 || st.Provisioning != 1 {
		t.Errorf("status counts = %+v, want battery's", st)
	}
}

//= docs/requirements/03-pools.md#deletion
//= type=test
//# The Pool Controller SHALL NOT release a Lease of a deleted
//# Pool other than one it claimed itself under PO-031.

//= docs/requirements/03-pools.md#deletion
//= type=test
//# While battery refuses `DeletePool` for a deleted Pool that has
//# leased or quarantined MicroVMs, the Pool Controller SHALL set the Pool's
//# condition `Ready` false with the reason `DeletionBlocked` and a message
//# that gives those counts.

// TestADeletedPoolWaitsForItsClaims covers PO-032 and PO-033: the drain
// takes the available MicroVMs and leaves the ones claims hold, and the
// quarantined one; the Pool stays, DeletionBlocked, with the counts.
func TestADeletedPoolWaitsForItsClaims(t *testing.T) {
	b := newStubBattery()
	pool, ref := heldDeletedPool(b, battery.PoolStatus{Available: 1, Leased: 2, Quarantined: 1})
	// Two Leases claims hold, which the drain did not hand out.
	b.leases["claim-a"], b.leases["claim-b"] = ref, ref
	s := newTestPoolScope(pool, b)

	err := runDeletion(t, s)
	if !errors.Is(err, battery.ErrFailedPrecondition) {
		t.Fatalf("chain error = %v, want the refusal, for the workqueue's backoff", err)
	}
	for _, call := range b.Calls() {
		if strings.HasPrefix(call, "ReleaseVM ") && call != "ReleaseVM lease-1" {
			t.Errorf("the drain called %q, a Lease it did not claim", call)
		}
	}
	if _, ok := b.leases["claim-a"]; !ok {
		t.Error("a claim's Lease was released")
	}
	ready := wantReady(t, s, PoolReasonDeletionBlocked)
	if !strings.Contains(ready.Message, "2 leased and 1 quarantined") {
		t.Errorf("Ready message = %q, want the leased and quarantined counts", ready.Message)
	}
	if !controllerutil.ContainsFinalizer(s.Pool, PoolFinalizer) {
		t.Error("the finalizer went while battery still holds the Pool")
	}
	if st := s.Pool.Status; st.Available != 0 || st.Leased != 2 || st.Quarantined != 1 {
		t.Errorf("status counts = %+v, want battery's after the drain", st)
	}

	// The claims end; the next reconcile deletes the Pool.
	b.status[ref] = battery.PoolStatus{Quarantined: 1}
	s = newTestPoolScope(s.Pool, b)
	if err := runDeletion(t, s); !errors.Is(err, battery.ErrFailedPrecondition) {
		t.Fatalf("chain error with a quarantined MicroVM = %v, want the refusal", err)
	}
	wantReady(t, s, PoolReasonDeletionBlocked)
	b.status[ref] = battery.PoolStatus{}
	s = newTestPoolScope(s.Pool, b)
	if err := runDeletion(t, s); err != nil {
		t.Fatalf("chain: %v", err)
	}
	if controllerutil.ContainsFinalizer(s.Pool, PoolFinalizer) {
		t.Error("the finalizer is still on the Pool battery deleted")
	}
}

//= docs/requirements/03-pools.md#deletion
//= type=test
//# While battery refuses `DeletePool` for a deleted Pool that has
//# no leased or quarantined MicroVM, the Pool Controller SHALL set the Pool's
//# condition `Ready` false with the reason `Draining`.

// TestADeletedPoolDrainingSaysSo covers PO-034: a Pool battery still
// refuses with no leased or quarantined MicroVM, here one being deleted,
// or one whose drain claim failed, says Draining.
func TestADeletedPoolDrainingSaysSo(t *testing.T) {
	t.Run("a MicroVM still being deleted", func(t *testing.T) {
		b := newStubBattery()
		pool, ref := heldDeletedPool(b, battery.PoolStatus{})
		b.hidden[ref] = 1
		s := newTestPoolScope(pool, b)
		if err := runDeletion(t, s); err != nil {
			t.Fatalf("chain: %v", err)
		}
		wantReady(t, s, PoolReasonDraining)
		if s.Result.RequeueAfter != poolDrainRequeue {
			t.Errorf("RequeueAfter = %v, want %v", s.Result.RequeueAfter, poolDrainRequeue)
		}
	})

	t.Run("a drain claim failed", func(t *testing.T) {
		b := newStubBattery()
		pool, _ := heldDeletedPool(b, battery.PoolStatus{Available: 1})
		b.claimErr = battery.ErrUnavailable
		s := newTestPoolScope(pool, b)
		if err := runDeletion(t, s); !errors.Is(err, battery.ErrUnavailable) {
			t.Fatalf("chain error = %v, want the failed claim", err)
		}
		wantReady(t, s, PoolReasonDraining)
		if !controllerutil.ContainsFinalizer(s.Pool, PoolFinalizer) {
			t.Error("the finalizer went while battery still holds the Pool")
		}
	})
}

// TestADeletedPoolBatteryLosesIsLetGo: battery refused, then no longer
// knows the Pool when asked for it (PO-003's "reported it unknown").
func TestADeletedPoolBatteryLosesIsLetGo(t *testing.T) {
	b := newStubBattery()
	pool := deletedPool()
	b.deleteErr = battery.ErrFailedPrecondition
	s := newTestPoolScope(pool, b)
	if err := runDeletion(t, s); err != nil {
		t.Fatalf("chain: %v", err)
	}
	wantCalls(t, b, "DeletePool ci/runners", "GetPool ci/runners")
	if controllerutil.ContainsFinalizer(s.Pool, PoolFinalizer) {
		t.Error("the finalizer is still on a Pool battery does not know")
	}
}

// TestPoolDeletionAgainstTheFakeBattery drives a Pool's deletion through
// the reconciler, the fake client and the fake battery, which refuses
// DeletePool as battery v0.3.3 does (BA-070): the Pool battery filled,
// with one MicroVM a claim holds, is drained, waits DeletionBlocked for
// the claim, and goes once the claim's Lease is released (PO-003, PO-030
// to PO-033).
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
	reconcile := func() { _, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: key}) }
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
	reconcile()
	eventually(t, "the Pool filled", available(3))
	claim, err := bc.ClaimVM(ctx, ref)
	if err != nil {
		t.Fatalf("ClaimVM: %v", err)
	}
	eventually(t, "the Pool refilled", available(3))

	if err := k8s.Delete(ctx, pool.DeepCopy()); err != nil {
		t.Fatalf("Delete Pool: %v", err)
	}
	eventually(t, "the Pool drained, blocked by the claim", func() (bool, string) {
		reconcile()
		got := &batteryv1alpha1.Pool{}
		if err := k8s.Get(ctx, key, got); err != nil {
			return false, err.Error()
		}
		ready := readyCondition(got)
		return ready != nil && ready.Reason == PoolReasonDeletionBlocked &&
				got.Status.Available == 0 && got.Status.Leased == 1,
			fmt.Sprintf("Ready %+v, status %+v", ready, got.Status)
	})
	if n := len(fl.MicroVMs()); n != 1 {
		t.Fatalf("%d MicroVMs on the Host while the claim holds one, want 1", n)
	}

	if err := bc.ReleaseVM(ctx, claim.LeaseID); err != nil {
		t.Fatalf("ReleaseVM: %v", err)
	}
	eventually(t, "the Pool deleted", func() (bool, string) {
		reconcile()
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
