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

package controller

import (
	"context"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/reconcile"
)

// poolStallWait is how long a Pool has to stay stalled with one shortfall
// before the Pool Controller says it is not filling (PO-045): about six of
// battery's ticks, each of which provisions the shortfall again (BA-075).
// It is well above the moment a Pool is stalled between a claim or a
// deletion and the start of its replacement.
const poolStallWait = time.Minute

// poolStalls is the Pool Controller's memory of its stalled Pools, for
// poolStall. It lives as long as the Operator's process and is shared by
// every reconcile; a restart of the Operator loses it, which only starts
// each wait again.
type poolStalls struct {
	mu    sync.Mutex
	pools map[types.NamespacedName]*poolStallState
}

// poolStallState is what the Pool Controller has seen of one Pool with a
// shortfall.
type poolStallState struct {
	// shortfall is the Pool's shortfall the sightings are for; a change
	// starts them again.
	shortfall int32
	// since is when the Pool Controller first saw the Pool stalled with
	// this shortfall, which is when the state was made.
	since time.Time
	// notFilling is set once the Pool Controller has seen the Pool stalled
	// with this shortfall a minute or more after since (PO-045). It stays
	// while battery provisions, and goes with the rest of the state when
	// the shortfall changes.
	notFilling bool
}

// poolNotFilling is what poolStall tells poolReadiness about a Pool that
// is not filling (PO-045, PO-046).
type poolNotFilling struct {
	// shortfall is the Pool's shortfall.
	shortfall int32
	// since is when the Pool Controller first saw the Pool stalled with it.
	since time.Time
}

func newPoolStalls() *poolStalls {
	return &poolStalls{pools: map[types.NamespacedName]*poolStallState{}}
}

// forget drops a Pool's sightings: the Pool has no shortfall, or poolStall
// does not apply to it.
func (r *poolStalls) forget(key types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pools, key)
}

// observe records what one reconcile saw of a Pool with a shortfall: the
// shortfall, and whether the Pool is stalled. It answers whether the Pool
// is not filling and, while it is stalled and not yet known not to fill,
// how long until the Pool Controller has to look again.
func (r *poolStalls) observe(key types.NamespacedName, shortfall int32, stalled bool,
	now time.Time) (nf *poolNotFilling, after time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.pools[key]
	if st != nil && st.shortfall != shortfall {
		delete(r.pools, key)
		st = nil
	}
	//= docs/requirements/03-pools.md#refill
	//# While the Pool Controller has seen a Pool stalled with its
	//# current shortfall at two reconciles at least one minute apart, and the
	//# sum of the Pool's available, leased and provisioning MicroVMs is less
	//# than its size, the Pool Controller SHALL set the Pool's condition `Ready`
	//# false with the reason `NotFilling`.
	//
	// A sighting that finds battery provisioning neither counts nor
	// starts the minute again: the shortfall leaves the provisioning
	// MicroVMs out, so an attempt that fails changes nothing.
	if st == nil {
		if !stalled {
			return nil, 0
		}
		st = &poolStallState{shortfall: shortfall, since: now}
		r.pools[key] = st
	}
	if stalled && !now.Before(st.since.Add(poolStallWait)) {
		st.notFilling = true
	}
	if st.notFilling {
		return &poolNotFilling{shortfall: st.shortfall, since: st.since}, 0
	}
	if stalled {
		return nil, st.since.Add(poolStallWait).Sub(now)
	}
	return nil, 0
}

// poolShortfall is the Pool's shortfall (03-pools.md#refill), from
// battery's counts in the scope, and whether the Pool's strategy is one
// the shortfall is defined for. It is what battery provisions for the
// Pool on each tick, less what it is provisioning already (BA-075). It
// leaves the provisioning MicroVMs out, so that an attempt that fails
// does not look like progress.
func poolShortfall(s *poolScope) (int32, bool) {
	st := s.held.Status
	switch s.Object.Spec.Replenishment.Type {
	case batteryv1alpha1.ReplenishImmediateOnLease:
		return s.Object.Spec.Size - st.Available, true
	case batteryv1alpha1.ReplenishReplaceOnDelete:
		return s.Object.Spec.Size - (st.Available + st.Leased), true
	default:
		return 0, false
	}
}

// poolStall notices a Pool that battery is not filling. battery v0.4.0
// provisions a Pool's shortfall on every tick (BA-075), so the Pool
// Controller sends it nothing for such a Pool; but battery reports a
// failed provision nowhere the Operator can read, so poolStall tells
// poolReadiness once a Pool has been stalled with the same shortfall for
// a minute, and the Ready condition says so (PO-045, PO-046). Refactor
// the NotFilling reason once battery reports why a provision fails.
//
// Until #242, poolReseed stood here and sent battery the Pool's unchanged
// spec with UpdatePool, to make battery v0.3.3 seed the Pool again
// (PO-036 to PO-038, withdrawn).
type poolStall struct{}

func (poolStall) Reconcile(_ context.Context, s *poolScope) (reconcile.Result, error) {
	if s.Stalls == nil {
		return reconcile.Result{}, nil
	}
	key := client.ObjectKeyFromObject(s.Object)
	if s.refusal != nil || s.held == nil || len(s.hosts) == 0 ||
		s.Object.Generation != s.Object.Status.ObservedGeneration {
		s.Stalls.forget(key)
		return reconcile.Result{}, nil
	}
	shortfall, eventDriven := poolShortfall(s)
	if !eventDriven || shortfall <= 0 {
		s.Stalls.forget(key)
		return reconcile.Result{}, nil
	}

	//= docs/requirements/03-pools.md#refill
	//# The Pool Controller SHALL treat a Pool as stalled while its
	//# replenishment strategy is `ImmediateOnLease` or `ReplaceOnDelete`,
	//# battery holds the Pool and has accepted its current spec, its selector
	//# matches a Host, battery reports no provisioning MicroVM in it, and its
	//# shortfall is greater than zero.
	stalled := s.held.Status.Provisioning == 0
	nf, after := s.Stalls.observe(key, shortfall, stalled, s.Clock.Now())
	s.notFilling = nf
	if after > 0 {
		//= docs/requirements/03-pools.md#refill
		//# While a Pool is stalled and PO-045 does not yet hold for its
		//# shortfall, the Pool Controller SHALL reconcile the Pool again no later
		//# than one minute after it first saw the Pool stalled with that shortfall.
		return reconcile.Result{RequeueAfter: after}, nil
	}
	return reconcile.Result{}, nil
}
