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
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/testr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/controller/claim"
	"github.com/phoban01/battery-operator/internal/fakebattery"
	"github.com/phoban01/battery-operator/internal/fakeflintlock"
)

// recorder is an eventConsumer that records what it is given, in order,
// into a log it shares with other recorders.
type recorder struct {
	name string
	log  *eventLog
	// fail, while positive, is how many more subscribed hooks fail.
	fail atomic.Int32
}

// eventLog is the shared, ordered record of the recorders.
type eventLog struct {
	mu      sync.Mutex
	entries []string
}

func (l *eventLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, s)
}

func (l *eventLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.entries)
}

func (r *recorder) subscribed(context.Context) error {
	if r.fail.Add(-1) >= 0 {
		r.log.add(r.name + " subscribed, failed")
		return errors.New("recovery failed")
	}
	r.log.add(r.name + " subscribed")
	return nil
}

func (r *recorder) handle(_ context.Context, e *battery.Event) {
	r.log.add(fmt.Sprintf("%s event %d", r.name, e.ID))
}

func (r *recorder) resync(context.Context) { r.log.add(r.name + " resync") }

// TestBatteryEventsFansOutOneSubscription: every registered controller's
// side is served by one subscription. Each subscription runs every side's
// subscribed hook before any event is read, and each event reaches every
// side, in the stream's order. A subscribed hook that fails drops the
// subscription before the others run and before any event is read, and
// the next subscription runs them all again.
func TestBatteryEventsFansOutOneSubscription(t *testing.T) {
	log := &eventLog{}
	pools := &recorder{name: "pools", log: log}
	claims := &recorder{name: "claims", log: log}
	claims.fail.Store(1)
	b := &downBattery{}
	e := &BatteryEvents{Battery: b, Backoff: fixedBackoff(10 * time.Millisecond), Clock: clock.Real{}, Log: testr.New(t)}
	e.add(pools)
	e.add(claims)

	// The first subscription's hook fails; the second one is followed.
	first := b.open()
	startBatteryEvents(t, e)
	eventually(t, "the first subscription to be dropped", func() (bool, string) {
		select {
		case <-first.ended:
			return true, ""
		default:
			return false, fmt.Sprintf("%q", log.get())
		}
	})
	second := b.open()
	eventually(t, "the second subscription", func() (bool, string) {
		return e.Connected(), fmt.Sprintf("connected %v", e.Connected())
	})
	second.events <- &battery.Event{ID: 1}
	second.events <- &battery.Event{ID: 2}

	// battery goes away and comes back: a third subscription.
	third := b.open()
	second.end()
	third.events <- &battery.Event{ID: 3}

	const poolsSubscribed = "pools subscribed"
	want := []string{
		poolsSubscribed, "claims subscribed, failed",
		poolsSubscribed, "claims subscribed",
		"pools event 1", "claims event 1",
		"pools event 2", "claims event 2",
		poolsSubscribed, "claims subscribed",
		"pools event 3", "claims event 3",
	}
	eventually(t, "every event to reach both sides", func() (bool, string) {
		got := log.get()
		return len(got) >= len(want), fmt.Sprintf("%q", got)
	})
	if got := log.get(); !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// watchedBattery is the Operator's battery client, counting Subscribe
// calls and running onRecovery at the start of each ListLeases, which only
// the claims' recovery calls here.
type watchedBattery struct {
	battery.Client
	subscribes atomic.Int32
	onRecovery func()
}

func (w *watchedBattery) Subscribe(ctx context.Context, f battery.EventFilter) (battery.EventStream, error) {
	w.subscribes.Add(1)
	return w.Client.Subscribe(ctx, f)
}

func (w *watchedBattery) ListLeases(ctx context.Context, p *battery.PoolRef) ([]*battery.LeaseRecord, error) {
	w.onRecovery()
	return w.Client.ListLeases(ctx, p)
}

//= docs/requirements/03-pools.md#pool-status
//= type=test
//# When battery's `Events` stream reports an event for a Pool, the
//# Pool Controller SHALL refresh that Pool's status.

//= docs/requirements/02-claims.md#recovery
//= type=test
//# When the Claim Controller starts, and when its connection to
//# battery is restored, the Claim Controller SHALL reconcile every Bound claim that is not being deleted against
//# battery's Leases.

// TestBatteryEventsServesBothControllersAgainstTheFakeBattery covers #102:
// the Pool and Claim Controllers' sides share one subscription to the fake
// battery. On the Claim Controller's start, and again after battery drops
// the stream, the claims are recovered before any event of the new
// subscription is read, and battery's replay reaches both sides: the
// Pool's events ask for a reconcile of the Pool, and the deletion of a
// claim's MicroVM sends the claim.
func TestBatteryEventsServesBothControllersAgainstTheFakeBattery(t *testing.T) {
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

	// A Pool, declared and filled, whose MicroVM is leased and released
	// before the Operator subscribes: battery's replay has the deletion.
	pool := finalizedPool()
	poolKey := client.ObjectKeyFromObject(pool).String()
	pools := newPoolFakeClient(t, pool, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: testHostA}})
	r := &PoolReconciler{Client: pools, Battery: bc, Hosts: newStubHosts(testHostA), Clock: clock.NewFake(poolTestEpoch)}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	ref := poolRef(pool)
	eventually(t, "a MicroVM AVAILABLE in the Pool", func() (bool, string) {
		p, err := bc.GetPool(ctx, ref)
		if err != nil {
			return false, err.Error()
		}
		return p.Status.Available > 0, fmt.Sprintf("%+v", p.Status)
	})
	lease, err := bc.ClaimVM(ctx, ref)
	if err != nil {
		t.Fatalf("ClaimVM: %v", err)
	}
	if err := bc.ReleaseVM(ctx, lease.LeaseID); err != nil {
		t.Fatalf("ReleaseVM: %v", err)
	}

	// The claim that recorded the MicroVM, still Bound.
	claims := fake.NewClientBuilder().
		WithScheme(poolTestScheme(t)).
		WithObjects(claimOn(testClaimName, lease.VMUID)).
		WithIndex(&batteryv1alpha1.MicroVMClaim{}, claim.MicroVMUIDIndex, func(o client.Object) []string {
			return claim.MicroVMUID(o.(*batteryv1alpha1.MicroVMClaim))
		}).
		Build()

	pe := newPoolEvents(pools, logr.Discard())
	deleted := &claim.DeletedVMs{}
	// At each recovery, which runs in the watcher's goroutine, what the
	// sides had been given of the new subscription: the Pool side's
	// requests are drained there, while nothing else adds to them.
	type seen struct {
		requests int
		deleted  bool
	}
	var mu sync.Mutex
	var recoveries []seen
	bw := &watchedBattery{Client: bc, onRecovery: func() {
		n := len(pe.requests)
		for range n {
			<-pe.requests
		}
		mu.Lock()
		defer mu.Unlock()
		recoveries = append(recoveries, seen{requests: n, deleted: deleted.Has(lease.VMUID)})
	}}
	events := &BatteryEvents{
		Battery: bw,
		Resync:  time.Hour,
		Backoff: fixedBackoff(10 * time.Millisecond),
		Clock:   clock.Real{},
		Log:     testr.New(t),
	}
	events.add(pe)
	out := make(chan event.GenericEvent)
	events.add(&claimEvents{
		Reader:  claims,
		Deleted: deleted,
		Out:     out,
		Recovery: &claimRecovery{
			Battery: bw,
			Reader:  claims,
			Leases:  &claim.RecoveredLeases{},
			Out:     out,
			Log:     testr.New(t),
		},
		Log: testr.New(t),
	})
	startBatteryEvents(t, events)

	take := func(what string) {
		t.Helper()
		select {
		case e := <-out:
			if e.Object.GetName() != testClaimName {
				t.Errorf("%s: sent %s, want %s", what, e.Object.GetName(), testClaimName)
			}
		case <-time.After(poolEventsTimeout):
			t.Fatalf("%s: no claim was sent", what)
		}
	}
	// poolAsked checks that the Pool side has asked for the Pool, and for
	// nothing else. The Pool side is given each event before the claim
	// side, so what it was given of an event the claim side sent a claim
	// for is on its channel by then.
	poolAsked := func(what string) {
		t.Helper()
		n := len(pe.requests)
		if n == 0 {
			t.Errorf("%s: the Pool side asked for nothing, want the Pool", what)
		}
		for _, got := range takeRequests(t, pe, n) {
			if got != poolKey {
				t.Errorf("%s: the Pool side asked for %s, want %s", what, got, poolKey)
			}
		}
	}

	// The start: the recovery, then the replay, whose deletion reaches the
	// claim side and whose Pool events reach the Pool side.
	take("the recovery on start")
	take("the deletion replayed on start")
	if !deleted.Has(lease.VMUID) {
		t.Error("the claim was sent before its MicroVM was recorded deleted")
	}
	poolAsked("the replay on start")

	// battery drops the stream: a new subscription, a new recovery, and
	// the replay reaches both sides again.
	fb.SetFaults(fakebattery.Faults{DropEventsStream: true})
	take("the recovery on reconnecting")
	take("the deletion replayed on reconnecting")
	poolAsked("the replay on reconnecting")

	if n := bw.subscribes.Load(); n != 2 {
		t.Errorf("Subscribe called %d times, want 2: one per connection, for both sides", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(recoveries) != 2 {
		t.Fatalf("recovered %d times, want 2: on start and on reconnecting", len(recoveries))
	}
	if first := recoveries[0]; first.requests != 0 || first.deleted {
		t.Errorf("on start, the recovery ran after the stream was read (%+v), want before", first)
	}
}
