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
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/controller/inventory"
)

// The test Pool's name and namespace.
const (
	testPoolName      = "runners"
	testPoolNamespace = "ci"
)

// poolTestEpoch is the fake clock's start in the Pool Controller's tests.
var poolTestEpoch = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

// stubBattery is a hand-written battery.Client for the Pool Controller's
// subreconciler tests. It holds Pools by ref, records the calls it gets,
// and answers a call with the error set for it, if any. The embedded
// interface is nil, so a call the Pool Controller should not make panics.
type stubBattery struct {
	battery.Client

	mu    sync.Mutex
	pools map[battery.PoolRef]battery.PoolSpec
	// status is the PoolStatus GetPool reports for a Pool it holds.
	status map[battery.PoolRef]battery.PoolStatus
	calls  []string

	getErr, createErr, updateErr, deleteErr error
}

func newStubBattery() *stubBattery {
	return &stubBattery{pools: map[battery.PoolRef]battery.PoolSpec{}, status: map[battery.PoolRef]battery.PoolStatus{}}
}

func (b *stubBattery) record(call string) {
	b.calls = append(b.calls, call)
}

func (b *stubBattery) Calls() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...)
}

func (b *stubBattery) GetPool(_ context.Context, ref battery.PoolRef) (*battery.Pool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.record("GetPool " + ref.String())
	if b.getErr != nil {
		return nil, b.getErr
	}
	spec, ok := b.pools[ref]
	if !ok {
		return nil, battery.ErrNotFound
	}
	return &battery.Pool{Spec: spec, Status: b.status[spec.Ref]}, nil
}

func (b *stubBattery) CreatePool(_ context.Context, spec battery.PoolSpec) (*battery.Pool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.record("CreatePool " + spec.Ref.String())
	if b.createErr != nil {
		return nil, b.createErr
	}
	if _, ok := b.pools[spec.Ref]; ok {
		return nil, battery.ErrAlreadyExists
	}
	b.pools[spec.Ref] = spec
	return &battery.Pool{Spec: spec, Status: b.status[spec.Ref]}, nil
}

func (b *stubBattery) UpdatePool(_ context.Context, spec battery.PoolSpec) (*battery.Pool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.record("UpdatePool " + spec.Ref.String())
	if b.updateErr != nil {
		return nil, b.updateErr
	}
	if _, ok := b.pools[spec.Ref]; !ok {
		return nil, battery.ErrNotFound
	}
	b.pools[spec.Ref] = spec
	return &battery.Pool{Spec: spec, Status: b.status[spec.Ref]}, nil
}

func (b *stubBattery) DeletePool(_ context.Context, ref battery.PoolRef) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.record("DeletePool " + ref.String())
	if b.deleteErr != nil {
		return b.deleteErr
	}
	if _, ok := b.pools[ref]; !ok {
		return battery.ErrNotFound
	}
	delete(b.pools, ref)
	return nil
}

// spec returns the spec battery holds for ref.
func (b *stubBattery) spec(ref battery.PoolRef) (battery.PoolSpec, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	spec, ok := b.pools[ref]
	return spec, ok
}

// testPool is a valid Pool at generation 1, with the defaults the CRD
// would apply.
func testPool() *batteryv1alpha1.Pool {
	return &batteryv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{Name: testPoolName, Namespace: testPoolNamespace, Generation: 1},
		Spec: batteryv1alpha1.PoolSpec{
			Template: batteryv1alpha1.MicroVMTemplate{
				VCPU:       2,
				MemoryInMb: 2048,
				Kernel:     batteryv1alpha1.Kernel{Image: "ghcr.io/example/kernel:6.1"},
				RootVolume: batteryv1alpha1.Volume{ID: "root", ContainerSource: ptrTo("ghcr.io/example/root:24.04")},
			},
			Size:          3,
			Replenishment: batteryv1alpha1.ReplenishmentStrategy{Type: batteryv1alpha1.ReplenishImmediateOnLease},
			Hooks:         batteryv1alpha1.PoolHooks{FailurePolicy: batteryv1alpha1.HookFailureDeleteAndReplace},
			Lease: batteryv1alpha1.PoolLease{
				HeartbeatInterval: &metav1.Duration{Duration: 10 * time.Second},
				ExpiryThreshold:   &metav1.Duration{Duration: 30 * time.Second},
			},
		},
	}
}

func ptrTo[T any](v T) *T { return &v }

// poolTestScheme knows the core types and the Operator's.
func poolTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := batteryv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// newPoolFakeClient is controller-runtime's fake client holding objs, with
// the Pool's status subresource.
func newPoolFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	//= docs/requirements/08-test-doubles.md#test-environments
	//# The unit tests SHALL test each subreconciler, and the rest of
	//# the logic of the controllers and the Exec Agent, against the fake battery,
	//# the fake `flintlockd` and a fake Kubernetes client, without a Kubernetes
	//# API server.
	return fake.NewClientBuilder().
		WithScheme(poolTestScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&batteryv1alpha1.Pool{}).
		Build()
}

// newTestPoolScope is a scope for pool whose Kubernetes client holds a
// Node, unlabelled, for each of hosts, and whose Hosts are those. The
// test Pool's selector is empty, so it matches them all. The
// subreconcilers never write to the API server.
func newTestPoolScope(pool *batteryv1alpha1.Pool, b battery.Client, hosts ...string) *poolScope {
	nodes := make([]client.Object, 0, len(hosts))
	for _, h := range hosts {
		nodes = append(nodes, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: h}})
	}
	c := fake.NewClientBuilder().WithObjects(nodes...).Build()
	return newPoolScope(pool, c, b, newStubHosts(hosts...), logr.Discard(), clock.NewFake(poolTestEpoch))
}

// stubHosts is PoolHosts for the Pool Controller's tests: the Hosts the
// Inventory Controller has published, matched against the Nodes as
// inventory.HostSet matches them.
type stubHosts struct {
	mu      sync.Mutex
	names   map[string]bool
	synced  bool
	changed chan struct{}
}

// newStubHosts is a stubHosts published with the Hosts named.
func newStubHosts(names ...string) *stubHosts {
	h := &stubHosts{changed: make(chan struct{})}
	h.publish(names...)
	return h
}

// unsyncedStubHosts is a stubHosts the Inventory Controller has not yet
// published.
func unsyncedStubHosts() *stubHosts {
	return &stubHosts{changed: make(chan struct{})}
}

// publish replaces the Hosts and wakes Changed's waiters.
func (h *stubHosts) publish(names ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.names = map[string]bool{}
	for _, n := range names {
		h.names[n] = true
	}
	h.synced = true
	close(h.changed)
	h.changed = make(chan struct{})
}

func (h *stubHosts) Synced() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.synced
}

func (h *stubHosts) Changed() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.changed
}

func (h *stubHosts) Matching(ctx context.Context, r client.Reader, selector map[string]string) ([]string, error) {
	if !h.Synced() {
		return nil, inventory.ErrNotSynced
	}
	nodes := &corev1.NodeList{}
	if err := r.List(ctx, nodes, client.MatchingLabels(selector)); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var names []string
	for _, n := range nodes.Items {
		if h.names[n.Name] {
			names = append(names, n.Name)
		}
	}
	slices.Sort(names)
	return names, nil
}
