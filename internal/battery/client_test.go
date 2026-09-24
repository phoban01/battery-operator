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

package battery_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/fakebattery"
	"github.com/phoban01/battery-operator/internal/fakeflintlock"
)

const (
	testNamespace = "operator-ns"
	testTimeout   = 30 * time.Second
	hostA         = "host-a"
)

var testEpoch = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

// startBattery serves a fake battery on a loopback TCP port until the test
// ends. Zero cfg fields take the fake's defaults, except the reconcile
// interval, which is short so that Pools fill quickly on the wall clock.
func startBattery(t *testing.T, cfg fakebattery.Config) *fakebattery.Battery {
	t.Helper()
	if cfg.ReconcileInterval == 0 {
		cfg.ReconcileInterval = 10 * time.Millisecond
	}
	b := fakebattery.New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("fake battery: Serve: %v", err)
		}
	})
	select {
	case <-b.Ready():
	case err := <-done:
		t.Fatalf("fake battery: Serve: %v", err)
	case <-time.After(testTimeout):
		t.Fatal("fake battery did not start listening")
	}
	return b
}

// hostsWith returns the fake battery's Hosts with one fake flintlockd per
// name, reported at name:9090.
func hostsWith(t *testing.T, names ...string) *fakebattery.Hosts {
	t.Helper()
	hosts := fakebattery.NewHosts()
	for _, name := range names {
		fl := fakeflintlock.New(fakeflintlock.Config{Name: name})
		conn, err := fl.Conn()
		if err != nil {
			t.Fatalf("connecting to fake flintlockd %s: %v", name, err)
		}
		t.Cleanup(func() {
			_ = conn.Close()
			_ = fl.Close()
		})
		hosts.Add(name, name+":9090", conn)
	}
	return hosts
}

// dial connects to addr with test settings; the connection is closed when
// the test ends.
func dial(t *testing.T, cfg battery.Config) *battery.Connection {
	t.Helper()
	if cfg.ReconnectBase == 0 {
		cfg.ReconnectBase = 10 * time.Millisecond
	}
	if cfg.ReconnectMax == 0 {
		cfg.ReconnectMax = 50 * time.Millisecond
	}
	c, err := battery.Dial(cfg)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	t.Cleanup(cancel)
	return ctx
}

func ref(name string) battery.PoolRef {
	return battery.PoolRef{Name: name, Namespace: testNamespace}
}

func spec(name string, size int32, hosts ...string) battery.PoolSpec {
	minSize := int32(1)
	return battery.PoolSpec{
		Ref:            ref(name),
		Template:       &types.MicroVMSpec{Namespace: testNamespace, Vcpu: 1, MemoryInMb: 256},
		Size:           size,
		FlintlockHosts: hosts,
		Replenishment: battery.ReplenishmentStrategy{
			Type:    poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD,
			MinSize: &minSize,
		},
		CreateCommands:           []string{"/opt/setup.sh"},
		HookFailurePolicy:        poolmgrv1.HookFailurePolicy_DELETE_AND_REPLACE,
		HeartbeatInterval:        10 * time.Second,
		HeartbeatExpiryThreshold: 30 * time.Second,
	}
}

func TestPoolAdmin(t *testing.T) {
	ctx := testContext(t)
	b := startBattery(t, fakebattery.Config{})
	c := dial(t, battery.Config{Address: b.Addr()})

	want := spec("small", 0)
	created, err := c.CreatePool(ctx, want)
	if err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	if created.Spec.Ref != want.Ref || created.Spec.Size != 0 ||
		created.Spec.Replenishment.Type != want.Replenishment.Type ||
		created.Spec.Replenishment.MinSize == nil || *created.Spec.Replenishment.MinSize != 1 ||
		created.Spec.HeartbeatExpiryThreshold != want.HeartbeatExpiryThreshold ||
		created.Spec.Template.GetMemoryInMb() != 256 ||
		len(created.Spec.CreateCommands) != 1 {
		t.Errorf("CreatePool returned %+v, want the spec it was given", created.Spec)
	}
	if _, err := c.CreatePool(ctx, want); !errors.Is(err, battery.ErrAlreadyExists) {
		t.Errorf("CreatePool of an existing Pool: got %v, want ErrAlreadyExists", err)
	}

	want.Size = 2
	updated, err := c.UpdatePool(ctx, want)
	if err != nil {
		t.Fatalf("UpdatePool: %v", err)
	}
	if updated.Spec.Size != 2 {
		t.Errorf("UpdatePool returned size %d, want 2", updated.Spec.Size)
	}
	if _, err := c.UpdatePool(ctx, spec("missing", 1)); !errors.Is(err, battery.ErrNotFound) {
		t.Errorf("UpdatePool of an unknown Pool: got %v, want ErrNotFound", err)
	}

	got, err := c.GetPool(ctx, want.Ref)
	if err != nil {
		t.Fatalf("GetPool: %v", err)
	}
	if got.Spec.Size != 2 {
		t.Errorf("GetPool returned size %d, want 2", got.Spec.Size)
	}

	other := spec("other", 0)
	other.Ref.Namespace = "elsewhere"
	if _, err := c.CreatePool(ctx, other); err != nil {
		t.Fatalf("CreatePool in another namespace: %v", err)
	}
	pools, err := c.ListPools(ctx, testNamespace)
	if err != nil {
		t.Fatalf("ListPools: %v", err)
	}
	if len(pools) != 1 || pools[0].Spec.Ref != want.Ref {
		t.Errorf("ListPools(%s) = %v, want only %s", testNamespace, pools, want.Ref)
	}
	all, err := c.ListPools(ctx, "")
	if err != nil {
		t.Fatalf("ListPools of every namespace: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("ListPools of every namespace returned %d Pools, want 2", len(all))
	}

	if err := c.DeletePool(ctx, want.Ref); err != nil {
		t.Fatalf("DeletePool: %v", err)
	}
	if _, err := c.GetPool(ctx, want.Ref); !errors.Is(err, battery.ErrNotFound) {
		t.Errorf("GetPool of a deleted Pool: got %v, want ErrNotFound", err)
	}
	if err := c.DeletePool(ctx, want.Ref); !errors.Is(err, battery.ErrNotFound) {
		t.Errorf("DeletePool of an unknown Pool: got %v, want ErrNotFound", err)
	}
}

func TestLeaseAndEvents(t *testing.T) {
	ctx := testContext(t)
	b := startBattery(t, fakebattery.Config{Hosts: hostsWith(t, hostA)})
	c := dial(t, battery.Config{Address: b.Addr()})

	pool := ref("warm")
	events, err := c.Subscribe(ctx, battery.EventFilter{Pool: &pool})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = events.Close() }()
	if _, err := c.CreatePool(ctx, spec(pool.Name, 1, hostA)); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	for {
		e, err := events.Recv(ctx)
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if e.Pool != pool {
			t.Fatalf("an event of Pool %s reached a subscription to %s", e.Pool, pool)
		}
		if e.Type == poolmgrv1.EventType_VM_AVAILABLE {
			if e.VMUID == "" || e.At.IsZero() {
				t.Errorf("VM_AVAILABLE event %+v has no MicroVM or time", e)
			}
			break
		}
	}

	claim, err := c.ClaimVM(ctx, pool)
	if err != nil {
		t.Fatalf("ClaimVM: %v", err)
	}
	if claim.LeaseID == "" || claim.VMUID == "" {
		t.Errorf("ClaimVM returned %+v, want a lease id and a MicroVM uid", claim)
	}
	if claim.Host != (battery.HostRef{Name: hostA, Address: hostA + ":9090"}) {
		t.Errorf("ClaimVM returned Host %+v, want %s at %s:9090", claim.Host, hostA, hostA)
	}

	expires, err := c.Heartbeat(ctx, claim.LeaseID)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if !expires.After(time.Now()) {
		t.Errorf("Heartbeat returned expiry %v, want one in the future", expires)
	}

	checkListLeases(ctx, t, c, claim, pool, expires)

	if err := c.DeletePool(ctx, pool); !errors.Is(err, battery.ErrFailedPrecondition) {
		t.Errorf("DeletePool with a Lease outstanding: got %v, want ErrFailedPrecondition", err)
	}

	if err := c.ReleaseVM(ctx, claim.LeaseID); err != nil {
		t.Fatalf("ReleaseVM: %v", err)
	}
	if err := c.ReleaseVM(ctx, claim.LeaseID); !errors.Is(err, battery.ErrNotFound) {
		t.Errorf("ReleaseVM of a released Lease: got %v, want ErrNotFound", err)
	}
	if _, err := c.Heartbeat(ctx, claim.LeaseID); !errors.Is(err, battery.ErrNotFound) {
		t.Errorf("Heartbeat of a released Lease: got %v, want ErrNotFound", err)
	}
	if leases, err := c.ListLeases(ctx, nil); err != nil || len(leases) != 0 {
		t.Errorf("ListLeases after the release = (%v, %v), want none", leases, err)
	}
}

// checkListLeases reads claim's Lease of pool, which the last Heartbeat set
// to expire at expires, through every Pool's Leases and through pool's. It
// is read as the Heartbeat left it, since listing renews nothing, and a
// Pool battery does not know lists nothing, without an error.
func checkListLeases(ctx context.Context, t *testing.T, c *battery.Connection,
	claim *battery.Claim, pool battery.PoolRef, expires time.Time) {
	t.Helper()
	for _, filter := range []*battery.PoolRef{nil, &pool} {
		leases, err := c.ListLeases(ctx, filter)
		if err != nil {
			t.Fatalf("ListLeases(%v): %v", filter, err)
		}
		if len(leases) != 1 {
			t.Fatalf("ListLeases(%v) = %d leases, want 1", filter, len(leases))
		}
		got := leases[0]
		if got.LeaseID != claim.LeaseID || got.VMUID != claim.VMUID || got.Pool != pool ||
			!got.ExpiresAt.Equal(expires) || got.ClaimedAt.IsZero() || got.LastHeartbeatAt.Before(got.ClaimedAt) {
			t.Errorf("ListLeases(%v) = %+v, want lease %s of %s on %s expiring at %v",
				filter, got, claim.LeaseID, claim.VMUID, pool, expires)
		}
	}
	other := ref("unknown")
	if leases, err := c.ListLeases(ctx, &other); err != nil || len(leases) != 0 {
		t.Errorf("ListLeases of an unknown Pool = (%v, %v), want none and no error", leases, err)
	}
}

func TestEventStreamDropIsUnavailable(t *testing.T) {
	ctx := testContext(t)
	b := startBattery(t, fakebattery.Config{})
	c := dial(t, battery.Config{Address: b.Addr()})

	events, err := c.Subscribe(ctx, battery.EventFilter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = events.Close() }()

	// A Recv the caller gives up on returns the caller's error.
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := events.Recv(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Recv on an expired context: got %v, want context.DeadlineExceeded", err)
	}

	// The stream may not be registered with the fake yet; drop until it is.
	for {
		b.SetFaults(fakebattery.Faults{DropEventsStream: true})
		wait, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		_, err := events.Recv(wait)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) {
			continue
		}
		if !errors.Is(err, battery.ErrUnavailable) {
			t.Fatalf("Recv on a dropped stream: got %v, want ErrUnavailable", err)
		}
		break
	}
	if _, err := events.Recv(ctx); !errors.Is(err, battery.ErrUnavailable) {
		t.Errorf("Recv after the stream ended: got %v, want ErrUnavailable again", err)
	}
}

//= docs/requirements/06-deployment.md#battery-connection
//= type=test
//# The Operator SHALL map battery's gRPC status codes to errors
//# that distinguish a missing object, an exhausted Pool and an unavailable
//# battery.

// TestStatusCodesMapToSentinels covers DP-011: each of the three outcomes
// the controllers branch on comes back as its own sentinel, and as none of
// the others.
func TestStatusCodesMapToSentinels(t *testing.T) {
	ctx := testContext(t)
	b := startBattery(t, fakebattery.Config{})
	c := dial(t, battery.Config{Address: b.Addr()})

	// A Pool on no Host never fills, so claiming from it is exhausted.
	if _, err := c.CreatePool(ctx, spec("empty", 1)); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	sentinels := []error{battery.ErrNotFound, battery.ErrExhausted, battery.ErrUnavailable}
	check := func(what string, err, want error) {
		t.Helper()
		for _, s := range sentinels {
			if errors.Is(err, s) != (s == want) {
				t.Errorf("%s: got %v, want only %v", what, err, want)
				return
			}
		}
	}

	_, err := c.GetPool(ctx, ref("missing"))
	check("GetPool of an unknown Pool", err, battery.ErrNotFound)
	_, err = c.ClaimVM(ctx, ref("missing"))
	check("ClaimVM on an unknown Pool", err, battery.ErrNotFound)
	_, err = c.ClaimVM(ctx, ref("empty"))
	check("ClaimVM on an empty Pool", err, battery.ErrExhausted)

	_, err = c.CreatePool(ctx, battery.PoolSpec{Ref: ref("")})
	if !errors.Is(err, battery.ErrInvalid) {
		t.Errorf("CreatePool without a name: got %v, want ErrInvalid", err)
	}

	b.SetFaults(fakebattery.Faults{UnavailableFor: time.Hour})
	_, err = c.ClaimVM(ctx, ref("empty"))
	check("ClaimVM while battery is unavailable", err, battery.ErrUnavailable)
	// Opening a stream does not wait for battery's answer, so the refusal
	// can arrive on the first Recv instead.
	events, err := c.Subscribe(ctx, battery.EventFilter{})
	if err == nil {
		_, err = events.Recv(ctx)
		_ = events.Close()
	}
	check("Subscribe while battery is unavailable", err, battery.ErrUnavailable)

	// A battery that is not there at all is unavailable too.
	gone := dial(t, battery.Config{Address: closedAddress(t)})
	_, err = gone.ListPools(ctx, "")
	check("ListPools with no battery listening", err, battery.ErrUnavailable)
}

//= docs/requirements/06-deployment.md#battery-connection
//= type=test
//# The Operator SHALL call battery over gRPC on the loopback
//# address of DP-001, with a deadline on every unary call.

// TestDialRefusesAnAddressOffLoopback covers the loopback half of DP-010.
func TestDialRefusesAnAddressOffLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:50051", "127.1.2.3:1", "[::1]:50051", "localhost:50051", battery.DefaultAddress} {
		c, err := battery.Dial(battery.Config{Address: addr})
		if err != nil {
			t.Errorf("Dial(%q): %v, want it accepted", addr, err)
			continue
		}
		_ = c.Close()
	}
	for _, addr := range []string{"", "10.0.0.7:50051", "battery:50051", "0.0.0.0:50051", "[::]:50051", "127.0.0.1", "dns:///localhost:50051"} {
		if c, err := battery.Dial(battery.Config{Address: addr}); err == nil {
			_ = c.Close()
			t.Errorf("Dial(%q) succeeded, want it refused", addr)
		}
	}
}

//= docs/requirements/06-deployment.md#battery-connection
//= type=test
//# The Operator SHALL call battery over gRPC on the loopback
//# address of DP-001, with a deadline on every unary call.

// TestEveryUnaryCallHasADeadline covers the deadline half of DP-010: a
// battery that never answers ClaimVM, because its latency is measured on a
// clock that does not move, is given up on after the ClaimVM timeout, as
// ErrUnavailable, although the caller set no deadline. A caller's own
// earlier deadline still wins.
func TestEveryUnaryCallHasADeadline(t *testing.T) {
	clk := clock.NewFake(testEpoch)
	b := startBattery(t, fakebattery.Config{Clock: clk})
	c := dial(t, battery.Config{Address: b.Addr(), CallTimeout: time.Hour, ClaimTimeout: 200 * time.Millisecond})
	b.SetFaults(fakebattery.Faults{ClaimLatency: time.Hour})

	start := time.Now()
	_, err := c.ClaimVM(context.Background(), ref("any"))
	if !errors.Is(err, battery.ErrUnavailable) {
		t.Errorf("ClaimVM that battery never answers: got %v, want ErrUnavailable", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("ClaimVM returned after %v, want about the 200ms ClaimVM timeout", took)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.ClaimVM(ctx, ref("any")); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("ClaimVM past the caller's own deadline: got %v, want context.DeadlineExceeded", err)
	}
}

//= docs/requirements/06-deployment.md#battery-connection
//= type=test
//# While battery is unavailable, the Operator SHALL report itself
//# not ready on its readiness endpoint.

// TestReadinessFollowsBattery covers DP-012 through controller-runtime's
// readiness handler, with Check registered as the Operator's main does: not
// ready while battery answers UNAVAILABLE, ready again once it recovers,
// and not ready while nothing listens at battery's address.
func TestReadinessFollowsBattery(t *testing.T) {
	clk := clock.NewFake(testEpoch)
	b := startBattery(t, fakebattery.Config{Clock: clk})
	c := dial(t, battery.Config{Address: b.Addr()})

	ready := func(conn *battery.Connection) int {
		t.Helper()
		h := http.StripPrefix("/readyz", &healthz.Handler{Checks: map[string]healthz.Checker{"battery": conn.Check}})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		return rec.Code
	}

	if code := ready(c); code != http.StatusOK {
		t.Fatalf("readyz with battery answering: %d, want 200", code)
	}
	b.SetFaults(fakebattery.Faults{UnavailableFor: time.Minute})
	if code := ready(c); code == http.StatusOK {
		t.Errorf("readyz while battery is unavailable: %d, want not ready", code)
	}
	clk.Advance(time.Minute)
	if code := ready(c); code != http.StatusOK {
		t.Errorf("readyz once battery is available again: %d, want 200", code)
	}

	gone := dial(t, battery.Config{Address: closedAddress(t)})
	if code := ready(gone); code == http.StatusOK {
		t.Errorf("readyz with no battery listening: %d, want not ready", code)
	}
}

// TestSlowHandshakeStillConnects is the regression test, from
// flintlock-runner, for the connection attempt deadline. With
// WithConnectParams grpc-go gives each attempt MinConnectTimeout or the
// current backoff, whichever is longer; were MinConnectTimeout the 50ms
// ReconnectMax, a handshake slower than that, which a busy machine running
// the race detector produces now and then, would be cut off and retried
// with the same deadline forever. A proxy that holds every connection
// stands in for the busy machine.
func TestSlowHandshakeStillConnects(t *testing.T) {
	ctx := testContext(t)
	b := startBattery(t, fakebattery.Config{})
	slow := startSlowProxy(t, b.Addr(), 300*time.Millisecond)
	c := dial(t, battery.Config{Address: slow})

	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := c.ListPools(callCtx, testNamespace); err != nil {
		t.Fatalf("ListPools through a proxy that takes 300ms to connect: %v", err)
	}
}

// closedAddress returns a loopback address nothing listens on.
func closedAddress(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()
	return addr
}

// startSlowProxy forwards TCP connections to upstream, holding each one for
// delay before it dials upstream, so that the server's HTTP/2 preface
// reaches the client no sooner than delay after it connected.
func startSlowProxy(t *testing.T, upstream string, delay time.Duration) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("slow proxy: listen: %v", err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = lis.Close()
		wg.Wait()
	})
	wg.Go(func() {
		for {
			down, err := lis.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer func() { _ = down.Close() }()
				time.Sleep(delay)
				up, err := net.Dial("tcp", upstream)
				if err != nil {
					return
				}
				defer func() { _ = up.Close() }()
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(up, down); done <- struct{}{} }()
				go func() { _, _ = io.Copy(down, up); done <- struct{}{} }()
				<-done
			})
		}
	})
	return lis.Addr().String()
}
