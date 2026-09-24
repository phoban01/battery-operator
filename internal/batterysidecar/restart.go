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

// DefaultMountTimeout is how long the Restarter waits for the Operator's
// mounts to hold what battery is to restart with. The kubelet refreshes a
// ConfigMap or Secret volume within about a minute or two of the object
// changing.
const DefaultMountTimeout = 5 * time.Minute

// ErrMountTimeout is the cause when the Operator's mounts do not come to
// hold what battery is to restart with within the mount timeout: the
// object changed again meanwhile, and the caller has to read it anew.
// battery has not been signalled.
var ErrMountTimeout = errors.New("the mounted files did not change to what battery is to restart with")

// Config configures the Restarter from the Operator's flags.
type Config struct {
	// ConfigMap names the ConfigMap, in the Operator's namespace, that holds
	// battery's configuration: what the Inventory Controller writes.
	ConfigMap string
	// ConfigFile is the Operator's mount of that ConfigMap's ConfigKey.
	ConfigFile string
	// ClientSecret names the Secret, in the Operator's namespace, that holds
	// battery's flintlockd client certificate: what cert-manager renews.
	ClientSecret string
	// ClientCertFile is the Operator's mount of that Secret's certificate.
	ClientCertFile string
}

// BindFlags binds c to the Operator's command line flags.
func (c *Config) BindFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.ConfigMap, "battery-config-map", DefaultConfigMap,
		"The ConfigMap, in the Operator's namespace, holding battery's configuration. "+
			"The Operator's RBAC grants access to the default name only.")
	fs.StringVar(&c.ConfigFile, "battery-config-file", DefaultConfigFile,
		"Where the Operator's container mounts battery's configuration, the volume battery's container mounts too.")
	fs.StringVar(&c.ClientSecret, "battery-client-secret", DefaultClientSecret,
		"The Secret, in the Operator's namespace, holding battery's flintlockd client certificate; battery restarts "+
			"when it changes. The Operator's RBAC grants access to the default name only.")
	fs.StringVar(&c.ClientCertFile, "battery-client-cert-file", ClientCertFile,
		"Where the Operator's container mounts battery's flintlockd client certificate, the volume battery's "+
			"container mounts too.")
}

// Mounts is what battery is to restart with: what the Operator's mounts of
// battery's volumes have to hold before battery is signalled.
type Mounts struct {
	// Config is battery's configuration, as just written to its ConfigMap.
	Config []byte
	// ClientCertificate is battery's flintlockd client certificate, as its
	// Secret holds it; nil to restart without waiting for it.
	ClientCertificate []byte
}

// Restarter restarts battery after its configuration or its client
// certificate changes (DP-006, DP-007): the single mechanism the Inventory
// Controller invokes. See the package documentation for why it signals
// battery through a shared process namespace.
//
// It is also the gate the controllers' battery client waits on: Wait
// returns only while no restart is in progress.
type Restarter struct {
	// ConfigFile is the Operator's mount of battery's configuration.
	ConfigFile string
	// ClientCertFile is the Operator's mount of battery's client
	// certificate.
	ClientCertFile string
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
	// MountTimeout bounds the wait for the Operator's mounts to hold what
	// battery is to restart with; DefaultMountTimeout when zero.
	MountTimeout time.Duration

	// restart serialises restarts.
	restart sync.Mutex
	// mu guards done.
	mu sync.Mutex
	// done is nil while no restart is in progress, and otherwise is closed
	// when the one in progress ends.
	done chan struct{}
}

// Restart makes battery run with want: the configuration the caller has
// just written to battery's ConfigMap, and the client certificate its
// Secret holds. It returns once battery answers again, or with an error,
// when ctx ends first, the mounts do not come to hold want within the
// mount timeout (ErrMountTimeout), or battery's processes cannot be read
// or signalled. Wait's callers wait from just before battery is signalled
// until Restart returns, however it returns. Concurrent calls run one
// after the other.
func (r *Restarter) Restart(ctx context.Context, want Mounts) error {
	r.restart.Lock()
	defer r.restart.Unlock()

	timeout := r.MountTimeout
	if timeout <= 0 {
		timeout = DefaultMountTimeout
	}
	deadline := r.clock().Now().Add(timeout)
	if err := r.waitForFile(ctx, deadline, "the ConfigMap volume to hold the new configuration", r.ConfigFile,
		want.Config); err != nil {
		return err
	}
	if want.ClientCertificate != nil {
		//= docs/requirements/06-deployment.md#battery-sidecar
		//# When the client certificate in the Secret of DP-005 changes,
		//# the Operator SHALL restart battery through the mechanism of DP-006, and
		//# SHALL signal battery only once the Operator's own mount of that Secret
		//# holds the new certificate.
		if err := r.waitForFile(ctx, deadline, "the TLS volume to hold the client certificate", r.ClientCertFile,
			want.ClientCertificate); err != nil {
			return err
		}
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
	c := r.clock()
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

// waitForFile polls until the file at path holds want, or fails with
// ErrMountTimeout once deadline has passed. A file missing is a volume
// being swapped, and is waited out too.
func (r *Restarter) waitForFile(ctx context.Context, deadline time.Time, what, path string, want []byte) error {
	return r.poll(ctx, what, func() (bool, error) {
		got, err := os.ReadFile(path)
		switch {
		case err == nil && bytes.Equal(got, want):
			return true, nil
		case err != nil && !errors.Is(err, os.ErrNotExist):
			return false, err
		case r.clock().Now().After(deadline):
			return false, ErrMountTimeout
		}
		return false, nil
	})
}

// clock is the Restarter's Clock, clock.Real when nil.
func (r *Restarter) clock() clock.Clock {
	if r.Clock == nil {
		return clock.Real{}
	}
	return r.Clock
}
