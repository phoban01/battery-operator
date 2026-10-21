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
	"flag"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"

	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/batterysidecar"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/reconcile"
)

// The Inventory Controller runs the shared chain of internal/reconcile
// (CLAUDE.md, "Controller structure") over a scope of its own. Its
// "object" is battery's Hosts, not a Kubernetes object, so its scope
// embeds no reconcile.Scope: the controller builds the scope from the
// Nodes, battery's Hosts as ListHosts gives them and battery's client
// certificate, and the subreconcilers bring battery's Hosts in step with
// the Nodes through battery's HostAdmin API.

// Scope is one reconcile of the inventory.
type Scope struct {
	// Nodes are the cluster's Nodes, as listed for this reconcile.
	Nodes []corev1.Node
	// Held is battery's Hosts by name, as ListHosts gave them at the start
	// of the reconcile; the subreconcilers keep it up to date with what
	// they change.
	Held map[string]battery.Host
	// ClientCertificate is battery's flintlockd client certificate as its
	// Secret holds it now, nil while there is no such Secret.
	ClientCertificate []byte

	// Observed is the Hosts the Nodes make now, name to flintlockd
	// address, before any settling; Admit sets it.
	Observed Hosts
	// Desired is the Hosts battery should place on: every settled change
	// applied to the Hosts battery places on now; Settle sets it.
	Desired Hosts
	// Errs are the failures of single Hosts. A step that fails for one
	// Host goes on with the others, and Report returns them all.
	Errs []error

	// State is what the controller remembers between reconciles.
	State *State
	// HostSet is where the Hosts battery places on are published for the
	// Pool Controller.
	HostSet *HostSet
	// Admin is battery's HostAdmin service.
	Admin battery.HostAdmin
	// Mount reads the Operator's own mount of battery's client
	// certificate.
	Mount CertificateMount
	// Options are the controller's timings.
	Options Options
	Log     logr.Logger
	Clock   clock.Clock
}

// State is what the Inventory Controller remembers between reconciles.
// Reconciles of the inventory run one at a time, so it needs no lock.
type State struct {
	// seen is, for each Node that differs from battery's Hosts or did when
	// last looked at, what it made last time and since when.
	seen map[string]seen
	// reloaded is the Digest of the client certificate every Host battery
	// holds was last updated with (DP-035); "" until the first time after
	// the Operator starts.
	reloaded string
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

// Options are the Inventory Controller's timings.
type Options struct {
	// SettleTime is how long a change must hold before it is acted on
	// (IN-011).
	SettleTime time.Duration
	// RemoveRetry is how long the controller waits before it asks battery
	// again to remove a Host that battery refused to remove (IN-016).
	RemoveRetry time.Duration
	// MountPoll is how long the controller waits before it looks again at
	// its mount of battery's client certificate, while the mount does not
	// yet hold the certificate the Secret holds (DP-035).
	MountPoll time.Duration
}

// Defaults for Options.
const (
	DefaultSettleTime  = 30 * time.Second
	DefaultRemoveRetry = 30 * time.Second
	DefaultMountPoll   = 5 * time.Second
)

// BindFlags binds o to the Operator's command line flags, with the
// defaults.
func (o *Options) BindFlags(fs *flag.FlagSet) {
	fs.DurationVar(&o.SettleTime, "inventory-settle-time", DefaultSettleTime,
		"How long a change in whether a Node is a Host must hold before the Inventory Controller acts on it.")
	fs.DurationVar(&o.RemoveRetry, "inventory-remove-retry", DefaultRemoveRetry,
		"How long the Inventory Controller waits before it asks battery again to remove a Host whose Node is gone, "+
			"while battery refuses because a Pool names the Host or a MicroVM is still on it.")
}

// Validate reports an Options the controller cannot run with. A zero
// RemoveRetry or MountPoll takes its default.
func (o Options) Validate() error {
	if o.SettleTime < 0 {
		return fmt.Errorf("inventory settle time %s is negative", o.SettleTime)
	}
	if o.RemoveRetry < 0 {
		return fmt.Errorf("inventory remove retry %s is negative", o.RemoveRetry)
	}
	if o.MountPoll < 0 {
		return fmt.Errorf("inventory mount poll %s is negative", o.MountPoll)
	}
	return nil
}

// removeRetry is RemoveRetry, or its default when zero.
func (o Options) removeRetry() time.Duration {
	if o.RemoveRetry <= 0 {
		return DefaultRemoveRetry
	}
	return o.RemoveRetry
}

// mountPoll is MountPoll, or its default when zero.
func (o Options) mountPoll() time.Duration {
	if o.MountPoll <= 0 {
		return DefaultMountPoll
	}
	return o.MountPoll
}

//= docs/requirements/04-inventory.md#admission
//# The Inventory Controller SHALL configure battery to reach every
//# Host's `flintlockd` over TLS, verifying the serving certificate against
//# the configured certificate authority and presenting the Operator's client
//# certificate for `flintlockd`.

// TLS is how battery reaches every Host's flintlockd: mutual TLS, with the
// client certificate and key and the serving CA at the paths the Manifests
// mount them on in battery's container (DP-005).
var TLS = battery.HostTLS{
	CAFile:   batterysidecar.ServingCAFile,
	CertFile: batterysidecar.ClientCertFile,
	KeyFile:  batterysidecar.ClientKeyFile,
}

// NewChain is the Inventory Controller's chain, in order.
func NewChain() reconcile.Chain[*Scope] {
	return reconcile.Chain[*Scope]{
		Steps: []reconcile.SubReconciler[*Scope]{
			Admit{},
			Settle{},
			Withdraw{},
			Cordon{},
			Register{},
			Secure{},
			Remove{},
			Reload{},
		},
		Finally: []reconcile.SubReconciler[*Scope]{
			Publish{},
			Report{},
		},
	}
}
