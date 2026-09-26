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
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/clock"
)

// The reseed wait of PO-036 and PO-037 (03-pools.md#refill). It starts at
// one minute, well above the moment a Pool is stalled between a claim or a
// deletion and the start of its replacement, and far below the hours a
// stuck Pool would otherwise wait. It doubles after each reseed up to 30
// minutes, so a Pool whose provisioning keeps failing, as when flintlockd
// refuses its template, costs battery one UpdatePool and one round of
// creates every 30 minutes.
const (
	poolReseedWait    = time.Minute
	poolReseedMaxWait = 30 * time.Minute
)

// poolReseeds is the Pool Controller's memory of its stalled Pools, for the
// workaround in poolReseed. It lives as long as the Operator's process and
// is shared by every reconcile; a restart of the Operator loses it, which
// only starts each wait again.
type poolReseeds struct {
	backoff clock.Backoff

	mu    sync.Mutex
	pools map[types.NamespacedName]*poolReseedState
}

// poolReseedState is one Pool's wait.
type poolReseedState struct {
	// shortfall is the Pool's shortfall the wait is for; a change starts
	// the wait again (PO-037).
	shortfall int32
	// reseeds is how many times poolReseed has sent the Pool's spec to
	// battery for this shortfall, which sets the wait (PO-037).
	reseeds int
	// stalledSince is when the Pool Controller first saw the Pool stalled,
	// zero while it is not.
	stalledSince time.Time
	// lastSent is when the Pool Controller last sent the Pool's spec to
	// battery, for a reseed or a change, which starts battery's reconciler
	// again.
	lastSent time.Time
}

func newPoolReseeds() *poolReseeds {
	//= docs/requirements/03-pools.md#refill
	//# The Pool Controller SHALL make a Pool's reseed wait one
	//# minute, double it after each `UpdatePool` of PO-036 up to 30 minutes,
	//# and set it back to one minute when the Pool's shortfall changes or
	//# reaches zero.
	return &poolReseeds{
		backoff: clock.Exponential{Base: poolReseedWait, Max: poolReseedMaxWait},
		pools:   map[types.NamespacedName]*poolReseedState{},
	}
}

// forget drops a Pool's wait: the Pool has no shortfall, or poolReseed does
// not apply to it.
func (r *poolReseeds) forget(key types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pools, key)
}

// observe records what one reconcile saw of a Pool with a shortfall: the
// shortfall, whether the Pool is stalled, and whether the reconcile sent
// the Pool's spec to battery already. It answers whether the reseed is due
// and, when it is not, how long until the Pool Controller has to look
// again.
func (r *poolReseeds) observe(key types.NamespacedName, shortfall int32, stalled, sent bool,
	now time.Time) (due bool, after time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.pools[key]
	if st == nil || st.shortfall != shortfall {
		st = &poolReseedState{shortfall: shortfall}
		r.pools[key] = st
	}
	if sent {
		st.lastSent = now
	}
	wait := r.backoff.Next(st.reseeds)
	if !stalled {
		st.stalledSince = time.Time{}
		return false, wait
	}
	if st.stalledSince.IsZero() {
		st.stalledSince = now
	}
	start := st.stalledSince
	if st.lastSent.After(start) {
		start = st.lastSent
	}
	if dueAt := start.Add(wait); now.Before(dueAt) {
		return false, dueAt.Sub(now)
	}
	return true, 0
}

// reseeded records a reseed of the Pool at now and returns the next wait.
func (r *poolReseeds) reseeded(key types.NamespacedName, now time.Time) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.pools[key]
	if st == nil {
		return r.backoff.Next(0)
	}
	st.reseeds++
	st.lastSent = now
	return r.backoff.Next(st.reseeds)
}

// poolShortfall is the Pool's shortfall (03-pools.md#refill), from
// battery's counts in the scope, and whether the Pool's strategy is one
// battery tops up only when its reconciler starts and on its events
// (BA-076). While nothing is provisioning, the shortfall is what battery
// v0.3.3 would provision for the Pool if its reconciler started now
// (BA-075). It leaves the provisioning MicroVMs out, so that a reseed's
// own provisioning, which fails if the Pool is still stuck, does not look
// like progress and set the wait back.
func poolShortfall(s *poolScope) (int32, bool) {
	st := s.held.Status
	switch s.Pool.Spec.Replenishment.Type {
	case batteryv1alpha1.ReplenishImmediateOnLease:
		return s.Pool.Spec.Size - st.Available, true
	case batteryv1alpha1.ReplenishReplaceOnDelete:
		return s.Pool.Spec.Size - (st.Available + st.Leased), true
	default:
		return 0, false
	}
}

// poolReseed works around battery v0.3.3 seeding an IMMEDIATE_ON_LEASE or
// REPLACE_ON_DELETE Pool only once per reconciler, whether or not the
// seed's provisions succeed (BA-075), and topping it up after that only on
// a claim or a deletion (BA-076). A Pool whose seed fails, or whose
// replacement fails, then stays short for good. poolReseed notices such a
// stalled Pool and, once it has made no progress for the reseed wait,
// sends battery the Pool's unchanged spec with UpdatePool. battery v0.3.3
// restarts the Pool's reconciler on every UpdatePool, even with an
// unchanged spec, and the new reconciler seeds the Pool again (BA-074).
// battery does not promise that restart; this relies on it.
//
// Remove poolReseed, and PO-035 to PO-038, once battery retries a failed
// seed on its tick.
type poolReseed struct{}

func (poolReseed) Reconcile(ctx context.Context, s *poolScope) (poolNext, error) {
	if s.Reseeds == nil {
		return poolContinue, nil
	}
	key := client.ObjectKeyFromObject(s.Pool)
	if s.refusal != nil || s.held == nil || len(s.hosts) == 0 ||
		s.Pool.Generation != s.Pool.Status.ObservedGeneration {
		s.Reseeds.forget(key)
		return poolContinue, nil
	}
	shortfall, eventDriven := poolShortfall(s)
	if !eventDriven || shortfall <= 0 {
		s.Reseeds.forget(key)
		return poolContinue, nil
	}

	//= docs/requirements/03-pools.md#refill
	//# The Pool Controller SHALL treat a Pool as stalled while its
	//# replenishment strategy is `ImmediateOnLease` or `ReplaceOnDelete`,
	//# battery holds the Pool and has accepted its current spec, its selector
	//# matches a Host, battery reports no provisioning MicroVM in it, and its
	//# shortfall is greater than zero.
	stalled := s.held.Status.Provisioning == 0
	now := s.Clock.Now()
	due, after := s.Reseeds.observe(key, shortfall, stalled, s.sent, now)
	if !due {
		//= docs/requirements/03-pools.md#refill
		//# While a Pool's shortfall is greater than zero and its
		//# replenishment strategy is `ImmediateOnLease` or `ReplaceOnDelete`, the
		//# Pool Controller SHALL reconcile the Pool again no later than the end of
		//# its reseed wait.
		s.requeueAfter(after)
		return poolContinue, nil
	}

	//= docs/requirements/03-pools.md#refill
	//# When a Pool has been stalled for its reseed wait, counted
	//# from the later of the time the Pool Controller first saw it stalled and
	//# the Pool Controller's last `UpdatePool` for it, the Pool Controller SHALL
	//# send battery the Pool's unchanged spec with `UpdatePool`.
	ref := poolRef(s.Pool)
	held, err := s.Battery.UpdatePool(ctx, poolSpecToBattery(s.Pool, s.hosts))
	if err != nil {
		return poolStop, fmt.Errorf("sending Pool %s to battery again to seed it: %w", ref, err)
	}
	s.held = held
	next := s.Reseeds.reseeded(key, now)
	s.requeueAfter(next)
	s.Log.Info("Sent unchanged Pool to battery to seed it again", "pool", ref.String(),
		"shortfall", shortfall, "nextWait", next.String())
	return poolContinue, nil
}
