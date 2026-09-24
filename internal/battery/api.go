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
	"errors"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
)

// The enum types are aliases of the generated proto enums. Use the proto
// constants, for example poolmgrv1.EventType_VM_AVAILABLE.
type (
	// ReplenishmentStrategyType is the proto ReplenishmentStrategyType.
	ReplenishmentStrategyType = poolmgrv1.ReplenishmentStrategyType
	// HookFailurePolicy is the proto HookFailurePolicy.
	HookFailurePolicy = poolmgrv1.HookFailurePolicy
	// EventType is the proto EventType.
	EventType = poolmgrv1.EventType
)

// Sentinel errors. Every error a Client method returns for a status battery
// answered with wraps one of these or carries the status code in its
// message; use errors.Is.
var (
	// ErrNotFound is NOT_FOUND: an unknown Pool or an unknown Lease. On
	// ReleaseVM it means the Lease is already gone.
	ErrNotFound = errors.New("battery: not found")
	// ErrExhausted is RESOURCE_EXHAUSTED from ClaimVM: the Pool has no
	// AVAILABLE MicroVM.
	ErrExhausted = errors.New("battery: pool exhausted")
	// ErrUnavailable is UNAVAILABLE, a lost connection, or a call that
	// battery did not answer within the call deadline.
	ErrUnavailable = errors.New("battery: unavailable")
	// ErrAlreadyExists is ALREADY_EXISTS from CreatePool.
	ErrAlreadyExists = errors.New("battery: already exists")
	// ErrInvalid is INVALID_ARGUMENT: a request battery rejects, such as a
	// PoolSpec that does not validate.
	ErrInvalid = errors.New("battery: invalid request")
	// ErrFailedPrecondition is FAILED_PRECONDITION, which battery answers
	// DeletePool with while the Pool still has MicroVMs or Leases.
	ErrFailedPrecondition = errors.New("battery: failed precondition")
)

// PoolRef identifies a Pool (proto PoolRef).
type PoolRef struct {
	Name      string
	Namespace string
}

// String renders namespace/name for logs and error messages.
func (r PoolRef) String() string { return r.Namespace + "/" + r.Name }

// ReplenishmentStrategy is the proto ReplenishmentStrategy.
type ReplenishmentStrategy struct {
	Type ReplenishmentStrategyType
	// MinSize is meaningful only for MIN_SIZE_THRESHOLD.
	MinSize *int32
}

// PoolSpec is the proto PoolSpec.
type PoolSpec struct {
	Ref PoolRef
	// Template is the flintlock MicroVMSpec every MicroVM of the Pool is
	// created from. The client clones it on the way out and on the way in.
	Template *types.MicroVMSpec
	// Size is the target number of warm MicroVMs.
	Size int32
	// FlintlockHosts are the names of the Hosts the Pool may place on.
	FlintlockHosts    []string
	Replenishment     ReplenishmentStrategy
	CreateCommands    []string
	PreLeaseCommands  []string
	HookFailurePolicy HookFailurePolicy
	// HeartbeatInterval and HeartbeatExpiryThreshold are the Pool's Lease
	// settings; zero leaves them to battery.
	HeartbeatInterval        time.Duration
	HeartbeatExpiryThreshold time.Duration
}

// PoolStatus is the proto PoolStatus.
type PoolStatus struct {
	Available    int32
	Leased       int32
	Provisioning int32
	Quarantined  int32
}

// Pool is a spec with its live status (proto Pool).
type Pool struct {
	Spec   PoolSpec
	Status PoolStatus
}

// HostRef is the proto HostInfo of a ClaimVMResponse: the Host that runs
// the leased MicroVM.
type HostRef struct {
	// Name is the Host's name in battery's configuration.
	Name string
	// Address is the Host's flintlockd gRPC endpoint.
	Address string
}

// Claim is a successful ClaimVMResponse.
type Claim struct {
	// LeaseID is presented on every Heartbeat and on ReleaseVM.
	LeaseID string
	// VMUID is the flintlock uid of the leased MicroVM.
	VMUID string
	// NetworkInterfaces is copied from flintlock's MicroVMStatus.
	NetworkInterfaces map[string]*types.NetworkInterfaceStatus
	// Host is the Host that runs the MicroVM.
	Host HostRef
}

// LeaseRecord is one proto LeaseRecord, as ListLeases returns it: battery's
// stored record of a Lease.
type LeaseRecord struct {
	// LeaseID is the id ClaimVM returned.
	LeaseID string
	// VMUID is the flintlock uid of the leased MicroVM.
	VMUID string
	Pool  PoolRef
	// ClaimedAt is when ClaimVM granted the Lease.
	ClaimedAt time.Time
	// LastHeartbeatAt is when the last Heartbeat renewed the Lease; until
	// the first, it is the claim time.
	LastHeartbeatAt time.Time
	// ExpiresAt is the expiry the last ClaimVM or Heartbeat set. It can be
	// in the past: battery keeps an expired Lease until its sweeper removes
	// it, and a Heartbeat before then still renews it.
	ExpiresAt time.Time
}

// Event is one proto Event.
type Event struct {
	// ID is monotonic per Pool.
	ID   int64
	Pool PoolRef
	// VMUID is empty for Pool-level events.
	VMUID string
	Type  EventType
	At    time.Time
	// Payload is the type-specific payload_json, left for the consumer to
	// decode.
	Payload json.RawMessage
}

// EventFilter narrows a subscription to one Pool; a nil Pool subscribes to
// every Pool.
type EventFilter struct {
	Pool *PoolRef
}

// EventStream is one Events.Subscribe subscription.
type EventStream interface {
	// Recv blocks for the next event. Once the stream has ended without the
	// caller asking, Recv returns an error wrapping ErrUnavailable, and
	// keeps returning it; the caller subscribes again. A Recv abandoned on
	// its own context returns the context's error and loses no event.
	Recv(ctx context.Context) (*Event, error)
	// Close ends the subscription. It is safe to call more than once.
	Close() error
}

// Client is what the controllers call battery through: one method per RPC
// of the PoolAdmin, Lease and Events services. Connection implements it
// over gRPC; a controller test can implement it by hand. Implementations
// are safe for concurrent use.
type Client interface {
	// CreatePool returns ErrAlreadyExists for a Pool battery knows.
	CreatePool(ctx context.Context, spec PoolSpec) (*Pool, error)
	// UpdatePool returns ErrNotFound for a Pool battery does not know.
	UpdatePool(ctx context.Context, spec PoolSpec) (*Pool, error)
	// DeletePool returns ErrNotFound for an unknown Pool and
	// ErrFailedPrecondition while the Pool is still in use.
	DeletePool(ctx context.Context, ref PoolRef) error
	// GetPool returns ErrNotFound for an unknown Pool.
	GetPool(ctx context.Context, ref PoolRef) (*Pool, error)
	// ListPools lists the Pools in a namespace; empty lists every
	// namespace.
	ListPools(ctx context.Context, namespace string) ([]*Pool, error)
	// ClaimVM leases a warm MicroVM from the Pool. It returns ErrExhausted
	// when none is AVAILABLE and ErrNotFound for an unknown Pool.
	ClaimVM(ctx context.Context, pool PoolRef) (*Claim, error)
	// Heartbeat extends a Lease and returns its new expiry. ErrNotFound
	// means the Lease no longer exists.
	Heartbeat(ctx context.Context, leaseID string) (expiresAt time.Time, err error)
	// ReleaseVM ends a Lease. ErrNotFound means it had already ended.
	ReleaseVM(ctx context.Context, leaseID string) error
	// ListLeases reads battery's Leases without renewing any, ordered by
	// lease id. A nil pool lists every Pool's; a Pool battery does not know
	// lists none, without an error. A Lease that is not listed has ended.
	ListLeases(ctx context.Context, pool *PoolRef) ([]*LeaseRecord, error)
	// Subscribe opens an event stream. It returns ErrUnavailable when
	// battery cannot be reached; the stream reports later drops itself.
	Subscribe(ctx context.Context, filter EventFilter) (EventStream, error)
}
