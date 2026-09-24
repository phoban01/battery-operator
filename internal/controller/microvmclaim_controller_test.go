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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/controller/claim"
	"github.com/phoban01/battery-operator/internal/execagent"
)

// The claim these tests reconcile, and the Lease battery gives it.
const (
	testClaimName  = "runner-1"
	testClaimLease = "lease-1"
)

var testClaimKey = client.ObjectKey{Namespace: "ci", Name: testClaimName}

// testClaim is a new claim on the Pool small.
func testClaim() *batteryv1alpha1.MicroVMClaim {
	return &batteryv1alpha1.MicroVMClaim{
		ObjectMeta: metav1.ObjectMeta{Name: testClaimKey.Name, Namespace: testClaimKey.Namespace},
		Spec: batteryv1alpha1.MicroVMClaimSpec{
			PoolRef:            batteryv1alpha1.PoolReference{Name: "small"},
			ServiceAccountName: "runner",
		},
	}
}

// newClaimFakeClient is a fake client holding cl and objs, with the
// client-go types and the MicroVMClaim status subresource.
func newClaimFakeClient(t *testing.T, cl *batteryv1alpha1.MicroVMClaim, objs ...client.Object) (*runtime.Scheme, client.Client) {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := batteryv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(append([]client.Object{cl}, objs...)...).
		WithStatusSubresource(&batteryv1alpha1.MicroVMClaim{}).
		Build()
	return s, c
}

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
	return &battery.Claim{LeaseID: testClaimLease, VMUID: "vm-1", Host: battery.HostRef{Name: "node-a"}}, nil
}

// TestMicroVMClaimReconcilerBindsAfterTheFinalizer drives the controller
// through a claim's binding, one reconcile at a time, as the watch on the
// claim would: the finalizer is written first, an exhausted Pool leaves
// the claim Pending with a retry, and the claim binds once battery has a
// MicroVM, taking the Exec Agent's address from its Node on the reconcile
// after that. It checks the wiring; each step's behaviour is tested in
// package claim.
func TestMicroVMClaimReconcilerBindsAfterTheFinalizer(t *testing.T) {
	ctx := context.Background()
	s, c := newClaimFakeClient(t, testClaim(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:        "node-a",
		Annotations: map[string]string{execagent.AnnotationAddress: "10.0.0.7:7443"},
	}})
	b := &claimVMStub{answers: []error{battery.ErrExhausted, nil}}
	r := &MicroVMClaimReconciler{
		Client:    c,
		Scheme:    s,
		APIReader: c,
		Battery:   b,
		Clock:     clock.NewFake(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)),
		Backoff:   claim.Backoff{Min: time.Second, Max: time.Minute},
	}
	key := testClaimKey
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
	if got.Status.Phase != batteryv1alpha1.MicroVMClaimBound || got.Status.LeaseID != testClaimLease {
		t.Fatalf("status = %+v, want Bound on lease-1", got.Status)
	}
	if !meta.IsStatusConditionTrue(got.Status.Conditions, batteryv1alpha1.ConditionSynced) {
		t.Errorf("Synced is not true after battery answered: %+v", got.Status.Conditions)
	}

	// Later: a Bound claim is not claimed again, and takes its Exec
	// Agent's address from the Node.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Host == nil || got.Status.Host.AgentAddress != "10.0.0.7:7443" {
		t.Errorf("host = %+v, want the Node's agent address 10.0.0.7:7443", got.Status.Host)
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

// releaseVMStub is a battery.Client whose ReleaseVM gives the next of
// answers. Every other method panics through the nil embedded Client.
type releaseVMStub struct {
	battery.Client
	answers []error
	leases  []string
}

func (s *releaseVMStub) ReleaseVM(_ context.Context, leaseID string) error {
	err := s.answers[len(s.leases)]
	s.leases = append(s.leases, leaseID)
	return err
}

// TestMicroVMClaimReconcilerReleasesADeletedClaim drives the controller
// through the deletion of a Bound claim: battery answers UNAVAILABLE while
// flintlockd has not confirmed the MicroVM's deletion, so the claim keeps
// its finalizer and is retried, and it goes away once battery has released
// the Lease. It checks the wiring; the behaviour is tested in package
// claim.
func TestMicroVMClaimReconcilerReleasesADeletedClaim(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	deleted := metav1.NewTime(now)
	cl := testClaim()
	cl.Finalizers = []string{batteryv1alpha1.ReleaseFinalizer}
	cl.DeletionTimestamp = &deleted
	cl.Status = batteryv1alpha1.MicroVMClaimStatus{Phase: batteryv1alpha1.MicroVMClaimBound, LeaseID: testClaimLease}
	s, c := newClaimFakeClient(t, cl)
	b := &releaseVMStub{answers: []error{battery.ErrUnavailable, nil}}
	r := &MicroVMClaimReconciler{
		Client:    c,
		Scheme:    s,
		APIReader: c,
		Battery:   b,
		Clock:     clock.NewFake(now),
		Backoff:   claim.Backoff{Min: time.Second, Max: time.Minute},
	}
	key := testClaimKey
	req := ctrl.Request{NamespacedName: key}
	got := &batteryv1alpha1.MicroVMClaim{}

	// First: battery has not released the Lease yet.
	res, err := r.Reconcile(ctx, req)
	if err != nil || res.RequeueAfter != time.Second {
		t.Fatalf("first Reconcile = %+v, %v; want a retry after 1s", res, err)
	}
	if err := c.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	if len(got.Finalizers) != 1 || got.Status.Phase != batteryv1alpha1.MicroVMClaimBound {
		t.Fatalf("after UNAVAILABLE: finalizers %v, phase %q; want the finalizer kept and Bound", got.Finalizers, got.Status.Phase)
	}
	if meta.IsStatusConditionTrue(got.Status.Conditions, batteryv1alpha1.ConditionSynced) {
		t.Errorf("Synced is true after UNAVAILABLE: %+v", got.Status.Conditions)
	}

	// Second: the Lease is released, and the claim goes away.
	if res, err := r.Reconcile(ctx, req); err != nil || res.RequeueAfter != 0 {
		t.Fatalf("second Reconcile = %+v, %v", res, err)
	}
	if err := c.Get(ctx, key, got); !apierrors.IsNotFound(err) {
		t.Errorf("Get = %v, want the claim gone", err)
	}
	if len(b.leases) != 2 || b.leases[0] != testClaimLease || b.leases[1] != testClaimLease {
		t.Errorf("ReleaseVM calls = %v, want two for lease-1", b.leases)
	}
}
