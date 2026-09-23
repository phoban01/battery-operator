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
	"encoding/json"
	"testing"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"google.golang.org/grpc"
)

// testClient is a small typed client over battery's generated clients on a
// connection to the fake, so that the tests read as operations on Pools
// and Leases rather than as proto plumbing. Errors are the gRPC status
// errors the fake returned.
type testClient struct {
	conn   *grpc.ClientConn
	admin  poolmgrv1.PoolAdminClient
	lease  poolmgrv1.LeaseClient
	events poolmgrv1.EventsClient
}

// newTestClient connects to b in process; the connection is closed when
// the test ends.
func newTestClient(t *testing.T, b *Battery) *testClient {
	t.Helper()
	conn, err := b.Conn()
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return clientOver(conn)
}

func clientOver(conn *grpc.ClientConn) *testClient {
	return &testClient{
		conn:   conn,
		admin:  poolmgrv1.NewPoolAdminClient(conn),
		lease:  poolmgrv1.NewLeaseClient(conn),
		events: poolmgrv1.NewEventsClient(conn),
	}
}

func (c *testClient) CreatePool(ctx context.Context, spec poolSpec) (*pool, error) {
	resp, err := c.admin.CreatePool(ctx, &poolmgrv1.CreatePoolRequest{Spec: specToProto(spec)})
	if err != nil {
		return nil, err
	}
	return poolFromProto(resp)
}

func (c *testClient) UpdatePool(ctx context.Context, spec poolSpec) (*pool, error) {
	resp, err := c.admin.UpdatePool(ctx, &poolmgrv1.UpdatePoolRequest{Spec: specToProto(spec)})
	if err != nil {
		return nil, err
	}
	return poolFromProto(resp)
}

func (c *testClient) DeletePool(ctx context.Context, ref PoolRef) error {
	_, err := c.admin.DeletePool(ctx, &poolmgrv1.DeletePoolRequest{Ref: refToProto(ref)})
	return err
}

func (c *testClient) GetPool(ctx context.Context, ref PoolRef) (*pool, error) {
	resp, err := c.admin.GetPool(ctx, &poolmgrv1.GetPoolRequest{Ref: refToProto(ref)})
	if err != nil {
		return nil, err
	}
	return poolFromProto(resp)
}

func (c *testClient) ListPools(ctx context.Context, namespace string) ([]*poolmgrv1.Pool, error) {
	req := &poolmgrv1.ListPoolsRequest{}
	if namespace != "" {
		req.Namespace = &namespace
	}
	resp, err := c.admin.ListPools(ctx, req)
	if err != nil {
		return nil, err
	}
	return resp.GetPools(), nil
}

// claimed is a successful ClaimVMResponse.
type claimed struct {
	LeaseID string
	VMUID   string
	Host    string
	Address string
}

func (c *testClient) ClaimVM(ctx context.Context, ref PoolRef) (*claimed, error) {
	resp, err := c.lease.ClaimVM(ctx, &poolmgrv1.ClaimVMRequest{Pool: refToProto(ref)})
	if err != nil {
		return nil, err
	}
	return &claimed{
		LeaseID: resp.GetLeaseId(),
		VMUID:   resp.GetVmUid(),
		Host:    resp.GetHost().GetName(),
		Address: resp.GetHost().GetAddress(),
	}, nil
}

func (c *testClient) Heartbeat(ctx context.Context, leaseID string) (time.Time, error) {
	resp, err := c.lease.Heartbeat(ctx, &poolmgrv1.HeartbeatRequest{LeaseId: leaseID})
	if err != nil {
		return time.Time{}, err
	}
	return resp.GetExpiresAt().AsTime(), nil
}

func (c *testClient) ReleaseVM(ctx context.Context, leaseID string) error {
	_, err := c.lease.ReleaseVM(ctx, &poolmgrv1.ReleaseVMRequest{LeaseId: leaseID})
	return err
}

// event is one proto Event.
type event struct {
	ID      int64
	Pool    PoolRef
	VMUID   string
	Type    poolmgrv1.EventType
	At      time.Time
	Payload json.RawMessage
}

// eventStream is one Subscribe stream. Once it has failed it keeps
// returning the same error.
type eventStream struct {
	stream grpc.ServerStreamingClient[poolmgrv1.Event]
	cancel context.CancelFunc
	err    error
}

// Subscribe subscribes to one Pool's events, or to every Pool's when
// filter is nil.
func (c *testClient) Subscribe(ctx context.Context, filter *PoolRef) (*eventStream, error) {
	req := &poolmgrv1.SubscribeRequest{}
	if filter != nil {
		req.Pool = refToProto(*filter)
	}
	ctx, cancel := context.WithCancel(ctx)
	stream, err := c.events.Subscribe(ctx, req)
	if err != nil {
		cancel()
		return nil, err
	}
	return &eventStream{stream: stream, cancel: cancel}, nil
}

func (s *eventStream) Recv() (*event, error) {
	if s.err != nil {
		return nil, s.err
	}
	e, err := s.stream.Recv()
	if err != nil {
		s.err = err
		return nil, err
	}
	return &event{
		ID:      e.GetId(),
		Pool:    PoolRef{Name: e.GetPoolName(), Namespace: e.GetPoolNamespace()},
		VMUID:   e.GetVmUid(),
		Type:    e.GetType(),
		At:      e.GetCreatedAt().AsTime(),
		Payload: json.RawMessage(e.GetPayloadJson()),
	}, nil
}

func (s *eventStream) Close() { s.cancel() }

func poolFromProto(p *poolmgrv1.Pool) (*pool, error) {
	spec, err := specFromProto(p.GetSpec())
	if err != nil {
		return nil, err
	}
	st := p.GetStatus()
	return &pool{Spec: spec, Status: poolStatus{
		Available:    st.GetAvailableCount(),
		Leased:       st.GetLeasedCount(),
		Provisioning: st.GetProvisioningCount(),
		Quarantined:  st.GetQuarantinedCount(),
	}}, nil
}
