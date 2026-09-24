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
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
)

// labelZone is the label the placement tests select on.
const labelZone = "zone"

// The Hosts of the placement tests.
const (
	testHostB = "host-b"
	testHostC = "host-c"
)

var (
	zoneA = map[string]string{labelZone: "a"}
	zoneB = map[string]string{labelZone: "b"}
)

// labelledNode is a Node with the given labels.
func labelledNode(name string, labels map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

// placementFixture is the Pool Controller against the fake battery and
// the fake client, with three Nodes: host-a and host-b in zone a, host-c
// in zone b, all of them Hosts.
type placementFixture struct {
	t     *testing.T
	ctx   context.Context
	k8s   client.Client
	bc    battery.Client
	hosts *stubHosts
	r     *PoolReconciler
	key   types.NamespacedName
}

func newPlacementFixture(t *testing.T, pool *batteryv1alpha1.Pool) *placementFixture {
	t.Helper()
	bc := startPoolFakeBattery(t)
	k8s := newPoolFakeClient(t, pool,
		labelledNode(testHostA, zoneA), labelledNode(testHostB, zoneA), labelledNode(testHostC, zoneB))
	hosts := newStubHosts(testHostA, testHostB, testHostC)
	return &placementFixture{
		t:     t,
		ctx:   context.Background(),
		k8s:   k8s,
		bc:    bc,
		hosts: hosts,
		r:     &PoolReconciler{Client: k8s, Battery: bc, Hosts: hosts, Clock: clock.NewFake(poolTestEpoch)},
		key:   client.ObjectKeyFromObject(pool),
	}
}

func (f *placementFixture) reconcile() {
	f.t.Helper()
	if _, err := f.r.Reconcile(f.ctx, ctrl.Request{NamespacedName: f.key}); err != nil {
		f.t.Fatalf("Reconcile: %v", err)
	}
}

// placed is the flintlock_hosts battery holds for the Pool.
func (f *placementFixture) placed() []string {
	f.t.Helper()
	held, err := f.bc.GetPool(f.ctx, battery.PoolRef{Namespace: f.key.Namespace, Name: f.key.Name})
	if err != nil {
		f.t.Fatalf("GetPool: %v", err)
	}
	return held.Spec.FlintlockHosts
}

func (f *placementFixture) wantPlaced(want ...string) {
	f.t.Helper()
	if got := f.placed(); !slices.Equal(got, want) {
		f.t.Errorf("battery's flintlock_hosts = %q, want %q", got, want)
	}
}

func (f *placementFixture) pool() *batteryv1alpha1.Pool {
	f.t.Helper()
	got := &batteryv1alpha1.Pool{}
	if err := f.k8s.Get(f.ctx, f.key, got); err != nil {
		f.t.Fatalf("Get Pool: %v", err)
	}
	return got
}

// relabel replaces a Node's labels.
func (f *placementFixture) relabel(name string, labels map[string]string) {
	f.t.Helper()
	n := &corev1.Node{}
	if err := f.k8s.Get(f.ctx, client.ObjectKey{Name: name}, n); err != nil {
		f.t.Fatal(err)
	}
	n.Labels = labels
	if err := f.k8s.Update(f.ctx, n); err != nil {
		f.t.Fatal(err)
	}
}

// zoneAPool is finalizedPool selecting zone a.
func zoneAPool() *batteryv1alpha1.Pool {
	pool := finalizedPool()
	pool.Spec.Placement.NodeSelector = zoneA
	return pool
}

//= docs/requirements/03-pools.md#placement
//= type=test
//# The Pool Controller SHALL set a Pool's `flintlock_hosts` in
//# battery to the names of the Hosts whose Nodes match the Pool's
//# `spec.placement.nodeSelector`.

// TestPoolIsPlacedOnTheHostsItsSelectorMatches covers PO-010: the Pool is
// declared with the Hosts whose Nodes carry its labels, and only Nodes
// that are Hosts count. An empty selector matches every Host.
func TestPoolIsPlacedOnTheHostsItsSelectorMatches(t *testing.T) {
	t.Run("zone a", func(t *testing.T) {
		f := newPlacementFixture(t, zoneAPool())
		f.reconcile()
		f.wantPlaced(testHostA, testHostB)
	})
	t.Run("a Node that is not a Host", func(t *testing.T) {
		f := newPlacementFixture(t, zoneAPool())
		f.hosts.publish(testHostA, testHostC)
		f.reconcile()
		f.wantPlaced(testHostA)
	})
	t.Run("an empty selector", func(t *testing.T) {
		f := newPlacementFixture(t, finalizedPool())
		f.reconcile()
		f.wantPlaced(testHostA, testHostB, testHostC)
	})
}

// TestPoolWaitsForTheHostsToBePublished: before the Inventory Controller
// has published battery's Hosts, the Pool Controller cannot tell where a
// Pool may run, and sends battery nothing.
func TestPoolWaitsForTheHostsToBePublished(t *testing.T) {
	b := newStubBattery()
	s := newTestPoolScope(finalizedPool(), b)
	s.Hosts = unsyncedStubHosts()
	if err := runPoolChain(context.Background(), s, poolChain()); err != nil {
		t.Fatalf("chain: %v", err)
	}
	wantCalls(t, b)
	if len(s.Pool.Status.Conditions) != 0 {
		t.Errorf("conditions = %+v, want none", s.Pool.Status.Conditions)
	}
}

//= docs/requirements/03-pools.md#placement
//= type=test
//# When the set of Hosts a Pool's selector matches changes, the
//# Pool Controller SHALL update the Pool in battery with `UpdatePool`.

// TestPoolFollowsTheHostsItsSelectorMatches covers PO-011 and the issue's
// "done when": labelling a Host into and out of the Pool's selector, and
// the Inventory Controller publishing fewer Hosts, update the Pool in the
// fake battery, with no new generation.
func TestPoolFollowsTheHostsItsSelectorMatches(t *testing.T) {
	f := newPlacementFixture(t, zoneAPool())
	f.reconcile()
	f.wantPlaced(testHostA, testHostB)

	f.relabel(testHostC, zoneA)
	f.reconcile()
	f.wantPlaced(testHostA, testHostB, testHostC)

	f.relabel(testHostA, zoneB)
	f.reconcile()
	f.wantPlaced(testHostB, testHostC)

	f.hosts.publish(testHostA, testHostC)
	f.reconcile()
	f.wantPlaced(testHostC)

	if got := f.pool(); got.Generation != 1 || got.Status.ObservedGeneration != 1 {
		t.Errorf("generation %d, observed %d; want 1 and 1", got.Generation, got.Status.ObservedGeneration)
	}
}

// TestPoolWhoseHostsAreUnchangedIsNotUpdated: a reconcile that finds the
// Pool placed as its selector says sends nothing.
func TestPoolWhoseHostsAreUnchangedIsNotUpdated(t *testing.T) {
	b := newStubBattery()
	pool := declaredPool()
	holdPool(b, pool, []string{testHostB, testHostA}, battery.PoolStatus{})
	s := newTestPoolScope(pool, b, testHostA, testHostB)
	if err := runPoolChain(context.Background(), s, poolChain()); err != nil {
		t.Fatalf("chain: %v", err)
	}
	wantCalls(t, b, "GetPool ci/runners")
}

// TestHostsChangedEnqueuesEveryPool: the Pool Controller's watch of the
// Hosts asks for every Pool once it starts with the Hosts published, and
// again at each change (PO-011).
func TestHostsChangedEnqueuesEveryPool(t *testing.T) {
	other := finalizedPool()
	other.Name = "builders"
	hosts := unsyncedStubHosts()
	r := &PoolReconciler{Client: newPoolFakeClient(t, finalizedPool(), other), Hosts: hosts}
	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	t.Cleanup(q.ShutDown)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := r.hostsChangedSource(ctx, q); err != nil {
		t.Fatal(err)
	}

	take := func() []string {
		t.Helper()
		var got []string
		deadline := time.Now().Add(poolEventsTimeout)
		for len(got) < 2 && time.Now().Before(deadline) {
			if q.Len() == 0 {
				time.Sleep(time.Millisecond)
				continue
			}
			req, _ := q.Get()
			q.Done(req)
			q.Forget(req)
			got = append(got, req.String())
		}
		slices.Sort(got)
		return got
	}
	want := []string{"ci/builders", poolRef(testPool()).String()}

	time.Sleep(10 * time.Millisecond)
	if q.Len() != 0 {
		t.Fatalf("enqueued %d Pools before the Hosts were published", q.Len())
	}
	hosts.publish(testHostA)
	if got := take(); !slices.Equal(got, want) {
		t.Errorf("after publishing: %q, want %q", got, want)
	}
	hosts.publish(testHostB)
	if got := take(); !slices.Equal(got, want) {
		t.Errorf("after a change: %q, want %q", got, want)
	}
}

// TestNodeLabelsChanged checks which Node updates reach the Pool
// Controller.
func TestNodeLabelsChanged(t *testing.T) {
	base := func() *corev1.Node { return labelledNode(testHostA, map[string]string{labelZone: "a"}) }
	for name, tc := range map[string]struct {
		change func(*corev1.Node)
		want   bool
	}{
		"relabelled":    {func(n *corev1.Node) { n.Labels[labelZone] = "b" }, true},
		"label added":   {func(n *corev1.Node) { n.Labels["gpu"] = "yes" }, true},
		"label removed": {func(n *corev1.Node) { delete(n.Labels, labelZone) }, true},
		"cordoned":      {func(n *corev1.Node) { n.Spec.Unschedulable = true }, false},
		"annotated":     {func(n *corev1.Node) { n.Annotations = map[string]string{"x": "y"} }, false},
	} {
		n := base()
		tc.change(n)
		if got := nodeLabelsChanged.Update(event.UpdateEvent{ObjectOld: base(), ObjectNew: n}); got != tc.want {
			t.Errorf("%s: passed %v, want %v", name, got, tc.want)
		}
	}
}

//= docs/requirements/03-pools.md#placement
//= type=test
//# While a Pool's selector matches no Host, the Pool Controller
//# SHALL set the Pool's condition `Ready` false with the reason
//# `NoEligibleHost`.

// TestPoolWhoseSelectorMatchesNoHostSaysSo covers PO-012: a Pool whose
// selector matches no Host is held by battery with no Hosts and is not
// Ready, with the reason NoEligibleHost, until a Host is labelled into its
// selector.
func TestPoolWhoseSelectorMatchesNoHostSaysSo(t *testing.T) {
	pool := finalizedPool()
	pool.Spec.Placement.NodeSelector = map[string]string{labelZone: "c"}
	f := newPlacementFixture(t, pool)
	f.reconcile()
	f.wantPlaced()
	wantCondition(t, f.pool(), batteryv1alpha1.PoolConditionReady, metav1.ConditionFalse, PoolReasonNoEligibleHost)

	f.relabel(testHostC, map[string]string{labelZone: "c"})
	f.reconcile()
	f.wantPlaced(testHostC)
	if ready := readyCondition(f.pool()); ready == nil || ready.Reason == PoolReasonNoEligibleHost {
		t.Errorf("Ready = %+v once a Host matches, want another reason", ready)
	}

	t.Run("battery does not hold the Pool", func(t *testing.T) {
		s := newTestPoolScope(declaredPool(), newStubBattery())
		if _, err := (poolReadiness{}).Reconcile(context.Background(), s); err != nil {
			t.Fatal(err)
		}
		wantCondition(t, s.Pool, batteryv1alpha1.PoolConditionReady, metav1.ConditionFalse, PoolReasonNoEligibleHost)
	})
}
