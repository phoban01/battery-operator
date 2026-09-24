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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

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
	// Backoff is how a Pending claim, and a release that battery did not
	// answer, are retried; zero is
	// claim.DefaultBackoff.
	Backoff claim.Backoff
	// ConcurrentReconciles is how many claims are reconciled at once; one
	// fewer may call ClaimVM at a time (CL-018). Zero is
	// DefaultClaimConcurrentReconciles; SetupWithManager refuses fewer
	// than two.
	ConcurrentReconciles int

	// slots bounds the ClaimVM calls in flight; SetupWithManager sets it.
	slots *claim.BindSlots
	// deleted holds the MicroVMs battery's Events stream reported deleted
	// (CL-013); SetupWithManager sets it.
	deleted *claim.DeletedVMs
}

// DefaultClaimConcurrentReconciles is the default of the Operator's flag
// --claim-concurrent-reconciles.
const DefaultClaimConcurrentReconciles = 4

// eventResubscribeWait is how long the event watcher waits before it
// subscribes to battery's Events again after the stream ended.
const eventResubscribeWait = 2 * time.Second

// +kubebuilder:rbac:groups=battery.liquidmetal-x.dev,resources=microvmclaims,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=battery.liquidmetal-x.dev,resources=microvmclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=battery.liquidmetal-x.dev,resources=microvmclaims/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

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
			claim.Release{Backoff: backoff},
			claim.EnsureFinalizer{},
			claim.Bind{Slots: r.slots},
			claim.Pending{Backoff: backoff},
			// A Bound claim: an event from battery first, then a pending
			// renewal, and only then the expiry check, which waits for
			// any pending renewal (#85). They stop the chain only once
			// they have expired the claim.
			claim.ExpireDeleted{Deleted: r.deleted},
			claim.Renew{Backoff: backoff},
			claim.CheckExpiry{Backoff: backoff},
			// Last, so that a Node it cannot read never holds up a
			// renewal (CL-018).
			claim.AgentAddress{},
		},
		Finally: []claimscope.Subreconciler{
			claim.Synced{},
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
// Besides its claims, it watches Nodes: a change to the Exec Agent's
// address in a Node's report reconciles every claim bound on that Node
// (claim.AgentAddress). It also adds the watcher of battery's Events
// stream (CL-013), whose claims come in through a channel source.
func (r *MicroVMClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	n := r.ConcurrentReconciles
	if n == 0 {
		n = DefaultClaimConcurrentReconciles
	}
	slots, err := claim.NewBindSlots(n)
	if err != nil {
		return err
	}
	r.slots = slots
	clk := r.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	r.deleted = &claim.DeletedVMs{Clock: clk}

	if err := mgr.GetFieldIndexer().IndexField(context.Background(),
		&batteryv1alpha1.MicroVMClaim{}, claim.NodeNameField, claim.NodeName); err != nil {
		return fmt.Errorf("indexing MicroVMClaims by node name: %w", err)
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &batteryv1alpha1.MicroVMClaim{},
		claim.MicroVMUIDIndex, func(o client.Object) []string {
			return claim.MicroVMUID(o.(*batteryv1alpha1.MicroVMClaim))
		}); err != nil {
		return fmt.Errorf("indexing MicroVMClaims by MicroVM uid: %w", err)
	}
	deletions := make(chan event.GenericEvent)
	if err := mgr.Add(&claimEvents{
		Battery: r.Battery,
		Reader:  mgr.GetClient(),
		Deleted: r.deleted,
		Out:     deletions,
		Retry:   eventResubscribeWait,
		Clock:   clk,
		Log:     mgr.GetLogger().WithName("microvmclaim-events"),
	}); err != nil {
		return err
	}

	//= docs/requirements/02-claims.md#renewal
	//# While battery has not yet answered `ClaimVM` calls for other
	//# claims, the Claim Controller SHALL still reconcile a Bound claim that has
	//# a pending renewal.
	return ctrl.NewControllerManagedBy(mgr).
		For(&batteryv1alpha1.MicroVMClaim{}).
		Watches(&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(claim.ClaimsOnNode(mgr.GetClient())),
			builder.WithPredicates(claim.AgentAddressChanged())).
		WatchesRawSource(source.Channel(deletions, &handler.EnqueueRequestForObject{})).
		WithOptions(controller.Options{MaxConcurrentReconciles: n}).
		Named("microvmclaim").
		Complete(r)
}
