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

// Restore puts back battery's Hosts when something other than the
// Inventory Controller has changed them in its configuration: applying the
// Manifests again, which ship the configuration with no Hosts, is the case
// it is for. battery already runs with the Hosts written last, since it
// read its file when it started, so Restore rewrites the file alone and
// does not restart battery. A restart that was pending stays pending, and
// Resume finishes it with the Hosts put back.
//
// Where no Hosts are recorded as written, Restore records the Hosts the
// configuration names: battery started with those.
type Restore struct{}

func (Restore) Reconcile(ctx context.Context, s *Scope) (Result, error) {
	if s.Config.Written == nil {
		s.Config.Written = s.Config.Hosts.clone()
		if err := s.Store.Save(ctx, s.Config); err != nil {
			return Result{Stop: true}, err
		}
		s.Log.Info("Recorded the Hosts battery's configuration names", "hosts", slices.Sorted(maps.Keys(s.Config.Hosts)))
		return Result{}, nil
	}
	if s.Config.Hosts.Equal(s.Config.Written) {
		return Result{}, nil
	}

	//= docs/requirements/04-inventory.md#keeping
	//# If battery's configuration names Hosts other than those the
	//# Inventory Controller last wrote to it, then the Inventory Controller SHALL
	//# write those Hosts back to battery's configuration without restarting
	//# battery.
	raw, err := batterysidecar.Render(s.Config.Written.List())
	if err != nil {
		return Result{Stop: true}, err
	}
	found := s.Config.Hosts
	restored := s.Config
	restored.Hosts = s.Config.Written.clone()
	restored.Raw = raw
	if err := s.Store.Save(ctx, restored); err != nil {
		return Result{Stop: true}, err
	}
	s.Config = restored
	s.Log.Info("Restored battery's Hosts in its configuration, which something else had changed",
		"hosts", slices.Sorted(maps.Keys(restored.Hosts)), "found", slices.Sorted(maps.Keys(found)))
	return Result{}, nil
}

// Resume finishes a restart a previous reconcile started and did not
// finish: its configuration is written and marked pending. It then
// publishes the Hosts battery runs with, if they have not been yet.
type Resume struct{}

func (Resume) Reconcile(ctx context.Context, s *Scope) (Result, error) {
	if s.Config.Pending {
		s.Log.Info("Resumed restart of battery with a configuration written earlier", "hosts", slices.Sorted(maps.Keys(s.Config.Hosts)))
		if err := restart(ctx, s); err != nil {
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

// Renew notices a renewed client certificate: one its Secret holds that
// battery has not restarted with. The restart it needs waits for a restart
// window like a Host change, so that the two go into one restart.
//
// Where no certificate is recorded, which is only before the Inventory
// Controller has restarted battery for the first time, battery read the
// one its Secret holds when its pod started, and Renew records that one
// without restarting battery.
type Renew struct{}

func (Renew) Reconcile(ctx context.Context, s *Scope) (Result, error) {
	if s.ClientCertificate == nil {
		// No Secret: battery cannot have started without it, and nothing
		// has been renewed.
		return Result{}, nil
	}
	digest := Digest(s.ClientCertificate)
	switch s.Config.ClientCertificate {
	case digest:
		return Result{}, nil
	case "":
		s.Config.ClientCertificate = digest
		if err := s.Store.Save(ctx, s.Config); err != nil {
			return Result{Stop: true}, err
		}
		s.Log.Info("Recorded the client certificate battery started with", "sha256", digest)
		return Result{}, nil
	}

	//= docs/requirements/06-deployment.md#battery-sidecar
	//# When the client certificate in the Secret of DP-005 changes,
	//# the Operator SHALL restart battery through the mechanism of DP-006
	s.Renewed = true
	return Result{}, nil
}

// Window batches the settled changes, and a renewed client certificate,
// into restarts of battery: the first one waiting opens a restart window,
// and the chain goes on to Apply only once the window has closed. A window
// whose changes all flapped back closes without a restart.
type Window struct {
	// Length is the restart window.
	Length time.Duration
}

func (w Window) Reconcile(_ context.Context, s *Scope) (Result, error) {
	opened, open := s.State.WindowOpened()
	if s.Desired.Equal(s.Config.Hosts) && !s.Renewed {
		if open {
			s.State.windowOpened = time.Time{}
			s.Log.Info("Closed restart window without restarting battery")
		}
		// A Drain for changes that have since flapped back may have
		// published fewer Hosts than battery runs with: give them back.
		s.State.drainStarted = time.Time{}
		s.HostSet.publish(s.Config.Hosts)
		return Result{Stop: true}, nil
	}

	//= docs/requirements/04-inventory.md#applying
	//# When a change has settled and no restart window is open, the
	//# Inventory Controller SHALL open a restart window of the configured
	//# length, and SHALL apply every change that has settled by the time the
	//# window closes in a single restart of battery.

	//= docs/requirements/06-deployment.md#battery-sidecar
	//# The Operator SHALL restart battery for a changed client
	//# certificate at the close of a restart window of IN-012, in the same
	//# single restart as every change to battery's Hosts that has settled by
	//# then.
	now := s.Clock.Now()
	if !open {
		opened = now
		s.State.windowOpened = now
		s.Log.Info("Opened restart window", "closesAt", now.Add(w.Length),
			"hostsChanged", !s.Desired.Equal(s.Config.Hosts), "clientCertificateRenewed", s.Renewed)
	}
	if left := opened.Add(w.Length).Sub(now); left > 0 {
		return Result{Stop: true, RequeueAfter: left}, nil
	}
	return Result{}, nil
}

// Apply writes the Hosts battery should have to its configuration and
// restarts battery with it, and with the client certificate its Secret
// holds, once.
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
	pending := Config{
		Hosts: s.Desired, Raw: raw, Pending: true,
		ClientCertificate: s.Config.ClientCertificate, Written: s.Desired,
	}
	if err := s.Store.Save(ctx, pending); err != nil {
		return Result{Stop: true}, err
	}
	// The window has closed: whatever happens to this restart, a change
	// from here on waits for a window of its own, and a failed restart is
	// Resume's to finish.
	s.State.windowOpened = time.Time{}
	s.State.drainStarted = time.Time{}
	s.Config = pending
	s.Log.Info("Wrote battery's configuration", "hosts", slices.Sorted(maps.Keys(s.Desired)),
		"clientCertificateRenewed", s.Renewed)
	if err := restart(ctx, s); err != nil {
		return Result{Stop: true}, err
	}
	return Result{}, nil
}

// restart restarts battery with the pending configuration in s and the
// client certificate its Secret holds, then marks the configuration
// applied, records the certificate and publishes the Hosts.
func restart(ctx context.Context, s *Scope) error {
	want := batterysidecar.Mounts{Config: s.Config.Raw, ClientCertificate: s.ClientCertificate}
	if err := s.Restarter.Restart(ctx, want); err != nil {
		return fmt.Errorf("restarting battery: %w", err)
	}
	applied := s.Config
	applied.Pending = false
	if s.ClientCertificate != nil {
		applied.ClientCertificate = Digest(s.ClientCertificate)
	}
	if err := s.Store.Save(ctx, applied); err != nil {
		return err
	}
	s.Config = applied
	s.Renewed = false
	s.HostSet.publish(s.Config.Hosts)
	s.Log.Info("Restarted battery", "hosts", slices.Sorted(maps.Keys(s.Config.Hosts)),
		"clientCertificate", s.Config.ClientCertificate)
	return nil
}
