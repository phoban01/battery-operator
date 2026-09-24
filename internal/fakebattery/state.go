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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
)

// VMPhase is battery's VMPhase.
type VMPhase = poolmgrv1.VMPhase

// PoolRef identifies a Pool by name and namespace.
type PoolRef struct {
	Name      string
	Namespace string
}

// String renders namespace/name.
func (r PoolRef) String() string { return r.Namespace + "/" + r.Name }

// replenishment is a PoolSpec's replenishment strategy.
type replenishment struct {
	Type poolmgrv1.ReplenishmentStrategyType
	// MinSize is meaningful only for MIN_SIZE_THRESHOLD.
	MinSize *int32
}

// poolSpec is a PoolSpec as the fake works with it: durations as
// durations, and the template cloned away from the request.
type poolSpec struct {
	Ref                      PoolRef
	Template                 *types.MicroVMSpec
	Size                     int32
	FlintlockHosts           []string
	Replenishment            replenishment
	CreateCommands           []string
	PreLeaseCommands         []string
	HookFailurePolicy        poolmgrv1.HookFailurePolicy
	HeartbeatInterval        time.Duration
	HeartbeatExpiryThreshold time.Duration
}

// poolStatus is a Pool's population by phase, as PoolStatus reports it.
type poolStatus struct {
	Available    int32
	Leased       int32
	Provisioning int32
	Quarantined  int32
}

// pool pairs a spec with its live status.
type pool struct {
	Spec   poolSpec
	Status poolStatus
}

// poolKey identifies a Pool by namespace and name.
type poolKey struct {
	name, namespace string
}

func keyOf(ref PoolRef) poolKey { return poolKey{name: ref.Name, namespace: ref.Namespace} }

func (k poolKey) ref() PoolRef { return PoolRef{Name: k.name, Namespace: k.namespace} }

func (k poolKey) String() string { return k.namespace + "/" + k.name }

// poolState is one declared Pool.
type poolState struct {
	key  poolKey
	spec poolSpec
	// nextEvent is the per-Pool monotonic event id (proto Event.id).
	nextEvent int64
	// rr is the round-robin cursor (PlacementRoundRobin).
	rr int
	// fresh is true between a create or update and the next tick; the
	// initial fill of a fresh Pool is not reported as POOL_SIZE_BELOW_TARGET.
	fresh bool
	// warned records, per lease id, the expiry for which VM_EXPIRING_SOON
	// was emitted; a heartbeat moves the expiry and re-arms the warning.
	warned map[string]int64
}

// vmState is one MicroVM the fake manages.
type vmState struct {
	id      int64
	uid     string
	pool    poolKey
	host    string
	phase   VMPhase
	leaseID string
	// deleteEvent is the VM_DELETED_* event to emit once the Host confirms
	// the deletion, or zero for a deletion that has no event of its own.
	deleteEvent poolmgrv1.EventType
	// deleteInFlight is set while a DeleteMicroVM call for this MicroVM is
	// on its Host. It keeps one deletion in flight at a time, so that a Host
	// that is slow to answer collects neither a second outstanding call nor
	// a second goroutine per reconcile interval. Guarded by b.mu.
	deleteInFlight bool
	// released is set once VM_RELEASED has been emitted for this MicroVM, so
	// a retried ReleaseVM does not emit it twice.
	released           bool
	createdAt, updated time.Time
}

// leaseState is one outstanding Lease.
type leaseState struct {
	rec LeaseRecord
}

// dropLeaseLocked forgets a Lease and everything the control loop recorded
// against it, so that nothing outlives the Lease that owned it. Every path
// that ends a Lease goes through it: expiry, release, the deletion of the
// MicroVM and shutdown.
func (b *Battery) dropLeaseLocked(leaseID string) {
	ls, ok := b.leases[leaseID]
	if !ok {
		return
	}
	delete(b.leases, leaseID)
	if ps, ok := b.pools[keyOf(ls.rec.Pool)]; ok {
		delete(ps.warned, leaseID)
	}
}

// counts is a Pool's population by phase.
type counts struct {
	available, leased, provisioning, quarantined int32
}

func (c counts) status() poolStatus {
	return poolStatus{Available: c.available, Leased: c.leased, Provisioning: c.provisioning, Quarantined: c.quarantined}
}

// countsLocked summarises a Pool's MicroVMs the way battery's CountVMs does:
// PRE_LEASE_HOOK_RUNNING counts as leased, CREATE_HOOK_RUNNING as
// provisioning, and DELETING and FAILED count nowhere.
func (b *Battery) countsLocked(key poolKey) counts {
	var c counts
	for _, vm := range b.vms {
		if vm.pool != key {
			continue
		}
		switch vm.phase {
		case poolmgrv1.VMPhase_AVAILABLE:
			c.available++
		case poolmgrv1.VMPhase_LEASED, poolmgrv1.VMPhase_PRE_LEASE_HOOK_RUNNING:
			c.leased++
		case poolmgrv1.VMPhase_PROVISIONING, poolmgrv1.VMPhase_CREATE_HOOK_RUNNING:
			c.provisioning++
		case poolmgrv1.VMPhase_QUARANTINED:
			c.quarantined++
		case poolmgrv1.VMPhase_DELETING, poolmgrv1.VMPhase_FAILED:
		}
	}
	return c
}

// poolLocked resolves ref or returns NOT_FOUND.
func (b *Battery) poolLocked(ref PoolRef) (*poolState, error) {
	ps, ok := b.pools[keyOf(ref)]
	if !ok {
		return nil, errNotFound("pool %s not found", ref)
	}
	return ps, nil
}

// aliveLocked reports whether vm is still tracked and its Pool still exists.
// Provisioning re-checks it after every Host call, because the Pool may have
// been deleted or the fake stopped in the meantime.
func (b *Battery) aliveLocked(vm *vmState) bool {
	if _, ok := b.vms[vm.id]; !ok {
		return false
	}
	_, ok := b.pools[vm.pool]
	return ok
}

// oldestAvailableLocked picks the AVAILABLE MicroVM of a Pool that has been
// available longest, for deterministic claims.
func (b *Battery) oldestAvailableLocked(key poolKey) *vmState {
	var best *vmState
	for _, vm := range b.vms {
		if vm.pool != key || vm.phase != poolmgrv1.VMPhase_AVAILABLE {
			continue
		}
		if best == nil || vm.updated.Before(best.updated) || (vm.updated.Equal(best.updated) && vm.id < best.id) {
			best = vm
		}
	}
	return best
}

// pickHostLocked chooses the Host for a new MicroVM in ps. Only names the
// Hosts know are candidates. PlacementLeastVMs picks the candidate with the
// fewest MicroVMs of this Pool that are not DELETING or FAILED, ties broken
// by the order of flintlock_hosts, which is battery's PickHost;
// PlacementRoundRobin cycles through the candidates.
func (b *Battery) pickHostLocked(ps *poolState) (string, error) {
	var candidates []string
	for _, name := range ps.spec.FlintlockHosts {
		if b.cfg.Hosts.known(name) {
			candidates = append(candidates, name)
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("pool %s has no known host among flintlock_hosts %v: %w", ps.key, ps.spec.FlintlockHosts, errUnknownHost)
	}
	if b.cfg.Placement == PlacementRoundRobin {
		host := candidates[ps.rr%len(candidates)]
		ps.rr++
		return host, nil
	}
	perHost := make(map[string]int, len(candidates))
	for _, vm := range b.vms {
		if vm.pool != ps.key {
			continue
		}
		switch vm.phase {
		case poolmgrv1.VMPhase_DELETING, poolmgrv1.VMPhase_FAILED:
			continue
		default:
			perHost[vm.host]++
		}
	}
	best := candidates[0]
	for _, h := range candidates[1:] {
		if perHost[h] < perHost[best] {
			best = h
		}
	}
	return best, nil
}

// vmRecordsLocked converts every MicroVM to its record, in creation order.
func (b *Battery) vmRecordsLocked() []VMRecord {
	vms := make([]*vmState, 0, len(b.vms))
	for _, vm := range b.vms {
		vms = append(vms, vm)
	}
	slices.SortFunc(vms, func(x, y *vmState) int { return cmp.Compare(x.id, y.id) })
	out := make([]VMRecord, 0, len(vms))
	for _, vm := range vms {
		out = append(out, VMRecord{
			UID:       vm.uid,
			Pool:      vm.pool.ref(),
			Host:      vm.host,
			Phase:     vm.phase,
			LeaseID:   vm.leaseID,
			CreatedAt: vm.createdAt,
			UpdatedAt: vm.updated,
		})
	}
	return out
}

func sortLeases(ls []LeaseRecord) {
	slices.SortFunc(ls, func(x, y LeaseRecord) int {
		if c := x.ClaimedAt.Compare(y.ClaimedAt); c != 0 {
			return c
		}
		return strings.Compare(x.LeaseID, y.LeaseID)
	})
}

// newLeaseID returns an opaque lease token.
func newLeaseID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails.
	return hex.EncodeToString(b[:])
}

// newVMID returns a MicroVM id for a Pool, shaped as battery v0.3.3 shapes
// it: the Pool's name, a dash and eight random hex digits.
func newVMID(pool string) string {
	var b [4]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails.
	return pool + "-" + hex.EncodeToString(b[:])
}
