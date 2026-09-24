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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/controller/claim"
	"github.com/phoban01/battery-operator/internal/controller/claimscope"
)

// MicroVMClaimReconciler is the Claim Controller
// (docs/requirements/02-claims.md). It is a thin layer: it builds a
// claimscope.Scope for the claim, runs the subreconcilers of package claim
// in order, and patches the claim once at the end.
type MicroVMClaimReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIReader reads claims from the API server, bypassing the cache,
	// where a stale claim could make battery lease a second MicroVM.
	APIReader client.Reader
	// Battery is the Operator's battery client.
	Battery battery.Client
	// Clock defaults to clock.Real.
	Clock clock.Clock
	// Backoff is how a Pending claim is retried; zero is
	// claim.DefaultBackoff.
	Backoff claim.Backoff
}

// +kubebuilder:rbac:groups=battery.liquidmetal-x.dev,resources=microvmclaims,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=battery.liquidmetal-x.dev,resources=microvmclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=battery.liquidmetal-x.dev,resources=microvmclaims/finalizers,verbs=update

// Reconcile runs the claim's subreconcilers and writes what they changed.
func (r *MicroVMClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	c := &batteryv1alpha1.MicroVMClaim{}
	if err := r.Get(ctx, req.NamespacedName, c); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	s := claimscope.New(c)
	s.Client = r.Client
	s.APIReader = r.APIReader
	s.Battery = r.Battery
	s.Log = logf.FromContext(ctx)
	s.Clock = r.Clock
	if s.Clock == nil {
		s.Clock = clock.Real{}
	}

	err := r.chain().Run(ctx, s)
	// The patch runs even when the chain failed, so that what the chain
	// did before the failure is not lost.
	if perr := s.Patch(ctx); perr != nil {
		err = errors.Join(err, perr)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: s.Result.RequeueAfter}, nil
}

// chain is the Claim Controller's subreconcilers, in the order they run.
func (r *MicroVMClaimReconciler) chain() claimscope.Chain {
	backoff := r.Backoff
	if backoff == (claim.Backoff{}) {
		backoff = claim.DefaultBackoff
	}
	return claimscope.Chain{
		Steps: []claimscope.Subreconciler{
			claim.EnsureFinalizer{},
			claim.Bind{},
			claim.Pending{Backoff: backoff},
		},
		Finally: []claimscope.Subreconciler{
			claim.Synced{},
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *MicroVMClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&batteryv1alpha1.MicroVMClaim{}).
		Named("microvmclaim").
		Complete(r)
}
