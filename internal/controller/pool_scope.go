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

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
)

// The Pool Controller's scope and subreconciler interface are local, in
// the shape of the shared Scope[T] and SubReconciler[T] that #58 brings
// (CLAUDE.md, "Controller structure"). Moving onto those changes no logic:
// poolScope becomes Scope[*Pool] with the Pool-specific fields beside it,
// and each subreconciler keeps its Reconcile.

// poolScope is one reconcile of one Pool. Subreconcilers change the Pool
// and the result here and never write to the API server; patch writes the
// Pool once, at the end.
type poolScope struct {
	// Pool is the Pool as the subreconcilers leave it.
	Pool *batteryv1alpha1.Pool
	// fetched is the Pool as it was read, the base of the patch.
	fetched *batteryv1alpha1.Pool

	Client  client.Client
	Battery battery.Client
	Log     logr.Logger
	Clock   clock.Clock

	// Result is the result so far.
	Result ctrl.Result

	// refusal is battery's refusal of the Pool's spec in this reconcile,
	// from CreatePool or UpdatePool, wrapping battery.ErrInvalid.
	refusal error

	// held is the Pool as battery last answered for it in this reconcile,
	// from GetPool, CreatePool or UpdatePool; nil while battery does not
	// hold the Pool. The status subreconcilers read battery's PoolStatus
	// from it (PO-020).
	held *battery.Pool
}

func newPoolScope(pool *batteryv1alpha1.Pool, c client.Client, b battery.Client, log logr.Logger, clk clock.Clock) *poolScope {
	return &poolScope{
		Pool:    pool,
		fetched: pool.DeepCopy(),
		Client:  c,
		Battery: b,
		Log:     log,
		Clock:   clk,
	}
}

// setCondition sets one of the Pool's conditions for its current
// generation, stamping a transition with the scope's clock.
func (s *poolScope) setCondition(conditionType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&s.Pool.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: s.Pool.Generation,
		LastTransitionTime: metav1.NewTime(s.Clock.Now()),
	})
}

// patch writes what the subreconcilers changed: the status first, then
// the metadata (the finalizer), because removing the last finalizer lets
// the API server delete the Pool. A Pool already gone is not an error.
func (s *poolScope) patch(ctx context.Context) error {
	base := client.MergeFrom(s.fetched)
	var errs []error
	if !equality.Semantic.DeepEqual(s.fetched.Status, s.Pool.Status) {
		if err := s.Client.Status().Patch(ctx, s.Pool.DeepCopy(), base); client.IgnoreNotFound(err) != nil {
			errs = append(errs, err)
		}
	}
	if !equality.Semantic.DeepEqual(s.fetched.ObjectMeta, s.Pool.ObjectMeta) {
		if err := s.Client.Patch(ctx, s.Pool.DeepCopy(), base); client.IgnoreNotFound(err) != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// poolNext says whether the chain goes on after a subreconciler.
type poolNext int

const (
	// poolContinue runs the next subreconciler.
	poolContinue poolNext = iota
	// poolStop ends the chain; the scope is still patched.
	poolStop
)

// poolSubreconciler is one concern of the Pool Controller. It reads and
// changes the scope; an error ends the chain and is returned from the
// reconcile, so the workqueue retries it with backoff.
type poolSubreconciler interface {
	Reconcile(ctx context.Context, s *poolScope) (poolNext, error)
}

// runPoolChain runs the subreconcilers in order until one stops the chain
// or fails.
func runPoolChain(ctx context.Context, s *poolScope, chain []poolSubreconciler) error {
	for _, sub := range chain {
		next, err := sub.Reconcile(ctx, s)
		if err != nil {
			return err
		}
		if next == poolStop {
			return nil
		}
	}
	return nil
}
