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

package reconcile

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/phoban01/battery-operator/internal/clock"
)

// EnsureFinalizer puts the finalizer Name on the scope's object. When it
// has just added it, it stops the chain, so that the controller's write
// stores the finalizer before any later step acts on the object outside
// the cluster; the write is a change to the object, which brings it back
// for the next reconcile. An object that already has the finalizer goes
// on.
//
// This is the step the Claim and Pool Controllers each have their own of
// (claim.EnsureFinalizer, poolFinalizer). It leaves an object that is being
// deleted to the chain's deletion step, which comes before it.
type EnsureFinalizer[S Scoped[T], T client.Object] struct {
	// Name is the finalizer.
	Name string
}

// Reconcile implements SubReconciler.
func (f EnsureFinalizer[S, T]) Reconcile(_ context.Context, s S) (Result, error) {
	b := s.Base()
	if controllerutil.AddFinalizer(b.Object, f.Name) {
		b.Log.V(1).Info("Added finalizer", "finalizer", f.Name)
		return Result{Stop: true}, nil
	}
	return Result{}, nil
}

// SetCondition sets the condition conditionType in conditions, the
// conditions of obj, for obj's current generation, stamping a transition
// with clk's time, and reports whether conditions changed. It is the Pool
// Controller's setCondition, made generic.
func SetCondition(conditions *[]metav1.Condition, obj client.Object, clk clock.Clock,
	conditionType string, status metav1.ConditionStatus, reason, message string) bool {
	return meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: obj.GetGeneration(),
		LastTransitionTime: metav1.NewTime(clk.Now()),
	})
}
