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
	"runtime"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/phoban01/battery-operator/internal/clock"
)

// TestConnAcrossTheFakesLife pins what a connection is worth at each point
// of the fake's life: before Run the RPCs are served and only provisioning
// waits for the control loop; after the shutdown a connection taken earlier
// reports UNAVAILABLE, as one to a real battery that went away does, and
// Conn says the fake is shut down rather than pointing at a server that is
// no longer listening.
func TestConnAcrossTheFakesLife(t *testing.T) {
	clk := clock.NewFake(testEpoch)
	b := New(Config{Clock: clk, ReconcileInterval: testInterval})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	early := newTestClient(t, b)
	if _, err := early.ListPools(ctx, testNamespace); err != nil {
		t.Fatalf("ListPools before Run: %v", err)
	}

	runDone := make(chan error, 1)
	go func() { runDone <- b.Run(ctx) }()
	if err := clk.BlockUntil(ctx, 1); err != nil {
		t.Fatalf("waiting for the control loop's timer: %v", err)
	}
	cancel()
	if err := <-runDone; err != nil {
		t.Fatalf("Run returned %v", err)
	}

	if _, err := early.ListPools(context.Background(), testNamespace); statusCode(t, err) != codes.Unavailable {
		t.Fatalf("ListPools on a connection to a stopped fake = %v, want UNAVAILABLE", err)
	}
	if _, err := b.Conn(); !errors.Is(err, ErrStopped) {
		t.Fatalf("Conn after the shutdown = %v, want ErrStopped", err)
	}
}

// TestServeRunsOnce checks that a second Serve is refused rather than
// binding a second listener and closing the ready channel again, which
// would panic on the caller's goroutine.
func TestServeRunsOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	b := New(Config{
		Listen:            defaultListen,
		Clock:             clock.NewFake(testEpoch),
		ReconcileInterval: testInterval,
	})
	served := make(chan error, 1)
	go func() { served <- b.Serve(ctx) }()
	select {
	case <-b.Ready():
	case err := <-served:
		t.Fatalf("Serve returned before listening: %v", err)
	}

	if err := b.Serve(ctx); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Serve = %v, want ErrAlreadyRunning", err)
	}

	cancel()
	if err := <-served; err != nil {
		t.Fatalf("Serve returned %v", err)
	}
}

// TestServeShutdownIsNotReportedAsAnEarlyStop drives the shutdown race in
// Serve's three-way select. Cancelling the caller's context cancels the
// control loop's context too, so at a normal shutdown ctx.Done() and the
// loop's result become ready a moment apart, and the select may pick
// either. Taking the loop's branch must not turn an ordinary shutdown into
// "control loop stopped while serving". The race is rare, so this drives
// many shutdowns rather than one.
func TestServeShutdownIsNotReportedAsAnEarlyStop(t *testing.T) {
	t.Parallel()

	for i := range 200 {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		b := New(Config{
			Listen:            defaultListen,
			Clock:             clock.NewFake(testEpoch),
			ReconcileInterval: testInterval,
		})
		served := make(chan error, 1)
		go func() { served <- b.Serve(ctx) }()
		select {
		case <-b.Ready():
		case err := <-served:
			cancel()
			t.Fatalf("iteration %d: Serve returned before listening: %v", i, err)
		}

		// Give the serving goroutine every chance to be somewhere
		// inconvenient when the cancellation lands.
		runtime.Gosched()
		cancel()
		if err := <-served; err != nil {
			t.Fatalf("iteration %d: Serve reported %v; a shutdown the caller"+
				" asked for is not an early stop", i, err)
		}
	}
}

// TestStopDeletesAMicroVMWhoseCreateWasInFlight stops the fake while a
// replenishing CreateMicroVM is still on the Host. The Host makes the
// MicroVM whether or not its caller is still waiting, as flintlockd does,
// so a stop that cancelled the create would get an error and no uid,
// forget the reservation and leave the MicroVM on the Host for ever.
func TestStopDeletesAMicroVMWhoseCreateWasInFlight(t *testing.T) {
	h := newHarness(t, Config{}, "host-1")
	host := h.stubs["host-1"]
	gate := make(chan struct{})
	host.set(func(s *testHost) { s.createGate = gate })

	if _, err := h.client.CreatePool(h.ctx, h.spec("p", 1, "host-1")); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	select {
	case <-host.createEntered:
	case <-h.ctx.Done():
		t.Fatal("the fake never started a create")
	}

	// The fake's context is cancelled before the Host answers, so a create
	// that shares it has been given up on by the time the gate opens.
	h.cancel()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		h.stop()
	}()
	close(gate)
	<-stopped

	if created, _ := host.counts(); created != 1 {
		t.Fatalf("microvms created = %d, want 1", created)
	}
	if n := host.live(); n != 0 {
		t.Fatalf("%d microvm(s) left on the host after the fake stopped, want none", n)
	}
}

// TestFirstConnDuringShutdownIsSafe takes the very first connection to a
// fake while its Run is returning. The loopback server the connection
// needs is built by New, so the shutdown only reads it; building it lazily
// would be a write racing that read. Run it under -race.
func TestFirstConnDuringShutdownIsSafe(t *testing.T) {
	for range 50 {
		clk := clock.NewFake(testEpoch)
		b := New(Config{Clock: clk, ReconcileInterval: testInterval})

		ctx, cancel := context.WithCancel(context.Background())
		runDone := make(chan error, 1)
		go func() { runDone <- b.Run(ctx) }()
		// The control loop is past its first tick and waiting.
		if err := clk.BlockUntil(context.Background(), 1); err != nil {
			t.Fatalf("waiting for the control loop's timer: %v", err)
		}

		type result struct {
			conn *grpc.ClientConn
			err  error
		}
		conns := make(chan result, 1)
		go func() {
			conn, err := b.Conn()
			conns <- result{conn, err}
		}()
		cancel()

		got := <-conns
		switch {
		case got.err == nil:
			if err := got.conn.Close(); err != nil {
				t.Fatalf("closing the connection taken during the shutdown: %v", err)
			}
		case !errors.Is(got.err, ErrStopped):
			t.Fatalf("Conn during the shutdown = %v, want a connection or ErrStopped", got.err)
		}
		if err := <-runDone; err != nil {
			t.Fatalf("Run returned %v", err)
		}
	}
}
