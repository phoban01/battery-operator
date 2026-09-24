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
	"fmt"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	mvmv1 "github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// strategy is a Pool's replenishment strategy. The event-driven part is
// battery's: IMMEDIATE_ON_LEASE starts one MicroVM per claim,
// REPLACE_ON_DELETE one per deletion, MIN_SIZE_THRESHOLD acts only on the
// tick. The tick additionally tops every Pool up to its target, which is how
// a fresh Pool fills and how a Pool recovers from a failed create or a
// quarantined MicroVM; battery's tick does that only for
// MIN_SIZE_THRESHOLD, and battery tops the event-driven strategies up only
// once, when a Pool's reconciler starts. The target is the idle warm size for
// IMMEDIATE_ON_LEASE, where size is headroom rather than a ceiling, and the
// total population for the other two.
type strategy struct {
	typ     poolmgrv1.ReplenishmentStrategyType
	size    int32
	minSize int32
}

func strategyOf(spec poolSpec) strategy {
	s := strategy{typ: spec.Replenishment.Type, size: spec.Size}
	if spec.Replenishment.MinSize != nil {
		s.minSize = *spec.Replenishment.MinSize
	}
	return s
}

// onClaimed is how many MicroVMs to start after a successful claim.
func (s strategy) onClaimed() int {
	if s.typ == poolmgrv1.ReplenishmentStrategyType_IMMEDIATE_ON_LEASE {
		return 1
	}
	return 0
}

// onDeleted is how many MicroVMs to start after a deletion.
func (s strategy) onDeleted() int {
	if s.typ == poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE {
		return 1
	}
	return 0
}

// tickDeficit is how many MicroVMs the tick starts to reach the target.
func (s strategy) tickDeficit(c counts) int {
	var deficit int32
	switch s.typ {
	case poolmgrv1.ReplenishmentStrategyType_IMMEDIATE_ON_LEASE:
		deficit = s.size - (c.available + c.provisioning)
	case poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD:
		if c.available >= s.minSize {
			return 0
		}
		deficit = s.size - (c.available + c.leased + c.provisioning)
	case poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE:
		deficit = s.size - (c.available + c.leased + c.provisioning)
	default:
		return 0
	}
	return int(max(deficit, 0))
}

//= docs/requirements/08-test-doubles.md#fake-battery
//# The fake battery SHALL run on an injectable clock, so that a
//# test can expire a Lease without waiting.

// tick is one pass of the control loop: expire and warn on Leases, retry
// deletions a Host refused, and top every Pool up. Every time it reads
// comes from Config.Clock, so a test that advances a fake clock past a
// Lease's expiry has the Lease expired on the next tick. The tick is the
// fake's sweep: until it runs, a Lease past its expiry is still held and a
// heartbeat renews it, as in battery.
func (b *Battery) tick(ctx context.Context) {
	//= docs/requirements/10-battery.md#expiry
	//# battery SHALL delete an expired Lease only in a sweep, which
	//# runs once every `sweep_interval` while battery runs, and which deletes
	//# every Lease whose expiry is at or before the time of the sweep.

	//= docs/requirements/10-battery.md#expiry
	//= type=implication
	//# If a `Heartbeat` renews a Lease after a sweep has listed it as
	//# expired and before the sweep deletes it, then battery SHALL keep the Lease
	//# with its renewed expiry.

	//= docs/requirements/10-battery.md#expiry
	//# When a sweep deletes a Lease, battery SHALL delete the Lease's
	//# MicroVM through `flintlockd`, and SHALL retry the deletion in every later
	//# sweep until `flintlockd` confirms it.

	// The implication: the tick lists and deletes expired Leases under b.mu,
	// which heartbeat also takes, so no heartbeat lands in between.
	now := b.cfg.Clock.Now()
	var expired, pending []*vmState

	b.mu.Lock()
	for id, ls := range b.leases {
		ps, ok := b.pools[keyOf(ls.rec.Pool)]
		if !ok {
			continue
		}
		if !ls.rec.ExpiresAt.After(now) {
			b.dropLeaseLocked(id)
			if vm := b.vmByUID[ls.rec.VMUID]; vm != nil && vm.phase == poolmgrv1.VMPhase_LEASED {
				expired = append(expired, vm)
			}
			continue
		}
		if ls.rec.ExpiresAt.Sub(now) <= warningWindow(ps.spec) && ps.warned[id] != ls.rec.ExpiresAt.UnixNano() {
			ps.warned[id] = ls.rec.ExpiresAt.UnixNano()
			b.emitLocked(ps.key, ls.rec.VMUID, poolmgrv1.EventType_VM_EXPIRING_SOON,
				map[string]any{payloadLeaseID: id, "expires_at": ls.rec.ExpiresAt.UTC().Format(time.RFC3339Nano)})
		}
	}
	for _, vm := range b.vms {
		if vm.phase == poolmgrv1.VMPhase_DELETING && vm.uid != "" && !vm.deleteInFlight {
			pending = append(pending, vm)
		}
	}
	for _, ps := range b.pools {
		c := b.countsLocked(ps.key)
		deficit := strategyOf(ps.spec).tickDeficit(c)
		if deficit > 0 && !ps.fresh {
			b.emitLocked(ps.key, "", poolmgrv1.EventType_POOL_SIZE_BELOW_TARGET, map[string]any{
				"target": ps.spec.Size, "available": c.available, "leased": c.leased,
				"provisioning": c.provisioning, "quarantined": c.quarantined,
			})
		}
		ps.fresh = false
		b.provisionNLocked(ps, deficit, "tick")
	}
	b.mu.Unlock()

	for _, vm := range expired {
		b.spawn(func() {
			if err := b.deleteVM(ctx, vm, poolmgrv1.EventType_VM_DELETED_DUE_TO_EXPIRY); err != nil {
				b.log.Error(err, "Deferred deletion of expired MicroVM", "uid", vm.uid, "host", vm.host)
			}
		})
	}
	for _, vm := range pending {
		b.spawn(func() {
			// Zero, not vm.deleteEvent: the event the first attempt recorded
			// is already on the MicroVM, and reading it here would be a read
			// outside b.mu.
			if err := b.deleteVM(ctx, vm, 0); err != nil {
				b.log.Error(err, "Deferred deletion of MicroVM again", "uid", vm.uid, "host", vm.host)
			}
		})
	}
}

// warningWindow is how long before expiry VM_EXPIRING_SOON fires: one
// heartbeat interval, or half the threshold when the Pool declares none.
func warningWindow(spec poolSpec) time.Duration {
	if spec.HeartbeatInterval > 0 {
		return spec.HeartbeatInterval
	}
	return spec.HeartbeatExpiryThreshold / 2
}

// spawn runs fn on a tracked goroutine so that Run can wait for it.
func (b *Battery) spawn(fn func()) {
	b.wg.Go(fn)
}

// provisionNLocked reserves n MicroVMs in ps, placing each, and starts
// their provisioning. Nothing happens before Run or after it stops; the
// first tick catches up. It emits POOL_REPLENISHING when it starts any.
// The stopped check is what makes it safe to add to b.wg here: stop sets
// that field under the same lock before it waits.
func (b *Battery) provisionNLocked(ps *poolState, n int, reason string) {
	if n <= 0 || b.stopped || b.runCtx == nil || b.runCtx.Err() != nil {
		return
	}
	var reserved []*vmState
	for range n {
		host, err := b.pickHostLocked(ps)
		if err != nil {
			b.log.Error(err, "Skipped placing MicroVM", "pool", ps.key.String())
			break
		}
		b.nextVM++
		now := b.cfg.Clock.Now()
		vm := &vmState{id: b.nextVM, pool: ps.key, host: host, phase: poolmgrv1.VMPhase_PROVISIONING, createdAt: now, updated: now}
		b.vms[vm.id] = vm
		reserved = append(reserved, vm)
	}
	if len(reserved) == 0 {
		return
	}
	b.emitLocked(ps.key, "", poolmgrv1.EventType_POOL_REPLENISHING, map[string]any{"count": len(reserved), "reason": reason})
	ctx := b.runCtx
	for _, vm := range reserved {
		b.spawn(func() { b.provision(ctx, vm) })
	}
}

//= docs/requirements/08-test-doubles.md#fake-battery
//# The fake battery SHALL create, place and delete MicroVMs only
//# through the fake `flintlockd`.

// provision is battery's pipeline for one reserved MicroVM: CreateMicroVM
// on the placed Host through its flintlock MicroVM client, wait for
// CREATED, run the create hooks through its MicroVMExec client, mark it
// AVAILABLE. Every failure after the create goes through the Pool's hook
// failure policy.
func (b *Battery) provision(ctx context.Context, vm *vmState) {
	h, err := b.cfg.Hosts.get(vm.host)
	if err != nil {
		b.dropReserved(vm, err)
		return
	}
	b.mu.Lock()
	ps, ok := b.pools[vm.pool]
	if !ok {
		b.mu.Unlock()
		b.dropReserved(vm, errors.New("pool deleted"))
		return
	}
	spec := proto.CloneOf(ps.spec.Template)
	hooks := ps.spec.CreateCommands
	b.mu.Unlock()
	// As battery v0.3.3 does, each MicroVM gets its own id, the Pool's name
	// and eight random hex digits, and the Pool's namespace unless the
	// template names one.
	spec.Id = newVMID(vm.pool.name)
	if spec.GetNamespace() == "" {
		spec.Namespace = vm.pool.namespace
	}

	// The create is not cancelled when the fake stops. A Host does not undo
	// a CreateMicroVM its caller stopped waiting for, so a create cut short
	// by stop could leave a MicroVM behind with no uid for cleanupAll to
	// delete it by. It runs detached from ctx instead, bounded like the
	// cleanup, and a MicroVM it makes while the fake is stopping is recorded
	// below like any other and deleted by cleanupAll once stop has waited
	// for this goroutine.
	createCtx, cancelCreate := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	created, err := h.vm.CreateMicroVM(createCtx, &mvmv1.CreateMicroVMRequest{Microvm: spec})
	cancelCreate()
	if err != nil {
		b.dropReserved(vm, fmt.Errorf("create microvm on %s: %w", vm.host, err))
		return
	}
	uid := created.GetMicrovm().GetSpec().GetUid()
	if uid == "" {
		b.dropReserved(vm, fmt.Errorf("create microvm on %s: host returned no uid", vm.host))
		return
	}

	b.mu.Lock()
	if !b.aliveLocked(vm) {
		b.mu.Unlock()
		b.deleteOrphan(h, uid)
		return
	}
	vm.uid = uid
	b.vmByUID[uid] = vm
	vm.updated = b.cfg.Clock.Now()
	b.emitLocked(vm.pool, uid, poolmgrv1.EventType_VM_PROVISIONED, map[string]any{payloadHost: vm.host})
	b.mu.Unlock()

	if ctx.Err() != nil {
		// The fake is stopping. The MicroVM has its uid now, so cleanupAll
		// deletes it; running the rest of the pipeline would only fail it.
		return
	}
	if err := b.waitCreated(ctx, h, uid); err != nil {
		b.applyHookFailurePolicy(ctx, vm, HookCreate, err)
		return
	}
	if !b.transition(vm, poolmgrv1.VMPhase_CREATE_HOOK_RUNNING) {
		b.deleteOrphan(h, uid)
		return
	}
	if err := b.runHooks(ctx, vm, HookCreate, hooks); err != nil {
		b.applyHookFailurePolicy(ctx, vm, HookCreate, err)
		return
	}
	b.mu.Lock()
	if !b.aliveLocked(vm) {
		b.mu.Unlock()
		b.deleteOrphan(h, uid)
		return
	}
	b.setPhaseLocked(vm, poolmgrv1.VMPhase_AVAILABLE)
	b.emitLocked(vm.pool, uid, poolmgrv1.EventType_VM_AVAILABLE, map[string]any{payloadHost: vm.host})
	b.mu.Unlock()
}

// transition moves vm to phase if it is still alive and reports whether it
// was.
func (b *Battery) transition(vm *vmState, phase VMPhase) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.aliveLocked(vm) {
		return false
	}
	b.setPhaseLocked(vm, phase)
	return true
}

// dropReserved forgets a reservation whose CreateMicroVM never produced a
// MicroVM. The next tick provisions again, so a Host that keeps failing is
// retried at the reconcile interval rather than in a loop.
func (b *Battery) dropReserved(vm *vmState, cause error) {
	b.mu.Lock()
	delete(b.vms, vm.id)
	b.mu.Unlock()
	if !isCtxErr(cause) {
		b.log.Error(cause, "Failed to provision MicroVM", "pool", vm.pool.String(), "host", vm.host)
	}
}

// deleteOrphan deletes a MicroVM whose Pool disappeared or whose record was
// removed while it was being provisioned.
func (b *Battery) deleteOrphan(h host, uid string) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if _, err := h.vm.DeleteMicroVM(ctx, &mvmv1.DeleteMicroVMRequest{Uid: uid}); err != nil && status.Code(err) != codes.NotFound {
		b.log.Error(err, "Left orphaned MicroVM behind", "uid", uid, "host", h.name)
	}
}

// waitCreated polls GetMicroVM until the MicroVM is CREATED, or fails when
// it is FAILED or Config.ReadyTimeout passes on the fake's clock. The first
// check is immediate.
func (b *Battery) waitCreated(ctx context.Context, h host, uid string) error {
	deadline := b.cfg.Clock.Now().Add(b.cfg.ReadyTimeout)
	for {
		resp, err := h.vm.GetMicroVM(ctx, &mvmv1.GetMicroVMRequest{Uid: uid})
		if err != nil {
			return fmt.Errorf("get microvm %s: %w", uid, err)
		}
		switch resp.GetMicrovm().GetStatus().GetState() {
		case types.MicroVMStatus_CREATED:
			return nil
		case types.MicroVMStatus_FAILED:
			return fmt.Errorf("microvm %s failed to create", uid)
		case types.MicroVMStatus_PENDING, types.MicroVMStatus_DELETING:
		}
		if !b.cfg.Clock.Now().Before(deadline) {
			return fmt.Errorf("microvm %s not created within %s", uid, b.cfg.ReadyTimeout)
		}
		if err := b.sleep(ctx, createPollInterval); err != nil {
			return err
		}
	}
}

// sleep waits d on the fake's clock, or until ctx is done.
func (b *Battery) sleep(ctx context.Context, d time.Duration) error {
	timer := b.cfg.Clock.NewTimer(d)
	select {
	case <-timer.C():
		return nil
	case <-ctx.Done():
		timer.Stop()
		return ctx.Err()
	}
}

// applyHookFailurePolicy is battery's ApplyHookFailurePolicy: QUARANTINE
// keeps the MicroVM for inspection, anything else deletes it; both emit
// VM_HOOK_FAILED. Cleanup runs detached from ctx so a cancelled claim cannot
// strand a MicroVM in a transient phase.
func (b *Battery) applyHookFailurePolicy(ctx context.Context, vm *vmState, hook HookKind, cause error) {
	b.mu.Lock()
	if !b.aliveLocked(vm) {
		b.mu.Unlock()
		return
	}
	policy := b.pools[vm.pool].spec.HookFailurePolicy
	vm.leaseID = ""
	b.emitLocked(vm.pool, vm.uid, poolmgrv1.EventType_VM_HOOK_FAILED,
		map[string]any{"hook": string(hook), "error": cause.Error(), "policy": policy.String()})
	if policy == poolmgrv1.HookFailurePolicy_QUARANTINE {
		b.setPhaseLocked(vm, poolmgrv1.VMPhase_QUARANTINED)
		b.mu.Unlock()
		b.log.Info("Quarantined MicroVM after its hook failed", "hook", string(hook), "uid", vm.uid, "pool", vm.pool.String(), "err", cause.Error())
		return
	}
	// Leave the provisioning count in the same critical section as the
	// event, so a tick that runs between here and the Host call already
	// sees the deficit.
	b.setPhaseLocked(vm, poolmgrv1.VMPhase_DELETING)
	b.mu.Unlock()
	b.log.Info("Deleting MicroVM after its hook failed", "hook", string(hook), "uid", vm.uid, "pool", vm.pool.String(), "err", cause.Error())

	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := b.deleteVM(cleanup, vm, 0); err != nil {
		b.log.Error(err, "Deferred deletion of MicroVM whose hook failed", "uid", vm.uid, "host", vm.host)
	}
}

// errDeletionInFlight is what deleteVM reports to a caller that asks for a
// deletion the fake has already put on the Host and is still waiting for.
// The caller does nothing: the attempt in flight finishes the deletion, or
// fails and leaves it to the tick.
var errDeletionInFlight = errors.New("fake battery: deletion already in flight")

// deleteVM marks vm DELETING, deletes it on its Host and, when the Host
// confirms, finishes the deletion with event as the VM_DELETED_* event to
// emit (zero to keep the one already recorded). On a Host error the MicroVM
// stays DELETING and the tick retries. Only one attempt per MicroVM is in
// flight at a time, so a Host that never answers cannot accumulate calls or
// goroutines; a caller that arrives while one is in flight gets
// errDeletionInFlight. Not called with b.mu held.
func (b *Battery) deleteVM(ctx context.Context, vm *vmState, event poolmgrv1.EventType) error {
	b.mu.Lock()
	if _, ok := b.vms[vm.id]; !ok {
		b.mu.Unlock()
		return nil
	}
	b.setPhaseLocked(vm, poolmgrv1.VMPhase_DELETING)
	if event != 0 {
		vm.deleteEvent = event
	}
	if vm.deleteInFlight {
		b.mu.Unlock()
		return fmt.Errorf("delete microvm %s on %s: %w", vm.uid, vm.host, errDeletionInFlight)
	}
	vm.deleteInFlight = true
	hostName, uid := vm.host, vm.uid
	b.mu.Unlock()

	err := b.deleteOnHost(ctx, hostName, uid)

	b.mu.Lock()
	defer b.mu.Unlock()
	vm.deleteInFlight = false
	if err != nil {
		return err
	}
	b.finishDeletionLocked(vm)
	return nil
}

// deleteOnHost is the Host half of a deletion. A MicroVM the Host no longer
// knows about counts as deleted.
func (b *Battery) deleteOnHost(ctx context.Context, hostName, uid string) error {
	if uid == "" {
		return nil
	}
	h, err := b.cfg.Hosts.get(hostName)
	if err != nil {
		return err
	}
	if _, err := h.vm.DeleteMicroVM(ctx, &mvmv1.DeleteMicroVMRequest{Uid: uid}); err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("delete microvm %s on %s: %w", uid, hostName, err)
	}
	return nil
}

//= docs/requirements/10-battery.md#events
//# battery SHALL record `VM_DELETED_DUE_TO_EXPIRY` or
//# `VM_DELETED_ON_RELEASE` for a leased MicroVM only after `flintlockd` has
//# confirmed its deletion.

// finishDeletionLocked removes a deleted MicroVM and its Lease, emits its
// VM_DELETED_* event and starts the strategy's replacement. deleteVM calls
// it only once the Host has confirmed the deletion.
func (b *Battery) finishDeletionLocked(vm *vmState) {
	if _, ok := b.vms[vm.id]; !ok {
		return
	}
	delete(b.vms, vm.id)
	if vm.uid != "" {
		delete(b.vmByUID, vm.uid)
	}
	payload := map[string]any{payloadHost: vm.host}
	if vm.leaseID != "" {
		b.dropLeaseLocked(vm.leaseID)
		payload[payloadLeaseID] = vm.leaseID
	}
	ps, ok := b.pools[vm.pool]
	if !ok {
		return
	}
	if vm.deleteEvent != 0 {
		b.emitLocked(vm.pool, vm.uid, vm.deleteEvent, payload)
	}
	b.provisionNLocked(ps, strategyOf(ps.spec).onDeleted(), "vm deleted")
}
