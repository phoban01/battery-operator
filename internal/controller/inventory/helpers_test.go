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

package inventory

import (
	"context"
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

	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/batterysidecar"
	"github.com/phoban01/battery-operator/internal/clock"
)

// The timings of these tests: different, so that a controller that
// confuses them fails.
const (
	settle = 30 * time.Second
	window = time.Minute
	drain  = 10 * time.Second
)

// Two Hosts and their flintlockd addresses.
const (
	nodeA = "node-a"
	nodeB = "node-b"
	addrA = "10.0.0.1:9090"
	addrB = "10.0.0.2:9090"
)

// configMapKey is battery's ConfigMap in these tests.
var configMapKey = client.ObjectKey{Namespace: "battery-system", Name: batterysidecar.DefaultConfigMap}

// fakeRestarter records every restart of battery.
type fakeRestarter struct {
	mu sync.Mutex
	// configs are the configurations battery was restarted with.
	configs [][]byte
	// at are the times of the restarts.
	at []time.Time
	// clock says when a restart happens.
	clock clock.Clock
	// err, when set, fails every restart.
	err error
	// during, when set, runs inside each restart, before it returns.
	during func()
}

func (f *fakeRestarter) Restart(_ context.Context, config []byte) error {
	f.mu.Lock()
	during, err := f.during, f.err
	f.mu.Unlock()
	if during != nil {
		during()
	}
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configs = append(f.configs, config)
	f.at = append(f.at, f.clock.Now())
	return nil
}

func (f *fakeRestarter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.configs)
}

// harness is the Inventory Controller against the fake client, a fake
// clock and a fake Restarter: what InventoryReconciler builds, without
// the manager.
type harness struct {
	t         *testing.T
	c         client.WithWatch
	store     ConfigMapStore
	clock     *clock.Fake
	state     *State
	hosts     *HostSet
	restarter *fakeRestarter
	pools     *fakePools
	options   Options
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// shippedConfigMap is battery's ConfigMap as the Manifests ship it: no
// Hosts, so the placeholder alone.
func shippedConfigMap(t *testing.T) *corev1.ConfigMap {
	t.Helper()
	raw, err := batterysidecar.Render(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: configMapKey.Namespace, Name: configMapKey.Name},
		Data:       map[string]string{batterysidecar.ConfigKey: string(raw)},
	}
}

// newHarness starts a harness with battery's shipped configuration and
// the Nodes given.
func newHarness(t *testing.T, nodes ...*corev1.Node) *harness {
	t.Helper()
	objs := make([]client.Object, 0, 1+len(nodes))
	objs = append(objs, shippedConfigMap(t))
	for _, n := range nodes {
		objs = append(objs, n)
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).Build()
	clk := clock.NewFake(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC))
	return &harness{
		t:         t,
		c:         c,
		store:     ConfigMapStore{Reader: c, Writer: c, Key: configMapKey},
		clock:     clk,
		state:     NewState(),
		hosts:     NewHostSet(),
		restarter: &fakeRestarter{clock: clk},
		pools:     &fakePools{},
		options:   Options{SettleTime: settle, RestartWindow: window, DrainTimeout: drain},
	}
}

// scope builds the scope one reconcile runs with, as the controller does.
func (h *harness) scope() *Scope {
	h.t.Helper()
	ctx := context.Background()
	nodes := &corev1.NodeList{}
	if err := h.c.List(ctx, nodes); err != nil {
		h.t.Fatal(err)
	}
	config, err := h.store.Load(ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	return &Scope{
		Nodes:     nodes.Items,
		Config:    config,
		State:     h.state,
		HostSet:   h.hosts,
		Pools:     h.pools,
		Store:     h.store,
		Restarter: h.restarter,
		Log:       logr.Discard(),
		Clock:     h.clock,
	}
}

// reconcile runs the whole chain once.
func (h *harness) reconcile() (Result, error) {
	h.t.Helper()
	s := h.scope()
	err := NewChain(h.options).Run(context.Background(), s)
	return s.Result, err
}

// mustReconcile runs the chain and fails the test on an error.
func (h *harness) mustReconcile() Result {
	h.t.Helper()
	r, err := h.reconcile()
	if err != nil {
		h.t.Fatalf("reconcile: %v", err)
	}
	return r
}

// advance moves the clock on by d and reconciles, as the requeue would.
func (h *harness) advance(d time.Duration) Result {
	h.t.Helper()
	h.clock.Advance(d)
	return h.mustReconcile()
}

// update changes a Node in the fake client.
func (h *harness) update(name string, change func(*corev1.Node)) {
	h.t.Helper()
	ctx := context.Background()
	n := &corev1.Node{}
	if err := h.c.Get(ctx, client.ObjectKey{Name: name}, n); err != nil {
		h.t.Fatal(err)
	}
	change(n)
	if err := h.c.Update(ctx, n); err != nil {
		h.t.Fatal(err)
	}
}

// create adds a Node to the fake client.
func (h *harness) create(n *corev1.Node) {
	h.t.Helper()
	if err := h.c.Create(context.Background(), n); err != nil {
		h.t.Fatal(err)
	}
}

// stored is battery's configuration as its ConfigMap holds it.
func (h *harness) stored() Config {
	h.t.Helper()
	config, err := h.store.Load(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return config
}

// published is the Hosts the HostSet holds, or nil before it is synced.
func (h *harness) published() Hosts {
	h.t.Helper()
	list, err := h.hosts.Hosts()
	if err != nil {
		return nil
	}
	out := Hosts{}
	for _, host := range list {
		out[host.Name] = host.Address
	}
	return out
}

// lastRestart is the configuration of battery's last restart, parsed.
func (h *harness) lastRestart() *batterysidecar.File {
	h.t.Helper()
	h.restarter.mu.Lock()
	defer h.restarter.mu.Unlock()
	if len(h.restarter.configs) == 0 {
		h.t.Fatal("battery was never restarted")
	}
	f, err := batterysidecar.Parse(h.restarter.configs[len(h.restarter.configs)-1])
	if err != nil {
		h.t.Fatal(err)
	}
	return f
}

// host is a Node whose Exec Agent reports it ready, with its flintlockd at
// addr.
func host(name, addr string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Annotations: map[string]string{
				AnnotationReady:             annotationTrue,
				AnnotationFlintlockdAddress: addr,
			},
		},
	}
}

// Changes to a Node.
func cordon(n *corev1.Node)   { n.Spec.Unschedulable = true }
func notReady(n *corev1.Node) { n.Annotations[AnnotationReady] = "false" }
func ready(n *corev1.Node)    { n.Annotations[AnnotationReady] = annotationTrue }

// hostsOf is the Hosts in a configuration file, placeholder included.
func hostsOf(f *batterysidecar.File) Hosts {
	out := Hosts{}
	for _, h := range f.Hosts {
		out[h.Name] = h.Address
	}
	return out
}

// fakePools is battery's Pools as Drain lists them: each Pool's
// flintlock_hosts.
type fakePools struct {
	mu    sync.Mutex
	hosts map[string][]string
	err   error
	lists int
}

func (f *fakePools) ListPools(context.Context, string) ([]*battery.Pool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	if f.err != nil {
		return nil, f.err
	}
	var out []*battery.Pool
	for name, hosts := range f.hosts {
		out = append(out, &battery.Pool{Spec: battery.PoolSpec{
			Ref:            battery.PoolRef{Namespace: "ci", Name: name},
			FlintlockHosts: hosts,
		}})
	}
	return out, nil
}

// set places the Pool of that name on hosts.
func (f *fakePools) set(name string, hosts ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hosts == nil {
		f.hosts = map[string][]string{}
	}
	f.hosts[name] = hosts
}

// fail makes ListPools fail with err, or succeed again with nil.
func (f *fakePools) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}
