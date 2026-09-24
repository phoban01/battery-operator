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
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
)

// Defaults for PoolEvents.
const (
	// DefaultPoolResync is how often every Pool's status is refreshed
	// while the subscription to battery's Events stream is down (PO-024).
	DefaultPoolResync = 30 * time.Second
	// poolEventsRetryBase and poolEventsRetryMax pace the attempts to
	// subscribe again after the stream drops or battery refuses.
	poolEventsRetryBase = 500 * time.Millisecond
	poolEventsRetryMax  = 30 * time.Second
	// poolEventsBuffer is how many Pools may wait to be enqueued.
	poolEventsBuffer = 256
)

// PoolEvents is the Operator's one subscription to battery's Events
// stream. It turns each event into a reconcile of the Pool it names
// (PO-023), and while it is not subscribed it asks for a reconcile of
// every Pool at the resync interval instead (PO-024). The Pool Controller
// watches Requests through a source.Channel.
//
// On every subscription battery replays its outbox, so events recorded
// while the stream was down arrive once it is back; the resync covers the
// time in between.
type PoolEvents struct {
	// Battery is the Operator's battery client.
	Battery battery.Client
	// Pools lists the Pools to refresh on a resync.
	Pools client.Reader
	// Resync is the resync interval; zero is DefaultPoolResync.
	Resync time.Duration
	// Backoff paces the attempts to subscribe again; nil is exponential
	// from half a second up to 30 seconds.
	Backoff clock.Backoff
	// Clock drives the resync and the backoff; nil is the wall clock.
	Clock clock.Clock
	// Log is the logger; the zero Logger discards.
	Log logr.Logger

	requests  chan event.TypedGenericEvent[*batteryv1alpha1.Pool]
	connected atomic.Bool
}

// NewPoolEvents returns a PoolEvents with its Requests channel.
func NewPoolEvents(b battery.Client, pools client.Reader, resync time.Duration) *PoolEvents {
	return &PoolEvents{
		Battery:  b,
		Pools:    pools,
		Resync:   resync,
		requests: make(chan event.TypedGenericEvent[*batteryv1alpha1.Pool], poolEventsBuffer),
	}
}

// Requests is the channel of Pools to reconcile, for source.Channel.
func (e *PoolEvents) Requests() <-chan event.TypedGenericEvent[*batteryv1alpha1.Pool] {
	return e.requests
}

// Connected reports whether the subscription is open.
func (e *PoolEvents) Connected() bool { return e.connected.Load() }

// Start runs the subscription until ctx ends; it implements
// manager.Runnable. It returns nil when ctx ends.
func (e *PoolEvents) Start(ctx context.Context) error {
	clk := e.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	resync := e.Resync
	if resync <= 0 {
		resync = DefaultPoolResync
	}
	backoff := e.Backoff
	if backoff == nil {
		backoff = clock.Exponential{Base: poolEventsRetryBase, Max: poolEventsRetryMax}
	}
	for {
		stream := e.subscribe(ctx, clk, resync, backoff)
		if stream == nil {
			return nil
		}
		e.connected.Store(true)
		e.Log.Info("Subscribed to battery events")
		err := e.pump(ctx, stream)
		e.connected.Store(false)
		_ = stream.Close()
		if ctx.Err() != nil {
			return nil
		}
		e.Log.Info("Lost the subscription to battery events", "error", err.Error())
	}
}

// subscribe subscribes to every Pool's events, trying again with backoff
// until battery accepts or ctx ends, and refreshing every Pool at the
// resync interval meanwhile. It returns nil once ctx ends.
func (e *PoolEvents) subscribe(ctx context.Context, clk clock.Clock, resync time.Duration,
	backoff clock.Backoff) battery.EventStream {
	//= docs/requirements/03-pools.md#pool-status
	//# While the Pool Controller's subscription to battery's `Events`
	//# stream is not connected, the Pool Controller SHALL refresh every Pool's
	//# status at the configured resync interval.
	tick := clk.NewTimer(resync)
	defer tick.Stop()
	for attempt := 0; ; attempt++ {
		stream, err := e.Battery.Subscribe(ctx, battery.EventFilter{})
		if err == nil {
			return stream
		}
		if ctx.Err() != nil {
			return nil
		}
		e.Log.V(1).Info("Failed to subscribe to battery events", "error", err.Error(), "attempt", attempt)
		retry := clk.NewTimer(backoff.Next(attempt))
	wait:
		for {
			select {
			case <-ctx.Done():
				retry.Stop()
				return nil
			case <-tick.C():
				e.resyncAll(ctx)
				tick.Reset(resync)
			case <-retry.C():
				break wait
			}
		}
	}
}

// pump enqueues the Pool of each event until the stream ends, and returns
// the error it ended with.
func (e *PoolEvents) pump(ctx context.Context, stream battery.EventStream) error {
	for {
		ev, err := stream.Recv(ctx)
		if err != nil {
			return err
		}

		//= docs/requirements/03-pools.md#pool-status
		//# When battery's `Events` stream reports an event for a Pool, the
		//# Pool Controller SHALL refresh that Pool's status.
		if ev.Pool.Name == "" {
			continue
		}
		if !e.enqueue(ctx, ev.Pool.Namespace, ev.Pool.Name) {
			return ctx.Err()
		}
	}
}

// resyncAll enqueues every Pool the Pool Controller knows.
func (e *PoolEvents) resyncAll(ctx context.Context) {
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
func (e *PoolEvents) enqueue(ctx context.Context, namespace, name string) bool {
	pool := &batteryv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	select {
	case e.requests <- event.TypedGenericEvent[*batteryv1alpha1.Pool]{Object: pool}:
		return true
	case <-ctx.Done():
		return false
	}
}
