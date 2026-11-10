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

package battery

import (
	"context"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
)

// HostTLS is the proto HostTLS: how battery connects to a Host's
// flintlockd. The files are paths in battery's container, not the
// Operator's.
type HostTLS struct {
	// Insecure connects without TLS; the Operator never sets it (IN-004).
	Insecure bool
	// CAFile verifies the Host's flintlockd serving certificate.
	CAFile string
	// CertFile and KeyFile are the client certificate battery presents.
	CertFile string
	KeyFile  string
}

// Host is the proto Host: one of battery's Hosts.
type Host struct {
	// Name is the Host's name, which Pools list in flintlock_hosts.
	Name string
	// Address is the Host's flintlockd gRPC endpoint, host:port.
	Address string
	TLS     HostTLS
	// Cordoned is true while battery places no new MicroVM on the Host.
	// AddHost and UpdateHost ignore it; CordonHost and UncordonHost set it.
	Cordoned bool
	// CordonedReason is the note the last CordonHost or DrainHost gave.
	CordonedReason string
	// Draining is true from DrainHost until UncordonHost. A draining Host
	// is cordoned, and battery deletes every MicroVM on it that is not
	// leased, on every sweep (BA-090, BA-091). AddHost and UpdateHost
	// ignore it.
	Draining bool
	// DrainingAt is when the drain began; zero unless Draining.
	DrainingAt time.Time
}

// HostStatus is the proto HostStatus: a Host with what is still counted
// against it.
type HostStatus struct {
	Host Host
	// VMCount is the number of MicroVMs on the Host in any phase, and of
	// placements in flight, across every Pool. battery removes a Host only
	// at zero.
	VMCount int32
	// FlintlockVersion is the version flintlockd last reported; empty
	// until battery has checked the Host.
	FlintlockVersion string
}

// HostAdmin is what the Inventory Controller calls battery's HostAdmin
// service through: one method per RPC. Connection implements it.
type HostAdmin interface {
	// ListHosts lists every Host battery holds.
	ListHosts(ctx context.Context) ([]*HostStatus, error)
	// GetHost returns ErrNotFound for a Host battery does not hold.
	GetHost(ctx context.Context, name string) (*HostStatus, error)
	// AddHost adds a Host, uncordoned. Unless skipValidation is set,
	// battery first connects to it and checks its flintlockd version, and
	// answers ErrFailedPrecondition if either fails. It returns
	// ErrAlreadyExists for a Host battery holds.
	AddHost(ctx context.Context, host Host, skipValidation bool) (*Host, error)
	// UpdateHost replaces a Host's address and TLS settings and connects to
	// it again, which reads its certificate again; it keeps the Host's
	// cordon state. It returns ErrNotFound for a Host battery does not
	// hold, and validates as AddHost does.
	UpdateHost(ctx context.Context, host Host, skipValidation bool) (*Host, error)
	// RemoveHost returns ErrNotFound for a Host battery does not hold, and
	// ErrFailedPrecondition while a Pool names the Host or anything is
	// still on it.
	RemoveHost(ctx context.Context, name string) error
	// CordonHost stops battery placing new MicroVMs on the Host. Cordoning
	// a cordoned Host updates the reason. It returns
	// ErrFailedPrecondition for a Host battery does not hold.
	CordonHost(ctx context.Context, name, reason string) (*Host, error)
	// UncordonHost lets battery place on the Host again, and ends its
	// drain. It returns ErrFailedPrecondition for a Host battery does not
	// hold.
	UncordonHost(ctx context.Context, name string) (*Host, error)
	// DrainHost cordons the Host, marks it draining and deletes every
	// MicroVM on it that is not leased, now and on every later sweep
	// until UncordonHost; leased MicroVMs end by release or expiry
	// (BA-090 to BA-092). It is idempotent, a non-empty reason replaces
	// the Host's cordon reason, and it returns ErrNotFound for a Host
	// battery does not hold (BA-095, BA-096).
	DrainHost(ctx context.Context, name, reason string) (*Host, error)
}

var _ HostAdmin = (*Connection)(nil)

// ListHosts implements HostAdmin.
func (c *Connection) ListHosts(ctx context.Context) ([]*HostStatus, error) {
	resp, err := c.hosts.ListHosts(ctx, &poolmgrv1.ListHostsRequest{})
	if err != nil {
		return nil, mapErr(ctx, err)
	}
	out := make([]*HostStatus, 0, len(resp.GetHosts()))
	for _, h := range resp.GetHosts() {
		out = append(out, hostStatusFromProto(h))
	}
	return out, nil
}

// GetHost implements HostAdmin.
func (c *Connection) GetHost(ctx context.Context, name string) (*HostStatus, error) {
	resp, err := c.hosts.GetHost(ctx, &poolmgrv1.GetHostRequest{Name: name})
	if err != nil {
		return nil, mapErr(ctx, err)
	}
	return hostStatusFromProto(resp), nil
}

// AddHost implements HostAdmin.
func (c *Connection) AddHost(ctx context.Context, host Host, skipValidation bool) (*Host, error) {
	resp, err := c.hosts.AddHost(ctx, &poolmgrv1.AddHostRequest{Host: hostToProto(host), SkipValidation: skipValidation})
	if err != nil {
		return nil, mapErr(ctx, err)
	}
	h := hostFromProto(resp)
	return &h, nil
}

// UpdateHost implements HostAdmin.
func (c *Connection) UpdateHost(ctx context.Context, host Host, skipValidation bool) (*Host, error) {
	resp, err := c.hosts.UpdateHost(ctx, &poolmgrv1.UpdateHostRequest{Host: hostToProto(host), SkipValidation: skipValidation})
	if err != nil {
		return nil, mapErr(ctx, err)
	}
	h := hostFromProto(resp)
	return &h, nil
}

// RemoveHost implements HostAdmin.
func (c *Connection) RemoveHost(ctx context.Context, name string) error {
	_, err := c.hosts.RemoveHost(ctx, &poolmgrv1.RemoveHostRequest{Name: name})
	return mapErr(ctx, err)
}

// CordonHost implements HostAdmin.
func (c *Connection) CordonHost(ctx context.Context, name, reason string) (*Host, error) {
	resp, err := c.hosts.CordonHost(ctx, &poolmgrv1.CordonHostRequest{Name: name, Reason: reason})
	if err != nil {
		return nil, mapErr(ctx, err)
	}
	h := hostFromProto(resp)
	return &h, nil
}

// UncordonHost implements HostAdmin.
func (c *Connection) UncordonHost(ctx context.Context, name string) (*Host, error) {
	resp, err := c.hosts.UncordonHost(ctx, &poolmgrv1.UncordonHostRequest{Name: name})
	if err != nil {
		return nil, mapErr(ctx, err)
	}
	h := hostFromProto(resp)
	return &h, nil
}

// DrainHost implements HostAdmin.
func (c *Connection) DrainHost(ctx context.Context, name, reason string) (*Host, error) {
	resp, err := c.hosts.DrainHost(ctx, &poolmgrv1.DrainHostRequest{Name: name, Reason: reason})
	if err != nil {
		return nil, mapErr(ctx, err)
	}
	h := hostFromProto(resp)
	return &h, nil
}

// hostToProto renders the fields AddHost and UpdateHost read: the cordon
// and drain fields are battery's to set.
func hostToProto(h Host) *poolmgrv1.Host {
	return &poolmgrv1.Host{
		Name:    h.Name,
		Address: h.Address,
		Tls: &poolmgrv1.HostTLS{
			Insecure: h.TLS.Insecure,
			CaFile:   h.TLS.CAFile,
			CertFile: h.TLS.CertFile,
			KeyFile:  h.TLS.KeyFile,
		},
	}
}

func hostFromProto(h *poolmgrv1.Host) Host {
	t := h.GetTls()
	return Host{
		Name:    h.GetName(),
		Address: h.GetAddress(),
		TLS: HostTLS{
			Insecure: t.GetInsecure(),
			CAFile:   t.GetCaFile(),
			CertFile: t.GetCertFile(),
			KeyFile:  t.GetKeyFile(),
		},
		Cordoned:       h.GetCordoned(),
		CordonedReason: h.GetCordonedReason(),
		Draining:       h.GetDraining(),
		DrainingAt:     h.GetDrainingAt().AsTime(),
	}
}

func hostStatusFromProto(s *poolmgrv1.HostStatus) *HostStatus {
	return &HostStatus{
		Host:             hostFromProto(s.GetHost()),
		VMCount:          s.GetVmCount(),
		FlintlockVersion: s.GetFlintlockVersion(),
	}
}
