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

	"github.com/phoban01/battery-operator/internal/clock"
)

// The Inventory Controller's scope, subreconciler and chain are local, in
// the shape of the shared Scope[T], SubReconciler[T] and chain that #58
// brings (CLAUDE.md, "Controller structure"). Its "object" is battery's
// configuration: the controller builds the scope from the Nodes and the
// configuration as stored, and the subreconcilers decide what battery's
// Hosts should be. Writing that configuration and restarting battery is
// battery's side of the Operator, as a call to battery's API would be, and
// goes through Store and Restarter.

// Restarter restarts battery with a configuration just written: the one
// restart hook, batterysidecar.Restarter in production (DP-006).
type Restarter interface {
	// Restart returns once battery runs with config and answers again.
	Restart(ctx context.Context, config []byte) error
}

// Scope is one reconcile of the inventory.
type Scope struct {
	// Nodes are the cluster's Nodes, as listed for this reconcile.
	Nodes []corev1.Node
	// Config is battery's configuration as stored; Apply and Resume keep it
	// up to date with what they write.
	Config Config

	// Observed is the Hosts the Nodes make now, name to flintlockd
	// address, before any settling; Admit sets it.
	Observed Hosts
	// Desired is the Hosts battery should have: every settled change
	// applied to Config's Hosts; Settle sets it.
	Desired Hosts

	// State is what the controller remembers between reconciles.
	State *State
	// HostSet is where the Hosts battery runs with are published.
	HostSet *HostSet

	Store     Store
	Restarter Restarter
	Log       logr.Logger
	Clock     clock.Clock

	// Result is the chain's result so far.
	Result Result
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

// Result is what a subreconciler asks of the chain.
type Result struct {
	// Stop ends the chain after this subreconciler.
	Stop bool
	// RequeueAfter, when positive, asks for another reconcile after this
	// long.
	RequeueAfter time.Duration
}

// merge folds o into the result so far: the chain stops if either asks it
// to, and the shortest positive RequeueAfter wins.
func (r Result) merge(o Result) Result {
	out := Result{Stop: r.Stop || o.Stop, RequeueAfter: r.RequeueAfter}
	if o.RequeueAfter > 0 && (out.RequeueAfter <= 0 || o.RequeueAfter < out.RequeueAfter) {
		out.RequeueAfter = o.RequeueAfter
	}
	return out
}

// Subreconciler is one concern of the Inventory Controller.
type Subreconciler interface {
	// Reconcile changes the scope and says whether the chain continues,
	// requeues or stops. An error stops the chain.
	Reconcile(ctx context.Context, s *Scope) (Result, error)
}

// Chain is the Inventory Controller's subreconcilers, in order.
type Chain []Subreconciler

// Run runs the chain until a subreconciler stops it or fails, and folds
// each one's result into s.Result.
func (c Chain) Run(ctx context.Context, s *Scope) error {
	for _, sub := range c {
		r, err := sub.Reconcile(ctx, s)
		s.Result = s.Result.merge(r)
		if err != nil {
			return err
		}
		if r.Stop {
			return nil
		}
	}
	return nil
}

// Options are the Inventory Controller's timings.
type Options struct {
	// SettleTime is how long a change must hold before it is acted on
	// (IN-011).
	SettleTime time.Duration
	// RestartWindow is how long a restart window stays open (IN-012).
	RestartWindow time.Duration
}

// Defaults for Options.
const (
	DefaultSettleTime    = 30 * time.Second
	DefaultRestartWindow = time.Minute
)

// BindFlags binds o to the Operator's command line flags, with the
// defaults.
func (o *Options) BindFlags(fs *flag.FlagSet) {
	fs.DurationVar(&o.SettleTime, "inventory-settle-time", DefaultSettleTime,
		"How long a change in whether a Node is a Host must hold before the Inventory Controller acts on it.")
	fs.DurationVar(&o.RestartWindow, "inventory-restart-window", DefaultRestartWindow,
		"How long the Inventory Controller gathers settled changes to battery's Hosts before it restarts battery "+
			"once with all of them.")
}

// Validate reports an Options the controller cannot run with.
func (o Options) Validate() error {
	if o.SettleTime < 0 {
		return fmt.Errorf("inventory settle time %s is negative", o.SettleTime)
	}
	if o.RestartWindow < 0 {
		return fmt.Errorf("inventory restart window %s is negative", o.RestartWindow)
	}
	return nil
}

// NewChain is the Inventory Controller's chain, in order.
func NewChain(o Options) Chain {
	return Chain{
		Resume{},
		Admit{},
		Settle{Time: o.SettleTime},
		Window{Length: o.RestartWindow},
		Apply{},
	}
}
