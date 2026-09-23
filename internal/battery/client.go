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
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
)

// Connection is the Operator's connection to battery: one long-lived gRPC
// ClientConn carrying the three generated service clients. It implements
// Client, and Check is the Operator's readiness check.
type Connection struct {
	conn   *grpc.ClientConn
	admin  poolmgrv1.PoolAdminClient
	lease  poolmgrv1.LeaseClient
	events poolmgrv1.EventsClient
}

var _ Client = (*Connection)(nil)

// Dial builds the connection to battery. It refuses an address that is not
// loopback. The ClientConn has keepalive, exponential reconnect backoff and
// an interceptor that puts the call timeout on every unary call. It
// connects lazily: Dial does not fail because battery is down; the first
// call, or Check, does.
func Dial(cfg Config) (*Connection, error) {
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	creds, err := transportCredentials(cfg.TLS)
	if err != nil {
		return nil, err
	}
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                cfg.KeepaliveTime,
			Timeout:             cfg.KeepaliveTimeout,
			PermitWithoutStream: true,
		}),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay:  cfg.ReconnectBase,
				Multiplier: backoff.DefaultConfig.Multiplier,
				Jitter:     backoff.DefaultConfig.Jitter,
				MaxDelay:   cfg.ReconnectMax,
			},
			MinConnectTimeout: max(cfg.ReconnectMax, minConnectTimeout),
		}),
		grpc.WithChainUnaryInterceptor(deadlineInterceptor(cfg.CallTimeout)),
	}

	conn, err := grpc.NewClient(cfg.Address, opts...)
	if err != nil {
		return nil, fmt.Errorf("battery: dial %s: %w", cfg.Address, err)
	}
	return &Connection{
		conn:   conn,
		admin:  poolmgrv1.NewPoolAdminClient(conn),
		lease:  poolmgrv1.NewLeaseClient(conn),
		events: poolmgrv1.NewEventsClient(conn),
	}, nil
}

// deadlineInterceptor bounds every unary call with d. A caller that set an
// earlier deadline keeps it, because context.WithTimeout never extends one.
func deadlineInterceptor(d time.Duration) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		//= docs/requirements/06-deployment.md#battery-connection
		//# The Operator SHALL call battery over gRPC on the loopback
		//# address of DP-001, with a deadline on every unary call.
		ctx, cancel := context.WithTimeout(ctx, d)
		defer cancel()
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// Close closes the connection. Open EventStreams end with ErrUnavailable.
func (c *Connection) Close() error {
	if err := c.conn.Close(); err != nil {
		return fmt.Errorf("battery: closing the connection: %w", err)
	}
	return nil
}

// Ping asks battery for its Pools in a namespace no Pool can have and
// returns the mapped error. battery v0.1.0 filters ListPools by namespace
// before it counts any Pool's MicroVMs, so the answer costs it one read of
// its Pool specs.
func (c *Connection) Ping(ctx context.Context) error {
	_, err := c.ListPools(ctx, pingNamespace)
	return err
}

// pingNamespace is not a valid Kubernetes namespace name, so no Pool the
// Operator declares is in it.
const pingNamespace = "_readiness"

//= docs/requirements/06-deployment.md#battery-connection
//# While battery is unavailable, the Operator SHALL report itself
//# not ready on its readiness endpoint.

// Check is a controller-runtime healthz.Checker for the readiness endpoint:
// it fails while battery does not answer. The kubelet's probe timeout
// bounds it through the request's context, and the call timeout does
// otherwise.
func (c *Connection) Check(req *http.Request) error {
	if err := c.Ping(req.Context()); err != nil {
		return fmt.Errorf("battery does not answer: %w", err)
	}
	return nil
}

// CreatePool implements Client.
func (c *Connection) CreatePool(ctx context.Context, spec PoolSpec) (*Pool, error) {
	resp, err := c.admin.CreatePool(ctx, &poolmgrv1.CreatePoolRequest{Spec: specToProto(spec)})
	if err != nil {
		return nil, mapErr(ctx, err)
	}
	return poolFromProto(resp), nil
}

// UpdatePool implements Client.
func (c *Connection) UpdatePool(ctx context.Context, spec PoolSpec) (*Pool, error) {
	resp, err := c.admin.UpdatePool(ctx, &poolmgrv1.UpdatePoolRequest{Spec: specToProto(spec)})
	if err != nil {
		return nil, mapErr(ctx, err)
	}
	return poolFromProto(resp), nil
}

// DeletePool implements Client.
func (c *Connection) DeletePool(ctx context.Context, ref PoolRef) error {
	_, err := c.admin.DeletePool(ctx, &poolmgrv1.DeletePoolRequest{Ref: refToProto(ref)})
	return mapErr(ctx, err)
}

// GetPool implements Client.
func (c *Connection) GetPool(ctx context.Context, ref PoolRef) (*Pool, error) {
	resp, err := c.admin.GetPool(ctx, &poolmgrv1.GetPoolRequest{Ref: refToProto(ref)})
	if err != nil {
		return nil, mapErr(ctx, err)
	}
	return poolFromProto(resp), nil
}

// ListPools implements Client.
func (c *Connection) ListPools(ctx context.Context, namespace string) ([]*Pool, error) {
	req := &poolmgrv1.ListPoolsRequest{}
	if namespace != "" {
		req.Namespace = &namespace
	}
	resp, err := c.admin.ListPools(ctx, req)
	if err != nil {
		return nil, mapErr(ctx, err)
	}
	out := make([]*Pool, 0, len(resp.GetPools()))
	for _, p := range resp.GetPools() {
		out = append(out, poolFromProto(p))
	}
	return out, nil
}

// ClaimVM implements Client.
func (c *Connection) ClaimVM(ctx context.Context, pool PoolRef) (*Claim, error) {
	resp, err := c.lease.ClaimVM(ctx, &poolmgrv1.ClaimVMRequest{Pool: refToProto(pool)})
	if err != nil {
		return nil, mapErr(ctx, err)
	}
	return &Claim{
		LeaseID:           resp.GetLeaseId(),
		VMUID:             resp.GetVmUid(),
		NetworkInterfaces: resp.GetNetworkInterfaces(),
		Host: HostRef{
			Name:    resp.GetHost().GetName(),
			Address: resp.GetHost().GetAddress(),
		},
	}, nil
}

// Heartbeat implements Client.
func (c *Connection) Heartbeat(ctx context.Context, leaseID string) (time.Time, error) {
	resp, err := c.lease.Heartbeat(ctx, &poolmgrv1.HeartbeatRequest{LeaseId: leaseID})
	if err != nil {
		return time.Time{}, mapErr(ctx, err)
	}
	return resp.GetExpiresAt().AsTime(), nil
}

// ReleaseVM implements Client.
func (c *Connection) ReleaseVM(ctx context.Context, leaseID string) error {
	_, err := c.lease.ReleaseVM(ctx, &poolmgrv1.ReleaseVMRequest{LeaseId: leaseID})
	return mapErr(ctx, err)
}

// Subscribe implements Client. The stream lives until Close, until the
// caller's context ends or until battery drops it.
//
// A failure is mapped against the caller's context, not the stream's own:
// mapErr reads the context to tell a cancellation the caller asked for from
// one it did not.
func (c *Connection) Subscribe(ctx context.Context, filter EventFilter) (EventStream, error) {
	req := &poolmgrv1.SubscribeRequest{}
	if filter.Pool != nil {
		req.Pool = refToProto(*filter.Pool)
	}
	streamCtx, cancel := context.WithCancel(ctx)
	stream, err := c.events.Subscribe(streamCtx, req)
	if err != nil {
		cancel()
		return nil, mapErr(ctx, err)
	}
	return newEventStream(streamCtx, cancel, stream), nil
}

// eventStream adapts the generated server-streaming client to EventStream.
// One reader goroutine, started by Subscribe, owns the gRPC stream and
// hands results to Recv, so a Recv abandoned on its own context does not
// lose the event it was waiting for. Recv is not for concurrent use.
type eventStream struct {
	cancel context.CancelFunc
	// streamCtx ends on Close; the reader goroutine exits with it.
	streamCtx context.Context
	results   chan streamResult
	// terminal is the error the stream ended with, once Recv has returned
	// it; every later Recv repeats it.
	terminal error
}

type streamResult struct {
	event *poolmgrv1.Event
	err   error
}

func newEventStream(ctx context.Context, cancel context.CancelFunc,
	stream grpc.ServerStreamingClient[poolmgrv1.Event]) *eventStream {
	s := &eventStream{cancel: cancel, streamCtx: ctx, results: make(chan streamResult)}
	go func() {
		for {
			e, err := stream.Recv()
			select {
			case s.results <- streamResult{event: e, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return s
}

// Recv implements EventStream.
func (s *eventStream) Recv(ctx context.Context) (*Event, error) {
	if s.terminal != nil {
		return nil, s.terminal
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.streamCtx.Done():
		s.terminal = fmt.Errorf("%w: events stream closed", ErrUnavailable)
		return nil, s.terminal
	case r := <-s.results:
		if r.err != nil {
			s.terminal = fmt.Errorf("%w: events stream ended: %s", ErrUnavailable, status.Convert(r.err).Message())
			return nil, s.terminal
		}
		return eventFromProto(r.event), nil
	}
}

// Close implements EventStream.
func (s *eventStream) Close() error {
	s.cancel()
	return nil
}

//= docs/requirements/06-deployment.md#battery-connection
//# The Operator SHALL map battery's gRPC status codes to errors
//# that distinguish a missing object, an exhausted Pool and an unavailable
//# battery.

// mapErr translates a gRPC status into the package's sentinel errors. A
// cancellation or deadline the caller asked for comes back as the context's
// error; one it did not, such as the call timeout or a connection that went
// away, is ErrUnavailable. Codes without a sentinel keep their code in the
// message.
func mapErr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("battery: %w", err)
	}
	var sentinel error
	switch st.Code() {
	case codes.NotFound:
		sentinel = ErrNotFound
	case codes.ResourceExhausted:
		sentinel = ErrExhausted
	case codes.Unavailable:
		sentinel = ErrUnavailable
	case codes.AlreadyExists:
		sentinel = ErrAlreadyExists
	case codes.InvalidArgument:
		sentinel = ErrInvalid
	case codes.FailedPrecondition:
		sentinel = ErrFailedPrecondition
	case codes.Canceled, codes.DeadlineExceeded:
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		// battery, which got the caller's deadline as the call's timeout,
		// can end the call a moment before the caller's own timer fires.
		if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
			return context.DeadlineExceeded
		}
		sentinel = ErrUnavailable
	default:
		return fmt.Errorf("battery: %s: %s", st.Code(), st.Message())
	}
	return fmt.Errorf("%w: %s", sentinel, st.Message())
}

// eventFromProto converts one wire Event.
func eventFromProto(e *poolmgrv1.Event) *Event {
	out := &Event{
		ID:    e.GetId(),
		Pool:  PoolRef{Name: e.GetPoolName(), Namespace: e.GetPoolNamespace()},
		VMUID: e.GetVmUid(),
		Type:  e.GetType(),
		At:    e.GetCreatedAt().AsTime(),
	}
	if e.GetPayloadJson() != "" {
		out.Payload = json.RawMessage(e.GetPayloadJson())
	}
	return out
}
