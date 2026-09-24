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
	"time"

	"github.com/phoban01/battery-operator/internal/batterysidecar"
)

// The Node report the Inventory Controller reads (EA-034, EA-035). They
// repeat internal/execagent's AnnotationReady and
// AnnotationFlintlockdAddress, which a test checks, so that the Operator
// does not link the Exec Agent's package.
const (
	AnnotationReady             = "battery.liquidmetal-x.dev/exec-agent-ready"
	AnnotationFlintlockdAddress = "battery.liquidmetal-x.dev/flintlockd-address"

	// annotationTrue is the value of a true annotation: the Node report's
	// readiness, and RestartPendingAnnotation.
	annotationTrue = "true"
)

// Resume finishes a restart a previous reconcile started and did not
// finish: its configuration is written and marked pending. It then
// publishes the Hosts battery runs with, if they have not been yet.
type Resume struct{}

func (Resume) Reconcile(ctx context.Context, s *Scope) (Result, error) {
	if s.Config.Pending {
		s.Log.Info("Resumed restart of battery with a configuration written earlier", "hosts", slices.Sorted(maps.Keys(s.Config.Hosts)))
		if err := restart(ctx, s, s.Config.Raw); err != nil {
			return Result{Stop: true}, err
		}
	}
	if !s.HostSet.Synced() {
		s.HostSet.publish(s.Config.Hosts)
	}
	return Result{}, nil
}

// Admit works out the Hosts the Nodes make now.
type Admit struct{}

func (Admit) Reconcile(_ context.Context, s *Scope) (Result, error) {
	s.Observed = Hosts{}
	for i := range s.Nodes {
		n := &s.Nodes[i]
		if !n.DeletionTimestamp.IsZero() {
			continue // going away: as good as deleted
		}
		//= docs/requirements/04-inventory.md#admission
		//# The Inventory Controller SHALL treat a Node as a Host only while
		//# the Node's Node report says the Host is ready and the Node is schedulable.
		ready := n.Annotations[AnnotationReady] == annotationTrue
		schedulable := !n.Spec.Unschedulable

		//= docs/requirements/04-inventory.md#admission
		//# The Inventory Controller SHALL give battery each Host under the
		//# Node's name, at the `flintlockd` address the Host's Node report gives.
		address := n.Annotations[AnnotationFlintlockdAddress]
		// A report with no address gives battery nowhere to reach the Host.
		if ready && schedulable && address != "" {
			s.Observed[n.Name] = address
		}
	}
	return Result{}, nil
}

// Settle works out the Hosts battery should have: battery's current Hosts,
// with every change that has held for the settle time applied. A change
// is a Node becoming a Host, ceasing to be one, or its flintlockd address
// moving.
type Settle struct {
	// Time is the settle time.
	Time time.Duration
}

func (st Settle) Reconcile(_ context.Context, s *Scope) (Result, error) {
	now := s.Clock.Now()
	names := map[string]bool{}
	for _, m := range []map[string]string{s.Observed, s.Config.Hosts} {
		for name := range m {
			names[name] = true
		}
	}
	for name := range s.State.seen {
		names[name] = true
	}

	s.Desired = Hosts{}
	var res Result
	for name := range names {
		observed := s.Observed[name] // "" when not a Host
		configured := s.Config.Hosts[name]
		last, ok := s.State.seen[name]
		if !ok || last.address != observed {
			last = seen{address: observed, since: now}
		}
		if observed == configured {
			// Nothing to apply: a change that flapped back is forgotten.
			delete(s.State.seen, name)
			if configured != "" {
				s.Desired[name] = configured
			}
			continue
		}
		s.State.seen[name] = last

		//= docs/requirements/04-inventory.md#applying
		//# The Inventory Controller SHALL act on a change in whether a Node
		//# is a Host only after the change has held for the configured settle time.
		if held := now.Sub(last.since); held < st.Time {
			if configured != "" {
				s.Desired[name] = configured
			}
			res = res.merge(Result{RequeueAfter: st.Time - held})
			continue
		}

		//= docs/requirements/04-inventory.md#admission
		//# When a Node that is a Host is cordoned or deleted, or its Node
		//# report says the Host is not ready, the Inventory Controller SHALL remove
		//# the Host from battery's Hosts.
		if observed != "" {
			s.Desired[name] = observed
		}
	}
	return res, nil
}

// Window batches the settled changes into restarts of battery: the first
// settled change waiting opens a restart window, and the chain goes on to
// Apply only once the window has closed. A window whose changes all
// flapped back closes without a restart.
type Window struct {
	// Length is the restart window.
	Length time.Duration
}

func (w Window) Reconcile(_ context.Context, s *Scope) (Result, error) {
	opened, open := s.State.WindowOpened()
	if s.Desired.Equal(s.Config.Hosts) {
		if open {
			s.State.windowOpened = time.Time{}
			s.Log.Info("Closed restart window without restarting battery")
		}
		return Result{Stop: true}, nil
	}

	//= docs/requirements/04-inventory.md#applying
	//# When a change has settled and no restart window is open, the
	//# Inventory Controller SHALL open a restart window of the configured
	//# length, and SHALL apply every change that has settled by the time the
	//# window closes in a single restart of battery.
	now := s.Clock.Now()
	if !open {
		opened = now
		s.State.windowOpened = now
		s.Log.Info("Opened restart window", "closesAt", now.Add(w.Length))
	}
	if left := opened.Add(w.Length).Sub(now); left > 0 {
		return Result{Stop: true, RequeueAfter: left}, nil
	}
	return Result{}, nil
}

// Apply writes the Hosts battery should have to its configuration and
// restarts battery with it, once.
type Apply struct{}

func (Apply) Reconcile(ctx context.Context, s *Scope) (Result, error) {
	//= docs/requirements/04-inventory.md#admission
	//# The Inventory Controller SHALL configure battery to reach every
	//# Host's `flintlockd` over TLS, verifying the serving certificate against
	//# the configured certificate authority and presenting the Operator's client
	//# certificate for `flintlockd`.
	raw, err := batterysidecar.Render(s.Desired.List())
	if err != nil {
		return Result{Stop: true}, err
	}

	//= docs/requirements/04-inventory.md#applying
	//# When the set of Hosts changes, the Inventory Controller SHALL
	//# write the new list to battery's configuration and restart battery.
	if err := s.Store.Save(ctx, raw, true); err != nil {
		return Result{Stop: true}, err
	}
	// The window has closed: whatever happens to this restart, a change
	// from here on waits for a window of its own, and a failed restart is
	// Resume's to finish.
	s.State.windowOpened = time.Time{}
	s.Config = Config{Hosts: s.Desired, Raw: raw, Pending: true}
	s.Log.Info("Wrote battery's configuration", "hosts", slices.Sorted(maps.Keys(s.Desired)))
	if err := restart(ctx, s, raw); err != nil {
		return Result{Stop: true}, err
	}
	return Result{}, nil
}

// restart restarts battery with raw, the pending configuration in s, then
// marks it applied and publishes its Hosts.
func restart(ctx context.Context, s *Scope, raw []byte) error {
	if err := s.Restarter.Restart(ctx, raw); err != nil {
		return fmt.Errorf("restarting battery: %w", err)
	}
	if err := s.Store.Save(ctx, raw, false); err != nil {
		return err
	}
	s.Config.Pending = false
	s.HostSet.publish(s.Config.Hosts)
	s.Log.Info("Restarted battery", "hosts", slices.Sorted(maps.Keys(s.Config.Hosts)))
	return nil
}
