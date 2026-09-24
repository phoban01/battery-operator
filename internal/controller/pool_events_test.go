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
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/fakebattery"
	"github.com/phoban01/battery-operator/internal/fakeflintlock"
)

// poolEventsTimeout bounds each wait in these tests.
const poolEventsTimeout = 30 * time.Second

// testHostA is the Host the Pools in these tests run on.
const testHostA = "host-a"

// startBatteryEvents runs e until the test ends.
func startBatteryEvents(t *testing.T, e *BatteryEvents) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("BatteryEvents.Start: %v", err)
		}
	})
}

// reconcileFromEvents stands in for the controller's workqueue: it calls
// r.Reconcile for each Pool e asks for, until the test ends.
func reconcileFromEvents(t *testing.T, r *PoolReconciler, e *poolEvents) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-e.Requests():
				req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ev.Object)}
				_, _ = r.Reconcile(ctx, req)
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// eventually polls cond until it holds or the timeout passes.
func eventually(t *testing.T, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(poolEventsTimeout)
	for {
		ok, got := cond()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; last saw %s", what, got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

//= docs/requirements/03-pools.md#pool-status
//= type=test
//# When battery's `Events` stream reports an event for a Pool, the
//# Pool Controller SHALL refresh that Pool's status.

// TestPoolStatusFollowsBatteryEvents covers PO-023 and the issue's "done
// when": replenishment and leases in the fake battery reach Pool.status
// through its Events stream, with the resync an hour away, and do so again
// after the fake drops the stream.
func TestPoolStatusFollowsBatteryEvents(t *testing.T) {
	ctx := context.Background()
	fl := fakeflintlock.New(fakeflintlock.Config{Name: testHostA})
	t.Cleanup(func() { _ = fl.Close() })
	flConn, err := fl.Conn()
	if err != nil {
		t.Fatalf("fake flintlockd: %v", err)
	}
	t.Cleanup(func() { _ = flConn.Close() })
	hosts := fakebattery.NewHosts()
	hosts.Add(testHostA, testHostA+":9090", flConn)
	bc, fb := startPoolFakeBatteryWith(t, fakebattery.Config{Hosts: hosts})

	pool := finalizedPool()
	k8s := newPoolFakeClient(t, pool, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: testHostA}})
	r := &PoolReconciler{Client: k8s, Battery: bc, Hosts: newStubHosts(testHostA), Clock: clock.NewFake(poolTestEpoch)}
	key := client.ObjectKeyFromObject(pool)
	ref := poolRef(pool)

	// Declare the Pool, which places it on host-a.
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	events := NewBatteryEvents(bc, time.Hour)
	pe := newPoolEvents(k8s, logr.Discard())
	events.add(pe)
	reconcileFromEvents(t, r, pe)
	startBatteryEvents(t, events)

	status := func(want func(batteryv1alpha1.PoolStatus) bool) func() (bool, string) {
		return func() (bool, string) {
			got := &batteryv1alpha1.Pool{}
			if err := k8s.Get(ctx, key, got); err != nil {
				return false, err.Error()
			}
			return want(got.Status), fmt.Sprintf("%+v", got.Status)
		}
	}
	isReady := func(st batteryv1alpha1.PoolStatus) bool {
		for _, c := range st.Conditions {
			if c.Type == batteryv1alpha1.PoolConditionReady {
				return c.Status == metav1.ConditionTrue
			}
		}
		return false
	}

	eventually(t, "the Pool to fill and be Ready", status(func(st batteryv1alpha1.PoolStatus) bool {
		return st.Available == 3 && isReady(st)
	}))

	if _, err := bc.ClaimVM(ctx, ref); err != nil {
		t.Fatalf("ClaimVM: %v", err)
	}
	eventually(t, "the lease and its replacement", status(func(st batteryv1alpha1.PoolStatus) bool {
		return st.Leased == 1 && st.Available == 3
	}))

	// The stream drops; BatteryEvents subscribes again and the next lease
	// still arrives by event.
	fb.SetFaults(fakebattery.Faults{DropEventsStream: true})
	eventually(t, "a new subscription", func() (bool, string) {
		return events.Connected(), fmt.Sprintf("connected %v", events.Connected())
	})
	if _, err := bc.ClaimVM(ctx, ref); err != nil {
		t.Fatalf("ClaimVM: %v", err)
	}
	eventually(t, "the second lease", status(func(st batteryv1alpha1.PoolStatus) bool {
		return st.Leased == 2
	}))
}

// downBattery is a battery.Client whose Subscribe fails until the test
// opens a stream, which then delivers what the test sends it.
type downBattery struct {
	battery.Client

	mu     sync.Mutex
	stream *chanStream
	calls  int
}

func (b *downBattery) Subscribe(context.Context, battery.EventFilter) (battery.EventStream, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	if b.stream == nil {
		return nil, fmt.Errorf("%w: connection refused", battery.ErrUnavailable)
	}
	s := b.stream
	b.stream = nil
	return s, nil
}

// open makes the next Subscribe succeed with a new stream.
func (b *downBattery) open() *chanStream {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stream = &chanStream{events: make(chan *battery.Event), ended: make(chan struct{})}
	return b.stream
}

// chanStream is an EventStream fed by the test; end drops it.
type chanStream struct {
	events chan *battery.Event
	ended  chan struct{}
	once   sync.Once
}

func (s *chanStream) Recv(ctx context.Context) (*battery.Event, error) {
	select {
	case e := <-s.events:
		return e, nil
	case <-s.ended:
		return nil, fmt.Errorf("%w: events stream ended", battery.ErrUnavailable)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *chanStream) end()         { s.once.Do(func() { close(s.ended) }) }
func (s *chanStream) Close() error { s.end(); return nil }

// fixedBackoff waits the same time before every attempt.
type fixedBackoff time.Duration

func (f fixedBackoff) Next(int) time.Duration { return time.Duration(f) }

// takeRequests reads n requests from e, sorted.
func takeRequests(t *testing.T, e *poolEvents, n int) []string {
	t.Helper()
	var got []string
	for range n {
		select {
		case ev := <-e.Requests():
			got = append(got, client.ObjectKeyFromObject(ev.Object).String())
		case <-time.After(poolEventsTimeout):
			t.Fatalf("got requests %q, then none", got)
		}
	}
	slices.Sort(got)
	return got
}

// noRequest checks that e asks for nothing for a while.
func noRequest(t *testing.T, e *poolEvents) {
	t.Helper()
	select {
	case ev := <-e.Requests():
		t.Errorf("unexpected request for %s", client.ObjectKeyFromObject(ev.Object))
	case <-time.After(50 * time.Millisecond):
	}
}

//= docs/requirements/03-pools.md#pool-status
//= type=test
//# While the Pool Controller's subscription to battery's `Events`
//# stream is not connected, the Pool Controller SHALL refresh every Pool's
//# status at the configured resync interval.

// TestPoolEventsResyncWhileTheStreamIsDown covers PO-024: while battery
// refuses the subscription, every Pool is refreshed each resync interval;
// once subscribed, only the Pools of events are; when the stream drops,
// the resync starts again.
func TestPoolEventsResyncWhileTheStreamIsDown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), poolEventsTimeout)
	defer cancel()
	other := testPool()
	other.Name = "builders"
	k8s := newPoolFakeClient(t, testPool(), other)
	clk := clock.NewFake(poolTestEpoch)
	b := &downBattery{}
	e := NewBatteryEvents(b, time.Minute)
	e.Clock = clk
	e.Backoff = fixedBackoff(90 * time.Second)
	pe := newPoolEvents(k8s, logr.Discard())
	e.add(pe)
	startBatteryEvents(t, e)
	both := []string{"ci/builders", "ci/runners"}

	// Down: the resync timer and the retry timer are armed.
	if err := clk.BlockUntil(ctx, 2); err != nil {
		t.Fatal(err)
	}
	noRequest(t, pe)
	clk.Advance(time.Minute)
	if got := takeRequests(t, pe, 2); !slices.Equal(got, both) {
		t.Errorf("first resync asked for %q, want %q", got, both)
	}

	// The retry at 90s subscribes.
	stream := b.open()
	clk.Advance(30 * time.Second)
	eventually(t, "the subscription", func() (bool, string) {
		return e.Connected(), fmt.Sprintf("connected %v", e.Connected())
	})

	// Connected: no resync, however long; an event asks for its Pool.
	clk.Advance(10 * time.Minute)
	noRequest(t, pe)
	stream.events <- &battery.Event{ID: 1, Pool: battery.PoolRef{Namespace: "ci", Name: "runners"}}
	if got := takeRequests(t, pe, 1); !slices.Equal(got, []string{"ci/runners"}) {
		t.Errorf("event asked for %q, want ci/runners", got)
	}

	// The stream drops and battery refuses again: the resync is back.
	stream.end()
	eventually(t, "the subscription to drop", func() (bool, string) {
		return !e.Connected(), fmt.Sprintf("connected %v", e.Connected())
	})
	if err := clk.BlockUntil(ctx, 2); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Minute)
	if got := takeRequests(t, pe, 2); !slices.Equal(got, both) {
		t.Errorf("resync after the drop asked for %q, want %q", got, both)
	}
}
