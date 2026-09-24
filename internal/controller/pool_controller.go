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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
)

// PoolReconciler is the Pool Controller (docs/requirements/03-pools.md): it
// declares each Pool to battery and mirrors battery's view of it into the
// Pool's status. It is a thin layer: it builds a poolScope,
// runs poolChain and patches the Pool once. The logic, and its
// requirement citations, are in the subreconcilers.
type PoolReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Battery is the Operator's battery client.
	Battery battery.Client
	// Clock stamps condition transitions; nil is the wall clock.
	Clock clock.Clock
	// Hosts are the Hosts the Inventory Controller has given battery, which
	// each Pool's selector is resolved against (PO-010): the Operator's
	// inventory.HostSet.
	Hosts PoolHosts
	// Events is the Operator's subscription to battery's Events stream: it
	// asks for a reconcile of each Pool an event names, and of every Pool
	// at its resync interval while it is down (PO-023, PO-024). Nil
	// watches Pools only.
	Events *PoolEvents
}

// +kubebuilder:rbac:groups=battery.liquidmetal-x.dev,resources=pools,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=battery.liquidmetal-x.dev,resources=pools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=battery.liquidmetal-x.dev,resources=pools/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// Reconcile runs the Pool Controller's chain for one Pool.
func (r *PoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	pool := &batteryv1alpha1.Pool{}
	if err := r.Get(ctx, req.NamespacedName, pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	clk := r.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	s := newPoolScope(pool, r.Client, r.Battery, r.Hosts, logf.FromContext(ctx), clk)
	chainErr := runPoolChain(ctx, s, poolChain())
	patchErr := s.patch(ctx)
	return s.Result, errors.Join(chainErr, patchErr)
}

// SetupWithManager sets up the controller with the Manager. Besides the
// Pools, it watches the Hosts battery runs with and the Nodes' labels,
// which together decide the Hosts each Pool's selector matches (PO-011).
func (r *PoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Hosts == nil {
		return errors.New("the Pool Controller needs the Hosts to place Pools on")
	}
	b := ctrl.NewControllerManagedBy(mgr).
		For(&batteryv1alpha1.Pool{}).
		Named("pool").
		WatchesRawSource(source.Func(r.hostsChangedSource)).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.everyPoolForNode),
			builder.WithPredicates(nodeLabelsChanged))
	if r.Events != nil {
		if err := mgr.Add(r.Events); err != nil {
			return err
		}
		b = b.WatchesRawSource(source.Channel(r.Events.Requests(),
			&handler.TypedEnqueueRequestForObject[*batteryv1alpha1.Pool]{}))
	}
	return b.Complete(r)
}
