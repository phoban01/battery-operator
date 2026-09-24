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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/fakebattery"
)

// ends and returns the Operator's client for it. The fake knows no Host:
// it holds Pools and their flintlock_hosts, and provisions nothing.
// as battery knows none of a Pool's until #20 resolves them.
func startPoolFakeBattery(t *testing.T) battery.Client {
	t.Helper()
	bc, _ := startPoolFakeBatteryWith(t, fakebattery.Config{})
	return bc
}

// startPoolFakeBatteryWith is startPoolFakeBattery for cfg, with a 10ms
// reconcile interval unless cfg sets one. It returns the fake too, for its
// fault switches.
func startPoolFakeBatteryWith(t *testing.T, cfg fakebattery.Config) (battery.Client, *fakebattery.Battery) {
	t.Helper()
	if cfg.ReconcileInterval == 0 {
		cfg.ReconcileInterval = 10 * time.Millisecond
	}
	b := fakebattery.New(cfg)
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
	return conn, b
}

// TestPoolLifecycleAgainstTheFakeBattery drives the Pool Controller through
// a Pool's life against the fake battery and the fake client: create it,
// change its size, delete it (PO-001, PO-002, PO-003).
func TestPoolLifecycleAgainstTheFakeBattery(t *testing.T) {
	ctx := context.Background()
	bc := startPoolFakeBattery(t)
	pool := testPool()
	k8s := newPoolFakeClient(t, pool)
	r := &PoolReconciler{Client: k8s, Battery: bc, Hosts: newStubHosts(), Clock: clock.NewFake(poolTestEpoch)}
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
	s := newPoolScope(fetched, k8s, newStubBattery(), newStubHosts(), ctrl.Log, clock.NewFake(poolTestEpoch))
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
	r := &PoolReconciler{Client: k8s, Battery: bc, Hosts: newStubHosts(), Clock: clock.NewFake(poolTestEpoch)}
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

// finalizerCheckingBattery is stubBattery that fails the test when
// CreatePool comes for a Pool the API server holds without the Pool
// Controller's finalizer.
type finalizerCheckingBattery struct {
	*stubBattery
	t   *testing.T
	k8s client.Client
}

func (b finalizerCheckingBattery) CreatePool(ctx context.Context, spec battery.PoolSpec) (*battery.Pool, error) {
	stored := &batteryv1alpha1.Pool{}
	key := types.NamespacedName{Namespace: spec.Ref.Namespace, Name: spec.Ref.Name}
	switch err := b.k8s.Get(ctx, key, stored); {
	case err != nil:
		b.t.Errorf("CreatePool %s for a Pool the API server does not hold: %v", spec.Ref, err)
	case !controllerutil.ContainsFinalizer(stored, PoolFinalizer):
		b.t.Errorf("CreatePool %s before the API server stored the finalizer: finalizers %v", spec.Ref, stored.Finalizers)
	}
	return b.stubBattery.CreatePool(ctx, spec)
}

//= docs/requirements/03-pools.md#declaration
//= type=test
//# When a Pool exists that battery does not hold, the Pool
//# Controller SHALL add the finalizer `battery.liquidmetal-x.dev/pool` to the
//# Pool before it creates it in battery with `CreatePool`, under the Pool's
//# namespace and name.

// TestPoolIsNotCreatedBeforeItsFinalizerIsStored covers the order of
// PO-001 through the reconciler and a fake client: while the API server
// refuses the patch that adds the finalizer, battery hears nothing of the
// Pool, and a Pool deleted then goes at once and leaves nothing in battery
// (#72). battery checks, at CreatePool, that the stored Pool has the
// finalizer.
func TestPoolIsNotCreatedBeforeItsFinalizerIsStored(t *testing.T) {
	ctx := context.Background()
	key := types.NamespacedName{Namespace: testPoolNamespace, Name: testPoolName}

	setup := func(t *testing.T) (*PoolReconciler, *stubBattery, client.Client, *bool) {
		refusePatch := true
		k8s := fake.NewClientBuilder().
			WithScheme(poolTestScheme(t)).
			WithObjects(testPool()).
			WithStatusSubresource(&batteryv1alpha1.Pool{}).
			WithInterceptorFuncs(interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if refusePatch {
						return apierrors.NewServiceUnavailable("the API server is away")
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
			}).
			Build()
		b := newStubBattery()
		r := &PoolReconciler{
			Client:  k8s,
			Battery: finalizerCheckingBattery{stubBattery: b, t: t, k8s: k8s},
			Hosts:   newStubHosts(),
			Clock:   clock.NewFake(poolTestEpoch),
		}
		return r, b, k8s, &refusePatch
	}

	t.Run("the finalizer is stored, then the Pool is created", func(t *testing.T) {
		r, b, _, refusePatch := setup(t)
		for range 2 {
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err == nil {
				t.Fatal("Reconcile: nil error while the finalizer's patch fails")
			}
			wantCalls(t, b)
		}

		*refusePatch = false
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		wantCalls(t, b)

		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		wantCalls(t, b, "GetPool ci/runners", "CreatePool ci/runners")
	})

	t.Run("a Pool deleted before its finalizer is stored leaves nothing in battery", func(t *testing.T) {
		r, b, k8s, _ := setup(t)
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err == nil {
			t.Fatal("Reconcile: nil error while the finalizer's patch fails")
		}
		if err := k8s.Delete(ctx, testPool()); err != nil {
			t.Fatalf("Delete Pool: %v", err)
		}
		if err := k8s.Get(ctx, key, &batteryv1alpha1.Pool{}); !apierrors.IsNotFound(err) {
			t.Fatalf("Get Pool after deletion: %v, want NotFound: it had no finalizer", err)
		}
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		wantCalls(t, b)
		if _, ok := b.spec(battery.PoolRef{Namespace: testPoolNamespace, Name: testPoolName}); ok {
			t.Error("battery holds a Pool the cluster no longer has")
		}
	})
}
