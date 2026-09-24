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

package claimscope

import (
	"context"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
)

func result(r Result, err error) Func {
	return func(context.Context, *Scope) (Result, error) { return r, err }
}

func TestRunFoldsResultsAndStops(t *testing.T) {
	ran := false
	never := Func(func(context.Context, *Scope) (Result, error) {
		ran = true
		return Result{}, nil
	})
	finallyRan := false
	finally := Func(func(context.Context, *Scope) (Result, error) {
		finallyRan = true
		return Result{Stop: true, RequeueAfter: time.Minute}, nil
	})
	s := New(&batteryv1alpha1.MicroVMClaim{})
	err := Chain{
		Steps: []Subreconciler{
			result(Result{RequeueAfter: 5 * time.Second}, nil),
			result(Result{RequeueAfter: 2 * time.Second}, nil),
			result(Result{}, nil),
			result(Result{Stop: true, RequeueAfter: 3 * time.Second}, nil),
			never,
		},
		Finally: []Subreconciler{finally},
	}.Run(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Error("a step ran after the chain stopped")
	}
	if !finallyRan {
		t.Error("the Finally subreconciler did not run after the chain stopped")
	}
	if want := (Result{Stop: true, RequeueAfter: 2 * time.Second}); s.Result != want {
		t.Errorf("Result = %+v, want %+v", s.Result, want)
	}
}

func TestRunStopsOnErrorAndStillRunsFinally(t *testing.T) {
	boom, bang := errors.New("boom"), errors.New("bang")
	ran := false
	never := Func(func(context.Context, *Scope) (Result, error) {
		ran = true
		return Result{}, nil
	})
	finallyRuns := 0
	finally := Func(func(context.Context, *Scope) (Result, error) {
		finallyRuns++
		return Result{}, bang
	})
	s := New(&batteryv1alpha1.MicroVMClaim{})
	err := Chain{
		Steps:   []Subreconciler{result(Result{}, boom), never},
		Finally: []Subreconciler{finally, finally},
	}.Run(context.Background(), s)
	if !errors.Is(err, boom) || !errors.Is(err, bang) {
		t.Errorf("err = %v, want boom and bang", err)
	}
	if ran {
		t.Error("a step ran after one failed")
	}
	if finallyRuns != 2 {
		t.Errorf("Finally subreconcilers ran %d times, want each of the 2 to run", finallyRuns)
	}
}

// newClient is a fake client holding claim, with the status subresource,
// that counts the patches it is sent.
func newClient(t *testing.T, claim *batteryv1alpha1.MicroVMClaim, patches, statusPatches *int) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := batteryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(claim).
		WithStatusSubresource(&batteryv1alpha1.MicroVMClaim{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, o ...client.PatchOption) error {
				*patches++
				return c.Patch(ctx, obj, p, o...)
			},
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, p client.Patch, o ...client.SubResourcePatchOption) error {
				*statusPatches++
				return c.SubResource(sub).Patch(ctx, obj, p, o...)
			},
		}).
		Build()
}

func scopeOf(t *testing.T, c client.Client) *Scope {
	t.Helper()
	got := &batteryv1alpha1.MicroVMClaim{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ci", Name: "runner-1"}, got); err != nil {
		t.Fatal(err)
	}
	s := New(got)
	s.Client = c
	return s
}

func aClaim() *batteryv1alpha1.MicroVMClaim {
	return &batteryv1alpha1.MicroVMClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "runner-1", Namespace: "ci"},
		Spec: batteryv1alpha1.MicroVMClaimSpec{
			PoolRef:            batteryv1alpha1.PoolReference{Name: "small"},
			ServiceAccountName: "runner",
		},
	}
}

func TestPatchWritesNothingUnchanged(t *testing.T) {
	var patches, statusPatches int
	s := scopeOf(t, newClient(t, aClaim(), &patches, &statusPatches))
	if err := s.Patch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if patches+statusPatches != 0 {
		t.Errorf("sent %d patches and %d status patches for an unchanged claim", patches, statusPatches)
	}
}

func TestPatchWritesMetadataAndStatus(t *testing.T) {
	ctx := context.Background()
	var patches, statusPatches int
	c := newClient(t, aClaim(), &patches, &statusPatches)
	s := scopeOf(t, c)
	s.Claim.Finalizers = append(s.Claim.Finalizers, batteryv1alpha1.ReleaseFinalizer)
	s.Claim.Status.Phase = batteryv1alpha1.MicroVMClaimBound
	s.Claim.Status.LeaseID = "lease-1"

	if err := s.Patch(ctx); err != nil {
		t.Fatal(err)
	}
	if patches != 1 || statusPatches != 1 {
		t.Errorf("sent %d patches and %d status patches, want one of each", patches, statusPatches)
	}
	got := &batteryv1alpha1.MicroVMClaim{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(s.Claim), got); err != nil {
		t.Fatal(err)
	}
	if len(got.Finalizers) != 1 || got.Status.LeaseID != "lease-1" || got.Status.Phase != batteryv1alpha1.MicroVMClaimBound {
		t.Errorf("stored claim = %+v %+v, want the finalizer and the Bound status", got.Finalizers, got.Status)
	}
}

func TestPatchWritesStatusAlone(t *testing.T) {
	var patches, statusPatches int
	s := scopeOf(t, newClient(t, aClaim(), &patches, &statusPatches))
	s.Claim.Status.Phase = batteryv1alpha1.MicroVMClaimPending
	if err := s.Patch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if patches != 0 || statusPatches != 1 {
		t.Errorf("sent %d patches and %d status patches, want only a status patch", patches, statusPatches)
	}
}

func TestPatchRefusesAStaleFinalizerList(t *testing.T) {
	ctx := context.Background()
	var patches, statusPatches int
	c := newClient(t, aClaim(), &patches, &statusPatches)
	s := scopeOf(t, c)

	// Another controller adds its finalizer after the scope was built.
	other := &batteryv1alpha1.MicroVMClaim{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(s.Claim), other); err != nil {
		t.Fatal(err)
	}
	other.Finalizers = []string{"example.com/other"}
	if err := c.Update(ctx, other); err != nil {
		t.Fatal(err)
	}

	s.Claim.Finalizers = append(s.Claim.Finalizers, batteryv1alpha1.ReleaseFinalizer)
	if err := s.Patch(ctx); err == nil {
		t.Error("Patch replaced a finalizer list it had not seen")
	}
}

// aDeletedClaim is aClaim with the release finalizer, deleted.
func aDeletedClaim() *batteryv1alpha1.MicroVMClaim {
	cl := aClaim()
	cl.Finalizers = []string{batteryv1alpha1.ReleaseFinalizer}
	deleted := metav1.NewTime(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC))
	cl.DeletionTimestamp = &deleted
	return cl
}

// TestPatchOfTheLastFinalizerWritesNoStatus: removing the last finalizer
// of a deleted claim deletes it, so there is no status left to write.
func TestPatchOfTheLastFinalizerWritesNoStatus(t *testing.T) {
	ctx := context.Background()
	var patches, statusPatches int
	c := newClient(t, aDeletedClaim(), &patches, &statusPatches)
	s := scopeOf(t, c)
	s.Claim.Finalizers = nil
	s.Claim.Status.Phase = batteryv1alpha1.MicroVMClaimPending

	if err := s.Patch(ctx); err != nil {
		t.Fatal(err)
	}
	if patches != 1 || statusPatches != 0 {
		t.Errorf("sent %d patches and %d status patches, want only the metadata patch", patches, statusPatches)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(s.Claim), &batteryv1alpha1.MicroVMClaim{}); !apierrors.IsNotFound(err) {
		t.Errorf("Get = %v, want the claim gone", err)
	}
	if s.Claim.Status.Phase != batteryv1alpha1.MicroVMClaimPending {
		t.Errorf("phase = %q, want the chain's status kept on the scope", s.Claim.Status.Phase)
	}
}

// TestPatchOfTheLastFinalizerOfAGoneClaim: a claim that went away before
// the patch removed its last finalizer is not an error.
func TestPatchOfTheLastFinalizerOfAGoneClaim(t *testing.T) {
	ctx := context.Background()
	var patches, statusPatches int
	c := newClient(t, aDeletedClaim(), &patches, &statusPatches)
	s := scopeOf(t, c)

	// An earlier reconcile removed the finalizer, and the claim is gone.
	other := &batteryv1alpha1.MicroVMClaim{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(s.Claim), other); err != nil {
		t.Fatal(err)
	}
	other.Finalizers = nil
	if err := c.Update(ctx, other); err != nil {
		t.Fatal(err)
	}

	s.Claim.Finalizers = nil
	if err := s.Patch(ctx); err != nil {
		t.Errorf("Patch = %v, want nil for a claim already gone", err)
	}
	if statusPatches != 0 {
		t.Errorf("sent %d status patches, want none", statusPatches)
	}
}
