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
	"fmt"
	"maps"
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

	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/reconcile"
)

// The timings of these tests: different, so that a controller that
// confuses them fails.
const (
	settle      = 30 * time.Second
	removeRetry = 20 * time.Second
	mountPoll   = 3 * time.Second
)

// Two Hosts and their flintlockd addresses.
const (
	nodeA = "node-a"
	nodeB = "node-b"
	addrA = "10.0.0.1:9090"
	addrB = "10.0.0.2:9090"
	addrC = "10.0.0.3:9090"
)

// Two client certificates: the one battery starts with, and its renewal.
var (
	certificate1 = []byte("-----BEGIN CERTIFICATE-----\none\n-----END CERTIFICATE-----\n")
	certificate2 = []byte("-----BEGIN CERTIFICATE-----\ntwo\n-----END CERTIFICATE-----\n")
)

// The HostAdmin calls, as fakeAdmin records them.
const (
	opAddHost      = "AddHost"
	opUpdateHost   = "UpdateHost"
	opCordonHost   = "CordonHost"
	opUncordonHost = "UncordonHost"
	opRemoveHost   = "RemoveHost"
	opGetHost      = "GetHost"
)

// call is one call the controller made to battery's HostAdmin.
type call struct {
	op, name string
	// skipValidation is AddHost's and UpdateHost's.
	skipValidation bool
	// published is what the HostSet held when the call came.
	published Hosts
}

func (c call) String() string { return c.op + " " + c.name }

// fakeAdmin is battery's HostAdmin service with battery's rules for the
// calls the Inventory Controller makes (BA-080, BA-083, BA-085, BA-088,
// BA-089), in memory. The fake battery (internal/fakebattery) serves the
// same over gRPC; this one lets a test see every call and fail any.
type fakeAdmin struct {
	mu    sync.Mutex
	hosts map[string]battery.Host
	// hidden are Hosts battery holds that ListHosts leaves out, so that a
	// test can reach AddHost's ALREADY_EXISTS (IN-019).
	hidden map[string]bool
	// naming is, for each Host, the Pools that name it; vms the MicroVMs
	// on it. RemoveHost refuses while either is not empty (BA-083).
	naming map[string][]string
	vms    map[string]int
	// unreachable are addresses AddHost and UpdateHost fail to check.
	unreachable map[string]bool
	// fail fails the call "op name" with its error.
	fail map[string]error
	// calls are every call that changed or read one Host, in order.
	calls []call
	// set is the HostSet, to record what it held at each call.
	set *HostSet
}

func newFakeAdmin(set *HostSet) *fakeAdmin {
	return &fakeAdmin{
		hosts: map[string]battery.Host{}, hidden: map[string]bool{}, naming: map[string][]string{},
		vms: map[string]int{}, unreachable: map[string]bool{}, fail: map[string]error{}, set: set,
	}
}

// record logs a call and returns the error a test set for it.
func (f *fakeAdmin) record(op, name string, skip bool) error {
	var published Hosts
	if list, err := f.set.Hosts(); err == nil {
		published = Hosts{}
		for _, h := range list {
			published[h.Name] = h.Address
		}
	}
	f.calls = append(f.calls, call{op: op, name: name, skipValidation: skip, published: published})
	return f.fail[op+" "+name]
}

func (f *fakeAdmin) ListHosts(context.Context) ([]*battery.HostStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail["ListHosts"]; err != nil {
		return nil, err
	}
	var out []*battery.HostStatus
	for _, name := range slices.Sorted(maps.Keys(f.hosts)) {
		if !f.hidden[name] {
			out = append(out, &battery.HostStatus{Host: f.hosts[name], VMCount: int32(f.vms[name])})
		}
	}
	return out, nil
}

func (f *fakeAdmin) GetHost(_ context.Context, name string) (*battery.HostStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(opGetHost, name, false); err != nil {
		return nil, err
	}
	h, ok := f.hosts[name]
	if !ok {
		return nil, fmt.Errorf("%w: host %s", battery.ErrNotFound, name)
	}
	return &battery.HostStatus{Host: h, VMCount: int32(f.vms[name])}, nil
}

func (f *fakeAdmin) AddHost(_ context.Context, host battery.Host, skip bool) (*battery.Host, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(opAddHost, host.Name, skip); err != nil {
		return nil, err
	}
	if _, ok := f.hosts[host.Name]; ok {
		return nil, fmt.Errorf("%w: host %s", battery.ErrAlreadyExists, host.Name)
	}
	if !skip && f.unreachable[host.Address] {
		return nil, fmt.Errorf("%w: host %s unreachable", battery.ErrFailedPrecondition, host.Name)
	}
	host.Cordoned, host.CordonedReason = false, ""
	f.hosts[host.Name] = host
	return &host, nil
}

func (f *fakeAdmin) UpdateHost(_ context.Context, host battery.Host, skip bool) (*battery.Host, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(opUpdateHost, host.Name, skip); err != nil {
		return nil, err
	}
	old, ok := f.hosts[host.Name]
	if !ok {
		return nil, fmt.Errorf("%w: host %s", battery.ErrNotFound, host.Name)
	}
	if !skip && f.unreachable[host.Address] {
		return nil, fmt.Errorf("%w: host %s unreachable", battery.ErrFailedPrecondition, host.Name)
	}
	host.Cordoned, host.CordonedReason = old.Cordoned, old.CordonedReason
	f.hosts[host.Name] = host
	return &host, nil
}

func (f *fakeAdmin) RemoveHost(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(opRemoveHost, name, false); err != nil {
		return err
	}
	if _, ok := f.hosts[name]; !ok {
		return fmt.Errorf("%w: host %s", battery.ErrNotFound, name)
	}
	if len(f.naming[name]) > 0 || f.vms[name] > 0 {
		return fmt.Errorf("%w: host %s still in use", battery.ErrFailedPrecondition, name)
	}
	delete(f.hosts, name)
	return nil
}

func (f *fakeAdmin) CordonHost(_ context.Context, name, reason string) (*battery.Host, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(opCordonHost, name, false); err != nil {
		return nil, err
	}
	h, ok := f.hosts[name]
	if !ok {
		return nil, fmt.Errorf("%w: host %s is not registered", battery.ErrFailedPrecondition, name)
	}
	h.Cordoned, h.CordonedReason = true, reason
	f.hosts[name] = h
	return &h, nil
}

func (f *fakeAdmin) UncordonHost(_ context.Context, name string) (*battery.Host, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(opUncordonHost, name, false); err != nil {
		return nil, err
	}
	h, ok := f.hosts[name]
	if !ok {
		return nil, fmt.Errorf("%w: host %s is not registered", battery.ErrFailedPrecondition, name)
	}
	h.Cordoned, h.CordonedReason = false, ""
	f.hosts[name] = h
	return &h, nil
}

// put makes battery hold a Host, as if added earlier.
func (f *fakeAdmin) put(h battery.Host) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hosts[h.Name] = h
}

// held is battery's Host of that name, and whether it holds one.
func (f *fakeAdmin) held(name string) (battery.Host, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.hosts[name]
	return h, ok
}

// ops is the calls made so far, as "op name".
func (f *fakeAdmin) ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.String())
	}
	return out
}

// reset forgets the calls made so far.
func (f *fakeAdmin) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// fakeMount is the Operator's mount of battery's client certificate.
type fakeMount struct {
	mu   sync.Mutex
	cert []byte
}

func (m *fakeMount) ClientCertificate() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cert, nil
}

func (m *fakeMount) set(cert []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cert = cert
}

// harness is the Inventory Controller against the fake client, a fake
// clock and fakeAdmin: what InventoryReconciler builds, without the
// manager.
type harness struct {
	t       *testing.T
	c       client.WithWatch
	clock   *clock.Fake
	state   *State
	hosts   *HostSet
	admin   *fakeAdmin
	mount   *fakeMount
	options Options
	// cert is the client certificate its Secret holds, nil for none.
	cert []byte
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// newHarness starts a harness with battery holding no Hosts, and the Nodes
// given. The client certificate's Secret and the mount hold certificate1.
func newHarness(t *testing.T, nodes ...*corev1.Node) *harness {
	t.Helper()
	objs := make([]client.Object, 0, len(nodes))
	for _, n := range nodes {
		objs = append(objs, n)
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).Build()
	hosts := NewHostSet()
	// As if the Operator had made battery read certificate1 already; the
	// tests of Reload start from NewState.
	state := NewState()
	state.reloaded = Digest(certificate1)
	return &harness{
		t:       t,
		c:       c,
		clock:   clock.NewFake(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)),
		state:   state,
		hosts:   hosts,
		admin:   newFakeAdmin(hosts),
		mount:   &fakeMount{cert: certificate1},
		options: Options{SettleTime: settle, RemoveRetry: removeRetry, MountPoll: mountPoll},
		cert:    certificate1,
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
	listed, err := h.admin.ListHosts(ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	held := map[string]battery.Host{}
	for _, st := range listed {
		held[st.Host.Name] = st.Host
	}
	return &Scope{
		Nodes:             nodes.Items,
		Held:              held,
		ClientCertificate: h.cert,
		State:             h.state,
		HostSet:           h.hosts,
		Admin:             h.admin,
		Mount:             h.mount,
		Options:           h.options,
		Log:               logr.Discard(),
		Clock:             h.clock,
	}
}

// reconcile runs the whole chain once.
func (h *harness) reconcile() (reconcile.Result, error) {
	h.t.Helper()
	return NewChain().Run(context.Background(), h.scope())
}

// mustReconcile runs the chain and fails the test on an error.
func (h *harness) mustReconcile() reconcile.Result {
	h.t.Helper()
	r, err := h.reconcile()
	if err != nil {
		h.t.Fatalf("reconcile: %v", err)
	}
	return r
}

// advance moves the clock on by d and reconciles, as the requeue would.
func (h *harness) advance(d time.Duration) reconcile.Result {
	h.t.Helper()
	h.clock.Advance(d)
	return h.mustReconcile()
}

// settled reconciles, waits the settle time and reconciles again: a
// change made before it reaches battery.
func (h *harness) settled() {
	h.t.Helper()
	h.mustReconcile()
	h.advance(settle)
}

// changeA changes node-a in the fake client.
func (h *harness) changeA(change func(*corev1.Node)) {
	h.t.Helper()
	ctx := context.Background()
	n := &corev1.Node{}
	if err := h.c.Get(ctx, client.ObjectKey{Name: nodeA}, n); err != nil {
		h.t.Fatal(err)
	}
	change(n)
	if err := h.c.Update(ctx, n); err != nil {
		h.t.Fatal(err)
	}
}

// remove deletes a Node from the fake client.
func (h *harness) remove(name string) {
	h.t.Helper()
	if err := h.c.Delete(context.Background(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
		h.t.Fatal(err)
	}
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

// heldHost is a Host battery holds, uncordoned, with the TLS settings of
// IN-004.
func heldHost(name, addr string) battery.Host {
	return battery.Host{Name: name, Address: addr, TLS: TLS}
}

// Changes to a Node.
func cordon(n *corev1.Node)   { n.Spec.Unschedulable = true }
func uncordon(n *corev1.Node) { n.Spec.Unschedulable = false }
func notReady(n *corev1.Node) { n.Annotations[AnnotationReady] = "false" }
func noAddress(n *corev1.Node) {
	delete(n.Annotations, AnnotationFlintlockdAddress)
}
func moveTo(addr string) func(*corev1.Node) {
	return func(n *corev1.Node) { n.Annotations[AnnotationFlintlockdAddress] = addr }
}
