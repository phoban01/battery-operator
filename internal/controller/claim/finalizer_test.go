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

package claim

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/controller/claimscope"
)

//= docs/requirements/02-claims.md#binding
//= type=test
//# When a claim that has no lease id in its status is
//# reconciled, the Claim Controller SHALL add the finalizer
//# `battery.liquidmetal-x.dev/release` to the claim before it calls battery's
//# `ClaimVM` for the claim's Pool.

// TestEnsureFinalizerAddsItAndStops covers CL-001: a new claim gets the
// finalizer, and the chain stops so that it is written before Bind runs.
func TestEnsureFinalizerAddsItAndStops(t *testing.T) {
	ctx := context.Background()
	b := answers(nil, nil)
	s, c := newScope(t, aClaim(), b)

	if err := claimscope.Run(ctx, s, EnsureFinalizer{}, Bind{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !s.Result.Stop {
		t.Error("the chain did not stop after the finalizer was added")
	}
	if n := len(b.claimCalls()); n != 0 {
		t.Errorf("ClaimVM was called %d times before the finalizer was written", n)
	}
	if err := s.Patch(ctx); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	got := &batteryv1alpha1.MicroVMClaim{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(s.Claim), got); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(got, batteryv1alpha1.ReleaseFinalizer) {
		t.Errorf("finalizers = %v, want %s", got.Finalizers, batteryv1alpha1.ReleaseFinalizer)
	}
}

// TestEnsureFinalizerContinuesWhenPresent: a claim that has the finalizer
// goes on to the next subreconciler.
func TestEnsureFinalizerContinuesWhenPresent(t *testing.T) {
	s, _ := newScope(t, aClaim(batteryv1alpha1.ReleaseFinalizer), answers(nil, nil))
	res, err := EnsureFinalizer{}.Reconcile(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if res != (claimscope.Result{}) {
		t.Errorf("result = %+v, want the chain to continue", res)
	}
}

// TestEnsureFinalizerStopsForADeletedClaim: a claim being deleted is never
// bound, and gets no finalizer.
func TestEnsureFinalizerStopsForADeletedClaim(t *testing.T) {
	cl := aClaim("example.com/other")
	now := metav1.NewTime(start)
	cl.DeletionTimestamp = &now
	s, _ := newScope(t, cl, answers(nil, nil))

	res, err := EnsureFinalizer{}.Reconcile(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stop {
		t.Error("the chain did not stop for a deleted claim")
	}
	if controllerutil.ContainsFinalizer(s.Claim, batteryv1alpha1.ReleaseFinalizer) {
		t.Error("a deleted claim got the release finalizer")
	}
}
