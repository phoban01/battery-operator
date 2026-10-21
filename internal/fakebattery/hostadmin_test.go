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
	"slices"
	"testing"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

// The TLS settings the tests give AddHost and UpdateHost: paths in
// battery's container, which the fake never reads.
var (
	testTLS  = &poolmgrv1.HostTLS{CaFile: "/etc/battery/tls/serving-ca.crt", CertFile: "/etc/battery/tls/tls.crt", KeyFile: "/etc/battery/tls/tls.key"}
	otherTLS = &poolmgrv1.HostTLS{CaFile: "/other/ca.crt", CertFile: "/other/tls.crt", KeyFile: "/other/tls.key"}
)

// hostAdmin is battery's generated HostAdmin client over the harness's
// connection.
func (h *harness) hostAdmin() poolmgrv1.HostAdminClient {
	return poolmgrv1.NewHostAdminClient(h.rawConn())
}

// dialer is a Config.Dial over test Hosts, by address: the Host at
// name:9090 for each name. An address it does not know cannot be reached.
func dialer(hosts ...*testHost) func(string) (grpc.ClientConnInterface, error) {
	byAddress := map[string]*testHost{}
	for _, h := range hosts {
		byAddress[h.name+":9090"] = h
	}
	return func(address string) (grpc.ClientConnInterface, error) {
		h, ok := byAddress[address]
		if !ok {
			return nil, errors.New("connection refused")
		}
		return h.conn, nil
	}
}

// hostRequest is a Host for AddHost or UpdateHost at name:9090 with
// testTLS.
func hostRequest(name string) *poolmgrv1.Host {
	return &poolmgrv1.Host{Name: name, Address: name + ":9090", Tls: proto.CloneOf(testTLS)}
}

//= docs/requirements/08-test-doubles.md#fake-battery
//= type=test
//# The fake battery SHALL serve battery's `HostAdmin` service,
//# and SHALL hold its Hosts as battery does, so that a Pool names only Hosts
//# the fake holds, a Host is removed only once no Pool names it and nothing
//# is on it, and no MicroVM is placed on a cordoned Host.

// TestHostAdminIsServed: the Hosts a test gives the fake are held from the
// start, uncordoned, and HostAdmin lists them; a Host added through it is
// listed beside them and takes MicroVMs over the connection Dial gave.
func TestHostAdminIsServed(t *testing.T) {
	b := newTestHost(t, hostB)
	h := newHarness(t, Config{Dial: dialer(b)}, hostA)
	admin := h.hostAdmin()

	list, err := admin.ListHosts(h.ctx, &poolmgrv1.ListHostsRequest{})
	if err != nil {
		t.Fatalf("ListHosts: %v", err)
	}
	if len(list.GetHosts()) != 1 || list.GetHosts()[0].GetHost().GetName() != hostA || list.GetHosts()[0].GetHost().GetCordoned() {
		t.Fatalf("ListHosts = %v, want host-a alone, uncordoned", list.GetHosts())
	}

	if _, err := admin.AddHost(h.ctx, &poolmgrv1.AddHostRequest{Host: hostRequest(hostB)}); err != nil {
		t.Fatalf("AddHost: %v", err)
	}
	list, err = admin.ListHosts(h.ctx, &poolmgrv1.ListHostsRequest{})
	if err != nil {
		t.Fatalf("ListHosts: %v", err)
	}
	names := make([]string, 0, len(list.GetHosts()))
	for _, hs := range list.GetHosts() {
		names = append(names, hs.GetHost().GetName())
	}
	if len(names) != 2 || names[0] != hostA || names[1] != hostB {
		t.Fatalf("ListHosts names = %v, want [host-a host-b]", names)
	}

	h.createPool(h.spec("pool", 1, hostB))
	if got := h.vmHosts(); got[hostB] != 1 || b.live() != 1 {
		t.Fatalf("placement = %v with %d on host-b's flintlockd, want the one microvm there", got, b.live())
	}
}

//= docs/requirements/10-battery.md#hosts
//= type=test
//# When `AddHost` names a Host battery does not hold, battery
//# SHALL store the Host uncordoned, with the address and TLS settings the
//# request gives, and, unless the request sets `skip_validation`, SHALL
//# first connect to the Host and check its `flintlockd` version, answering
//# `FAILED_PRECONDITION` and storing nothing if either fails.

// TestAddHost covers AddHost's validation and what it stores.
func TestAddHost(t *testing.T) {
	tests := []struct {
		name string
		// noDial leaves Config.Dial nil.
		noDial bool
		host   *poolmgrv1.Host
		skip   bool
		want   codes.Code
		// wantDials is the connections made to a Host that is stored.
		wantDials int
	}{
		{name: "a reachable host is stored", host: hostRequest(hostB), want: codes.OK, wantDials: 1},
		{
			name:      "a new host starts uncordoned whatever the request says",
			host:      &poolmgrv1.Host{Name: hostB, Address: hostB + ":9090", Tls: testTLS, Cordoned: true, CordonedReason: "no"},
			want:      codes.OK,
			wantDials: 1,
		},
		{name: "an unreachable host is refused", host: hostRequest(hostC), want: codes.FailedPrecondition},
		{name: "no dial reaches no host", noDial: true, host: hostRequest(hostB), want: codes.FailedPrecondition},
		{name: "skip_validation stores an unreachable host", host: hostRequest(hostC), skip: true, want: codes.OK},
		{name: "skip_validation with no dial stores the host", noDial: true, host: hostRequest(hostB), skip: true, want: codes.OK},
		{name: "no name", host: &poolmgrv1.Host{Address: "x:9090", Tls: testTLS}, want: codes.InvalidArgument},
		{name: "no address", host: &poolmgrv1.Host{Name: hostB, Tls: testTLS}, want: codes.InvalidArgument},
		{
			name: "insecure with files",
			host: &poolmgrv1.Host{Name: hostB, Address: hostB + ":9090", Tls: &poolmgrv1.HostTLS{Insecure: true, CaFile: "/ca"}},
			want: codes.InvalidArgument,
		},
		{name: "no certificate authority", host: &poolmgrv1.Host{Name: hostB, Address: hostB + ":9090"}, want: codes.InvalidArgument},
		{
			name: "a certificate without its key",
			host: &poolmgrv1.Host{Name: hostB, Address: hostB + ":9090", Tls: &poolmgrv1.HostTLS{CaFile: "/ca", CertFile: "/crt"}},
			want: codes.InvalidArgument,
		},
		{
			name: "invalid settings are refused even with skip_validation",
			host: &poolmgrv1.Host{Name: hostB, Address: hostB + ":9090"}, skip: true,
			want: codes.InvalidArgument,
		},
		{
			name:      "an insecure host",
			host:      &poolmgrv1.Host{Name: hostB, Address: hostB + ":9090", Tls: &poolmgrv1.HostTLS{Insecure: true}},
			want:      codes.OK,
			wantDials: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Dial: dialer(newTestHost(t, hostB))}
			if tc.noDial {
				cfg.Dial = nil
			}
			h := newHarness(t, cfg, hostA)
			admin := h.hostAdmin()
			got, err := admin.AddHost(h.ctx, &poolmgrv1.AddHostRequest{Host: tc.host, SkipValidation: tc.skip})
			if code := statusCode(t, err); code != tc.want {
				t.Fatalf("AddHost = %v, want %v", err, tc.want)
			}
			if tc.want != codes.OK {
				if names := h.hosts.Names(); len(names) != 1 || names[0] != hostA {
					t.Fatalf("hosts after a refused AddHost = %v, want host-a alone", names)
				}
				return
			}
			st, err := admin.GetHost(h.ctx, &poolmgrv1.GetHostRequest{Name: tc.host.GetName()})
			if err != nil {
				t.Fatalf("GetHost: %v", err)
			}
			for _, stored := range []*poolmgrv1.Host{got, st.GetHost()} {
				if stored.GetCordoned() || stored.GetCordonedReason() != "" || stored.GetCordonedAt() != nil {
					t.Fatalf("stored host %v is cordoned, want uncordoned", stored)
				}
				if stored.GetAddress() != tc.host.GetAddress() || !proto.Equal(stored.GetTls(), tc.host.GetTls()) {
					t.Fatalf("stored host %v, want the address and TLS settings of %v", stored, tc.host)
				}
			}
			if d := h.hosts.Dials(tc.host.GetName()); d != tc.wantDials {
				t.Fatalf("Dials = %d, want %d", d, tc.wantDials)
			}
		})
	}
}

//= docs/requirements/10-battery.md#hosts
//= type=test
//# If `AddHost` names a Host battery already holds, then battery
//# SHALL answer `ALREADY_EXISTS`, and SHALL leave the Host as it is.

// TestAddHostOnAHeldHost: a Host held from the start and one AddHost
// stored are both ALREADY_EXISTS, and neither changes.
func TestAddHostOnAHeldHost(t *testing.T) {
	h := newHarness(t, Config{Dial: dialer(newTestHost(t, hostB))}, hostA)
	admin := h.hostAdmin()
	if _, err := admin.AddHost(h.ctx, &poolmgrv1.AddHostRequest{Host: hostRequest(hostB)}); err != nil {
		t.Fatalf("AddHost: %v", err)
	}
	for _, name := range []string{hostA, hostB} {
		before, err := admin.GetHost(h.ctx, &poolmgrv1.GetHostRequest{Name: name})
		if err != nil {
			t.Fatalf("GetHost(%s): %v", name, err)
		}
		again := &poolmgrv1.Host{Name: name, Address: "elsewhere:9090", Tls: otherTLS}
		_, err = admin.AddHost(h.ctx, &poolmgrv1.AddHostRequest{Host: again, SkipValidation: true})
		if statusCode(t, err) != codes.AlreadyExists {
			t.Fatalf("AddHost(%s) again = %v, want ALREADY_EXISTS", name, err)
		}
		after, err := admin.GetHost(h.ctx, &poolmgrv1.GetHostRequest{Name: name})
		if err != nil {
			t.Fatalf("GetHost(%s): %v", name, err)
		}
		if !proto.Equal(before, after) {
			t.Fatalf("host %s changed: %v, want %v", name, after, before)
		}
	}
	if d := h.hosts.Dials(hostB); d != 1 {
		t.Fatalf("Dials(host-b) = %d, want 1: a refused AddHost connects to nothing", d)
	}
}

//= docs/requirements/10-battery.md#hosts
//= type=test
//# If `UpdateHost`, `RemoveHost` or `GetHost` names a Host
//# battery does not hold, then battery SHALL answer `NOT_FOUND`.

// TestUnknownHostIsNotFound, and CordonHost and UncordonHost answer
// FAILED_PRECONDITION for it, as battery does.
func TestUnknownHostIsNotFound(t *testing.T) {
	h := newHarness(t, Config{Dial: dialer(newTestHost(t, hostB))}, hostA)
	admin := h.hostAdmin()
	_, err := admin.UpdateHost(h.ctx, &poolmgrv1.UpdateHostRequest{Host: hostRequest(hostB)})
	if statusCode(t, err) != codes.NotFound {
		t.Errorf("UpdateHost = %v, want NOT_FOUND", err)
	}
	_, err = admin.RemoveHost(h.ctx, &poolmgrv1.RemoveHostRequest{Name: hostB})
	if statusCode(t, err) != codes.NotFound {
		t.Errorf("RemoveHost = %v, want NOT_FOUND", err)
	}
	_, err = admin.GetHost(h.ctx, &poolmgrv1.GetHostRequest{Name: hostB})
	if statusCode(t, err) != codes.NotFound {
		t.Errorf("GetHost = %v, want NOT_FOUND", err)
	}
	_, err = admin.CordonHost(h.ctx, &poolmgrv1.CordonHostRequest{Name: hostB})
	if statusCode(t, err) != codes.FailedPrecondition {
		t.Errorf("CordonHost = %v, want FAILED_PRECONDITION", err)
	}
	_, err = admin.UncordonHost(h.ctx, &poolmgrv1.UncordonHostRequest{Name: hostB})
	if statusCode(t, err) != codes.FailedPrecondition {
		t.Errorf("UncordonHost = %v, want FAILED_PRECONDITION", err)
	}
	if names := h.hosts.Names(); len(names) != 1 || names[0] != hostA {
		t.Fatalf("hosts = %v, want host-a alone", names)
	}
}

//= docs/requirements/10-battery.md#hosts
//= type=test
//# battery SHALL keep a Host's cordon state across `UpdateHost`,
//# and SHALL answer `CordonHost` on a cordoned Host, and `UncordonHost` on an
//# uncordoned one, with the Host and no error.

// TestCordonAndUpdateHost: cordoning twice updates the reason, UpdateHost
// replaces the address and TLS settings, connects again and keeps the
// cordon, and uncordoning twice is no error.
func TestCordonAndUpdateHost(t *testing.T) {
	b := newTestHost(t, hostB)
	h := newHarness(t, Config{Dial: dialer(b, newTestHost(t, hostC))}, hostA)
	admin := h.hostAdmin()
	if _, err := admin.AddHost(h.ctx, &poolmgrv1.AddHostRequest{Host: hostRequest(hostB)}); err != nil {
		t.Fatalf("AddHost: %v", err)
	}

	for _, reason := range []string{"drain", "maintenance"} {
		got, err := admin.CordonHost(h.ctx, &poolmgrv1.CordonHostRequest{Name: hostB, Reason: reason})
		if err != nil {
			t.Fatalf("CordonHost(%q): %v", reason, err)
		}
		if !got.GetCordoned() || got.GetCordonedReason() != reason || got.GetCordonedAt() == nil {
			t.Fatalf("CordonHost(%q) = %v, want cordoned with that reason", reason, got)
		}
	}

	// The same settings: battery connects again, so reads the certificate
	// again, and the cordon stays.
	got, err := admin.UpdateHost(h.ctx, &poolmgrv1.UpdateHostRequest{Host: hostRequest(hostB)})
	if err != nil {
		t.Fatalf("UpdateHost: %v", err)
	}
	if !got.GetCordoned() || got.GetCordonedReason() != "maintenance" {
		t.Fatalf("UpdateHost = %v, want the cordon kept", got)
	}
	if d := h.hosts.Dials(hostB); d != 2 {
		t.Fatalf("Dials = %d after UpdateHost, want 2", d)
	}

	// New settings replace the old ones; the cordon fields of the request
	// are ignored.
	moved := &poolmgrv1.Host{Name: hostB, Address: hostC + ":9090", Tls: otherTLS}
	if _, err := admin.UpdateHost(h.ctx, &poolmgrv1.UpdateHostRequest{Host: moved}); err != nil {
		t.Fatalf("UpdateHost to a new address: %v", err)
	}
	st, err := admin.GetHost(h.ctx, &poolmgrv1.GetHostRequest{Name: hostB})
	if err != nil {
		t.Fatalf("GetHost: %v", err)
	}
	if st.GetHost().GetAddress() != hostC+":9090" || !proto.Equal(st.GetHost().GetTls(), otherTLS) || !st.GetHost().GetCordoned() {
		t.Fatalf("GetHost = %v, want the new address and settings, still cordoned", st.GetHost())
	}

	// An unreachable address is refused, and changes nothing.
	gone := &poolmgrv1.Host{Name: hostB, Address: "nowhere:9090", Tls: testTLS}
	if _, err := admin.UpdateHost(h.ctx, &poolmgrv1.UpdateHostRequest{Host: gone}); statusCode(t, err) != codes.FailedPrecondition {
		t.Fatalf("UpdateHost to an unreachable address = %v, want FAILED_PRECONDITION", err)
	}
	if st2, _ := admin.GetHost(h.ctx, &poolmgrv1.GetHostRequest{Name: hostB}); st2.GetHost().GetAddress() != hostC+":9090" {
		t.Fatalf("address after a refused UpdateHost = %q, want it unchanged", st2.GetHost().GetAddress())
	}

	for range 2 {
		got, err := admin.UncordonHost(h.ctx, &poolmgrv1.UncordonHostRequest{Name: hostB})
		if err != nil {
			t.Fatalf("UncordonHost: %v", err)
		}
		if got.GetCordoned() || got.GetCordonedReason() != "" || got.GetCordonedAt() != nil {
			t.Fatalf("UncordonHost = %v, want uncordoned", got)
		}
	}
}

//= docs/requirements/10-battery.md#hosts
//= type=test
//# If `CreatePool` or `UpdatePool` names in `flintlock_hosts` a
//# Host battery does not hold, then battery SHALL refuse the call with
//# `INVALID_ARGUMENT`.

// TestPoolsNameOnlyHeldHosts: a Pool naming a Host the fake does not hold
// is refused, on create and on update, and accepted once the Host is
// added.
func TestPoolsNameOnlyHeldHosts(t *testing.T) {
	h := newHarness(t, Config{Dial: dialer(newTestHost(t, hostB))}, hostA)
	if _, err := h.client.CreatePool(h.ctx, h.spec("pool", 1, hostA, hostB)); statusCode(t, err) != codes.InvalidArgument {
		t.Fatalf("CreatePool naming host-b = %v, want INVALID_ARGUMENT", err)
	}
	if _, err := h.client.GetPool(h.ctx, h.ref("pool")); statusCode(t, err) != codes.NotFound {
		t.Fatalf("GetPool after a refused CreatePool = %v, want NOT_FOUND", err)
	}
	h.createPool(h.spec("pool", 1, hostA))
	if _, err := h.client.UpdatePool(h.ctx, h.spec("pool", 1, hostA, hostB)); statusCode(t, err) != codes.InvalidArgument {
		t.Fatalf("UpdatePool naming host-b = %v, want INVALID_ARGUMENT", err)
	}
	if p := h.pool("pool"); len(p.Spec.FlintlockHosts) != 1 {
		t.Fatalf("flintlock_hosts after a refused UpdatePool = %v, want [host-a]", p.Spec.FlintlockHosts)
	}
	if _, err := h.hostAdmin().AddHost(h.ctx, &poolmgrv1.AddHostRequest{Host: hostRequest(hostB)}); err != nil {
		t.Fatalf("AddHost: %v", err)
	}
	if _, err := h.client.UpdatePool(h.ctx, h.spec("pool", 1, hostA, hostB)); err != nil {
		t.Fatalf("UpdatePool once host-b is held: %v", err)
	}
}

//= docs/requirements/10-battery.md#hosts
//= type=test
//# If `RemoveHost` names a Host that a Pool names in its
//# `flintlock_hosts`, or that has a MicroVM in any phase or a placement in
//# flight, then battery SHALL refuse the call with `FAILED_PRECONDITION`,
//# and SHALL keep the Host.

// TestRemoveHost: refused while a Pool names the Host, then while a
// MicroVM is still on it, and done once neither holds.
func TestRemoveHost(t *testing.T) {
	h := newHarness(t, Config{}, hostA, hostB)
	admin := h.hostAdmin()
	h.createPool(h.spec("pool", 2, hostA, hostB))

	remove := func() error {
		_, err := admin.RemoveHost(h.ctx, &poolmgrv1.RemoveHostRequest{Name: hostB})
		return err
	}
	if err := remove(); statusCode(t, err) != codes.FailedPrecondition {
		t.Fatalf("RemoveHost while the pool names it = %v, want FAILED_PRECONDITION", err)
	}

	// The Pool drops the Host, but its MicroVM stays there.
	if _, err := admin.CordonHost(h.ctx, &poolmgrv1.CordonHostRequest{Name: hostB}); err != nil {
		t.Fatalf("CordonHost: %v", err)
	}
	if _, err := h.client.UpdatePool(h.ctx, h.spec("pool", 2, hostA)); err != nil {
		t.Fatalf("UpdatePool: %v", err)
	}
	st, err := admin.GetHost(h.ctx, &poolmgrv1.GetHostRequest{Name: hostB})
	if err != nil {
		t.Fatalf("GetHost: %v", err)
	}
	if st.GetVmCount() != 1 {
		t.Fatalf("vm_count = %d, want the one microvm still on host-b", st.GetVmCount())
	}
	if err := remove(); statusCode(t, err) != codes.FailedPrecondition {
		t.Fatalf("RemoveHost with a microvm on it = %v, want FAILED_PRECONDITION", err)
	}
	if !slices.Contains(h.hosts.Names(), hostB) {
		t.Fatal("host-b went after a refused RemoveHost")
	}

	// Deleting the Pool deletes its MicroVMs on their Hosts.
	if err := h.client.DeletePool(h.ctx, h.ref("pool")); err != nil {
		t.Fatalf("DeletePool: %v", err)
	}
	if err := remove(); err != nil {
		t.Fatalf("RemoveHost once nothing is on it: %v", err)
	}
	if names := h.hosts.Names(); len(names) != 1 || names[0] != hostA {
		t.Fatalf("hosts = %v, want host-a alone", names)
	}
}

//= docs/requirements/10-battery.md#hosts
//= type=test
//# battery SHALL place no new MicroVM on a cordoned Host, and
//# SHALL leave the MicroVMs already on it as they are.

// TestCordonedHostGetsNoNewMicroVM: with host-b cordoned, a Pool on both
// Hosts grows on host-a alone, and the MicroVM already on host-b stays;
// uncordoned, host-b takes MicroVMs again.
func TestCordonedHostGetsNoNewMicroVM(t *testing.T) {
	h := newHarness(t, Config{}, hostA, hostB)
	admin := h.hostAdmin()
	h.createPool(h.spec("pool", 2, hostA, hostB))
	before := h.b.VMs()
	var onB string
	for _, r := range before {
		if r.Host == hostB {
			onB = r.UID
		}
	}
	if onB == "" {
		t.Fatalf("no microvm on host-b before the cordon: %v", before)
	}

	if _, err := admin.CordonHost(h.ctx, &poolmgrv1.CordonHostRequest{Name: hostB, Reason: "test"}); err != nil {
		t.Fatalf("CordonHost: %v", err)
	}
	if _, err := h.client.UpdatePool(h.ctx, h.spec("pool", 4, hostA, hostB)); err != nil {
		t.Fatalf("UpdatePool: %v", err)
	}
	h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
	h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
	if got := h.vmHosts(); got[hostA] != 3 || got[hostB] != 1 {
		t.Fatalf("placement with host-b cordoned = %v, want 3 on host-a and the 1 already on host-b", got)
	}
	if !h.stubs[hostB].has(onB) {
		t.Fatalf("microvm %s left host-b when it was cordoned", onB)
	}

	if _, err := admin.UncordonHost(h.ctx, &poolmgrv1.UncordonHostRequest{Name: hostB}); err != nil {
		t.Fatalf("UncordonHost: %v", err)
	}
	if _, err := h.client.UpdatePool(h.ctx, h.spec("pool", 6, hostA, hostB)); err != nil {
		t.Fatalf("UpdatePool: %v", err)
	}
	h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
	h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
	if got := h.vmHosts(); got[hostA] != 3 || got[hostB] != 3 {
		t.Fatalf("placement once host-b is uncordoned = %v, want 3 on each", got)
	}
}
