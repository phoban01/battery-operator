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
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"

	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
)

// Defaults for BatteryEvents.
const (
	// DefaultPoolResync is how often every Pool's status is refreshed
	// while the subscription to battery's Events stream is down (PO-024).
	DefaultPoolResync = 30 * time.Second
	// batteryEventsRetryBase and batteryEventsRetryMax pace the attempts
	// to subscribe again after the stream drops or battery refuses.
	batteryEventsRetryBase = 500 * time.Millisecond
	batteryEventsRetryMax  = 30 * time.Second
)

// eventConsumer is a controller's side of BatteryEvents: what it does with
// the subscription. Each controller turns what it is given into requests on
// the channel its source.Channel watches.
type eventConsumer interface {
	// subscribed runs after each subscription succeeds, before any of its
	// events is read. An error drops the subscription, which is made
	// again after the backoff.
	subscribed(ctx context.Context) error
	// handle is given every event of the stream, in the stream's order.
	handle(ctx context.Context, e *battery.Event)
	// resync runs at the resync interval while there is no subscription.
	resync(ctx context.Context)
}

// BatteryEvents is the Operator's one subscription to battery's Events
// stream, for every Pool. It fans the stream out to the controllers that
// registered with it: the Pool Controller (PO-023, PO-024) and the Claim
// Controller (CL-013, CL-030). The manager runs it; each controller watches
// what its side sends through a source.Channel.
//
// For each subscription it first runs every controller's subscribed hook
// (the Claim Controller's recovery, CL-030), then gives each event of the
// stream to every controller in turn, in the stream's order. While it is
// not subscribed, it asks every controller to resync at the resync
// interval, and subscribes again with backoff. One backoff and one resync
// pace both controllers.
//
// On every subscription battery replays its outbox (BA-050), so events
// recorded while the stream was down reach both controllers once it is
// back; the resync, and the claim's expiry check (CL-016), cover what is
// lost anyway.
//
// The sides are served one at a time, so a side that stops taking what it
// is given holds up the other. Each controller's channel source moves what
// its side sends into its workqueue, which never blocks for long.
type BatteryEvents struct {
	// Battery is the Operator's battery client.
	Battery battery.Client
	// Resync is the resync interval; zero is DefaultPoolResync.
	Resync time.Duration
	// Backoff paces the attempts to subscribe again; nil is exponential
	// from half a second up to 30 seconds.
	Backoff clock.Backoff
	// Clock drives the resync and the backoff; nil is the wall clock.
	Clock clock.Clock
	// Log is the logger; the zero Logger discards.
	Log logr.Logger

	mu        sync.Mutex
	consumers []eventConsumer
	connected atomic.Bool
}

// NewBatteryEvents returns a BatteryEvents with the resync interval resync.
func NewBatteryEvents(b battery.Client, resync time.Duration) *BatteryEvents {
	return &BatteryEvents{Battery: b, Resync: resync}
}

// Connected reports whether the subscription is open.
func (e *BatteryEvents) Connected() bool { return e.connected.Load() }

// add registers a controller's side. Controllers register in their
// SetupWithManager, before the manager starts e.
func (e *BatteryEvents) add(c eventConsumer) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.consumers = append(e.consumers, c)
}

// registered is a copy of the consumers that registered so far.
func (e *BatteryEvents) registered() []eventConsumer {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]eventConsumer(nil), e.consumers...)
}

// Start runs the subscription until ctx ends; it implements
// manager.Runnable. It returns nil when ctx ends.
func (e *BatteryEvents) Start(ctx context.Context) error {
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
		backoff = clock.Exponential{Base: batteryEventsRetryBase, Max: batteryEventsRetryMax}
	}
	consumers := e.registered()
	// tick is the resync timer. It runs from the moment there is no
	// subscription, and is stopped while there is one.
	var tick clock.Timer
	defer func() {
		if tick != nil {
			tick.Stop()
		}
	}()
	for attempt := 0; ; attempt++ {
		if tick == nil {
			tick = clk.NewTimer(resync)
		}
		stream, err := e.Battery.Subscribe(ctx, battery.EventFilter{})
		if ctx.Err() != nil {
			if err == nil {
				_ = stream.Close()
			}
			return nil
		}
		if err != nil {
			e.Log.V(1).Info("Failed to subscribe to battery events", "error", err.Error(), "attempt", attempt)
		} else {
			tick.Stop()
			tick = nil
			e.connected.Store(true)
			e.Log.Info("Subscribed to battery events")
			read, err := e.follow(ctx, stream, consumers)
			e.connected.Store(false)
			_ = stream.Close()
			if ctx.Err() != nil {
				return nil
			}
			e.Log.Info("Lost the subscription to battery events", "error", err.Error())
			if read {
				// A stream that delivered events was a working
				// connection: the backoff starts afresh.
				attempt = 0
			}
			tick = clk.NewTimer(resync)
		}
		if !e.wait(ctx, clk, tick, resync, backoff.Next(attempt), consumers) {
			return nil
		}
	}
}

// follow runs the consumers' subscribed hooks, then hands every consumer
// each of the stream's events, in order, until the stream ends. It returns
// the error the stream, or a hook, ended with, and whether it read any
// event.
func (e *BatteryEvents) follow(ctx context.Context, stream battery.EventStream,
	consumers []eventConsumer) (bool, error) {
	for _, c := range consumers {
		if err := c.subscribed(ctx); err != nil {
			return false, err
		}
	}
	read := false
	for {
		ev, err := stream.Recv(ctx)
		if err != nil {
			return read, err
		}
		read = true
		for _, c := range consumers {
			c.handle(ctx, ev)
		}
	}
}

// wait waits d before the next attempt to subscribe, asking every consumer
// to resync each time tick fires meanwhile. It returns false once ctx
// ends.
func (e *BatteryEvents) wait(ctx context.Context, clk clock.Clock, tick clock.Timer, resync, d time.Duration,
	consumers []eventConsumer) bool {
	//= docs/requirements/03-pools.md#pool-status
	//# While the Pool Controller's subscription to battery's `Events`
	//# stream is not connected, the Pool Controller SHALL refresh every Pool's
	//# status at the configured resync interval.
	retry := clk.NewTimer(d)
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-tick.C():
			for _, c := range consumers {
				c.resync(ctx)
			}
			tick.Reset(resync)
		case <-retry.C():
			return true
		}
	}
}
