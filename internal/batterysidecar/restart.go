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

package batterysidecar

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/phoban01/battery-operator/internal/clock"
)

// ProcessName is the executable the Restarter signals: battery's daemon,
// /poolmgrd in its image.
const ProcessName = "poolmgrd"

// DefaultPollInterval is how often the Restarter looks again while it
// waits: for the ConfigMap volume to update, for battery to exit, and for
// battery to answer.
const DefaultPollInterval = 500 * time.Millisecond

// Config configures the Restarter from the Operator's flags.
type Config struct {
	// ConfigMap names the ConfigMap, in the Operator's namespace, that holds
	// battery's configuration: what the Inventory Controller writes.
	ConfigMap string
	// ConfigFile is the Operator's mount of that ConfigMap's ConfigKey.
	ConfigFile string
}

// BindFlags binds c to the Operator's command line flags.
func (c *Config) BindFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.ConfigMap, "battery-config-map", DefaultConfigMap,
		"The ConfigMap, in the Operator's namespace, holding battery's configuration. "+
			"The Operator's RBAC grants access to the default name only.")
	fs.StringVar(&c.ConfigFile, "battery-config-file", DefaultConfigFile,
		"Where the Operator's container mounts battery's configuration, the volume battery's container mounts too.")
}

// Restarter restarts battery after its configuration changes (DP-006): the
// single mechanism the Inventory Controller invokes. See the package
// documentation for why it signals battery through a shared process
// namespace.
//
// It is also the gate the controllers' battery client waits on: Wait
// returns only while no restart is in progress.
type Restarter struct {
	// ConfigFile is the Operator's mount of battery's configuration.
	ConfigFile string
	// Processes finds and signals battery; ProcFS in production.
	Processes Processes
	// Ping returns nil once battery answers. In production it is the
	// battery connection's Ping, which bypasses the gate.
	Ping func(ctx context.Context) error
	// Clock paces the waits; clock.Real when nil.
	Clock clock.Clock
	// PollInterval is how often each wait looks again;
	// DefaultPollInterval when zero.
	PollInterval time.Duration

	// restart serialises restarts.
	restart sync.Mutex
	// mu guards done.
	mu sync.Mutex
	// done is nil while no restart is in progress, and otherwise is closed
	// when the one in progress ends.
	done chan struct{}
}

// Restart makes battery run with config, the content the caller has just
// written to battery's ConfigMap. It returns once battery answers again,
// or with an error, when ctx ends first or battery's processes cannot be
// read or signalled. Wait's callers wait from just before battery is
// signalled until Restart returns, however it returns. Concurrent calls
// run one after the other.
func (r *Restarter) Restart(ctx context.Context, config []byte) error {
	r.restart.Lock()
	defer r.restart.Unlock()

	if err := r.poll(ctx, "the ConfigMap volume to hold the new configuration", func() (bool, error) {
		got, err := os.ReadFile(r.ConfigFile)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil // the volume is being swapped
		}
		return bytes.Equal(got, config), err
	}); err != nil {
		return err
	}

	//= docs/requirements/06-deployment.md#battery-sidecar
	//# The Operator SHALL restart battery through a single mechanism
	//# that the Inventory Controller invokes, and SHALL wait until battery answers
	//# again before any controller calls it.
	release := r.close()
	defer release()
	procs, err := r.Processes.Find(ProcessName)
	if err != nil {
		return fmt.Errorf("finding battery: %w", err)
	}
	// No battery process is running between the kubelet's restarts, and the
	// next one reads the new file; otherwise every one found is stopped.
	for _, p := range procs {
		if err := r.Processes.Terminate(p); err != nil {
			return fmt.Errorf("stopping battery: %w", err)
		}
	}
	for _, p := range procs {
		if err := r.poll(ctx, "battery to exit", func() (bool, error) {
			running, err := r.Processes.Running(p)
			return !running, err
		}); err != nil {
			return err
		}
	}
	return r.poll(ctx, "battery to answer", func() (bool, error) {
		return r.Ping(ctx) == nil, nil
	})
}

// Wait returns nil once no restart is in progress, or ctx's error if it
// ends first.
func (r *Restarter) Wait(ctx context.Context) error {
	r.mu.Lock()
	done := r.done
	r.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// close closes the gate, and returns the function that opens it again.
func (r *Restarter) close() func() {
	done := make(chan struct{})
	r.mu.Lock()
	r.done = done
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		r.done = nil
		r.mu.Unlock()
		close(done)
	}
}

// poll calls cond until it reports true or fails, or ctx ends.
func (r *Restarter) poll(ctx context.Context, what string, cond func() (bool, error)) error {
	c := r.Clock
	if c == nil {
		c = clock.Real{}
	}
	interval := r.PollInterval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	for {
		ok, err := cond()
		if err != nil {
			return fmt.Errorf("waiting for %s: %w", what, err)
		}
		if ok {
			return nil
		}
		t := c.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("waiting for %s: %w", what, ctx.Err())
		case <-t.C():
		}
	}
}
