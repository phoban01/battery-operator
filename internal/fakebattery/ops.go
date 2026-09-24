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
	"errors"
	"slices"
	"strings"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	mvmv1 "github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// The operations in this file are what the gRPC handlers call. They return
// gRPC status errors directly, because the handlers are their only callers.

func errNotFound(format string, args ...any) error {
	return status.Errorf(codes.NotFound, format, args...)
}

func errInvalid(format string, args ...any) error {
	return status.Errorf(codes.InvalidArgument, format, args...)
}

// validateSpec applies battery's PoolAdmin validation plus a check that the
// heartbeat expiry threshold is positive, because a zero threshold would
// expire every Lease on the next tick.
func validateSpec(spec *poolSpec) error {
	if spec.Ref.Name == "" {
		return errInvalid("spec.name is required")
	}
	if spec.Ref.Namespace == "" {
		return errInvalid("spec.namespace is required")
	}
	switch spec.Replenishment.Type {
	case poolmgrv1.ReplenishmentStrategyType_IMMEDIATE_ON_LEASE, poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE:
	case poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD:
		if spec.Replenishment.MinSize == nil || *spec.Replenishment.MinSize <= 0 {
			return errInvalid("spec.replenishment_strategy: MIN_SIZE_THRESHOLD requires a positive min_size")
		}
	default:
		return errInvalid("spec.replenishment_strategy: unknown replenishment strategy %v", spec.Replenishment.Type)
	}
	switch spec.HookFailurePolicy {
	case poolmgrv1.HookFailurePolicy_DELETE_AND_REPLACE, poolmgrv1.HookFailurePolicy_QUARANTINE:
	default:
		return errInvalid("spec.hook_failure_policy: invalid value %v", spec.HookFailurePolicy)
	}
	if spec.Template == nil {
		return errInvalid("spec.microvm_template is required")
	}
	if spec.Size < 0 {
		return errInvalid("spec.size must not be negative")
	}
	if spec.HeartbeatExpiryThreshold <= 0 {
		return errInvalid("spec.heartbeat_expiry_threshold must be positive")
	}
	// battery forces allow_guest_agent on, because its hooks need it.
	spec.Template = proto.CloneOf(spec.Template)
	spec.Template.AllowGuestAgent = true
	spec.FlintlockHosts = slices.Clone(spec.FlintlockHosts)
	spec.CreateCommands = slices.Clone(spec.CreateCommands)
	spec.PreLeaseCommands = slices.Clone(spec.PreLeaseCommands)
	return nil
}

// createPool implements PoolAdmin.CreatePool.
func (b *Battery) createPool(spec poolSpec) (*pool, error) {
	if err := validateSpec(&spec); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	key := keyOf(spec.Ref)
	if _, exists := b.pools[key]; exists {
		return nil, status.Errorf(codes.AlreadyExists, "pool %s already exists", key)
	}
	b.pools[key] = &poolState{key: key, spec: spec, fresh: true, warned: make(map[string]int64)}
	b.kickReconcile()
	return &pool{Spec: spec}, nil
}

// updatePool implements PoolAdmin.UpdatePool. Existing MicroVMs are kept,
// including any on a Host the new spec no longer lists; the new spec applies
// to every later decision.
func (b *Battery) updatePool(spec poolSpec) (*pool, error) {
	if err := validateSpec(&spec); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	ps, err := b.poolLocked(spec.Ref)
	if err != nil {
		return nil, err
	}
	ps.spec = spec
	ps.fresh = true
	b.kickReconcile()
	return &pool{Spec: spec, Status: b.countsLocked(ps.key).status()}, nil
}

// deletePool implements PoolAdmin.DeletePool. battery refuses while the Pool
// owns any MicroVM; the fake refuses only while a Lease is outstanding and
// otherwise deletes the Pool's MicroVMs from their Hosts.
func (b *Battery) deletePool(ctx context.Context, ref PoolRef) error {
	b.mu.Lock()
	ps, err := b.poolLocked(ref)
	if err != nil {
		b.mu.Unlock()
		return err
	}
	for _, ls := range b.leases {
		if keyOf(ls.rec.Pool) == ps.key {
			b.mu.Unlock()
			return status.Errorf(codes.FailedPrecondition, "pool %s has outstanding lease %s; release it first", ps.key, ls.rec.LeaseID)
		}
	}
	delete(b.pools, ps.key)
	var vms []*vmState
	for _, vm := range b.vms {
		if vm.pool == ps.key {
			vms = append(vms, vm)
		}
	}
	b.mu.Unlock()

	var firstErr error
	for _, vm := range vms {
		if vm.uid == "" {
			// CreateMicroVM is still in flight; the provisioner sees the
			// Pool is gone when it returns and deletes the MicroVM itself.
			continue
		}
		if err := b.deleteVM(ctx, vm, 0); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return status.Errorf(codes.Unavailable, "pool %s deleted but some microvms remain pending deletion: %v", ps.key, firstErr)
	}
	return nil
}

// getPool implements PoolAdmin.GetPool.
func (b *Battery) getPool(ref PoolRef) (*pool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ps, err := b.poolLocked(ref)
	if err != nil {
		return nil, err
	}
	return &pool{Spec: ps.spec, Status: b.countsLocked(ps.key).status()}, nil
}

// listPools implements PoolAdmin.ListPools; an empty namespace lists every
// Pool. Results are sorted by namespace then name.
func (b *Battery) listPools(namespace string) []*pool {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []*pool
	for key, ps := range b.pools {
		if namespace != "" && key.namespace != namespace {
			continue
		}
		out = append(out, &pool{Spec: ps.spec, Status: b.countsLocked(key).status()})
	}
	slices.SortFunc(out, func(x, y *pool) int {
		if c := strings.Compare(x.Spec.Ref.Namespace, y.Spec.Ref.Namespace); c != 0 {
			return c
		}
		return strings.Compare(x.Spec.Ref.Name, y.Spec.Ref.Name)
	})
	return out
}

// claimResult is what claimVM hands the handler to build ClaimVMResponse.
type claimResult struct {
	leaseID string
	vmUID   string
	ifaces  map[string]*types.NetworkInterfaceStatus
	// host and address are the Host the MicroVM is placed on.
	host    string
	address string
}

//= docs/requirements/08-test-doubles.md#fake-battery
//# The fake battery SHALL replenish Pools, expire Leases that are
//# not renewed within the Pool's expiry threshold, and answer `ClaimVM` on an
//# empty Pool with `RESOURCE_EXHAUSTED`, as battery v0.3.3 does.

// claimVM implements Lease.ClaimVM: after the injected claim latency it
// takes the longest-available MicroVM, runs the pre-lease hooks, creates the
// Lease and starts the strategy's replacement. It returns
// RESOURCE_EXHAUSTED when nothing is AVAILABLE and NOT_FOUND for an unknown
// Pool.
func (b *Battery) claimVM(ctx context.Context, ref PoolRef) (*claimResult, error) {
	//= docs/requirements/10-battery.md#claiming
	//# If a Pool has no MicroVM in the phase `AVAILABLE`, then battery
	//# SHALL answer `ClaimVM` for that Pool with `RESOURCE_EXHAUSTED` and create no
	//# Lease.

	//= docs/requirements/10-battery.md#claiming
	//# If battery holds no Pool of the name and namespace a `ClaimVM`
	//# names, then battery SHALL answer it with `NOT_FOUND`.

	//= docs/requirements/10-battery.md#claiming
	//# When `ClaimVM` succeeds, battery SHALL choose a new lease id,
	//# and set the Lease's expiry to the time of the claim plus the Pool's
	//# `heartbeat_expiry_threshold`.

	//= docs/requirements/10-battery.md#claiming
	//# Once battery has committed a Lease in `ClaimVM`, battery SHALL
	//# answer that `ClaimVM` with success.
	if err := b.claimLatency(ctx); err != nil {
		return nil, err
	}

	b.mu.Lock()
	ps, err := b.poolLocked(ref)
	if err != nil {
		b.mu.Unlock()
		return nil, err
	}
	vm := b.oldestAvailableLocked(ps.key)
	if vm == nil {
		b.mu.Unlock()
		return nil, status.Errorf(codes.ResourceExhausted, "no available vm in pool %s", ps.key)
	}
	//= docs/requirements/10-battery.md#claiming
	//# battery SHALL move a MicroVM out of the phase `AVAILABLE` in
	//# the same transaction in which `ClaimVM` selects it, so that two `ClaimVM`
	//# calls never lease the same MicroVM.
	//
	// The fake's transaction is b.mu, held from the selection to here.
	b.setPhaseLocked(vm, poolmgrv1.VMPhase_PRE_LEASE_HOOK_RUNNING)
	hooks := ps.spec.PreLeaseCommands
	b.mu.Unlock()

	if err := b.runHooks(ctx, vm, HookPreLease, hooks); err != nil {
		b.applyHookFailurePolicy(ctx, vm, HookPreLease, err)
		return nil, status.Errorf(codes.Internal, "pre-lease hook: %v", err)
	}

	b.mu.Lock()
	if !b.aliveLocked(vm) {
		b.mu.Unlock()
		return nil, errNotFound("pool %s was deleted during the claim", ps.key)
	}
	now := b.cfg.Clock.Now()
	leaseID := newLeaseID()
	b.leases[leaseID] = &leaseState{rec: LeaseRecord{
		LeaseID:         leaseID,
		VMUID:           vm.uid,
		Pool:            ps.key.ref(),
		ClaimedAt:       now,
		LastHeartbeatAt: now,
		ExpiresAt:       now.Add(ps.spec.HeartbeatExpiryThreshold),
	}}
	vm.leaseID = leaseID
	b.setPhaseLocked(vm, poolmgrv1.VMPhase_LEASED)
	b.emitLocked(ps.key, vm.uid, poolmgrv1.EventType_VM_CLAIMED, map[string]any{payloadLeaseID: leaseID, payloadHost: vm.host})
	b.provisionNLocked(ps, strategyOf(ps.spec).onClaimed(), "claim")
	res := &claimResult{leaseID: leaseID, vmUID: vm.uid, host: vm.host}
	b.mu.Unlock()

	// Best effort, as in battery: the Lease exists whether or not the Host
	// answers, so a failure here only leaves the interfaces empty.
	if h, err := b.cfg.Hosts.get(res.host); err == nil {
		res.address = h.address
		if resp, err := h.vm.GetMicroVM(ctx, &mvmv1.GetMicroVMRequest{Uid: res.vmUID}); err == nil {
			res.ifaces = resp.GetMicrovm().GetStatus().GetNetworkInterfaces()
		}
	}
	return res, nil
}

// claimLatency waits out Faults.ClaimLatency on the fake's clock.
func (b *Battery) claimLatency(ctx context.Context) error {
	b.mu.Lock()
	d := b.faults.ClaimLatency
	b.mu.Unlock()
	if d <= 0 {
		return nil
	}
	timer := b.cfg.Clock.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C():
		return nil
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	}
}

// listLeases implements Lease.ListLeases: the Leases the fake holds, of one
// Pool or of every Pool, ordered by lease id as battery orders them.
// RefuseHeartbeats hides every Lease, as Heartbeat's NOT_FOUND does.
func (b *Battery) listLeases(pool *PoolRef) []LeaseRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.faults.RefuseHeartbeats {
		return nil
	}
	out := make([]LeaseRecord, 0, len(b.leases))
	for _, ls := range b.leases {
		if pool == nil || ls.rec.Pool == *pool {
			out = append(out, ls.rec)
		}
	}
	slices.SortFunc(out, func(x, y LeaseRecord) int { return strings.Compare(x.LeaseID, y.LeaseID) })
	return out
}

// heartbeat implements Lease.Heartbeat: it moves the Lease's expiry to now
// plus the Pool's threshold and returns it. As in battery, a Lease past its
// expiry that the control loop has not swept yet is renewed like any other;
// only a Lease the fake no longer holds is NOT_FOUND. RefuseHeartbeats
// makes every Lease look gone.
func (b *Battery) heartbeat(leaseID string) (time.Time, error) {
	//= docs/requirements/10-battery.md#heartbeat
	//# When battery receives a `Heartbeat` for a Lease it still holds,
	//# battery SHALL set the Lease's expiry to the time of the `Heartbeat` plus
	//# the Pool's `heartbeat_expiry_threshold` and answer with that expiry,
	//# whether or not the Lease's previous expiry has passed.

	//= docs/requirements/10-battery.md#heartbeat
	//# If battery does not hold the Lease a `Heartbeat` names, then
	//# battery SHALL answer it with `NOT_FOUND`.
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.faults.RefuseHeartbeats {
		return time.Time{}, errNotFound("lease %s not found (fault injection: heartbeats refused)", leaseID)
	}
	ls, ok := b.leases[leaseID]
	if !ok {
		return time.Time{}, errNotFound("lease %s not found", leaseID)
	}
	ps, ok := b.pools[keyOf(ls.rec.Pool)]
	if !ok {
		return time.Time{}, errNotFound("pool %s of lease %s not found", ls.rec.Pool, leaseID)
	}
	now := b.cfg.Clock.Now()
	ls.rec.LastHeartbeatAt = now
	ls.rec.ExpiresAt = now.Add(ps.spec.HeartbeatExpiryThreshold)
	return ls.rec.ExpiresAt, nil
}

// releaseVM implements Lease.ReleaseVM: the MicroVM is deleted from its Host
// and the Lease removed. As in battery the Lease stays until the Host
// confirms the deletion; until then a retry gets UNAVAILABLE and the control
// loop keeps retrying the deletion.
func (b *Battery) releaseVM(ctx context.Context, leaseID string) error {
	//= docs/requirements/10-battery.md#release
	//# When battery receives a `ReleaseVM` for a Lease it holds,
	//# battery SHALL delete the Lease's MicroVM through `flintlockd` and answer
	//# with success only once `flintlockd` has confirmed the deletion and the
	//# Lease is deleted.

	//= docs/requirements/10-battery.md#release
	//# If `flintlockd` does not confirm the deletion of a released
	//# Lease's MicroVM, then battery SHALL answer the `ReleaseVM` with
	//# `UNAVAILABLE`, keep the Lease, and retry the deletion in every later sweep
	//# until `flintlockd` confirms it, deleting the Lease then.

	//= docs/requirements/10-battery.md#release
	//# If battery does not hold the Lease a `ReleaseVM` names, then
	//# battery SHALL answer it with `NOT_FOUND`.
	b.mu.Lock()
	ls, ok := b.leases[leaseID]
	if !ok {
		b.mu.Unlock()
		return errNotFound("lease %s not found", leaseID)
	}
	vm := b.vmByUID[ls.rec.VMUID]
	if vm == nil {
		b.dropLeaseLocked(leaseID)
		b.mu.Unlock()
		return nil
	}
	key := keyOf(ls.rec.Pool)
	if !vm.released {
		vm.released = true
		b.emitLocked(key, vm.uid, poolmgrv1.EventType_VM_RELEASED, map[string]any{payloadLeaseID: leaseID})
	}
	b.mu.Unlock()

	if err := b.deleteVM(ctx, vm, poolmgrv1.EventType_VM_DELETED_ON_RELEASE); err != nil {
		return status.Errorf(codes.Unavailable, "vm cleanup pending, retry later: %v", err)
	}
	return nil
}

// unavailable reports the injected UNAVAILABLE period. Every RPC checks it
// through the server interceptors.
func (b *Battery) unavailable() error {
	b.mu.Lock()
	until := b.unavailableUntil
	b.mu.Unlock()
	if !until.IsZero() && b.cfg.Clock.Now().Before(until) {
		return status.Error(codes.Unavailable, "fault injection: battery unavailable")
	}
	return nil
}

// setPhaseLocked moves vm to phase and stamps it.
func (b *Battery) setPhaseLocked(vm *vmState, phase VMPhase) {
	vm.phase = phase
	vm.updated = b.cfg.Clock.Now()
}

// isCtxErr reports whether err is a context cancellation or deadline, which
// the provisioner treats as a shutdown rather than a Host failure.
func isCtxErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		status.Code(err) == codes.Canceled || status.Code(err) == codes.DeadlineExceeded
}
