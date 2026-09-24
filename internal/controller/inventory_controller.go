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
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/controller/inventory"
)

// inventoryRequest is the Inventory Controller's one reconcile: every Node
// event leads to it, because battery's Hosts are one list.
var inventoryRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: "inventory"}}

// InventoryReconciler is the Inventory Controller
// (docs/requirements/04-inventory.md): it decides which Nodes are Hosts and
// gives battery the list. It is a thin layer: it lists the Nodes, loads
// battery's configuration, builds an inventory.Scope and runs the chain of
// package inventory. The logic, and its requirement citations, are there.
type InventoryReconciler struct {
	client.Client
	// Store holds battery's configuration: its ConfigMap in production.
	Store inventory.Store
	// Restarter is the one hook that restarts battery (DP-006).
	Restarter inventory.Restarter
	// Hosts is where the Hosts battery runs with are published for the
	// Pool Controller.
	Hosts *inventory.HostSet
	// Pools lists the Pools battery holds, so that a restart that removes
	// a Host waits for them to drop it (IN-013): the battery client.
	Pools inventory.PoolLister
	// Options are the settle time, the restart window and the drain
	// timeout.
	Options inventory.Options
	// Clock defaults to clock.Real.
	Clock clock.Clock

	// state is what the controller remembers between reconciles.
	state *inventory.State
}

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// Reconcile runs the Inventory Controller's chain.
func (r *InventoryReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	nodes := &corev1.NodeList{}
	if err := r.List(ctx, nodes); err != nil {
		return ctrl.Result{}, err
	}
	config, err := r.Store.Load(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	if r.state == nil {
		r.state = inventory.NewState()
	}
	clk := r.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	s := &inventory.Scope{
		Nodes:     nodes.Items,
		Config:    config,
		State:     r.state,
		HostSet:   r.Hosts,
		Pools:     r.Pools,
		Store:     r.Store,
		Restarter: r.Restarter,
		Log:       logf.FromContext(ctx),
		Clock:     clk,
	}
	err = inventory.NewChain(r.Options).Run(ctx, s)
	return ctrl.Result{RequeueAfter: s.Result.RequeueAfter}, err
}

// SetupWithManager watches Nodes, for the changes that can change whether
// a Node is a Host or where its flintlockd is.
func (r *InventoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := r.Options.Validate(); err != nil {
		return err
	}
	if r.Store == nil || r.Restarter == nil || r.Hosts == nil || r.Pools == nil {
		return errors.New("the Inventory Controller needs a Store, a Restarter, a HostSet and battery's Pools")
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("inventory").
		Watches(&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
				return []reconcile.Request{inventoryRequest}
			}),
			builder.WithPredicates(nodeReportChanged)).
		// One reconcile at a time: it restarts battery, and State is not
		// shared.
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

// nodeReportChanged passes a Node's creation and deletion, and an update
// that changes whether it is schedulable or its Node report's readiness or
// flintlockd address.
var nodeReportChanged = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		o, okOld := e.ObjectOld.(*corev1.Node)
		n, okNew := e.ObjectNew.(*corev1.Node)
		if !okOld || !okNew {
			return true
		}
		return o.Spec.Unschedulable != n.Spec.Unschedulable ||
			o.DeletionTimestamp.IsZero() != n.DeletionTimestamp.IsZero() ||
			o.Annotations[inventory.AnnotationReady] != n.Annotations[inventory.AnnotationReady] ||
			o.Annotations[inventory.AnnotationFlintlockdAddress] != n.Annotations[inventory.AnnotationFlintlockdAddress]
	},
}
