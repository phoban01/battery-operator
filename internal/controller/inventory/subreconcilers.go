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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"

	corev1 "k8s.io/api/core/v1"

	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/reconcile"
)

// The Node report the Inventory Controller reads (EA-034, EA-035). They
// repeat internal/execagent's AnnotationReady and
// AnnotationFlintlockdAddress, which a test checks, so that the Operator
// does not link the Exec Agent's package.
const (
	AnnotationReady             = "battery.liquidmetal-x.dev/exec-agent-ready"
	AnnotationFlintlockdAddress = "battery.liquidmetal-x.dev/flintlockd-address"

	// annotationTrue is the value of a true annotation.
	annotationTrue = "true"
)

// Admit works out the Hosts the Nodes make now.
type Admit struct{}

func (Admit) Reconcile(_ context.Context, s *Scope) (reconcile.Result, error) {
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
	return reconcile.Result{}, nil
}

// Settle works out the Hosts battery should place on: the Hosts it places
// on now, with every change that has held for the settle time applied. A
// change is a Node becoming a Host, ceasing to be one, or its flintlockd
// address moving.
type Settle struct{}

func (Settle) Reconcile(_ context.Context, s *Scope) (reconcile.Result, error) {
	now := s.Clock.Now()
	placeable := s.placeable()
	names := map[string]bool{}
	for _, m := range []Hosts{s.Observed, placeable} {
		for name := range m {
			names[name] = true
		}
	}
	for name := range s.State.seen {
		names[name] = true
	}

	s.Desired = Hosts{}
	var res reconcile.Result
	for name := range names {
		observed := s.Observed[name] // "" when not a Host
		current := placeable[name]   // "" when battery does not place on it
		last, ok := s.State.seen[name]
		if !ok || last.address != observed {
			last = seen{address: observed, since: now}
		}
		if observed == current {
			// Nothing to apply: a change that flapped back is forgotten.
			delete(s.State.seen, name)
			if current != "" {
				s.Desired[name] = current
			}
			continue
		}
		s.State.seen[name] = last

		//= docs/requirements/04-inventory.md#applying
		//# The Inventory Controller SHALL act on a change in whether a Node
		//# is a Host only after the change has held for the configured settle time.
		if held := now.Sub(last.since); held < s.Options.SettleTime {
			if current != "" {
				s.Desired[name] = current
			}
			res = res.Merge(reconcile.Result{RequeueAfter: s.Options.SettleTime - held})
			continue
		}
		if observed != "" {
			s.Desired[name] = observed
		}
	}
	return res, nil
}

// Withdraw takes from the Pool Controller every Host the next steps cordon
// or remove, and every Host whose address is about to change, before they
// call battery: it publishes the Hosts battery places on now that it is to
// go on placing on, at the same address.
type Withdraw struct{}

func (Withdraw) Reconcile(_ context.Context, s *Scope) (reconcile.Result, error) {
	//= docs/requirements/04-inventory.md#applying
	//# The Inventory Controller SHALL give the Pool Controller a Host
	//# only once battery holds it uncordoned at the Node's address, and SHALL
	//# take a Host from the Pool Controller before it cordons or removes the Host.
	publishPlaceable(s)
	return reconcile.Result{}, nil
}

// Cordon cordons in battery every Host battery places on that it should
// no longer: its Node is cordoned, not ready, gives no address, or is
// gone. Bound claims on the Host keep running, and battery places nothing
// new there (BA-084).
type Cordon struct{}

func (Cordon) Reconcile(ctx context.Context, s *Scope) (reconcile.Result, error) {
	for _, name := range slices.Sorted(maps.Keys(s.Held)) {
		h := s.Held[name]
		if h.Cordoned {
			continue
		}
		if _, ok := s.Desired[name]; ok {
			continue
		}
		reason := s.leaving(name)

		//= docs/requirements/04-inventory.md#admission
		//# When a Node that is a Host is cordoned, or its Node report
		//# says the Host is not ready or gives no `flintlockd` address, the
		//# Inventory Controller SHALL cordon the Host in battery with `CordonHost`,
		//# and SHALL keep it in battery's Hosts.
		got, err := s.Admin.CordonHost(ctx, name, reason)
		if err != nil {
			s.Errs = append(s.Errs, fmt.Errorf("cordoning Host %s in battery: %w", name, err))
			continue
		}
		s.Held[name] = *got
		s.Log.Info("Cordoned Host in battery", "host", name, "reason", reason)
	}
	return reconcile.Result{}, nil
}

// Register brings every Host battery should place on into battery,
// uncordoned, at its Node's address and with TLS: it adds a new one,
// updates one whose address or TLS settings differ, and uncordons one that
// is cordoned. battery checks that it can reach the Host when it is added
// or its address changes (BA-080); a Host that fails stays out of the
// Hosts battery places on, and the reconcile fails for it.
type Register struct{}

func (Register) Reconcile(ctx context.Context, s *Scope) (reconcile.Result, error) {
	for _, name := range slices.Sorted(maps.Keys(s.Desired)) {
		if err := register(ctx, s, name, s.Desired[name]); err != nil {
			s.Errs = append(s.Errs, err)
		}
	}
	return reconcile.Result{}, nil
}

// register brings the Host called name into battery at address.
func register(ctx context.Context, s *Scope, name, address string) error {
	//= docs/requirements/04-inventory.md#applying
	//# While a Node is a Host, the Inventory Controller SHALL keep a
	//# Host of the Node's name in battery, uncordoned, at the Node's `flintlockd`
	//# address and with the TLS settings of IN-004, adding it with `AddHost`,
	//# and changing its address or TLS settings with `UpdateHost` and uncordoning
	//# it with `UncordonHost` wherever battery's Host differs.
	want := battery.Host{Name: name, Address: address, TLS: TLS}
	h, held := s.Held[name]
	if !held {
		got, err := s.Admin.AddHost(ctx, want, false)
		switch {
		case err == nil:
			s.Held[name] = *got
			s.Log.Info("Added Host to battery", "host", name, "address", address)
			h = *got
		case errors.Is(err, battery.ErrAlreadyExists):
			//= docs/requirements/04-inventory.md#applying
			//# If `AddHost` answers that battery already holds a Host of that
			//# name, then the Inventory Controller SHALL read the Host with `GetHost` and
			//# bring it in step as IN-017 says, without failing the reconcile.
			st, err := s.Admin.GetHost(ctx, name)
			if err != nil {
				return fmt.Errorf("reading Host %s, which battery already holds: %w", name, err)
			}
			h = st.Host
			s.Held[name] = h
			s.Log.Info("Found Host already in battery", "host", name, "address", h.Address)
		default:
			return fmt.Errorf("adding Host %s to battery: %w", name, err)
		}
	}
	if h.Address != address || h.TLS != TLS {
		got, err := s.Admin.UpdateHost(ctx, want, false)
		if err != nil {
			return fmt.Errorf("updating Host %s in battery: %w", name, err)
		}
		s.Held[name] = *got
		s.Log.Info("Updated Host in battery", "host", name, "address", address, "previousAddress", h.Address)
		h = *got
	}
	if h.Cordoned {
		got, err := s.Admin.UncordonHost(ctx, name)
		if err != nil {
			return fmt.Errorf("uncordoning Host %s in battery: %w", name, err)
		}
		s.Held[name] = *got
		s.Log.Info("Uncordoned Host in battery", "host", name)
	}
	return nil
}

// Secure gives every other Host battery holds the TLS settings of IN-004,
// at the address battery has for it. A Host that battery v0.4 copied from
// its configuration file has none, and battery cannot reach it, nor delete
// the MicroVMs on it, until it has. battery does not check that it can
// reach the Host for this: the Host is cordoned, or about to be removed.
type Secure struct{}

func (Secure) Reconcile(ctx context.Context, s *Scope) (reconcile.Result, error) {
	for _, name := range slices.Sorted(maps.Keys(s.Held)) {
		h := s.Held[name]
		if _, ok := s.Desired[name]; ok || h.TLS == TLS {
			continue
		}

		//= docs/requirements/04-inventory.md#upgrade
		//# When battery holds a Host whose TLS settings are not those of
		//# IN-004, the Inventory Controller SHALL update the Host with `UpdateHost` to
		//# those settings, keeping its address.
		got, err := s.Admin.UpdateHost(ctx, battery.Host{Name: name, Address: h.Address, TLS: TLS}, true)
		if err != nil {
			if !errors.Is(err, battery.ErrNotFound) {
				s.Errs = append(s.Errs, fmt.Errorf("giving Host %s its TLS settings in battery: %w", name, err))
			}
			continue
		}
		s.Held[name] = *got
		s.Log.Info("Gave Host its TLS settings in battery", "host", name, "address", h.Address)
	}
	return reconcile.Result{}, nil
}

// Remove removes from battery every Host whose Node is gone, once it is
// cordoned. battery refuses while a Pool names the Host or a MicroVM is
// still on it (BA-083), and Remove asks again after the remove retry.
type Remove struct{}

func (Remove) Reconcile(ctx context.Context, s *Scope) (reconcile.Result, error) {
	live := map[string]bool{}
	for i := range s.Nodes {
		if s.Nodes[i].DeletionTimestamp.IsZero() {
			live[s.Nodes[i].Name] = true
		}
	}
	var res reconcile.Result
	for _, name := range slices.Sorted(maps.Keys(s.Held)) {
		if live[name] || !s.Held[name].Cordoned {
			continue // a Node's Host, or not yet cordoned: Cordon has failed for it
		}
		if _, ok := s.Desired[name]; ok {
			continue // still settling
		}

		//= docs/requirements/04-inventory.md#admission
		//# When battery holds a Host whose Node does not exist or is being
		//# deleted, the Inventory Controller SHALL cordon the Host and remove it with
		//# `RemoveHost`, and SHALL try again while battery refuses because a Pool
		//# names the Host or a MicroVM is still on it.
		err := s.Admin.RemoveHost(ctx, name)
		switch {
		case err == nil, errors.Is(err, battery.ErrNotFound):
			delete(s.Held, name)
			s.Log.Info("Removed Host from battery", "host", name)
		case errors.Is(err, battery.ErrFailedPrecondition):
			s.Log.Info("Waited to remove Host from battery, which still uses it", "host", name, "reason", err.Error(),
				"retryAfter", s.Options.removeRetry())
			res = res.Merge(reconcile.Result{RequeueAfter: s.Options.removeRetry()})
		default:
			s.Errs = append(s.Errs, fmt.Errorf("removing Host %s from battery: %w", name, err))
		}
	}
	return res, nil
}

// Reload makes battery read its client certificate again, when cert-manager
// has renewed it and once after the Operator starts: it updates every Host
// battery holds with the settings battery has for it, which makes battery
// connect to the Host again and read the certificate's files (BA-081). It
// waits until the Operator's own mount of the Secret, the volume battery
// reads, holds the certificate the Secret holds.
type Reload struct{}

func (Reload) Reconcile(ctx context.Context, s *Scope) (reconcile.Result, error) {
	if s.ClientCertificate == nil {
		// No Secret: battery has no certificate to read again.
		return reconcile.Result{}, nil
	}
	digest := Digest(s.ClientCertificate)
	if s.State.reloaded == digest {
		return reconcile.Result{}, nil
	}

	//= docs/requirements/06-deployment.md#battery-sidecar
	//# When the client certificate in the Secret of DP-005 changes,
	//# and once after the Operator starts, the Operator SHALL call `UpdateHost`
	//# for every Host battery holds, with the Host's address and TLS settings
	//# unchanged, once the Operator's own mount of that Secret holds the
	//# certificate the Secret holds.
	mounted, err := s.Mount.ClientCertificate()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.Errs = append(s.Errs, fmt.Errorf("reading the mounted client certificate: %w", err))
		return reconcile.Result{}, nil
	}
	if !bytes.Equal(mounted, s.ClientCertificate) {
		s.Log.V(1).Info("Waited for the mounted client certificate to match its Secret", "sha256", digest)
		return reconcile.Result{RequeueAfter: s.Options.mountPoll()}, nil
	}
	failed := false
	for _, name := range slices.Sorted(maps.Keys(s.Held)) {
		h := s.Held[name]
		if h.TLS != TLS {
			// No TLS settings yet, since Secure failed for it: battery has no
			// certificate to read again for it, refuses UpdateHost without
			// settings, and reads the files when Secure gives it them.
			continue
		}
		got, err := s.Admin.UpdateHost(ctx, battery.Host{Name: name, Address: h.Address, TLS: h.TLS}, true)
		switch {
		case err == nil:
			s.Held[name] = *got
		case errors.Is(err, battery.ErrNotFound):
			delete(s.Held, name)
		default:
			failed = true
			s.Errs = append(s.Errs, fmt.Errorf("reloading the client certificate for Host %s: %w", name, err))
		}
	}
	if failed {
		return reconcile.Result{}, nil
	}
	s.State.reloaded = digest
	s.Log.Info("Reloaded battery's client certificate on every Host", "sha256", digest,
		"hosts", slices.Sorted(maps.Keys(s.Held)))
	return reconcile.Result{}, nil
}

// Publish gives the Pool Controller the Hosts battery places on, after
// the steps have changed them, however the steps ended.
type Publish struct{}

func (Publish) Reconcile(_ context.Context, s *Scope) (reconcile.Result, error) {
	publishPlaceable(s)
	return reconcile.Result{}, nil
}

// Report fails the reconcile with every Host's failure, so that it is
// retried with back-off.
type Report struct{}

func (Report) Reconcile(_ context.Context, s *Scope) (reconcile.Result, error) {
	return reconcile.Result{}, errors.Join(s.Errs...)
}

// publishPlaceable publishes the Hosts battery places on now and is to go
// on placing on at the same address: held, uncordoned, and desired at the
// address battery has. Before Settle has run, every Host battery places on
// is desired.
func publishPlaceable(s *Scope) {
	out := Hosts{}
	for name, address := range s.placeable() {
		if s.Desired == nil || s.Desired[name] == address {
			out[name] = address
		}
	}
	s.HostSet.publish(out)
}

// placeable is the Hosts battery places on now: held and uncordoned, name
// to address.
func (s *Scope) placeable() Hosts {
	out := Hosts{}
	for name, h := range s.Held {
		if !h.Cordoned {
			out[name] = h.Address
		}
	}
	return out
}

// leaving says why the Host called name is no longer to be placed on, for
// CordonHost's reason.
func (s *Scope) leaving(name string) string {
	var node *corev1.Node
	for i := range s.Nodes {
		if s.Nodes[i].Name == name {
			node = &s.Nodes[i]
		}
	}
	switch {
	case node == nil || !node.DeletionTimestamp.IsZero():
		return "Node deleted"
	case node.Spec.Unschedulable:
		return "Node cordoned"
	case node.Annotations[AnnotationReady] != annotationTrue:
		return "Host not ready"
	case node.Annotations[AnnotationFlintlockdAddress] == "":
		return "Node report gives no flintlockd address"
	}
	return "not a Host"
}

// Digest is the SHA-256 digest of a client certificate, in hex.
func Digest(cert []byte) string {
	sum := sha256.Sum256(cert)
	return hex.EncodeToString(sum[:])
}
