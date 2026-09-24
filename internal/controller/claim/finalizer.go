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

	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/controller/claimscope"
)

// EnsureFinalizer puts the release finalizer on a claim.
//
// A claim it has just added the finalizer to stops the chain, so that the
// controller's patch writes the finalizer before any subreconciler calls
// battery. The patch is an update of the claim, which the controller
// watches, so the claim is reconciled again straight away and goes on to
// Bind.
//
// A claim that is being deleted also stops the chain: releasing its Lease
// is the release subreconciler's (CL-020, CL-021), and no deleted claim may
// be bound.
type EnsureFinalizer struct{}

var _ claimscope.Subreconciler = EnsureFinalizer{}

// Reconcile implements claimscope.Subreconciler.
func (EnsureFinalizer) Reconcile(_ context.Context, s *claimscope.Scope) (claimscope.Result, error) {
	if !s.Claim.DeletionTimestamp.IsZero() {
		return claimscope.Result{Stop: true}, nil
	}

	//= docs/requirements/02-claims.md#binding
	//# When a claim that has no lease id in its status is
	//# reconciled, the Claim Controller SHALL add the finalizer
	//# `battery.liquidmetal-x.dev/release` to the claim before it calls battery's
	//# `ClaimVM` for the claim's Pool.
	if controllerutil.AddFinalizer(s.Claim, batteryv1alpha1.ReleaseFinalizer) {
		s.Log.V(1).Info("Added the release finalizer to MicroVMClaim")
		return claimscope.Result{Stop: true}, nil
	}
	return claimscope.Result{}, nil
}
