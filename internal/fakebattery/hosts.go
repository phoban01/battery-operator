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

package fakebattery

import (
	"errors"
	"fmt"
	"slices"
	"sync"

	mvmv1 "github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1"
	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"google.golang.org/grpc"
)

// errUnknownHost is the cause when a flintlock_hosts name is not in Hosts.
var errUnknownHost = errors.New("unknown host")

// Hosts is the set of flintlockd Hosts the fake places MicroVMs on, by the
// names Pools list in flintlock_hosts. Each is a gRPC connection, over
// which the fake uses flintlock's generated MicroVM and MicroVMExec
// clients and nothing else; in tests the connection is to a fake
// flintlockd (fakeflintlock.Server.Conn). Hosts does not own the
// connections: the caller closes them. It is safe for concurrent use.
type Hosts struct {
	mu      sync.RWMutex
	entries map[string]host
}

// host is one Host's clients and the address ClaimVMResponse reports.
type host struct {
	name    string
	address string
	vm      mvmv1.MicroVMClient
	exec    execv1.MicroVMExecClient
}

// NewHosts returns an empty Hosts.
func NewHosts() *Hosts { return &Hosts{entries: make(map[string]host)} }

// Add registers or replaces the Host called name, reached over conn.
// address is what ClaimVMResponse.host.address reports for it, and may be
// empty.
func (h *Hosts) Add(name, address string, conn grpc.ClientConnInterface) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries[name] = host{
		name:    name,
		address: address,
		vm:      mvmv1.NewMicroVMClient(conn),
		exec:    execv1.NewMicroVMExecClient(conn),
	}
}

// Remove forgets a Host. MicroVMs already placed on it can no longer be
// deleted through the fake.
func (h *Hosts) Remove(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.entries, name)
}

// Names lists every Host, sorted.
func (h *Hosts) Names() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	names := make([]string, 0, len(h.entries))
	for n := range h.entries {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// known reports whether name is a Host.
func (h *Hosts) known(name string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.entries[name]
	return ok
}

// get returns the Host called name.
func (h *Hosts) get(name string) (host, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	e, ok := h.entries[name]
	if !ok {
		return host{}, fmt.Errorf("host %q: %w", name, errUnknownHost)
	}
	return e, nil
}
