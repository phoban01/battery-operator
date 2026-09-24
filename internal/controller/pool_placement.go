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
	"maps"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/controller/inventory"
)

// PoolHosts is where the Pool Controller resolves a Pool's selector: the
// Hosts the Inventory Controller has given battery (03-pools.md,
// Placement). inventory.HostSet is the one the Operator runs with.
type PoolHosts interface {
	// Matching returns the names, sorted, of the Hosts whose Nodes r lists
	// with the selector's labels, or inventory.ErrNotSynced before the
	// Inventory Controller has published battery's Hosts.
	Matching(ctx context.Context, r client.Reader, selector map[string]string) ([]string, error)
	// Changed returns a channel that is closed the next time the Hosts
	// change.
	Changed() <-chan struct{}
	// Synced reports whether the Hosts have been published.
	Synced() bool
}

var _ PoolHosts = (*inventory.HostSet)(nil)

// poolPlacement resolves the Pool's spec.placement.nodeSelector to the
// Hosts it matches, for poolDeclaration and poolUpdate to send to battery
// as flintlock_hosts and for poolReadiness to report on.
//
// Until the Inventory Controller has published battery's Hosts, which it
// does in its first reconcile, the Operator cannot tell which Hosts a Pool
// may use: the chain stops without sending battery anything, and the
// publication brings every Pool back (hostsChangedSource).
type poolPlacement struct{}

func (poolPlacement) Reconcile(ctx context.Context, s *poolScope) (poolNext, error) {
	//= docs/requirements/03-pools.md#placement
	//# The Pool Controller SHALL set a Pool's `flintlock_hosts` in
	//# battery to the names of the Hosts whose Nodes match the Pool's
	//# `spec.placement.nodeSelector`.
	hosts, err := s.Hosts.Matching(ctx, s.Client, s.Pool.Spec.Placement.NodeSelector)
	switch {
	case errors.Is(err, inventory.ErrNotSynced):
		s.Log.V(1).Info("Waiting for battery's Hosts before placing Pool", "pool", poolRef(s.Pool).String())
		return poolStop, nil
	case err != nil:
		return poolStop, fmt.Errorf("resolving the selector of Pool %s: %w", poolRef(s.Pool), err)
	}
	s.hosts = hosts
	return poolContinue, nil
}

// hostsChangedSource is the Pool Controller's watch of the Hosts battery
// runs with: each time they change, and once when it starts if they are
// published already, it asks for a reconcile of every Pool (PO-011). It
// takes the channel before it looks, so a change in between is not lost.
func (r *PoolReconciler) hostsChangedSource(ctx context.Context, q workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
	go func() {
		for {
			changed := r.Hosts.Changed()
			if r.Hosts.Synced() {
				for _, req := range r.everyPool(ctx) {
					q.Add(req)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-changed:
			}
		}
	}()
	return nil
}

// everyPool is a reconcile request for each Pool.
func (r *PoolReconciler) everyPool(ctx context.Context) []reconcile.Request {
	var pools batteryv1alpha1.PoolList
	if err := r.List(ctx, &pools); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list Pools to place")
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(pools.Items))
	for i := range pools.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&pools.Items[i])})
	}
	return reqs
}

// everyPoolForNode maps a Node event to every Pool: a Node's labels
// decide which Pools' selectors match its Host.
func (r *PoolReconciler) everyPoolForNode(ctx context.Context, _ client.Object) []reconcile.Request {
	return r.everyPool(ctx)
}

// nodeLabelsChanged passes a Node's creation and deletion, and an update
// that changes its labels: the changes that can change which Pools'
// selectors match it. Whether it is a Host is the HostSet's to say.
var nodeLabelsChanged = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		o, okOld := e.ObjectOld.(*corev1.Node)
		n, okNew := e.ObjectNew.(*corev1.Node)
		if !okOld || !okNew {
			return true
		}
		return !maps.Equal(o.Labels, n.Labels)
	},
}
