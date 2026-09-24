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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/controller/claim"
)

// claimVMStub is a battery.Client whose ClaimVM gives the next of answers.
// Every other method panics through the nil embedded Client.
type claimVMStub struct {
	battery.Client
	answers []error
	calls   int
}

func (s *claimVMStub) ClaimVM(context.Context, battery.PoolRef) (*battery.Claim, error) {
	err := s.answers[s.calls]
	s.calls++
	if err != nil {
		return nil, err
	}
	return &battery.Claim{LeaseID: "lease-1", VMUID: "vm-1", Host: battery.HostRef{Name: "node-a"}}, nil
}

// TestMicroVMClaimReconcilerBindsAfterTheFinalizer drives the controller
// through a claim's binding, one reconcile at a time, as the watch on the
// claim would: the finalizer is written first, an exhausted Pool leaves
// the claim Pending with a retry, and the claim binds once battery has a
// MicroVM. It checks the wiring; each step's behaviour is tested in
// package claim.
func TestMicroVMClaimReconcilerBindsAfterTheFinalizer(t *testing.T) {
	ctx := context.Background()
	s := runtime.NewScheme()
	if err := batteryv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(&batteryv1alpha1.MicroVMClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "runner-1", Namespace: "ci"},
			Spec: batteryv1alpha1.MicroVMClaimSpec{
				PoolRef:            batteryv1alpha1.PoolReference{Name: "small"},
				ServiceAccountName: "runner",
			},
		}).
		WithStatusSubresource(&batteryv1alpha1.MicroVMClaim{}).
		Build()
	b := &claimVMStub{answers: []error{battery.ErrExhausted, nil}}
	r := &MicroVMClaimReconciler{
		Client:    c,
		Scheme:    s,
		APIReader: c,
		Battery:   b,
		Clock:     clock.NewFake(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)),
		Backoff:   claim.Backoff{Min: time.Second, Max: time.Minute},
	}
	key := client.ObjectKey{Namespace: "ci", Name: "runner-1"}
	req := ctrl.Request{NamespacedName: key}
	got := &batteryv1alpha1.MicroVMClaim{}

	// First: the finalizer, and no call to battery.
	if res, err := r.Reconcile(ctx, req); err != nil || res.RequeueAfter != 0 {
		t.Fatalf("first Reconcile = %+v, %v", res, err)
	}
	if err := c.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	if len(got.Finalizers) != 1 || got.Finalizers[0] != batteryv1alpha1.ReleaseFinalizer || b.calls != 0 {
		t.Fatalf("after the first reconcile: finalizers %v, %d ClaimVM calls", got.Finalizers, b.calls)
	}

	// Second: the Pool is exhausted.
	res, err := r.Reconcile(ctx, req)
	if err != nil || res.RequeueAfter != time.Second {
		t.Fatalf("second Reconcile = %+v, %v; want a retry after 1s", res, err)
	}
	if err := c.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != batteryv1alpha1.MicroVMClaimPending {
		t.Fatalf("phase = %q, want Pending", got.Status.Phase)
	}

	// Third: a MicroVM is available.
	if res, err := r.Reconcile(ctx, req); err != nil || res.RequeueAfter != 0 {
		t.Fatalf("third Reconcile = %+v, %v", res, err)
	}
	if err := c.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != batteryv1alpha1.MicroVMClaimBound || got.Status.LeaseID != "lease-1" {
		t.Fatalf("status = %+v, want Bound on lease-1", got.Status)
	}

	// Later: a Bound claim is not claimed again.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if b.calls != 2 {
		t.Errorf("ClaimVM calls = %d, want 2", b.calls)
	}
}

// TestMicroVMClaimReconcilerIgnoresAGoneClaim: a claim deleted before its
// reconcile is not an error.
func TestMicroVMClaimReconcilerIgnoresAGoneClaim(t *testing.T) {
	s := runtime.NewScheme()
	if err := batteryv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).Build()
	r := &MicroVMClaimReconciler{Client: c, Scheme: s, APIReader: c, Battery: &claimVMStub{}}
	req := ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "ci", Name: "gone"}}
	if res, err := r.Reconcile(context.Background(), req); err != nil || res != (ctrl.Result{}) {
		t.Errorf("Reconcile = %+v, %v; want nothing", res, err)
	}
}
