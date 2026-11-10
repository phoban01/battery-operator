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
	"cmp"
	"context"
	"errors"
	"slices"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
)

// DrainHost implements poolmgrv1.HostAdminServer, as battery v0.6.0's
// HostAdmin.DrainHost (its ADR 2026-10-21-drain-host.md): it cordons the
// Host, marks it draining, and deletes every MicroVM on it that is not
// leased. The drain is a state of the Host: the tick marks and deletes
// again until UncordonHost ends it.
func (s *hostAdminServer) DrainHost(ctx context.Context, req *poolmgrv1.DrainHostRequest) (*poolmgrv1.Host, error) {
	return s.b.drainHost(ctx, req.GetName(), req.GetReason())
}

// drainHost is DrainHost under the fake's locks: the cordon, the drain flag
// and the MicroVMs' move to DELETING happen in one critical section, as
// battery's store.DrainHost commits them in one transaction. The Host
// deletions then run detached from the caller, as deletePool's do, and
// one the Host does not confirm stays DELETING for the tick.
func (b *Battery) drainHost(ctx context.Context, name, reason string) (*poolmgrv1.Host, error) {
	if name == "" {
		return nil, errInvalid("name is required")
	}
	b.mu.Lock()
	hs := b.cfg.Hosts
	hs.mu.Lock()
	e, ok := hs.entries[name]
	if !ok {
		hs.mu.Unlock()
		b.mu.Unlock()
		//= docs/requirements/10-battery.md#hosts
		//# If `DrainHost` names a Host battery does not hold, then battery
		//# SHALL answer `NOT_FOUND`.
		return nil, errNotFound("host %q not found", name)
	}
	now := b.cfg.Clock.Now()

	//= docs/requirements/10-battery.md#hosts
	//# When `DrainHost` names a Host battery holds, battery SHALL
	//# cordon the Host, mark it draining, and set every MicroVM on the Host
	//# whose phase is not `LEASED`, `PRE_LEASE_HOOK_RUNNING` or `DELETING` to
	//# `DELETING`, all in one transaction, and SHALL then delete each of those
	//# MicroVMs through `flintlockd` before it answers, leaving one `flintlockd`
	//# does not confirm in `DELETING` for the sweep.

	//= docs/requirements/10-battery.md#hosts
	//# If `DrainHost` names a draining Host, then battery SHALL keep the
	//# Host's `draining_at` and `cordoned_at`, SHALL replace its
	//# `cordoned_reason` only when the request gives one, and SHALL answer with
	//# the Host and no error.
	if !e.cordoned {
		e.cordoned = true
		e.cordonedAt = now
	}
	if !e.draining {
		e.draining = true
		e.drainingAt = now
	}
	if reason != "" {
		e.cordonedReason = reason
	}
	e.updatedAt = now
	out := e.proto()
	hs.mu.Unlock()
	vms := b.markDrainedLocked(name)
	b.mu.Unlock()

	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	for _, vm := range vms {
		if err := b.deleteVM(cleanup, vm, 0); err != nil && !errors.Is(err, errDeletionInFlight) {
			b.log.Error(err, "Deferred deletion of MicroVM of draining Host", "uid", vm.uid, "host", vm.host, "pool", vm.pool.String())
		}
	}
	return out, nil
}

// markDrainedLocked moves every unleased MicroVM on the Host called name to
// DELETING, records VM_DELETED_ON_HOST_DRAIN for each, and returns the ones
// the caller has to delete on the Host. A MicroVM whose CreateMicroVM has
// not returned has no uid to delete yet; its record goes now, and the
// provisioning deletes what it created when it finds the record gone. One
// already DELETING is left to the deletion in flight or the tick.
func (b *Battery) markDrainedLocked(name string) []*vmState {
	var vms []*vmState
	for _, vm := range b.vms {
		if vm.host != name {
			continue
		}
		//= docs/requirements/10-battery.md#hosts
		//# battery SHALL NOT delete a MicroVM in the phase `LEASED` or
		//# `PRE_LEASE_HOOK_RUNNING` because its Host is draining; such a MicroVM
		//# ends by release or expiry, as on a cordoned Host.
		switch vm.phase {
		case poolmgrv1.VMPhase_LEASED, poolmgrv1.VMPhase_PRE_LEASE_HOOK_RUNNING, poolmgrv1.VMPhase_DELETING:
			continue
		}
		vms = append(vms, vm)
	}
	slices.SortFunc(vms, func(x, y *vmState) int { return cmp.Compare(x.id, y.id) })
	var toDelete []*vmState
	for _, vm := range vms {
		//= docs/requirements/10-battery.md#hosts
		//# battery SHALL record `VM_DELETED_ON_HOST_DRAIN` once for each
		//# MicroVM it sets to `DELETING` because its Host is draining, when it
		//# sets the phase, and SHALL record no other `VM_DELETED_*` event for it.
		b.setPhaseLocked(vm, poolmgrv1.VMPhase_DELETING)
		vm.deleteEvent = 0
		if vm.uid == "" {
			b.finishDeletionLocked(vm)
			continue
		}
		b.emitLocked(vm.pool, vm.uid, poolmgrv1.EventType_VM_DELETED_ON_HOST_DRAIN, map[string]any{payloadHost: vm.host})
		toDelete = append(toDelete, vm)
	}
	return toDelete
}
