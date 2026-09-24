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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/phoban01/battery-operator/internal/batterysidecar"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/controller/inventory"
)

// inventoryRequest is the Inventory Controller's one reconcile: every Node
// event leads to it, because battery's Hosts are one list.
var inventoryRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: "inventory"}}

// InventoryReconciler is the Inventory Controller
// (docs/requirements/04-inventory.md): it decides which Nodes are Hosts and
// gives battery the list, and restarts battery when its client certificate
// is renewed (DP-007). It is a thin layer: it lists the Nodes, loads
// battery's configuration and reads its client certificate's Secret,
// builds an inventory.Scope and runs the chain of package inventory. The
// logic, and its requirement citations, are there.
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

	// ClientSecret names the Secret holding battery's client certificate.
	ClientSecret client.ObjectKey
	// Secrets reads that Secret. SetupWithManager sets it to a cache of
	// that one Secret.
	Secrets client.Reader

	// state is what the controller remembers between reconciles.
	state *inventory.State
}

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

//= docs/requirements/06-deployment.md#battery-sidecar
//# When the client certificate in the Secret of DP-005 changes,
//# the Operator SHALL restart battery through the mechanism of DP-006

// The Operator reads and watches battery's client certificate's Secret, by
// name, in its own namespace (DP-021); the Manifests name it
// battery-flintlockd-client.
// +kubebuilder:rbac:groups="",namespace=system,resources=secrets,verbs=get;list;watch,resourceNames=battery-flintlockd-client

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
	cert, err := r.clientCertificate(ctx)
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
		Nodes:             nodes.Items,
		Config:            config,
		ClientCertificate: cert,
		State:             r.state,
		HostSet:           r.Hosts,
		Pools:             r.Pools,
		Store:             r.Store,
		Restarter:         r.Restarter,
		Log:               logf.FromContext(ctx),
		Clock:             clk,
	}
	err = inventory.NewChain(r.Options).Run(ctx, s)
	return ctrl.Result{RequeueAfter: s.Result.RequeueAfter}, err
}

// clientCertificate reads battery's client certificate from its Secret:
// nil when the Secret, or its certificate, is not there.
func (r *InventoryReconciler) clientCertificate(ctx context.Context) ([]byte, error) {
	secret := &corev1.Secret{}
	if err := r.Secrets.Get(ctx, r.ClientSecret, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading Secret %s: %w", r.ClientSecret, err)
	}
	cert := secret.Data[batterysidecar.ClientCertKey]
	if len(cert) == 0 {
		return nil, nil
	}
	return cert, nil
}

// SetupWithManager watches Nodes, for the changes that can change whether
// a Node is a Host or where its flintlockd is, and battery's client
// certificate's Secret, through a cache of that one Secret.
func (r *InventoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := r.Options.Validate(); err != nil {
		return err
	}
	if r.Store == nil || r.Restarter == nil || r.Hosts == nil || r.Pools == nil {
		return errors.New("the Inventory Controller needs a Store, a Restarter, a HostSet and battery's Pools")
	}
	if r.ClientSecret.Name == "" || r.ClientSecret.Namespace == "" {
		return errors.New("the Inventory Controller needs the name and namespace of battery's client certificate's Secret")
	}
	secrets, err := singleObjectCache(mgr, r.ClientSecret.Namespace, r.ClientSecret.Name)
	if err != nil {
		return err
	}
	r.Secrets = secrets
	enqueue := func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{inventoryRequest}
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("inventory").
		Watches(&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(enqueue),
			builder.WithPredicates(nodeReportChanged)).
		WatchesRawSource(source.Kind(secrets, &corev1.Secret{},
			handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, s *corev1.Secret) []reconcile.Request {
				return enqueue(ctx, s)
			}))).
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
