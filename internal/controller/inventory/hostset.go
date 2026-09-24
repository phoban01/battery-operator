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
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/phoban01/battery-operator/internal/batterysidecar"
)

// Hosts is a set of Hosts: each Host's name, the Node's, to its flintlockd
// address.
type Hosts map[string]string

// Equal reports whether h and o are the same Hosts at the same addresses.
func (h Hosts) Equal(o Hosts) bool { return maps.Equal(h, o) }

// List is h as batterysidecar renders it, sorted by name.
func (h Hosts) List() []batterysidecar.Host {
	out := make([]batterysidecar.Host, 0, len(h))
	for _, name := range slices.Sorted(maps.Keys(h)) {
		out = append(out, batterysidecar.Host{Name: name, Address: h[name]})
	}
	return out
}

// ErrNotSynced is HostSet's answer before the Inventory Controller has
// published the Hosts battery runs with.
var ErrNotSynced = errors.New("the Inventory Controller has not yet published battery's Hosts")

// HostSet is the Hosts the Inventory Controller has given battery: the
// Hosts battery runs with, which is what the Pool Controller resolves a
// Pool's selector against (03-pools.md, Placement). It changes only once
// battery has restarted with a new set and answers again, never on the
// decision alone, so that no Pool names a Host before battery knows it.
//
// It is safe for concurrent use.
type HostSet struct {
	mu      sync.RWMutex
	hosts   Hosts
	synced  bool
	changed chan struct{}
}

// NewHostSet returns an empty HostSet that has not been published yet.
func NewHostSet() *HostSet {
	return &HostSet{changed: make(chan struct{})}
}

// publish replaces the set with hosts, and wakes Changed's waiters if that
// changes it.
func (h *HostSet) publish(hosts Hosts) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.synced && h.hosts.Equal(hosts) {
		return
	}
	h.hosts = maps.Clone(hosts)
	if h.hosts == nil {
		h.hosts = Hosts{}
	}
	h.synced = true
	close(h.changed)
	h.changed = make(chan struct{})
}

// Synced reports whether the set has been published: until then the
// Operator does not know which Hosts battery runs with.
func (h *HostSet) Synced() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.synced
}

// Hosts returns the Hosts battery runs with, sorted by name, or
// ErrNotSynced.
func (h *HostSet) Hosts() ([]batterysidecar.Host, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if !h.synced {
		return nil, ErrNotSynced
	}
	return h.hosts.List(), nil
}

// Has reports whether battery runs with the Host of that name.
func (h *HostSet) Has(name string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.hosts[name]
	return ok
}

// Changed returns a channel that is closed the next time the set changes.
// A caller that wants every change calls it again after each close.
func (h *HostSet) Changed() <-chan struct{} {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.changed
}

// Matching returns the names, sorted, of the Hosts whose Nodes match a
// Pool's spec.placement.nodeSelector: the Nodes r lists with those labels
// that battery runs with. An empty selector matches every Host. "The
// selector matches a Host" (PO-012, PO-022) is a non-empty answer. Before
// the set is published it returns ErrNotSynced.
func (h *HostSet) Matching(ctx context.Context, r client.Reader, selector map[string]string) ([]string, error) {
	if !h.Synced() {
		return nil, ErrNotSynced
	}
	nodes := &corev1.NodeList{}
	if err := r.List(ctx, nodes, client.MatchingLabels(selector)); err != nil {
		return nil, fmt.Errorf("listing Nodes: %w", err)
	}
	var names []string
	for _, n := range nodes.Items {
		if h.Has(n.Name) {
			names = append(names, n.Name)
		}
	}
	slices.Sort(names)
	return names, nil
}
