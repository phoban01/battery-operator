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

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
)

// poolEventsBuffer is how many Pools may wait to be enqueued.
const poolEventsBuffer = 256

// poolEvents is the Pool Controller's side of BatteryEvents. It turns each
// event into a reconcile of the Pool it names (PO-023), and each resync,
// which BatteryEvents asks for while it is not subscribed, into a
// reconcile of every Pool (PO-024). The Pool Controller watches its
// Requests through a source.Channel.
type poolEvents struct {
	// Pools lists the Pools to refresh on a resync.
	Pools client.Reader
	// Log is the logger; the zero Logger discards.
	Log logr.Logger

	requests chan event.TypedGenericEvent[*batteryv1alpha1.Pool]
}

var _ eventConsumer = (*poolEvents)(nil)

// newPoolEvents returns a poolEvents with its Requests channel.
func newPoolEvents(pools client.Reader, log logr.Logger) *poolEvents {
	return &poolEvents{
		Pools:    pools,
		Log:      log,
		requests: make(chan event.TypedGenericEvent[*batteryv1alpha1.Pool], poolEventsBuffer),
	}
}

// Requests is the channel of Pools to reconcile, for source.Channel.
func (e *poolEvents) Requests() <-chan event.TypedGenericEvent[*batteryv1alpha1.Pool] {
	return e.requests
}

// subscribed implements eventConsumer: a Pool needs nothing before the
// stream is read, since the stream's own events refresh it.
func (e *poolEvents) subscribed(context.Context) error { return nil }

// handle implements eventConsumer: it enqueues the Pool the event names.
func (e *poolEvents) handle(ctx context.Context, ev *battery.Event) {
	//= docs/requirements/03-pools.md#pool-status
	//# When battery's `Events` stream reports an event for a Pool, the
	//# Pool Controller SHALL refresh that Pool's status.
	if ev.Pool.Name == "" {
		return
	}
	e.enqueue(ctx, ev.Pool.Namespace, ev.Pool.Name)
}

// resync implements eventConsumer: it enqueues every Pool the Pool
// Controller knows.
func (e *poolEvents) resync(ctx context.Context) {
	var pools batteryv1alpha1.PoolList
	if err := e.Pools.List(ctx, &pools); err != nil {
		e.Log.Error(err, "Failed to list Pools to resync")
		return
	}
	for i := range pools.Items {
		if !e.enqueue(ctx, pools.Items[i].Namespace, pools.Items[i].Name) {
			return
		}
	}
}

// enqueue asks for a reconcile of one Pool; false means ctx ended first.
func (e *poolEvents) enqueue(ctx context.Context, namespace, name string) bool {
	pool := &batteryv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	select {
	case e.requests <- event.TypedGenericEvent[*batteryv1alpha1.Pool]{Object: pool}:
		return true
	case <-ctx.Done():
		return false
	}
}
