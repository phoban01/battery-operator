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
	"flag"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"

	"github.com/phoban01/battery-operator/internal/batterysidecar"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/reconcile"
)

// The Inventory Controller runs the shared chain of internal/reconcile
// (CLAUDE.md, "Controller structure") over a scope of its own. Its
// "object" is battery's configuration, not a Kubernetes object, so its
// scope embeds no reconcile.Scope: the controller builds the scope from the Nodes, the
// configuration as stored and battery's client certificate, and the
// subreconcilers decide what battery's Hosts should be and whether battery
// needs a restart. Writing that configuration and restarting battery is
// battery's side of the Operator, as a call to battery's API would be, and
// goes through Store and Restarter.

// Restarter restarts battery with a configuration just written and the
// client certificate its Secret holds: the one restart hook,
// batterysidecar.Restarter in production (DP-006).
type Restarter interface {
	// Restart returns once battery runs with want and answers again.
	Restart(ctx context.Context, want batterysidecar.Mounts) error
}

// Scope is one reconcile of the inventory.
type Scope struct {
	// Nodes are the cluster's Nodes, as listed for this reconcile.
	Nodes []corev1.Node
	// Config is battery's configuration as stored; Apply and Resume keep it
	// up to date with what they write.
	Config Config
	// ClientCertificate is battery's flintlockd client certificate as its
	// Secret holds it now, nil while there is no such Secret.
	ClientCertificate []byte

	// Observed is the Hosts the Nodes make now, name to flintlockd
	// address, before any settling; Admit sets it.
	Observed Hosts
	// Desired is the Hosts battery should have: every settled change
	// applied to Config's Hosts; Settle sets it.
	Desired Hosts
	// Renewed is true when ClientCertificate is not the one battery last
	// restarted with; Renew sets it.
	Renewed bool

	// State is what the controller remembers between reconciles.
	State *State
	// HostSet is where the Hosts battery runs with are published.
	HostSet *HostSet
	// Pools lists the Pools battery holds, for Drain.
	Pools PoolLister

	Store     Store
	Restarter Restarter
	Log       logr.Logger
	Clock     clock.Clock
}

// State is what the Inventory Controller remembers between reconciles.
// Reconciles of the inventory run one at a time, so it needs no lock.
type State struct {
	// seen is, for each Node that differs from battery's configuration or
	// did when last looked at, what it made last time and since when.
	seen map[string]seen
	// windowOpened is when the open restart window opened; zero while none
	// is open.
	windowOpened time.Time
	// drainStarted is when Drain began to wait for the Pools to drop the
	// Hosts the next restart removes; zero while it is not waiting.
	drainStarted time.Time
}

// seen is a Node as the Inventory Controller last saw it.
type seen struct {
	// address is its flintlockd address if it was a Host, "" otherwise.
	address string
	// since is when it last changed.
	since time.Time
}

// NewState returns the state of a controller that has seen nothing yet.
func NewState() *State { return &State{seen: map[string]seen{}} }

// WindowOpened is when the open restart window opened, and whether one is
// open.
func (st *State) WindowOpened() (time.Time, bool) {
	return st.windowOpened, !st.windowOpened.IsZero()
}

// Options are the Inventory Controller's timings.
type Options struct {
	// SettleTime is how long a change must hold before it is acted on
	// (IN-011).
	SettleTime time.Duration
	// RestartWindow is how long a restart window stays open (IN-012).
	RestartWindow time.Duration
	// DrainTimeout is the longest the controller waits, before a restart
	// that removes Hosts, for the Pools in battery to stop naming them
	// (IN-013).
	DrainTimeout time.Duration
}

// Defaults for Options.
const (
	DefaultSettleTime    = 30 * time.Second
	DefaultRestartWindow = time.Minute
	DefaultDrainTimeout  = 30 * time.Second
)

// BindFlags binds o to the Operator's command line flags, with the
// defaults.
func (o *Options) BindFlags(fs *flag.FlagSet) {
	fs.DurationVar(&o.SettleTime, "inventory-settle-time", DefaultSettleTime,
		"How long a change in whether a Node is a Host must hold before the Inventory Controller acts on it.")
	fs.DurationVar(&o.RestartWindow, "inventory-restart-window", DefaultRestartWindow,
		"How long the Inventory Controller gathers settled changes to battery's Hosts before it restarts battery "+
			"once with all of them.")
	fs.DurationVar(&o.DrainTimeout, "inventory-drain-timeout", DefaultDrainTimeout,
		"How long the Inventory Controller waits, before a restart of battery that removes Hosts, for battery's Pools "+
			"to stop naming them.")
}

// Validate reports an Options the controller cannot run with.
func (o Options) Validate() error {
	if o.SettleTime < 0 {
		return fmt.Errorf("inventory settle time %s is negative", o.SettleTime)
	}
	if o.RestartWindow < 0 {
		return fmt.Errorf("inventory restart window %s is negative", o.RestartWindow)
	}
	if o.DrainTimeout < 0 {
		return fmt.Errorf("inventory drain timeout %s is negative", o.DrainTimeout)
	}
	return nil
}

// NewChain is the Inventory Controller's chain, in order.
func NewChain(o Options) reconcile.Chain[*Scope] {
	return reconcile.Steps[*Scope](
		Restore{},
		Resume{},
		Admit{},
		Settle{Time: o.SettleTime},
		Renew{},
		Window{Length: o.RestartWindow},
		Drain{Timeout: o.DrainTimeout},
		Apply{},
	)
}
