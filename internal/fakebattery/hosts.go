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
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	mvmv1 "github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1"
	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// errUnknownHost is the cause when a flintlock_hosts name is not a Host the
// fake holds, or one it holds with no connection.
var errUnknownHost = errors.New("unknown host")

//= docs/requirements/10-battery.md#hosts
//= type=exception
//= reason=The fake reaches each Host over the connection a test, or its Dial, gives it, and reads no certificate, so there is none for it to read again.
//# battery SHALL read a Host's client certificate, key and
//# certificate authority each time it connects to the Host, which it does
//# when it starts and in `AddHost` and `UpdateHost`, and SHALL present that
//# certificate to the Host until it next connects to it.

// Hosts is the set of flintlockd Hosts the fake holds, by the names Pools
// list in flintlock_hosts: battery's Host registry. Each Host may have a
// gRPC connection, over which the fake uses flintlock's generated MicroVM
// and MicroVMExec clients and nothing else; in tests the connection is to a
// fake flintlockd (fakeflintlock.Server.Conn). The Hosts a test adds with
// Add are held from the start, uncordoned, as if added already; HostAdmin's
// AddHost, UpdateHost and RemoveHost change the set while the fake runs.
// Hosts does not own the connections: the caller closes them. It is safe
// for concurrent use.
type Hosts struct {
	mu      sync.RWMutex
	entries map[string]*host
}

// host is one Host: its registry record, and its clients when it has a
// connection.
type host struct {
	name    string
	address string
	// tls is the HostTLS AddHost or UpdateHost gave; nil for a Host Add
	// gave, which has no TLS settings.
	tls            *poolmgrv1.HostTLS
	cordoned       bool
	cordonedReason string
	cordonedAt     time.Time
	updatedAt      time.Time
	// vm and exec are nil when the Host has no connection: one added with
	// skip_validation that the fake's Dial could not reach.
	vm   mvmv1.MicroVMClient
	exec execv1.MicroVMExecClient
	// dials counts the connections the fake has made to the Host through
	// its Dial, in AddHost and UpdateHost.
	dials int
}

// NewHosts returns an empty Hosts.
func NewHosts() *Hosts { return &Hosts{entries: make(map[string]*host)} }

// Add holds the Host called name, reached over conn, uncordoned and with no
// TLS settings, or replaces its connection and address if it is held.
// address is what ClaimVMResponse.host.address and HostAdmin report for it,
// and may be empty.
func (h *Hosts) Add(name, address string, conn grpc.ClientConnInterface) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.entries[name]
	if !ok {
		e = &host{name: name}
		h.entries[name] = e
	}
	e.address = address
	e.setConn(conn)
}

// Remove forgets a Host, whatever is on it. MicroVMs already placed on it
// can no longer be deleted through the fake. HostAdmin's RemoveHost is the
// checked way.
func (h *Hosts) Remove(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.entries, name)
}

// Names lists every Host held, sorted.
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

// SetCordoned cordons or uncordons the Host called name, as CordonHost and
// UncordonHost do, for a test that sets up Hosts before the fake runs. It
// reports whether the fake holds the Host.
func (h *Hosts) SetCordoned(name string, cordoned bool, reason string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.entries[name]
	if !ok {
		return false
	}
	e.cordoned = cordoned
	e.cordonedReason = ""
	e.cordonedAt = time.Time{}
	if cordoned {
		e.cordonedReason = reason
		e.cordonedAt = time.Now()
	}
	return true
}

// Cordoned reports whether the Host called name is held and cordoned.
func (h *Hosts) Cordoned(name string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	e, ok := h.entries[name]
	return ok && e.cordoned
}

// Dials is how many times the fake has connected to the Host called name
// through Config.Dial: once for AddHost and once for each UpdateHost that
// connected. A Host held only through Add has none.
func (h *Hosts) Dials(name string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if e, ok := h.entries[name]; ok {
		return e.dials
	}
	return 0
}

// setConn sets e's clients from conn; nil clears them.
func (e *host) setConn(conn grpc.ClientConnInterface) {
	if conn == nil {
		e.vm, e.exec = nil, nil
		return
	}
	e.vm = mvmv1.NewMicroVMClient(conn)
	e.exec = execv1.NewMicroVMExecClient(conn)
}

// held reports whether name is a Host the fake holds.
func (h *Hosts) held(name string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.entries[name]
	return ok
}

// eligible reports whether a new MicroVM may be placed on name: the fake
// holds it and it is not cordoned, as battery's ineligibleHosts decides.
func (h *Hosts) eligible(name string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	e, ok := h.entries[name]
	return ok && !e.cordoned
}

// get returns the Host called name with its clients, or errUnknownHost when
// the fake does not hold it or holds it with no connection.
func (h *Hosts) get(name string) (host, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	e, ok := h.entries[name]
	if !ok || e.vm == nil {
		return host{}, fmt.Errorf("host %q: %w", name, errUnknownHost)
	}
	return *e, nil
}

// record is the Host called name as HostAdmin reports it, and whether it is
// held.
func (h *Hosts) record(name string) (*poolmgrv1.Host, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	e, ok := h.entries[name]
	if !ok {
		return nil, false
	}
	return e.proto(), true
}

// proto renders e as a poolmgr Host.
func (e *host) proto() *poolmgrv1.Host {
	out := &poolmgrv1.Host{
		Name:           e.name,
		Address:        e.address,
		Cordoned:       e.cordoned,
		CordonedReason: e.cordonedReason,
	}
	if e.tls != nil {
		out.Tls = proto.CloneOf(e.tls)
	}
	if !e.cordonedAt.IsZero() {
		out.CordonedAt = timestampOf(e.cordonedAt)
	}
	if !e.updatedAt.IsZero() {
		out.UpdatedAt = timestampOf(e.updatedAt)
	}
	return out
}
