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
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/phoban01/battery-operator/internal/clock"
)

// Defaults for zero Config fields and internal pacing.
const (
	defaultListen            = "127.0.0.1:0"
	defaultReconcileInterval = time.Second
	defaultReadyTimeout      = 60 * time.Second
	defaultEventReplay       = 100
	// createPollInterval paces GetMicroVM while a MicroVM boots and the
	// guest-agent readiness probe before create hooks. The first check is
	// immediate, so an instant Host needs no clock movement.
	createPollInterval = 250 * time.Millisecond
	// shutdownTimeout bounds the deletion of every MicroVM when Run stops.
	shutdownTimeout = 30 * time.Second
	// loopbackBuffer is the in-memory listener's buffer size.
	loopbackBuffer = 1 << 20
)

// ErrAlreadyRunning is returned by Run and Serve when the fake has already
// been started; a Battery runs once.
var ErrAlreadyRunning = errors.New("fake battery: already running or stopped")

// ErrStopped is returned by Conn once the fake has shut down. The fake
// keeps nothing across a shutdown, so a connection to it could only fail;
// it says so plainly. A connection taken before the shutdown fails with
// UNAVAILABLE from then on, as one to a real battery that went away does.
var ErrStopped = errors.New("fake battery: shut down")

// Placement names how the fake spreads MicroVMs over a Pool's
// flintlock_hosts.
type Placement string

// Placement strategies. PlacementLeastVMs is the default and is battery's
// PickHost; PlacementRoundRobin cycles through the Hosts.
const (
	PlacementLeastVMs   Placement = "least_vms"
	PlacementRoundRobin Placement = "round_robin"
)

// Config is the static configuration of the fake battery. Zero fields take
// the defaults given on each.
type Config struct {
	// Listen is Serve's gRPC listen address; empty is a free loopback port.
	Listen string
	// Hosts resolves flintlock_hosts names to the flintlockd connections
	// the fake creates, places and deletes MicroVMs through. Nil knows no
	// Host, so Pools declared against it never fill.
	Hosts *Hosts
	// Clock drives Lease expiry, the control loop and injected latency;
	// nil is the wall clock.
	Clock clock.Clock
	// Placement selects the placement strategy; empty is PlacementLeastVMs.
	Placement Placement
	// ReconcileInterval is how often Pools are topped up and expired Leases
	// swept; zero is one second.
	ReconcileInterval time.Duration
	// ReadyTimeout bounds the wait for a created MicroVM to boot and for
	// its guest agent to answer before its create hooks; zero is a minute.
	ReadyTimeout time.Duration
	// EventReplay is how many recent events per Pool a new subscriber is
	// replayed; zero is 100.
	EventReplay int
	// Log receives the fake's warnings; the zero Logger discards them.
	Log logr.Logger
}

// Battery is the fake battery. Build it with New, then Serve it, or Run it
// and use it through Conn.
type Battery struct {
	cfg Config
	log logr.Logger

	// mu guards every field below it up to events. Host calls are never made
	// while it is held.
	mu               sync.Mutex
	faults           Faults
	unavailableUntil time.Time
	pools            map[poolKey]*poolState
	vms              map[int64]*vmState
	vmByUID          map[string]*vmState
	leases           map[string]*leaseState
	nextVM           int64
	// runCtx is the control loop's context: nil before Run, done after it.
	runCtx context.Context
	// started is set by Run, stopped once it has begun shutting down. Both
	// are read under mu, so a caller that holds it and finds the fake
	// running can start background work knowing stop has not begun waiting
	// for it.
	started bool
	stopped bool
	// serving is claimed by Serve before it listens, so that the single
	// listener and the single close of ready belong to one call.
	serving bool
	wg      sync.WaitGroup

	events *eventBus
	// kick wakes the control loop for an immediate reconcile.
	kick chan struct{}

	addrMu sync.Mutex
	addr   string
	// ready is closed once Serve is listening.
	ready chan struct{}

	// loop is the in-memory server behind Conn. New builds it, before any
	// goroutine can see the Battery, and nothing writes it afterwards.
	loop *loopback
}

// loopback is the in-memory gRPC server behind Conn.
type loopback struct {
	lis *bufconn.Listener
	srv *grpc.Server
}

// New builds a fake battery from cfg, with the defaults documented on
// Config.
func New(cfg Config) *Battery {
	if cfg.Placement == "" {
		cfg.Placement = PlacementLeastVMs
	}
	if cfg.Listen == "" {
		cfg.Listen = defaultListen
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Real{}
	}
	if cfg.ReconcileInterval <= 0 {
		cfg.ReconcileInterval = defaultReconcileInterval
	}
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = defaultReadyTimeout
	}
	if cfg.EventReplay <= 0 {
		cfg.EventReplay = defaultEventReplay
	}
	if cfg.Hosts == nil {
		cfg.Hosts = NewHosts()
	}
	if cfg.Log.GetSink() == nil {
		cfg.Log = logr.Discard()
	}
	b := &Battery{
		cfg:     cfg,
		log:     cfg.Log.WithName("fake-battery"),
		pools:   make(map[poolKey]*poolState),
		vms:     make(map[int64]*vmState),
		vmByUID: make(map[string]*vmState),
		leases:  make(map[string]*leaseState),
		events:  newEventBus(cfg.EventReplay),
		kick:    make(chan struct{}, 1),
		ready:   make(chan struct{}),
	}
	lis := bufconn.Listen(loopbackBuffer)
	srv := b.newGRPCServer()
	go func() { _ = srv.Serve(lis) }()
	b.loop = &loopback{lis: lis, srv: srv}
	return b
}

// Config returns the configuration the fake was built with, defaults
// applied.
func (b *Battery) Config() Config { return b.cfg }

// Serve listens on Config.Listen, runs the control loop and serves the
// three gRPC services until ctx is cancelled. It returns nil after a clean
// shutdown, during which every MicroVM the fake created is deleted from its
// Host. A Battery serves once.
func (b *Battery) Serve(ctx context.Context) error {
	if err := b.claimServe(); err != nil {
		return err
	}
	lis, err := net.Listen("tcp", b.cfg.Listen)
	if err != nil {
		return fmt.Errorf("fake battery: listen %s: %w", b.cfg.Listen, err)
	}
	b.addrMu.Lock()
	b.addr = lis.Addr().String()
	b.addrMu.Unlock()
	close(b.ready)

	srv := b.newGRPCServer()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(lis) }()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- b.Run(runCtx) }()

	select {
	case <-ctx.Done():
		srv.Stop()
		<-serveErr
		return <-runErr
	case err := <-serveErr:
		cancel()
		<-runErr
		return fmt.Errorf("fake battery: serve: %w", err)
	case err := <-runErr:
		// Cancelling ctx cancels runCtx too, so at a normal shutdown both
		// this case and ctx.Done() become ready and the select picks at
		// random. A control loop that stopped because it was told to is
		// not the control loop stopping early.
		srv.Stop()
		<-serveErr
		if ctx.Err() != nil || err != nil {
			return err
		}
		return errors.New("fake battery: control loop stopped while serving")
	}
}

// claimServe reserves this Battery's single serve, so that a second Serve
// returns ErrAlreadyRunning instead of binding a second listener and
// closing the already closed ready channel.
func (b *Battery) claimServe() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.serving || b.started || b.stopped {
		return ErrAlreadyRunning
	}
	b.serving = true
	return nil
}

// Addr is the bound listen address once Serve is running, empty before.
func (b *Battery) Addr() string {
	b.addrMu.Lock()
	defer b.addrMu.Unlock()
	return b.addr
}

// Ready is closed once Serve is listening, so a caller that started Serve
// on another goroutine can wait for Addr without polling. It never closes
// if Serve fails to listen; select on Serve's result as well.
func (b *Battery) Ready() <-chan struct{} { return b.ready }

// Run drives the control loop without a network listener: it tops Pools
// up, expires Leases and retries deferred deletions every
// Config.ReconcileInterval on Config.Clock, and immediately after a Pool is
// created or updated. It blocks until ctx is cancelled, then waits for
// in-flight provisioning, deletes every MicroVM it created and returns nil.
// Serve calls it; call it directly for in-process use with Conn.
func (b *Battery) Run(ctx context.Context) error {
	//= docs/requirements/10-battery.md#expiry
	//= type=exception
	//= reason=The fake ticks once at start, which is how a fresh Pool fills; its state never outlives a stop, so no Lease from before the start is left to renew.
	//# battery SHALL run its first sweep one `sweep_interval` after it
	//# starts, not at start.

	//= docs/requirements/10-battery.md#restarts
	//= type=exception
	//= reason=The fake keeps its state in memory and loses it when Run returns; SetFaults with UnavailableFor stands in for a battery restart that keeps its database.
	//# battery SHALL keep its Pools, MicroVMs and Leases across a
	//# restart, in its database.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	b.mu.Lock()
	if b.started {
		b.mu.Unlock()
		return ErrAlreadyRunning
	}
	b.started = true
	b.runCtx = runCtx
	b.mu.Unlock()
	defer b.stop(cancel)

	timer := b.cfg.Clock.NewTimer(b.cfg.ReconcileInterval)
	defer timer.Stop()
	b.tick(runCtx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C():
			b.tick(runCtx)
			timer.Reset(b.cfg.ReconcileInterval)
		case <-b.kick:
			b.tick(runCtx)
		}
	}
}

// stop ends the control loop: it cancels background work, waits for it,
// deletes every MicroVM on its Host and shuts the loopback server down.
// Marking the fake stopped under b.mu before the wait is what keeps a
// handler from starting work between the cancellation and the wait: a
// handler that reaches provisionNLocked either holds the lock first, and so
// adds to the WaitGroup before stop takes it, or finds the fake stopped.
func (b *Battery) stop(cancel context.CancelFunc) {
	cancel()
	b.mu.Lock()
	b.stopped = true
	b.mu.Unlock()

	b.wg.Wait()
	b.cleanupAll()
	b.loop.srv.Stop()
}

// cleanupAll deletes every MicroVM the fake still knows about. The fake has
// no persistence, so a MicroVM left behind would be an orphan on its Host.
func (b *Battery) cleanupAll() {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	b.mu.Lock()
	vms := make([]*vmState, 0, len(b.vms))
	for _, vm := range b.vms {
		vms = append(vms, vm)
	}
	b.mu.Unlock()
	for _, vm := range vms {
		if err := b.deleteVM(ctx, vm, 0); err != nil {
			b.log.Error(err, "Left MicroVM behind at shutdown", "uid", vm.uid, "host", vm.host)
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for id := range b.leases {
		b.dropLeaseLocked(id)
	}
}

// kickReconcile wakes the control loop without blocking.
func (b *Battery) kickReconcile() {
	select {
	case b.kick <- struct{}{}:
	default:
	}
}

// Conn returns a new client connection to the fake over an in-memory
// listener, served by the same handlers as Serve. Consumers use battery's
// generated clients on it. opts are added after the fake's own. The caller
// closes the connection.
//
// Taken before Run, the connection works: every RPC is served, and only
// provisioning waits for the control loop. After Run has returned, Conn
// fails with ErrStopped, and a connection taken earlier fails with
// UNAVAILABLE.
func (b *Battery) Conn(opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	b.mu.Lock()
	stopped := b.stopped
	b.mu.Unlock()
	if stopped {
		return nil, ErrStopped
	}
	lis := b.loop.lis
	opts = append([]grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}, opts...)
	conn, err := grpc.NewClient("passthrough:///fake-battery", opts...)
	if err != nil {
		return nil, fmt.Errorf("fake battery: loopback: %w", err)
	}
	return conn, nil
}

// HookKind names a Pool hook for fault injection.
type HookKind string

// Hook kinds.
const (
	HookCreate   HookKind = "create"
	HookPreLease HookKind = "pre_lease"
)

// HookFailure makes the next Remaining runs of Hook in Pool fail, so that
// VM_HOOK_FAILED and the hook failure policy are exercised. A zero Pool
// matches every Pool.
type HookFailure struct {
	Pool      PoolRef
	Hook      HookKind
	Remaining int
}

//= docs/requirements/08-test-doubles.md#fake-battery
//# The fake battery SHALL let a test make it unavailable for a
//# period, delay its answers to `ClaimVM`, refuse heartbeats, fail a Pool's
//# hooks, and drop its `Events` streams.

// Faults are the fake's runtime fault switches. All zero is a healthy
// battery. They are read per request and may change while the fake runs.
type Faults struct {
	// ClaimLatency delays every ClaimVM on the fake's clock.
	ClaimLatency time.Duration
	// UnavailableFor makes every RPC fail with UNAVAILABLE for this long
	// after SetFaults, measured on the fake's clock.
	UnavailableFor time.Duration
	// HookFailures are pending hook failures, consumed as they fire.
	HookFailures []HookFailure
	// DropEventsStream ends every open Subscribe stream with UNAVAILABLE
	// once, then clears itself.
	DropEventsStream bool
	// RefuseHeartbeats makes Heartbeat return NOT_FOUND for every Lease,
	// which is how a Lease is lost without waiting out its threshold.
	RefuseHeartbeats bool
}

// SetFaults replaces the fault switches. UnavailableFor starts counting on
// the fake's clock when it is set; DropEventsStream ends every open
// Subscribe stream and clears itself; the other switches are read on each
// request.
func (b *Battery) SetFaults(f Faults) {
	f.HookFailures = append([]HookFailure(nil), f.HookFailures...)
	drop := f.DropEventsStream
	f.DropEventsStream = false

	b.mu.Lock()
	b.faults = f
	if f.UnavailableFor > 0 {
		b.unavailableUntil = b.cfg.Clock.Now().Add(f.UnavailableFor)
	} else {
		b.unavailableUntil = time.Time{}
	}
	b.mu.Unlock()

	if drop {
		b.events.dropAll()
	}
}

// Faults returns the current fault switches.
func (b *Battery) Faults() Faults {
	b.mu.Lock()
	defer b.mu.Unlock()
	f := b.faults
	f.HookFailures = append([]HookFailure(nil), f.HookFailures...)
	return f
}

// LeaseRecord is one outstanding Lease, as Leases reports it.
type LeaseRecord struct {
	LeaseID         string
	VMUID           string
	Pool            PoolRef
	ClaimedAt       time.Time
	LastHeartbeatAt time.Time
	ExpiresAt       time.Time
}

// VMRecord is one MicroVM the fake manages, as VMs reports it.
type VMRecord struct {
	// UID is empty while the MicroVM's CreateMicroVM has not returned.
	UID       string
	Pool      PoolRef
	Host      string
	Phase     VMPhase
	LeaseID   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Leases returns every outstanding Lease, sorted by claim time, then lease
// id, so that a test can assert that none is left.
func (b *Battery) Leases() []LeaseRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]LeaseRecord, 0, len(b.leases))
	for _, ls := range b.leases {
		out = append(out, ls.rec)
	}
	sortLeases(out)
	return out
}

// VMs returns every MicroVM the fake manages, in creation order, which is
// how a test sees placement.
func (b *Battery) VMs() []VMRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.vmRecordsLocked()
}
