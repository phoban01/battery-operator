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

package fakebattery

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	mvmv1 "github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1"
	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/fakeflintlock"
)

// Test support: fake flintlockd Hosts with knobs that make them misbehave,
// a harness that runs the fake battery on a fake clock, a small typed
// client over battery's generated clients, and event helpers.

const (
	testNamespace = "operator-ns"
	testInterval  = time.Second
	testTimeout   = 30 * time.Second

	hostA = "host-a"
	hostB = "host-b"
	hostC = "host-c"
	// setupHook is a create command.
	setupHook = "/opt/setup.sh"
)

var testEpoch = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

// testHost is one fake flintlockd and the connection the fake battery
// reaches it over. Client interceptors on that connection record every
// call and give the Host the misbehaviours a test needs; the flintlockd
// itself is the real fake.
type testHost struct {
	name string
	fl   *fakeflintlock.Server
	conn *grpc.ClientConn

	// deletions receives the uid of every MicroVM the Host deletes, so that
	// a test can wait for a deletion that has no Event of its own.
	deletions chan string
	// deleteEntered and createEntered announce every DeleteMicroVM and
	// every gated CreateMicroVM that reached the Host.
	deleteEntered chan struct{}
	createEntered chan struct{}

	mu        sync.Mutex
	created   []string
	deleted   []string
	attempted []string

	// stayPending reports every MicroVM PENDING to GetMicroVM.
	stayPending bool
	// createErr fails every CreateMicroVM; deleteErr every DeleteMicroVM.
	createErr error
	deleteErr error
	// deleteGate, while non-nil, holds every DeleteMicroVM until it is
	// closed, so a test can keep a deletion in flight.
	deleteGate chan struct{}
	// createGate, while non-nil, holds every CreateMicroVM until it is
	// closed or the caller gives up. Like flintlockd, the Host makes the
	// MicroVM either way: a caller that gave up gets its context's error
	// and no uid.
	createGate chan struct{}
	// execErr fails the opening of every exec stream.
	execErr error
}

func newTestHost(t *testing.T, name string) *testHost {
	t.Helper()
	h := &testHost{
		name:          name,
		fl:            fakeflintlock.New(fakeflintlock.Config{Name: name}),
		deletions:     make(chan string, 64),
		deleteEntered: make(chan struct{}, 64),
		createEntered: make(chan struct{}, 64),
	}
	conn, err := h.fl.Conn(grpc.WithChainUnaryInterceptor(h.unary), grpc.WithChainStreamInterceptor(h.stream))
	if err != nil {
		t.Fatalf("connecting to fake flintlockd %s: %v", name, err)
	}
	h.conn = conn
	t.Cleanup(func() {
		_ = conn.Close()
		_ = h.fl.Close()
	})
	return h
}

func (h *testHost) unary(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	switch method {
	case mvmv1.MicroVM_CreateMicroVM_FullMethodName:
		return h.create(ctx, func(ctx context.Context) error { return invoker(ctx, method, req, reply, cc, opts...) }, reply)
	case mvmv1.MicroVM_DeleteMicroVM_FullMethodName:
		return h.delete(ctx, func(ctx context.Context) error { return invoker(ctx, method, req, reply, cc, opts...) }, req)
	case mvmv1.MicroVM_GetMicroVM_FullMethodName:
		err := invoker(ctx, method, req, reply, cc, opts...)
		h.mu.Lock()
		pending := h.stayPending
		h.mu.Unlock()
		if resp, ok := reply.(*mvmv1.GetMicroVMResponse); ok && err == nil && pending {
			resp.GetMicrovm().GetStatus().State = types.MicroVMStatus_PENDING
		}
		return err
	}
	return invoker(ctx, method, req, reply, cc, opts...)
}

func (h *testHost) stream(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	if method == execv1.MicroVMExec_ExecCommand_FullMethodName {
		h.mu.Lock()
		err := h.execErr
		h.mu.Unlock()
		if err != nil {
			return nil, err
		}
	}
	return streamer(ctx, desc, cc, method, opts...)
}

func (h *testHost) create(ctx context.Context, invoke func(context.Context) error, reply any) error {
	h.mu.Lock()
	gate, createErr := h.createGate, h.createErr
	h.mu.Unlock()
	if createErr != nil {
		return createErr
	}
	record := func(err error) {
		if err != nil {
			return
		}
		uid := reply.(*mvmv1.CreateMicroVMResponse).GetMicrovm().GetSpec().GetUid()
		h.mu.Lock()
		h.created = append(h.created, uid)
		h.mu.Unlock()
	}
	if gate == nil {
		err := invoke(ctx)
		record(err)
		return err
	}
	select {
	case h.createEntered <- struct{}{}:
	default:
	}
	select {
	case <-gate:
	case <-ctx.Done():
	}
	err := invoke(context.WithoutCancel(ctx))
	record(err)
	if err == nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (h *testHost) delete(ctx context.Context, invoke func(context.Context) error, req any) error {
	uid := req.(*mvmv1.DeleteMicroVMRequest).GetUid()
	h.mu.Lock()
	h.attempted = append(h.attempted, uid)
	gate := h.deleteGate
	h.mu.Unlock()
	select {
	case h.deleteEntered <- struct{}{}:
	default:
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	h.mu.Lock()
	deleteErr := h.deleteErr
	h.mu.Unlock()
	if deleteErr != nil {
		return deleteErr
	}
	if err := invoke(ctx); err != nil {
		return err
	}
	h.mu.Lock()
	h.deleted = append(h.deleted, uid)
	h.mu.Unlock()
	select {
	case h.deletions <- uid:
	default:
	}
	return nil
}

// live is how many MicroVMs are on the Host.
func (h *testHost) live() int { return len(h.fl.MicroVMs()) }

// has reports whether uid is on the Host.
func (h *testHost) has(uid string) bool {
	return slices.ContainsFunc(h.fl.MicroVMs(), func(vm *types.MicroVM) bool { return vm.GetSpec().GetUid() == uid })
}

// deleteAttempts is how many DeleteMicroVM calls for uid reached the Host,
// including the ones it refused and the ones still held by deleteGate.
func (h *testHost) deleteAttempts(uid string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, got := range h.attempted {
		if got == uid {
			n++
		}
	}
	return n
}

func (h *testHost) counts() (created, deleted int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.created), len(h.deleted)
}

// commands lists the commands the Host ran, in order.
func (h *testHost) commands() []string {
	execs := h.fl.Execs()
	out := make([]string, 0, len(execs))
	for _, start := range execs {
		out = append(out, start.GetCmd())
	}
	return out
}

func (h *testHost) set(fn func(*testHost)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn(h)
}

// harness runs one fake battery on a fake clock with fake flintlockd
// Hosts, a client and a subscription to every event.
type harness struct {
	t      *testing.T
	ctx    context.Context
	clk    *clock.Fake
	hosts  *Hosts
	stubs  map[string]*testHost
	b      *Battery
	client *testClient
	events *eventStream
	// seen counts every event type waitEvent has read.
	seen map[poolmgrv1.EventType]int

	// cancel ends the fake's context; runDone carries Run's result. stop
	// uses them, and the cleanup calls stop.
	cancel   context.CancelFunc
	runDone  chan error
	stopOnce sync.Once
}

// newHarness builds and starts the fake. Zero cfg fields get test defaults:
// the fake clock, a one second reconcile interval and one fake flintlockd
// per name, reported at name:9090.
func newHarness(t *testing.T, cfg Config, hostNames ...string) *harness {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	h := &harness{t: t, ctx: ctx, clk: clock.NewFake(testEpoch), hosts: NewHosts(), stubs: make(map[string]*testHost), seen: make(map[poolmgrv1.EventType]int)}
	for _, name := range hostNames {
		s := newTestHost(t, name)
		h.stubs[name] = s
		h.hosts.Add(name, name+":9090", s.conn)
	}
	if cfg.Clock == nil {
		cfg.Clock = h.clk
	}
	if cfg.Hosts == nil {
		cfg.Hosts = h.hosts
	}
	if cfg.ReconcileInterval == 0 {
		cfg.ReconcileInterval = testInterval
	}
	h.b = New(cfg)
	h.client = newTestClient(t, h.b)
	events, err := h.client.Subscribe(ctx, nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	h.events = events

	h.cancel = cancel
	h.runDone = make(chan error, 1)
	go func() { h.runDone <- h.b.Run(ctx) }()
	t.Cleanup(func() {
		h.stop()
		events.Close()
	})
	return h
}

// stop ends the fake and waits for Run to return, which waits in turn for
// every goroutine the control loop spawned. A test calls it when it needs
// that barrier before asserting on what the Hosts saw; the cleanup calls it
// otherwise. It is idempotent.
func (h *harness) stop() {
	h.t.Helper()
	h.stopOnce.Do(func() {
		h.cancel()
		if err := <-h.runDone; err != nil {
			h.t.Errorf("Run returned %v", err)
		}
	})
}

func (h *harness) ref(name string) PoolRef {
	return PoolRef{Name: name, Namespace: testNamespace}
}

// spec is a valid IMMEDIATE_ON_LEASE PoolSpec on the given Hosts.
func (h *harness) spec(name string, size int32, hosts ...string) poolSpec {
	return poolSpec{
		Ref:                      h.ref(name),
		Template:                 &types.MicroVMSpec{Namespace: testNamespace, Vcpu: 1, MemoryInMb: 256},
		Size:                     size,
		FlintlockHosts:           hosts,
		Replenishment:            replenishment{Type: poolmgrv1.ReplenishmentStrategyType_IMMEDIATE_ON_LEASE},
		HookFailurePolicy:        poolmgrv1.HookFailurePolicy_DELETE_AND_REPLACE,
		HeartbeatInterval:        10 * time.Second,
		HeartbeatExpiryThreshold: 30 * time.Second,
	}
}

// createPool declares a Pool and waits until it is full.
func (h *harness) createPool(spec poolSpec) {
	h.t.Helper()
	if _, err := h.client.CreatePool(h.ctx, spec); err != nil {
		h.t.Fatalf("CreatePool(%s): %v", spec.Ref, err)
	}
	for range spec.Size {
		h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
	}
}

// tick waits for the control loop to arm its timer, then fires it.
func (h *harness) tick() {
	h.t.Helper()
	h.waitTimers(1)
	h.clk.Advance(testInterval)
}

// advance moves the clock by d, which fires the control loop's timer on the
// way when one was due, and waits for the loop to arm it again, so that the
// tick that ran at the new time has finished by the time advance returns.
func (h *harness) advance(d time.Duration) {
	h.t.Helper()
	h.clk.Advance(d)
	h.waitTimers(1)
}

// waitEvent reads events until one of type typ arrives and returns it.
func (h *harness) waitEvent(typ poolmgrv1.EventType) *event {
	h.t.Helper()
	got := h.collectUntil(typ)
	return got[len(got)-1]
}

// collectUntil reads events until one of type typ arrives and returns every
// event it read, in delivery order with the match last. Events of one Pool
// arrive in order, so the slice is proof of what did and did not happen
// before the match.
func (h *harness) collectUntil(typ poolmgrv1.EventType) []*event {
	h.t.Helper()
	var got []*event
	for {
		e, err := h.events.Recv()
		if err != nil {
			h.t.Fatalf("waiting for %s after %d events: %v", typ, len(got), err)
		}
		h.seen[e.Type]++
		got = append(got, e)
		if e.Type == typ {
			return got
		}
	}
}

// waitDeletion blocks until host reports that it deleted uid. Deletions that
// follow an Event are already visible when the Event arrives; this is for the
// ones that have no Event of their own, such as the deletion the hook
// failure policy performs.
func (h *harness) waitDeletion(host *testHost, uid string) {
	h.t.Helper()
	for {
		select {
		case got := <-host.deletions:
			if got == uid {
				return
			}
		case <-h.ctx.Done():
			h.t.Fatalf("waiting for host %s to delete %s: %v", host.name, uid, h.ctx.Err())
		}
	}
}

// waitDeleteCall blocks until one more DeleteMicroVM call has entered host,
// whether or not the Host answers it.
func (h *harness) waitDeleteCall(host *testHost) {
	h.t.Helper()
	select {
	case <-host.deleteEntered:
	case <-h.ctx.Done():
		h.t.Fatalf("waiting for a delete call on host %s: %v", host.name, h.ctx.Err())
	}
}

// eventOfType returns the first event of type typ in events.
func eventOfType(t *testing.T, events []*event, typ poolmgrv1.EventType) *event {
	t.Helper()
	for _, e := range events {
		if e.Type == typ {
			return e
		}
	}
	t.Fatalf("no %s event among %s", typ, eventTypes(events))
	return nil
}

// eventTypes renders the types of events for a failure message.
func eventTypes(events []*event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Type.String())
	}
	return out
}

// count returns how many events of type typ are in events.
func count(events []*event, typ poolmgrv1.EventType) int {
	n := 0
	for _, e := range events {
		if e.Type == typ {
			n++
		}
	}
	return n
}

// pool fetches a Pool's current view.
func (h *harness) pool(name string) *pool {
	h.t.Helper()
	p, err := h.client.GetPool(h.ctx, h.ref(name))
	if err != nil {
		h.t.Fatalf("GetPool(%s): %v", name, err)
	}
	return p
}

// claim claims from a Pool and waits for VM_CLAIMED so that the state the
// claim changed under the lock is visible to the next GetPool.
func (h *harness) claim(name string) *claimed {
	h.t.Helper()
	c, err := h.client.ClaimVM(h.ctx, h.ref(name))
	if err != nil {
		h.t.Fatalf("ClaimVM(%s): %v", name, err)
	}
	h.waitEvent(poolmgrv1.EventType_VM_CLAIMED)
	return c
}

// release releases a Lease and waits for the deletion to complete.
func (h *harness) release(leaseID string) {
	h.t.Helper()
	if err := h.client.ReleaseVM(h.ctx, leaseID); err != nil {
		h.t.Fatalf("ReleaseVM(%s): %v", leaseID, err)
	}
	h.waitEvent(poolmgrv1.EventType_VM_DELETED_ON_RELEASE)
}

func hostOf(records []VMRecord) map[string]int {
	out := make(map[string]int)
	for _, r := range records {
		out[r.Host]++
	}
	return out
}

// waitTimers blocks until at least n timers are armed on the fake clock, so
// that a test never fires a timer the code under test has not set yet.
func (h *harness) waitTimers(n int) {
	h.t.Helper()
	if err := h.clk.BlockUntil(h.ctx, n); err != nil {
		h.t.Fatalf("waiting for %d timers: %v", n, err)
	}
}

// rawConn is the harness's connection to the fake, for tests that use the
// generated clients directly.
func (h *harness) rawConn() *grpc.ClientConn { return h.client.conn }

// rawLease is the generated Lease client, for tests that assert on the
// wire messages.
func (h *harness) rawLease() poolmgrv1.LeaseClient { return h.client.lease }

// warned is how many Leases of a Pool the control loop still has an
// expiry warning recorded against. It is bookkeeping no RPC exposes, and a
// long-running fake must not accumulate it.
func (h *harness) warned(name string) int {
	h.t.Helper()
	h.b.mu.Lock()
	defer h.b.mu.Unlock()
	ps, ok := h.b.pools[keyOf(h.ref(name))]
	if !ok {
		h.t.Fatalf("pool %s is not declared", name)
	}
	return len(ps.warned)
}

// vmHosts counts the fake's MicroVMs per Host.
func (h *harness) vmHosts() map[string]int {
	h.t.Helper()
	return hostOf(h.b.VMs())
}

// uids lists the uids of the fake's MicroVMs in creation order, skipping any
// whose CreateMicroVM has not returned.
func (h *harness) uids() []string {
	h.t.Helper()
	var out []string
	for _, r := range h.b.VMs() {
		if r.UID != "" {
			out = append(out, r.UID)
		}
	}
	return out
}

// payloadOf decodes an event's payload_json.
func payloadOf(t *testing.T, e *event) map[string]any {
	t.Helper()
	if len(e.Payload) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(e.Payload, &m); err != nil {
		t.Fatalf("payload of %s: %v", e.Type, err)
	}
	return m
}

// statusCode is the gRPC code of err, or codes.OK for a nil error.
func statusCode(t *testing.T, err error) codes.Code {
	t.Helper()
	if err == nil {
		return codes.OK
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("not a gRPC status error: %v", err)
	}
	return st.Code()
}

// assertDeleted fails unless uid was deleted on the Host and is gone from it.
func assertDeleted(t *testing.T, host *testHost, uid string) {
	t.Helper()
	if host.has(uid) {
		t.Fatalf("microvm %s is still on host %s", uid, host.name)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if !slices.Contains(host.deleted, uid) {
		t.Fatalf("microvm %s was never deleted on host %s (deleted: %v)", uid, host.name, host.deleted)
	}
}

// minSize is the address of a MIN_SIZE_THRESHOLD min_size.
func minSize(n int32) *int32 { return &n }
