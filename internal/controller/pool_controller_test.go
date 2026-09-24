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
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/fakebattery"
)

// startPoolFakeBattery serves the fake battery on loopback until the test
// ends and returns the Operator's client for it. The fake knows no Host,
// as battery knows none of a Pool's until #20 resolves them.
func startPoolFakeBattery(t *testing.T) battery.Client {
	t.Helper()
	b := fakebattery.New(fakebattery.Config{ReconcileInterval: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("fake battery: Serve: %v", err)
		}
	})
	select {
	case <-b.Ready():
	case err := <-done:
		t.Fatalf("fake battery: Serve: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("fake battery did not start listening")
	}
	conn, err := battery.Dial(battery.Config{Address: b.Addr()})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestPoolLifecycleAgainstTheFakeBattery drives the Pool Controller through
// a Pool's life against the fake battery and the fake client: create it,
// change its size, delete it (PO-001, PO-002, PO-003).
func TestPoolLifecycleAgainstTheFakeBattery(t *testing.T) {
	ctx := context.Background()
	bc := startPoolFakeBattery(t)
	pool := testPool()
	k8s := newPoolFakeClient(t, pool)
	r := &PoolReconciler{Client: k8s, Battery: bc, Clock: clock.NewFake(poolTestEpoch)}
	key := types.NamespacedName{Namespace: testPoolNamespace, Name: testPoolName}
	ref := battery.PoolRef{Namespace: testPoolNamespace, Name: testPoolName}

	reconcile := func() {
		t.Helper()
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	get := func() *batteryv1alpha1.Pool {
		t.Helper()
		got := &batteryv1alpha1.Pool{}
		if err := k8s.Get(ctx, key, got); err != nil {
			t.Fatalf("Get Pool: %v", err)
		}
		return got
	}

	// Create: the first reconcile adds the finalizer, the next declares
	// the Pool.
	reconcile()
	if _, err := bc.GetPool(ctx, ref); err == nil {
		t.Fatal("battery holds the Pool before its finalizer is stored")
	}
	reconcile()
	held, err := bc.GetPool(ctx, ref)
	if err != nil {
		t.Fatalf("battery GetPool after create: %v", err)
	}
	if held.Spec.Size != 3 || held.Spec.Template.GetNamespace() != "ci" {
		t.Errorf("battery holds size %d in flintlock namespace %q, want 3 in %q",
			held.Spec.Size, held.Spec.Template.GetNamespace(), "ci")
	}
	got := get()
	if got.Status.ObservedGeneration != got.Generation {
		t.Errorf("observedGeneration = %d, want %d", got.Status.ObservedGeneration, got.Generation)
	}

	// Change the size.
	got.Spec.Size = 5
	if err := k8s.Update(ctx, got); err != nil {
		t.Fatalf("Update Pool: %v", err)
	}
	got = get()
	if got.Generation == got.Status.ObservedGeneration {
		// The fake client does not bump the generation on a spec change
		// as the API server does; do it here.
		got.Generation++
		if err := k8s.Update(ctx, got); err != nil {
			t.Fatalf("Update Pool generation: %v", err)
		}
	}
	reconcile()
	if held, err = bc.GetPool(ctx, ref); err != nil || held.Spec.Size != 5 {
		t.Fatalf("battery after resize: size %v, err %v; want 5", held, err)
	}
	got = get()
	if got.Status.ObservedGeneration != got.Generation {
		t.Errorf("observedGeneration = %d, want %d", got.Status.ObservedGeneration, got.Generation)
	}

	// Delete.
	if err := k8s.Delete(ctx, got); err != nil {
		t.Fatalf("Delete Pool: %v", err)
	}
	reconcile()
	if _, err := bc.GetPool(ctx, ref); err == nil {
		t.Error("battery still holds the Pool after its deletion")
	}
	if err := k8s.Get(ctx, key, &batteryv1alpha1.Pool{}); !apierrors.IsNotFound(err) {
		t.Errorf("Get Pool after deletion: %v, want NotFound", err)
	}

	// A Pool already gone reconciles to nothing.
	reconcile()
}

// TestPoolScopePatchesStatusAndFinalizerOnce: what the subreconcilers
// change reaches the API server in the final patch, and nothing else does.
func TestPoolScopePatchesStatusAndFinalizerOnce(t *testing.T) {
	ctx := context.Background()
	pool := testPool()
	k8s := newPoolFakeClient(t, pool)
	fetched := &batteryv1alpha1.Pool{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(pool), fetched); err != nil {
		t.Fatal(err)
	}
	s := newPoolScope(fetched, k8s, newStubBattery(), ctrl.Log, clock.NewFake(poolTestEpoch))
	s.Pool.Finalizers = append(s.Pool.Finalizers, PoolFinalizer)
	s.Pool.Status.ObservedGeneration = 1
	if err := s.patch(ctx); err != nil {
		t.Fatalf("patch: %v", err)
	}
	got := &batteryv1alpha1.Pool{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(pool), got); err != nil {
		t.Fatal(err)
	}
	if len(got.Finalizers) != 1 || got.Finalizers[0] != PoolFinalizer || got.Status.ObservedGeneration != 1 {
		t.Errorf("stored Pool: finalizers %v, observedGeneration %d", got.Finalizers, got.Status.ObservedGeneration)
	}
}

// TestPoolTheFakeBatteryRefusesIsRejected: a spec battery refuses (here a
// missing heartbeat expiry threshold, which the CRD would have defaulted)
// ends as Ready False with the reason Rejected and battery's message
// (PO-004).
func TestPoolTheFakeBatteryRefusesIsRejected(t *testing.T) {
	ctx := context.Background()
	bc := startPoolFakeBattery(t)
	pool := finalizedPool()
	pool.Spec.Lease.ExpiryThreshold = nil
	k8s := newPoolFakeClient(t, pool)
	r := &PoolReconciler{Client: k8s, Battery: bc, Clock: clock.NewFake(poolTestEpoch)}
	key := client.ObjectKeyFromObject(pool)
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := &batteryv1alpha1.Pool{}
	if err := k8s.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	ready := readyCondition(got)
	if ready == nil || ready.Reason != PoolReasonRejected || ready.Message != "spec.heartbeat_expiry_threshold must be positive" {
		t.Errorf("Ready = %+v, want Rejected with battery's message", ready)
	}
	if got.Status.ObservedGeneration != 0 {
		t.Errorf("observedGeneration = %d, want 0", got.Status.ObservedGeneration)
	}
}
