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

package v1alpha1_test

import (
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
)

const (
	testNamespace = "default"
	testLeaseID   = "lease-0001"
)

// newClaim returns a valid claim named name, for the Pool "builders" and the
// ServiceAccount "runner".
func newClaim(name string) *batteryv1alpha1.MicroVMClaim {
	return &batteryv1alpha1.MicroVMClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: batteryv1alpha1.MicroVMClaimSpec{
			PoolRef:            batteryv1alpha1.PoolReference{Name: "builders"},
			ServiceAccountName: "runner",
		},
	}
}

// createClaim creates claim and deletes it when the test ends.
func createClaim(t *testing.T, claim *batteryv1alpha1.MicroVMClaim) {
	t.Helper()
	ctx := context.Background()
	if err := k8sClient.Create(ctx, claim); err != nil {
		t.Fatalf("creating claim: %v", err)
	}
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, claim); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("deleting claim: %v", err)
		}
	})
}

func getClaim(t *testing.T, name string) *batteryv1alpha1.MicroVMClaim {
	t.Helper()
	got := &batteryv1alpha1.MicroVMClaim{}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: name}, got); err != nil {
		t.Fatalf("getting claim: %v", err)
	}
	return got
}

// wantInvalid fails the test unless err is the API server refusing the object
// as invalid with a message that contains msg.
func wantInvalid(t *testing.T, err error, msg string) {
	t.Helper()
	if err == nil {
		t.Fatalf("the API server accepted the change, want it refused with %q", msg)
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("got error %v, want an Invalid error", err)
	}
	if !strings.Contains(err.Error(), msg) {
		t.Fatalf("got error %q, want it to mention %q", err, msg)
	}
}

func TestMicroVMClaimCarriesPoolRefName(t *testing.T) {
	//= docs/requirements/01-resources.md#microvmclaim
	//= type=test
	//# The `MicroVMClaim` resource SHALL carry `spec.poolRef.name`,
	//# which names a `Pool` in the claim's own namespace.

	createClaim(t, newClaim("poolref"))

	got := getClaim(t, "poolref")
	if got.Spec.PoolRef.Name != "builders" {
		t.Errorf("spec.poolRef.name = %q, want %q", got.Spec.PoolRef.Name, "builders")
	}

	noPool := newClaim("poolref-empty")
	noPool.Spec.PoolRef.Name = ""
	wantInvalid(t, k8sClient.Create(context.Background(), noPool), "spec.poolRef.name")
}

func TestMicroVMClaimCarriesServiceAccountName(t *testing.T) {
	//= docs/requirements/01-resources.md#microvmclaim
	//= type=test
	//# The `MicroVMClaim` resource SHALL carry
	//# `spec.serviceAccountName`, which names the ServiceAccount in the claim's
	//# namespace that may use the claimed MicroVM.

	createClaim(t, newClaim("sa"))

	got := getClaim(t, "sa")
	if got.Spec.ServiceAccountName != "runner" {
		t.Errorf("spec.serviceAccountName = %q, want %q", got.Spec.ServiceAccountName, "runner")
	}

	noSA := newClaim("sa-empty")
	noSA.Spec.ServiceAccountName = ""
	wantInvalid(t, k8sClient.Create(context.Background(), noSA), "spec.serviceAccountName")
}

func TestMicroVMClaimImmutableFields(t *testing.T) {
	//= docs/requirements/01-resources.md#microvmclaim
	//= type=test
	//# The CRDs SHALL reject an update that changes a claim's
	//# `spec.serviceAccountName` or `spec.poolRef`.

	createClaim(t, newClaim("immutable"))

	tests := []struct {
		name   string
		mutate func(*batteryv1alpha1.MicroVMClaim)
		want   string
	}{
		{
			name:   "serviceAccountName",
			mutate: func(c *batteryv1alpha1.MicroVMClaim) { c.Spec.ServiceAccountName = "someone-else" },
			want:   "spec.serviceAccountName is immutable",
		},
		{
			name:   "poolRef",
			mutate: func(c *batteryv1alpha1.MicroVMClaim) { c.Spec.PoolRef.Name = "other-pool" },
			want:   "spec.poolRef is immutable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			claim := getClaim(t, "immutable")
			tt.mutate(claim)
			wantInvalid(t, k8sClient.Update(ctx, claim), tt.want)

			// A merge patch reaches the same validation as an update.
			orig := getClaim(t, "immutable")
			patched := orig.DeepCopy()
			tt.mutate(patched)
			wantInvalid(t, k8sClient.Patch(ctx, patched, client.MergeFrom(orig)), tt.want)
		})
	}

	// The rest of the spec stays mutable: the Holder renews by writing
	// spec.renewTime.
	claim := getClaim(t, "immutable")
	now := metav1.NewMicroTime(time.Now())
	claim.Spec.RenewTime = &now
	if err := k8sClient.Update(context.Background(), claim); err != nil {
		t.Fatalf("updating spec.renewTime on a claim: %v", err)
	}
}

func TestMicroVMClaimCarriesRenewTime(t *testing.T) {
	//= docs/requirements/01-resources.md#microvmclaim
	//= type=test
	//# The `MicroVMClaim` resource SHALL carry `spec.renewTime`, which
	//# the Holder sets to renew the claim's Lease.

	createClaim(t, newClaim("renew"))
	ctx := context.Background()

	// The Holder renews with a merge patch on spec, and needs no access to
	// the status subresource.
	renew := time.Date(2026, 9, 22, 17, 4, 10, 123456000, time.UTC)
	orig := getClaim(t, "renew")
	patched := orig.DeepCopy()
	rt := metav1.NewMicroTime(renew)
	patched.Spec.RenewTime = &rt
	if err := k8sClient.Patch(ctx, patched, client.MergeFrom(orig)); err != nil {
		t.Fatalf("patching spec.renewTime: %v", err)
	}

	got := getClaim(t, "renew")
	if got.Spec.RenewTime == nil {
		t.Fatal("spec.renewTime is unset after the patch")
	}
	if !got.Spec.RenewTime.Time.Equal(renew) {
		t.Errorf("spec.renewTime = %v, want %v, to the microsecond", got.Spec.RenewTime.Time, renew)
	}
}

func TestMicroVMClaimStatus(t *testing.T) {
	//= docs/requirements/01-resources.md#microvmclaim
	//= type=test
	//# The `MicroVMClaim` resource SHALL have a status subresource that
	//# carries the phase, one of `Pending`, `Bound`, `Expired` and `Released`,
	//# the lease id battery chose, the MicroVM's uid, the Host's node name, the
	//# Exec Agent's address, the time the claim was bound, the time its Lease
	//# expires, and the condition `Bound`.

	createClaim(t, newClaim("status"))
	ctx := context.Background()

	bound := metav1.NewTime(time.Date(2026, 9, 22, 17, 3, 58, 0, time.UTC))
	expires := metav1.NewTime(time.Date(2026, 9, 22, 17, 4, 40, 0, time.UTC))
	claim := getClaim(t, "status")
	claim.Status = batteryv1alpha1.MicroVMClaimStatus{
		Phase:          batteryv1alpha1.MicroVMClaimBound,
		LeaseID:        testLeaseID,
		MicroVM:        &batteryv1alpha1.MicroVMReference{UID: "01JB7ZK4QH3M8V6R2N5T9X0C1D"},
		Host:           &batteryv1alpha1.HostReference{NodeName: "node-a", AgentAddress: "10.0.1.23:10270"},
		BoundTime:      &bound,
		LeaseExpiresAt: &expires,
	}
	meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
		Type:   batteryv1alpha1.ConditionBound,
		Status: metav1.ConditionTrue,
		Reason: batteryv1alpha1.ReasonBound,
	})
	if err := k8sClient.Status().Update(ctx, claim); err != nil {
		t.Fatalf("updating status: %v", err)
	}

	got := getClaim(t, "status").Status
	if got.Phase != batteryv1alpha1.MicroVMClaimBound {
		t.Errorf("status.phase = %q, want Bound", got.Phase)
	}
	if got.LeaseID != testLeaseID {
		t.Errorf("status.leaseID = %q, want %q", got.LeaseID, testLeaseID)
	}
	if got.MicroVM == nil || got.MicroVM.UID != "01JB7ZK4QH3M8V6R2N5T9X0C1D" {
		t.Errorf("status.microVM = %+v, want uid 01JB7ZK4QH3M8V6R2N5T9X0C1D", got.MicroVM)
	}
	if got.Host == nil || got.Host.NodeName != "node-a" || got.Host.AgentAddress != "10.0.1.23:10270" {
		t.Errorf("status.host = %+v, want node-a at 10.0.1.23:10270", got.Host)
	}
	if got.BoundTime == nil || !got.BoundTime.Equal(&bound) {
		t.Errorf("status.boundTime = %v, want %v", got.BoundTime, bound)
	}
	if got.LeaseExpiresAt == nil || !got.LeaseExpiresAt.Equal(&expires) {
		t.Errorf("status.leaseExpiresAt = %v, want %v", got.LeaseExpiresAt, expires)
	}
	if !meta.IsStatusConditionTrue(got.Conditions, batteryv1alpha1.ConditionBound) {
		t.Errorf("status.conditions = %+v, want Bound True", got.Conditions)
	}

	// status is a subresource: a write to the main resource leaves it alone.
	claim = getClaim(t, "status")
	claim.Status.Phase = batteryv1alpha1.MicroVMClaimReleased
	claim.Status.LeaseID = "someone-elses"
	if err := k8sClient.Update(ctx, claim); err != nil {
		t.Fatalf("updating claim: %v", err)
	}
	if got := getClaim(t, "status").Status; got.Phase != batteryv1alpha1.MicroVMClaimBound || got.LeaseID != testLeaseID {
		t.Errorf("an update to the main resource changed status to phase %q, lease %q", got.Phase, got.LeaseID)
	}

	// Every phase is accepted, and nothing else.
	for _, phase := range []batteryv1alpha1.MicroVMClaimPhase{
		batteryv1alpha1.MicroVMClaimPending,
		batteryv1alpha1.MicroVMClaimBound,
		batteryv1alpha1.MicroVMClaimExpired,
		batteryv1alpha1.MicroVMClaimReleased,
	} {
		claim = getClaim(t, "status")
		claim.Status.Phase = phase
		if err := k8sClient.Status().Update(ctx, claim); err != nil {
			t.Errorf("setting status.phase to %s: %v", phase, err)
		}
	}
	claim = getClaim(t, "status")
	claim.Status.Phase = "Lost"
	wantInvalid(t, k8sClient.Status().Update(ctx, claim), "status.phase")
}
