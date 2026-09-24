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

package batterysidecar_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/batterysidecar"
)

// fakeProcesses plays the shared process namespace: battery's processes,
// which exit a while after SIGTERM, as the real one drains its API first.
type fakeProcesses struct {
	mu sync.Mutex
	// running maps each process to how many more Running calls it answers
	// true after it is terminated; -1 while it is not terminated.
	running    map[batterysidecar.Process]int
	terminated []batterysidecar.Process
	events     *eventLog
	findErr    error
	termErr    error
}

func newFakeProcesses(events *eventLog, procs ...batterysidecar.Process) *fakeProcesses {
	f := &fakeProcesses{running: map[batterysidecar.Process]int{}, events: events}
	for _, p := range procs {
		f.running[p] = -1
	}
	return f
}

func (f *fakeProcesses) Find(name string) ([]batterysidecar.Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name != batterysidecar.ProcessName {
		return nil, nil
	}
	if f.findErr != nil {
		return nil, f.findErr
	}
	var out []batterysidecar.Process
	for p, n := range f.running {
		if n != 0 {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakeProcesses) Terminate(p batterysidecar.Process) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.termErr != nil {
		return f.termErr
	}
	f.terminated = append(f.terminated, p)
	f.events.add("terminate")
	if f.running[p] < 0 {
		f.running[p] = 3
	}
	return nil
}

func (f *fakeProcesses) Running(p batterysidecar.Process) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.running[p]
	switch {
	case n < 0:
		return true, nil
	case n == 0:
		return false, nil
	}
	f.running[p] = n - 1
	if n == 1 {
		f.events.add("exited")
	}
	return true, nil
}

// eventAnswered is the event of battery answering again.
const eventAnswered = "answered"

// eventLog records what happened, in order.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

// fakePing answers once answering is set, and records its first answer.
type fakePing struct {
	mu        sync.Mutex
	answering bool
	events    *eventLog
	answered  bool
}

func (p *fakePing) set(answering bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.answering = answering
}

func (p *fakePing) ping(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.answering {
		return battery.ErrUnavailable
	}
	if !p.answered {
		p.answered = true
		p.events.add(eventAnswered)
	}
	return nil
}

// writeConfig writes battery's configuration where the Restarter reads it.
func writeConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newRestarter(t *testing.T, procs batterysidecar.Processes, ping *fakePing) *batterysidecar.Restarter {
	t.Helper()
	dir := t.TempDir()
	return &batterysidecar.Restarter{
		ConfigFile:     filepath.Join(dir, batterysidecar.ConfigKey),
		ClientCertFile: filepath.Join(dir, "tls.crt"),
		Processes:      procs,
		Ping:           ping.ping,
		PollInterval:   time.Millisecond,
	}
}

// eventually waits for cond, failing the test after a few seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

//= docs/requirements/06-deployment.md#battery-sidecar
//= type=test
//# The Operator SHALL restart battery through a single mechanism
//# that the Inventory Controller invokes, and SHALL wait until battery answers
//# again before any controller calls it.

// TestRestart checks the restart in order: nothing happens until the
// Operator's mount of the ConfigMap holds the new configuration; then
// battery gets SIGTERM; Restart waits for that process to exit, and then
// for battery to answer, before it returns; and a controller's call made
// meanwhile reaches battery only after battery answers.
func TestRestart(t *testing.T) {
	t.Parallel()
	events := &eventLog{}
	old := batterysidecar.Process{PID: 7, Start: 100}
	procs := newFakeProcesses(events, old)
	ping := &fakePing{answering: true, events: events}
	r := newRestarter(t, procs, ping)
	writeConfig(t, r.ConfigFile, "old")

	done := make(chan error, 1)
	go func() { done <- r.Restart(context.Background(), batterysidecar.Mounts{Config: []byte("new")}) }()

	// The kubelet has not updated the volume yet: battery is left alone,
	// and controllers are not held up.
	time.Sleep(20 * time.Millisecond)
	if got := events.list(); len(got) != 0 {
		t.Fatalf("battery was touched before its configuration changed: %v", got)
	}
	if err := r.Wait(context.Background()); err != nil {
		t.Fatalf("controllers are held while the volume updates: %v", err)
	}

	// battery stops answering once signalled.
	ping.set(false)
	writeConfig(t, r.ConfigFile, "new")
	eventually(t, "battery to exit", func() bool { return slices.Contains(events.list(), "exited") })

	// A controller calls battery while it restarts.
	stub := &stubClient{events: events}
	gated := batterysidecar.Gated{Client: stub, Gate: r}
	called := make(chan error, 1)
	go func() {
		_, err := gated.ListPools(context.Background(), "")
		called <- err
	}()
	time.Sleep(20 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("Restart returned %v before battery answered", err)
	case <-called:
		t.Fatal("a controller's call went through before battery answered")
	default:
	}

	ping.set(true)
	if err := <-done; err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if err := <-called; err != nil {
		t.Fatalf("the controller's call: %v", err)
	}
	want := []string{"terminate", "exited", eventAnswered, "call"}
	if got := events.list(); !slices.Equal(got, want) {
		t.Errorf("events %v, want %v", got, want)
	}
	if !slices.Equal(procs.terminated, []batterysidecar.Process{old}) {
		t.Errorf("terminated %v, want %v", procs.terminated, old)
	}
}

// TestRestartBetweenRestarts checks that when the kubelet is between two
// starts of battery, there is nothing to signal, and Restart waits for the
// next battery, which reads the new file, to answer.
func TestRestartBetweenRestarts(t *testing.T) {
	t.Parallel()
	events := &eventLog{}
	procs := newFakeProcesses(events)
	ping := &fakePing{answering: true, events: events}
	r := newRestarter(t, procs, ping)
	writeConfig(t, r.ConfigFile, "new")
	if err := r.Restart(context.Background(), batterysidecar.Mounts{Config: []byte("new")}); err != nil {
		t.Fatal(err)
	}
	if got := events.list(); !slices.Equal(got, []string{eventAnswered}) {
		t.Errorf("events %v, want battery to answer alone", got)
	}
}

// TestRestartFailures checks that a restart that fails, or whose caller
// gives up, reports it and releases the controllers.
func TestRestartFailures(t *testing.T) {
	t.Parallel()
	errBoom := errors.New("boom")
	for name, tc := range map[string]struct {
		setup func(*fakeProcesses, *fakePing)
		want  error
	}{
		"battery cannot be found":     {func(p *fakeProcesses, _ *fakePing) { p.findErr = errBoom }, errBoom},
		"battery cannot be signalled": {func(p *fakeProcesses, _ *fakePing) { p.termErr = errBoom }, errBoom},
		"battery never answers":       {func(_ *fakeProcesses, p *fakePing) { p.set(false) }, context.DeadlineExceeded},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			events := &eventLog{}
			procs := newFakeProcesses(events, batterysidecar.Process{PID: 7, Start: 1})
			ping := &fakePing{answering: true, events: events}
			tc.setup(procs, ping)
			r := newRestarter(t, procs, ping)
			writeConfig(t, r.ConfigFile, "new")
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			if err := r.Restart(ctx, batterysidecar.Mounts{Config: []byte("new")}); !errors.Is(err, tc.want) {
				t.Errorf("Restart: %v, want %v", err, tc.want)
			}
			if err := r.Wait(context.Background()); err != nil {
				t.Errorf("controllers are still held after a failed restart: %v", err)
			}
		})
	}
}

// TestRestartWaitsForVolume checks that a caller that gives up while the
// volume still holds the old configuration leaves battery alone.
func TestRestartWaitsForVolume(t *testing.T) {
	t.Parallel()
	events := &eventLog{}
	procs := newFakeProcesses(events, batterysidecar.Process{PID: 7, Start: 1})
	ping := &fakePing{answering: true, events: events}
	r := newRestarter(t, procs, ping)
	// The volume is mid-swap: no file at all.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := r.Restart(ctx, batterysidecar.Mounts{Config: []byte("new")}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Restart: %v, want the deadline", err)
	}
	if got := events.list(); len(got) != 0 {
		t.Errorf("battery was touched: %v", got)
	}
}

//= docs/requirements/06-deployment.md#battery-sidecar
//= type=test
//# When the client certificate in the Secret of DP-005 changes,
//# the Operator SHALL restart battery through the mechanism of DP-006, and
//# SHALL signal battery only once the Operator's own mount of that Secret
//# holds the new certificate.

// TestRestartWaitsForClientCertificate checks that a restart for a renewed
// client certificate leaves battery alone until the Operator's mount of the
// certificate holds the renewed one, even though the configuration is
// already there, and then restarts battery as any other restart does.
func TestRestartWaitsForClientCertificate(t *testing.T) {
	t.Parallel()
	events := &eventLog{}
	old := batterysidecar.Process{PID: 7, Start: 100}
	procs := newFakeProcesses(events, old)
	ping := &fakePing{answering: true, events: events}
	r := newRestarter(t, procs, ping)
	writeConfig(t, r.ConfigFile, "config")
	writeConfig(t, r.ClientCertFile, "old certificate")

	done := make(chan error, 1)
	go func() {
		done <- r.Restart(context.Background(), batterysidecar.Mounts{
			Config:            []byte("config"),
			ClientCertificate: []byte("renewed certificate"),
		})
	}()

	time.Sleep(20 * time.Millisecond)
	if got := events.list(); len(got) != 0 {
		t.Fatalf("battery was touched before its certificate's mount changed: %v", got)
	}
	writeConfig(t, r.ClientCertFile, "renewed certificate")
	if err := <-done; err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if !slices.Equal(procs.terminated, []batterysidecar.Process{old}) {
		t.Errorf("terminated %v, want %v", procs.terminated, old)
	}
	if got, want := events.list(), []string{"terminate", "exited", eventAnswered}; !slices.Equal(got, want) {
		t.Errorf("events %v, want %v", got, want)
	}
}

// TestRestartMountTimeout checks that a mount that never comes to hold
// what battery is to restart with, because the Secret changed again
// meanwhile, fails the restart with ErrMountTimeout and leaves battery
// alone, so that the caller can read the Secret anew.
func TestRestartMountTimeout(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]batterysidecar.Mounts{
		"configuration":      {Config: []byte("never")},
		"client certificate": {Config: []byte("config"), ClientCertificate: []byte("never")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			events := &eventLog{}
			procs := newFakeProcesses(events, batterysidecar.Process{PID: 7, Start: 1})
			ping := &fakePing{answering: true, events: events}
			r := newRestarter(t, procs, ping)
			r.MountTimeout = 20 * time.Millisecond
			writeConfig(t, r.ConfigFile, "config")
			writeConfig(t, r.ClientCertFile, "certificate")
			if err := r.Restart(context.Background(), want); !errors.Is(err, batterysidecar.ErrMountTimeout) {
				t.Errorf("Restart: %v, want ErrMountTimeout", err)
			}
			if got := events.list(); len(got) != 0 {
				t.Errorf("battery was touched: %v", got)
			}
		})
	}
}
