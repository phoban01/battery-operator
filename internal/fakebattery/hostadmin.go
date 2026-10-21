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
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

//= docs/requirements/08-test-doubles.md#fake-battery
//# The fake battery SHALL serve battery's `HostAdmin` service,
//# and SHALL hold its Hosts as battery does, so that a Pool names only Hosts
//# the fake holds, a Host is removed only once no Pool names it and nothing
//# is on it, and no MicroVM is placed on a cordoned Host.

//= docs/requirements/10-battery.md#hosts
//= type=exception
//= reason=The fake keeps its Hosts in memory and has no database, so there is nothing written by battery v0.4.0 for it to start on.
//# When battery starts on a database that battery v0.4.0 wrote,
//# battery SHALL keep the Hosts in it, with no TLS settings, and SHALL read no
//# Host from its configuration file.

//= docs/requirements/10-battery.md#hosts
//= type=exception
//= reason=The fake has no configuration file: a test gives it a Config.
//# If battery's configuration file has a `hosts` key, then battery
//# SHALL refuse to start.

// hostAdminServer implements poolmgrv1.HostAdminServer over the fake's
// Hosts, with battery's semantics (internal/api/hostadmin.go in battery
// v0.5.0).
type hostAdminServer struct {
	poolmgrv1.UnimplementedHostAdminServer
	b *Battery
}

func timestampOf(t time.Time) *timestamppb.Timestamp { return timestamppb.New(t) }

// errFailedPrecondition is FAILED_PRECONDITION with a message.
func errFailedPrecondition(format string, args ...any) error {
	return status.Errorf(codes.FailedPrecondition, format, args...)
}

// validateHostSpec applies the rules battery's flintlockclient.Dial
// applies to a Host's spec, which AddHost and UpdateHost check whether or
// not they skip validation.
func validateHostSpec(h *poolmgrv1.Host) error {
	if h.GetName() == "" {
		return errInvalid("host.name is required")
	}
	if h.GetAddress() == "" {
		return errInvalid("host %q: address is required", h.GetName())
	}
	t := h.GetTls()
	if t.GetInsecure() {
		if t.GetCertFile() != "" || t.GetKeyFile() != "" || t.GetCaFile() != "" {
			return errInvalid("host %q: tls: insecure hosts must not set cert_file/key_file/ca_file", h.GetName())
		}
		return nil
	}
	if t.GetCaFile() == "" {
		return errInvalid("host %q: tls: ca_file is required unless insecure is set", h.GetName())
	}
	if (t.GetCertFile() == "") != (t.GetKeyFile() == "") {
		return errInvalid("host %q: tls: cert_file and key_file must be set together", h.GetName())
	}
	return nil
}

// dialHost stands in for battery connecting to a Host and checking its
// flintlockd version: the Host is reachable when Config.Dial gives a
// connection for its address. Without skipValidation an unreachable Host is
// FAILED_PRECONDITION; with it, the Host is held with no connection.
func (b *Battery) dialHost(name, address string, skipValidation bool) (grpc.ClientConnInterface, error) {
	if b.cfg.Dial == nil {
		if skipValidation {
			return nil, nil
		}
		return nil, errFailedPrecondition("validate host %q: the fake battery has no Dial", name)
	}
	conn, err := b.cfg.Dial(address)
	if err == nil && conn == nil {
		err = fmt.Errorf("no connection for %s", address)
	}
	if err != nil {
		if skipValidation {
			return nil, nil
		}
		return nil, errFailedPrecondition("validate host %q: %v", name, err)
	}
	return conn, nil
}

// vmCountLocked is battery's CountVMsByHost: every MicroVM on the Host, in
// any phase, across every Pool. A placement in flight is a PROVISIONING
// MicroVM here, so it counts too.
func (b *Battery) vmCountLocked(name string) int32 {
	var n int32
	for _, vm := range b.vms {
		if vm.host == name {
			n++
		}
	}
	return n
}

// hostStatusLocked is the Host called name with its count, or false.
func (b *Battery) hostStatusLocked(name string) (*poolmgrv1.HostStatus, bool) {
	rec, ok := b.cfg.Hosts.record(name)
	if !ok {
		return nil, false
	}
	return &poolmgrv1.HostStatus{Host: rec, VmCount: b.vmCountLocked(name)}, true
}

// checkFlintlockHostsLocked refuses a Pool spec that names a Host the fake
// does not hold, as battery's checkFlintlockHosts does.
func (b *Battery) checkFlintlockHostsLocked(spec poolSpec) error {
	//= docs/requirements/10-battery.md#hosts
	//# If `CreatePool` or `UpdatePool` names in `flintlock_hosts` a
	//# Host battery does not hold, then battery SHALL refuse the call with
	//# `INVALID_ARGUMENT`.
	var unknown []string
	for _, name := range spec.FlintlockHosts {
		if !b.cfg.Hosts.held(name) && !slices.Contains(unknown, name) {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		return errInvalid("spec.flintlock_hosts: unknown hosts %s; add them with HostAdmin.AddHost first",
			strings.Join(unknown, ", "))
	}
	return nil
}

// addHost implements HostAdmin.AddHost.
func (b *Battery) addHost(req *poolmgrv1.AddHostRequest) (*poolmgrv1.Host, error) {
	spec := req.GetHost()
	if err := validateHostSpec(spec); err != nil {
		return nil, err
	}
	name := spec.GetName()

	//= docs/requirements/10-battery.md#hosts
	//# If `AddHost` names a Host battery already holds, then battery
	//# SHALL answer `ALREADY_EXISTS`, and SHALL leave the Host as it is.
	if b.cfg.Hosts.held(name) {
		return nil, status.Errorf(codes.AlreadyExists, "host %q already exists", name)
	}

	//= docs/requirements/10-battery.md#hosts
	//# When `AddHost` names a Host battery does not hold, battery
	//# SHALL store the Host uncordoned, with the address and TLS settings the
	//# request gives, and, unless the request sets `skip_validation`, SHALL
	//# first connect to the Host and check its `flintlockd` version, answering
	//# `FAILED_PRECONDITION` and storing nothing if either fails.
	conn, err := b.dialHost(name, spec.GetAddress(), req.GetSkipValidation())
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	hs := b.cfg.Hosts
	hs.mu.Lock()
	defer hs.mu.Unlock()
	if _, ok := hs.entries[name]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "host %q already exists", name)
	}
	e := &host{
		name:      name,
		address:   spec.GetAddress(),
		tls:       proto.CloneOf(spec.GetTls()),
		updatedAt: b.cfg.Clock.Now(),
	}
	e.setConn(conn)
	if conn != nil {
		e.dials++
	}
	hs.entries[name] = e
	b.kickReconcile()
	return e.proto(), nil
}

// updateHost implements HostAdmin.UpdateHost.
func (b *Battery) updateHost(req *poolmgrv1.UpdateHostRequest) (*poolmgrv1.Host, error) {
	spec := req.GetHost()
	if spec.GetName() == "" {
		return nil, errInvalid("host.name is required")
	}
	name := spec.GetName()

	//= docs/requirements/10-battery.md#hosts
	//# If `UpdateHost`, `RemoveHost` or `GetHost` names a Host
	//# battery does not hold, then battery SHALL answer `NOT_FOUND`.
	if !b.cfg.Hosts.held(name) {
		return nil, errNotFound("host %q not found", name)
	}
	if err := validateHostSpec(spec); err != nil {
		return nil, err
	}
	conn, err := b.dialHost(name, spec.GetAddress(), req.GetSkipValidation())
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	hs := b.cfg.Hosts
	hs.mu.Lock()
	defer hs.mu.Unlock()
	e, ok := hs.entries[name]
	if !ok {
		return nil, errNotFound("host %q not found", name)
	}

	//= docs/requirements/10-battery.md#hosts
	//# battery SHALL keep a Host's cordon state across `UpdateHost`,
	//# and SHALL answer `CordonHost` on a cordoned Host, and `UncordonHost` on an
	//# uncordoned one, with the Host and no error.
	e.address = spec.GetAddress()
	e.tls = proto.CloneOf(spec.GetTls())
	e.updatedAt = b.cfg.Clock.Now()
	e.setConn(conn)
	if conn != nil {
		e.dials++
	}
	b.kickReconcile()
	return e.proto(), nil
}

// removeHost implements HostAdmin.RemoveHost: under one lock, as battery's
// DeleteHost checks and deletes in one transaction.
func (b *Battery) removeHost(name string) error {
	if name == "" {
		return errInvalid("name is required")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.cfg.Hosts.held(name) {
		return errNotFound("host %q not found", name)
	}

	//= docs/requirements/10-battery.md#hosts
	//# If `RemoveHost` names a Host that a Pool names in its
	//# `flintlock_hosts`, or that has a MicroVM in any phase or a placement in
	//# flight, then battery SHALL refuse the call with `FAILED_PRECONDITION`,
	//# and SHALL keep the Host.
	var pools []string
	for key, ps := range b.pools {
		if slices.Contains(ps.spec.FlintlockHosts, name) {
			pools = append(pools, key.String())
		}
	}
	slices.Sort(pools)
	count := b.vmCountLocked(name)
	if len(pools) > 0 || count > 0 {
		var reasons []string
		if len(pools) > 0 {
			reasons = append(reasons, "named in flintlock_hosts by pools "+strings.Join(pools, ", "))
		}
		if count > 0 {
			reasons = append(reasons, fmt.Sprintf("vm_count is %d", count))
		}
		return errFailedPrecondition("host %q is still in use (%s): cordon it, drop it from each pool spec, and wait for vm_count to reach 0",
			name, strings.Join(reasons, "; "))
	}
	b.cfg.Hosts.Remove(name)
	return nil
}

// setCordoned implements HostAdmin.CordonHost and UncordonHost.
func (b *Battery) setCordoned(name string, cordoned bool, reason string) (*poolmgrv1.Host, error) {
	if name == "" {
		return nil, errInvalid("name is required")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	hs := b.cfg.Hosts
	hs.mu.Lock()
	defer hs.mu.Unlock()
	e, ok := hs.entries[name]
	if !ok {
		return nil, errFailedPrecondition("host %q is not registered", name)
	}
	now := b.cfg.Clock.Now()
	e.cordoned = cordoned
	e.updatedAt = now
	if cordoned {
		e.cordonedReason = reason
		e.cordonedAt = now
	} else {
		e.cordonedReason = ""
		e.cordonedAt = time.Time{}
		b.kickReconcile()
	}
	return e.proto(), nil
}

// CordonHost implements poolmgrv1.HostAdminServer.
func (s *hostAdminServer) CordonHost(_ context.Context, req *poolmgrv1.CordonHostRequest) (*poolmgrv1.Host, error) {
	return s.b.setCordoned(req.GetName(), true, req.GetReason())
}

// UncordonHost implements poolmgrv1.HostAdminServer.
func (s *hostAdminServer) UncordonHost(_ context.Context, req *poolmgrv1.UncordonHostRequest) (*poolmgrv1.Host, error) {
	return s.b.setCordoned(req.GetName(), false, "")
}

// ListHosts implements poolmgrv1.HostAdminServer: every Host held, sorted
// by name.
func (s *hostAdminServer) ListHosts(_ context.Context, _ *poolmgrv1.ListHostsRequest) (*poolmgrv1.ListHostsResponse, error) {
	b := s.b
	b.mu.Lock()
	defer b.mu.Unlock()
	resp := &poolmgrv1.ListHostsResponse{}
	for _, name := range b.cfg.Hosts.Names() {
		if hs, ok := b.hostStatusLocked(name); ok {
			resp.Hosts = append(resp.Hosts, hs)
		}
	}
	return resp, nil
}

// GetHost implements poolmgrv1.HostAdminServer. flintlock_version stays
// empty: the fake checks no version.
func (s *hostAdminServer) GetHost(_ context.Context, req *poolmgrv1.GetHostRequest) (*poolmgrv1.HostStatus, error) {
	if req.GetName() == "" {
		return nil, errInvalid("name is required")
	}
	b := s.b
	b.mu.Lock()
	defer b.mu.Unlock()
	hs, ok := b.hostStatusLocked(req.GetName())
	if !ok {
		return nil, errNotFound("host %q not found", req.GetName())
	}
	return hs, nil
}

// AddHost implements poolmgrv1.HostAdminServer.
func (s *hostAdminServer) AddHost(_ context.Context, req *poolmgrv1.AddHostRequest) (*poolmgrv1.Host, error) {
	return s.b.addHost(req)
}

// UpdateHost implements poolmgrv1.HostAdminServer.
func (s *hostAdminServer) UpdateHost(_ context.Context, req *poolmgrv1.UpdateHostRequest) (*poolmgrv1.Host, error) {
	return s.b.updateHost(req)
}

// RemoveHost implements poolmgrv1.HostAdminServer.
func (s *hostAdminServer) RemoveHost(_ context.Context, req *poolmgrv1.RemoveHostRequest) (*emptypb.Empty, error) {
	if err := s.b.removeHost(req.GetName()); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}
