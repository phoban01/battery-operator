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

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

//= docs/requirements/08-test-doubles.md#fake-battery
//# The fake battery SHALL serve battery v0.3.3's `PoolAdmin`,
//# `Lease` and `Events` services over gRPC with the generated server stubs.

// newGRPCServer builds a server with the three services registered from
// battery's generated stubs and the UNAVAILABLE fault interceptors. Serve
// and the loopback each build one over the same Battery.
func (b *Battery) newGRPCServer() *grpc.Server {
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(b.unaryFaultInterceptor),
		grpc.ChainStreamInterceptor(b.streamFaultInterceptor),
	)
	poolmgrv1.RegisterPoolAdminServer(srv, &poolAdminServer{b: b})
	poolmgrv1.RegisterLeaseServer(srv, &leaseServer{b: b})
	poolmgrv1.RegisterEventsServer(srv, &eventsServer{b: b})
	return srv
}

func (b *Battery) unaryFaultInterceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if err := b.unavailable(); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

func (b *Battery) streamFaultInterceptor(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if err := b.unavailable(); err != nil {
		return err
	}
	return handler(srv, ss)
}

// poolAdminServer implements poolmgrv1.PoolAdminServer.
type poolAdminServer struct {
	poolmgrv1.UnimplementedPoolAdminServer
	b *Battery
}

// CreatePool implements poolmgrv1.PoolAdminServer. It is ALREADY_EXISTS for
// a Pool that is already declared.
func (s *poolAdminServer) CreatePool(_ context.Context, req *poolmgrv1.CreatePoolRequest) (*poolmgrv1.Pool, error) {
	spec, err := specFromProto(req.GetSpec())
	if err != nil {
		return nil, err
	}
	p, err := s.b.createPool(spec)
	if err != nil {
		return nil, err
	}
	return poolToProto(p), nil
}

// UpdatePool implements poolmgrv1.PoolAdminServer. Existing MicroVMs are
// kept; the new spec applies to every later decision.
func (s *poolAdminServer) UpdatePool(_ context.Context, req *poolmgrv1.UpdatePoolRequest) (*poolmgrv1.Pool, error) {
	spec, err := specFromProto(req.GetSpec())
	if err != nil {
		return nil, err
	}
	p, err := s.b.updatePool(spec)
	if err != nil {
		return nil, err
	}
	return poolToProto(p), nil
}

// DeletePool implements poolmgrv1.PoolAdminServer. It refuses while the
// Pool owns any MicroVM, as battery does.
func (s *poolAdminServer) DeletePool(_ context.Context, req *poolmgrv1.DeletePoolRequest) (*emptypb.Empty, error) {
	if err := s.b.deletePool(refFromProto(req.GetRef())); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

// GetPool implements poolmgrv1.PoolAdminServer.
func (s *poolAdminServer) GetPool(_ context.Context, req *poolmgrv1.GetPoolRequest) (*poolmgrv1.Pool, error) {
	p, err := s.b.getPool(refFromProto(req.GetRef()))
	if err != nil {
		return nil, err
	}
	return poolToProto(p), nil
}

// ListPools implements poolmgrv1.PoolAdminServer. An empty namespace lists
// every Pool.
func (s *poolAdminServer) ListPools(_ context.Context, req *poolmgrv1.ListPoolsRequest) (*poolmgrv1.ListPoolsResponse, error) {
	resp := &poolmgrv1.ListPoolsResponse{}
	for _, p := range s.b.listPools(req.GetNamespace()) {
		resp.Pools = append(resp.Pools, poolToProto(p))
	}
	return resp, nil
}

// leaseServer implements poolmgrv1.LeaseServer.
type leaseServer struct {
	poolmgrv1.UnimplementedLeaseServer
	b *Battery
}

// ClaimVM implements poolmgrv1.LeaseServer. It fills the host field with
// the placed Host's name and address.
func (s *leaseServer) ClaimVM(ctx context.Context, req *poolmgrv1.ClaimVMRequest) (*poolmgrv1.ClaimVMResponse, error) {
	res, err := s.b.claimVM(ctx, refFromProto(req.GetPool()))
	if err != nil {
		return nil, err
	}
	return &poolmgrv1.ClaimVMResponse{
		LeaseId:           res.leaseID,
		VmUid:             res.vmUID,
		NetworkInterfaces: res.ifaces,
		Host:              &poolmgrv1.HostInfo{Name: res.host, Address: res.address},
	}, nil
}

// Heartbeat implements poolmgrv1.LeaseServer. It returns the Lease's new
// expiry, and NOT_FOUND once the Lease has expired.
func (s *leaseServer) Heartbeat(_ context.Context, req *poolmgrv1.HeartbeatRequest) (*poolmgrv1.HeartbeatResponse, error) {
	expires, err := s.b.heartbeat(req.GetLeaseId())
	if err != nil {
		return nil, err
	}
	return &poolmgrv1.HeartbeatResponse{ExpiresAt: timestamppb.New(expires)}, nil
}

// ReleaseVM implements poolmgrv1.LeaseServer. The Lease stays until the Host
// confirms the deletion; until then a retry gets UNAVAILABLE.
func (s *leaseServer) ReleaseVM(ctx context.Context, req *poolmgrv1.ReleaseVMRequest) (*emptypb.Empty, error) {
	if err := s.b.releaseVM(ctx, req.GetLeaseId()); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

// ListLeases implements poolmgrv1.LeaseServer. It renews nothing, lists a
// Lease past its expiry until the control loop sweeps it, and lists nothing,
// without an error, for a Pool it does not know.
func (s *leaseServer) ListLeases(_ context.Context, req *poolmgrv1.ListLeasesRequest) (*poolmgrv1.ListLeasesResponse, error) {
	//= docs/requirements/10-battery.md#list-leases
	//# When battery receives a `ListLeases`, battery SHALL answer with
	//# every Lease it holds, or every Lease of the one Pool the request names,
	//# each with its lease id, MicroVM uid, Pool, claim time, last heartbeat time
	//# and expiry, without renewing any of them.

	//= docs/requirements/10-battery.md#list-leases
	//# battery SHALL include in the answer to `ListLeases` a Lease
	//# whose expiry has passed but that no sweep has deleted yet.
	var filter *PoolRef
	if req.PoolRef != nil {
		ref := refFromProto(req.GetPoolRef())
		filter = &ref
	}
	resp := &poolmgrv1.ListLeasesResponse{}
	for _, l := range s.b.listLeases(filter) {
		resp.Leases = append(resp.Leases, leaseToProto(l))
	}
	return resp, nil
}

// eventsServer implements poolmgrv1.EventsServer.
type eventsServer struct {
	poolmgrv1.UnimplementedEventsServer
	b *Battery
}

// Subscribe replays the recent events of the requested Pool, or of every
// Pool, then streams live events until the client goes away or the stream
// is dropped by fault injection, which ends it with UNAVAILABLE.
func (s *eventsServer) Subscribe(req *poolmgrv1.SubscribeRequest, stream grpc.ServerStreamingServer[poolmgrv1.Event]) error {
	var filter *poolKey
	if ref := req.GetPool(); ref != nil {
		k := keyOf(refFromProto(ref))
		filter = &k
	}
	sub, backlog := s.b.events.subscribe(filter)
	defer s.b.events.unsubscribe(sub.id)
	for _, e := range backlog {
		if err := stream.Send(proto.CloneOf(e)); err != nil {
			return err
		}
	}
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case <-sub.dropped:
			return status.Error(codes.Unavailable, "events stream dropped")
		case e := <-sub.ch:
			if err := stream.Send(proto.CloneOf(e)); err != nil {
				return err
			}
		}
	}
}

// Proto conversions.

func refFromProto(r *poolmgrv1.PoolRef) PoolRef {
	return PoolRef{Name: r.GetName(), Namespace: r.GetNamespace()}
}

func refToProto(r PoolRef) *poolmgrv1.PoolRef {
	return &poolmgrv1.PoolRef{Name: r.Name, Namespace: r.Namespace}
}

// specFromProto converts a wire PoolSpec. A nil spec is INVALID_ARGUMENT.
func specFromProto(s *poolmgrv1.PoolSpec) (poolSpec, error) {
	if s == nil {
		return poolSpec{}, errInvalid("spec is required")
	}
	spec := poolSpec{
		Ref:               PoolRef{Name: s.GetName(), Namespace: s.GetNamespace()},
		Size:              s.GetSize(),
		FlintlockHosts:    s.GetFlintlockHosts(),
		CreateCommands:    s.GetCreateCommands(),
		PreLeaseCommands:  s.GetPreLeaseCommands(),
		HookFailurePolicy: s.GetHookFailurePolicy(),
	}
	if t := s.GetMicrovmTemplate(); t != nil {
		spec.Template = proto.CloneOf(t)
	}
	if rs := s.GetReplenishmentStrategy(); rs != nil {
		spec.Replenishment.Type = rs.GetType()
		if rs.MinSize != nil {
			v := rs.GetMinSize()
			spec.Replenishment.MinSize = &v
		}
	}
	if d := s.GetHeartbeatInterval(); d != nil {
		spec.HeartbeatInterval = d.AsDuration()
	}
	if d := s.GetHeartbeatExpiryThreshold(); d != nil {
		spec.HeartbeatExpiryThreshold = d.AsDuration()
	}
	return spec, nil
}

func specToProto(spec poolSpec) *poolmgrv1.PoolSpec {
	out := &poolmgrv1.PoolSpec{
		Name:                  spec.Ref.Name,
		Namespace:             spec.Ref.Namespace,
		Size:                  spec.Size,
		FlintlockHosts:        spec.FlintlockHosts,
		ReplenishmentStrategy: &poolmgrv1.ReplenishmentStrategy{Type: spec.Replenishment.Type},
		CreateCommands:        spec.CreateCommands,
		PreLeaseCommands:      spec.PreLeaseCommands,
		HookFailurePolicy:     spec.HookFailurePolicy,
	}
	if spec.Template != nil {
		out.MicrovmTemplate = proto.CloneOf(spec.Template)
	}
	if spec.Replenishment.MinSize != nil {
		v := *spec.Replenishment.MinSize
		out.ReplenishmentStrategy.MinSize = &v
	}
	if spec.HeartbeatInterval > 0 {
		out.HeartbeatInterval = durationpb.New(spec.HeartbeatInterval)
	}
	if spec.HeartbeatExpiryThreshold > 0 {
		out.HeartbeatExpiryThreshold = durationpb.New(spec.HeartbeatExpiryThreshold)
	}
	return out
}

func leaseToProto(l LeaseRecord) *poolmgrv1.LeaseRecord {
	return &poolmgrv1.LeaseRecord{
		LeaseId:         l.LeaseID,
		VmUid:           l.VMUID,
		PoolName:        l.Pool.Name,
		PoolNamespace:   l.Pool.Namespace,
		ClaimedAt:       timestamppb.New(l.ClaimedAt),
		LastHeartbeatAt: timestamppb.New(l.LastHeartbeatAt),
		ExpiresAt:       timestamppb.New(l.ExpiresAt),
	}
}

func poolToProto(p *pool) *poolmgrv1.Pool {
	return &poolmgrv1.Pool{
		Spec: specToProto(p.Spec),
		Status: &poolmgrv1.PoolStatus{
			AvailableCount:    p.Status.Available,
			LeasedCount:       p.Status.Leased,
			ProvisioningCount: p.Status.Provisioning,
			QuarantinedCount:  p.Status.Quarantined,
		},
	}
}
