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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
)

// stallHarness runs the Pool Controller's chain for one Pool again and
// again, as the workqueue would, against the stub battery, with one fake
// clock and one poolStalls across the reconciles.
type stallHarness struct {
	t      *testing.T
	b      *stubBattery
	clk    *clock.Fake
	stalls *poolStalls
	pool   *batteryv1alpha1.Pool
	ref    battery.PoolRef
}

func newStallHarness(t *testing.T, pool *batteryv1alpha1.Pool) *stallHarness {
	return &stallHarness{
		t:      t,
		b:      newStubBattery(),
		clk:    clock.NewFake(poolTestEpoch),
		stalls: newPoolStalls(),
		pool:   pool,
		ref:    poolRef(pool),
	}
}

// reconcile runs the chain once, against hosts, and returns the Result it
// asks for.
func (h *stallHarness) reconcile(hosts ...string) ctrl.Result {
	h.t.Helper()
	s := newTestPoolScope(h.pool.DeepCopy(), h.b, hosts...)
	s.Clock = h.clk
	s.Stalls = h.stalls
	res, err := poolChain().Run(context.Background(), s)
	if err != nil {
		h.t.Fatalf("chain: %v", err)
	}
	h.pool = s.Object
	return res.Ctrl()
}

// setStatus sets the counts battery reports for the Pool.
func (h *stallHarness) setStatus(st battery.PoolStatus) {
	h.b.mu.Lock()
	defer h.b.mu.Unlock()
	h.b.status[h.ref] = st
}

// updates counts the UpdatePool calls battery has had.
func (h *stallHarness) updates() int {
	n := 0
	for _, c := range h.b.Calls() {
		if strings.HasPrefix(c, "UpdatePool ") {
			n++
		}
	}
	return n
}

// wantStep advances the clock by d, reconciles, and checks the reason of
// Ready and the requeue the reconcile asked for.
func (h *stallHarness) wantStep(d time.Duration, reason string, requeue time.Duration) {
	h.t.Helper()
	h.clk.Advance(d)
	res := h.reconcile(testHostA)
	at := h.clk.Now().Sub(poolTestEpoch)
	if got := readyCondition(h.pool); got == nil || got.Reason != reason {
		h.t.Fatalf("after %s: Ready = %+v, want the reason %s", at, got, reason)
	}
	if res.RequeueAfter != requeue {
		h.t.Fatalf("after %s: RequeueAfter = %s, want %s", at, res.RequeueAfter, requeue)
	}
}

// wantMessage checks the message of Ready.
func (h *stallHarness) wantMessage(want string) {
	h.t.Helper()
	if got := readyCondition(h.pool).Message; got != want {
		h.t.Fatalf("Ready message = %q, want %q", got, want)
	}
}

// remembered reports whether poolStalls still holds the Pool.
func (h *stallHarness) remembered() bool {
	h.stalls.mu.Lock()
	defer h.stalls.mu.Unlock()
	_, ok := h.stalls.pools[client.ObjectKeyFromObject(h.pool)]
	return ok
}

//= docs/requirements/03-pools.md#refill
//= type=test
//# The Pool Controller SHALL treat a Pool as stalled while its
//# replenishment strategy is `ImmediateOnLease` or `ReplaceOnDelete`,
//# battery holds the Pool and has accepted its current spec, its selector
//# matches a Host, battery reports no provisioning MicroVM in it, and its
//# shortfall is greater than zero.

//= docs/requirements/03-pools.md#refill
//= type=test
//# While the Pool Controller has seen a Pool stalled with its
//# current shortfall at two reconciles at least one minute apart, and the
//# sum of the Pool's available, leased and provisioning MicroVMs is less
//# than its size, the Pool Controller SHALL set the Pool's condition `Ready`
//# false with the reason `NotFilling`.

//= docs/requirements/03-pools.md#refill
//= type=test
//# The Pool Controller SHALL give the condition `Ready` with the
//# reason `NotFilling` a message that gives the Pool's shortfall, the time
//# the Pool Controller first saw the Pool stalled with it, and the log of
//# the battery container as the place to find the cause.

//= docs/requirements/03-pools.md#refill
//= type=test
//# While a Pool is stalled and PO-045 does not yet hold for its
//# shortfall, the Pool Controller SHALL reconcile the Pool again no later
//# than one minute after it first saw the Pool stalled with that shortfall.

// TestAStalledPoolSaysNotFillingAfterAMinute covers PO-035, PO-045,
// PO-046 and PO-047: a Pool battery holds with nothing in it and nothing
// provisioning is reconciled again when a minute has passed, says
// NotFilling once it is still stalled then, keeps the reason while battery
// provisions for the next attempt, starts again when the shortfall
// changes, and is forgotten when it fills. The Pool Controller sends
// battery nothing for it.
func TestAStalledPoolSaysNotFillingAfterAMinute(t *testing.T) {
	const cause = "battery does not report why provisioning fails: see the log of the battery container"
	for _, typ := range []batteryv1alpha1.ReplenishmentStrategyType{
		batteryv1alpha1.ReplenishImmediateOnLease, batteryv1alpha1.ReplenishReplaceOnDelete,
	} {
		t.Run(string(typ), func(t *testing.T) {
			pool := finalizedPool()
			pool.Spec.Replenishment.Type = typ
			h := newStallHarness(t, pool)

			// Stalled from the start: come back when the minute ends.
			h.wantStep(0, PoolReasonBelowSize, time.Minute)
			h.wantStep(59*time.Second, PoolReasonBelowSize, time.Second)

			// Still stalled a minute later, with the same shortfall.
			h.wantStep(time.Second, PoolReasonNotFilling, 0)
			h.wantMessage("The Pool has been 3 MicroVMs short since 2026-09-24T12:00:00Z. " + cause)

			// battery provisions for the next attempt: the reason stays, and
			// so does the time.
			h.setStatus(battery.PoolStatus{Provisioning: 1})
			h.wantStep(time.Second, PoolReasonNotFilling, 0)
			h.wantMessage("The Pool has been 3 MicroVMs short since 2026-09-24T12:00:00Z. " + cause)

			// Provisioning makes up the size: Ready is true, as PO-022 says.
			h.setStatus(battery.PoolStatus{Provisioning: 3})
			h.wantStep(time.Second, PoolReasonAtSize, 0)

			// The provisioning fails: the shortfall is as it was, so the Pool
			// is not filling at once, not a minute later.
			h.setStatus(battery.PoolStatus{})
			h.wantStep(time.Second, PoolReasonNotFilling, 0)
			h.wantMessage("The Pool has been 3 MicroVMs short since 2026-09-24T12:00:00Z. " + cause)

			// One MicroVM became available: the shortfall changed, and a
			// minute must pass again.
			h.setStatus(battery.PoolStatus{Available: 1})
			h.wantStep(time.Second, PoolReasonBelowSize, time.Minute)
			h.wantStep(time.Minute-time.Second, PoolReasonBelowSize, time.Second)
			h.wantStep(time.Second, PoolReasonNotFilling, 0)
			h.wantMessage("The Pool has been 2 MicroVMs short since 2026-09-24T12:01:04Z. " + cause)

			// The Pool filled: nothing more to wait for, and nothing kept.
			h.setStatus(battery.PoolStatus{Available: 3})
			h.wantStep(time.Hour, PoolReasonAtSize, 0)
			if h.remembered() {
				t.Error("the Pool's sightings are still remembered once the Pool is full")
			}
			if n := h.updates(); n != 0 {
				t.Errorf("%d UpdatePool calls, want none: the Pool Controller sends battery nothing for a stalled Pool", n)
			}
		})
	}
}

//= docs/requirements/03-pools.md#refill
//= type=test
//# While the Pool Controller has seen a Pool stalled with its
//# current shortfall at two reconciles at least one minute apart, and the
//# sum of the Pool's available, leased and provisioning MicroVMs is less
//# than its size, the Pool Controller SHALL set the Pool's condition `Ready`
//# false with the reason `NotFilling`.

// TestASightingOfProvisioningDoesNotStartTheMinuteAgain covers PO-045
// against an attempt that fails slowly: a reconcile between the two
// stalled sightings that finds battery provisioning neither counts nor
// starts the minute again, and asks for no requeue, since battery's
// events bring the Pool back when the attempt ends.
func TestASightingOfProvisioningDoesNotStartTheMinuteAgain(t *testing.T) {
	h := newStallHarness(t, finalizedPool())
	h.wantStep(0, PoolReasonBelowSize, time.Minute)
	h.setStatus(battery.PoolStatus{Provisioning: 1})
	h.wantStep(30*time.Second, PoolReasonBelowSize, 0)
	h.setStatus(battery.PoolStatus{})
	h.wantStep(30*time.Second, PoolReasonNotFilling, 0)
	h.wantMessage("The Pool has been 3 MicroVMs short since 2026-09-24T12:00:00Z. " +
		"battery does not report why provisioning fails: see the log of the battery container")
}

// TestAPoolThatIsNotStalledIsLeftAlone covers PO-035: a Pool that
// battery is filling, a full Pool, a MinSizeThreshold Pool, a
// ReplaceOnDelete Pool whose MicroVMs are leased, and a Pool battery
// refuses or that no Host can run, are never reported as not filling, and
// never asked to come back for it.
func TestAPoolThatIsNotStalledIsLeftAlone(t *testing.T) {
	cases := []struct {
		name   string
		change func(*batteryv1alpha1.Pool)
		status battery.PoolStatus
		hosts  []string
	}{{
		name:   "provisioning",
		status: battery.PoolStatus{Available: 1, Provisioning: 1},
		hosts:  []string{testHostA},
	}, {
		name:   "full",
		status: battery.PoolStatus{Available: 3},
		hosts:  []string{testHostA},
	}, {
		name: "MinSizeThreshold",
		change: func(p *batteryv1alpha1.Pool) {
			p.Spec.Replenishment = batteryv1alpha1.ReplenishmentStrategy{
				Type: batteryv1alpha1.ReplenishMinSizeThreshold, MinSize: ptrTo(int32(1)),
			}
		},
		hosts: []string{testHostA},
	}, {
		name: "ReplaceOnDelete with every MicroVM leased",
		change: func(p *batteryv1alpha1.Pool) {
			p.Spec.Replenishment.Type = batteryv1alpha1.ReplenishReplaceOnDelete
		},
		status: battery.PoolStatus{Leased: 3},
		hosts:  []string{testHostA},
	}, {
		name: "no Host",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := finalizedPool()
			if tc.change != nil {
				tc.change(pool)
			}
			h := newStallHarness(t, pool)
			h.setStatus(tc.status)
			for range 3 {
				res := h.reconcile(tc.hosts...)
				if res.RequeueAfter != 0 {
					t.Fatalf("RequeueAfter = %s, want none", res.RequeueAfter)
				}
				if ready := readyCondition(h.pool); ready.Reason == PoolReasonNotFilling {
					t.Fatalf("Ready = %+v, want not NotFilling", ready)
				}
				h.clk.Advance(time.Hour)
			}
			if n := h.updates(); n != 0 {
				t.Errorf("%d UpdatePool calls, want none", n)
			}
			if h.remembered() {
				t.Error("the Pool is remembered as stalled")
			}
		})
	}

	t.Run("refused", func(t *testing.T) {
		h := newStallHarness(t, finalizedPool())
		h.wantStep(0, PoolReasonBelowSize, time.Minute)
		h.b.mu.Lock()
		h.b.updateErr = fmt.Errorf("%w: bad template", battery.ErrInvalid)
		h.b.mu.Unlock()
		// A new generation that battery refuses: the refusal stands, and
		// the Pool is no longer one the Pool Controller watches for a
		// stall.
		h.pool.Generation = 2
		for range 3 {
			h.wantStep(time.Hour, PoolReasonRejected, 0)
			h.pool.Status.ObservedGeneration = 1
		}
		if h.remembered() {
			t.Error("a refused Pool is remembered as stalled")
		}
	})
}

// wantPoolReady checks the reason, and the message where want is not empty,
// of the Pool's Ready condition.
func wantPoolReady(t *testing.T, pool *batteryv1alpha1.Pool, status metav1.ConditionStatus, reason, message string) {
	t.Helper()
	ready := readyCondition(pool)
	if ready == nil {
		t.Fatalf("no Ready condition; want %s %s", status, reason)
	}
	if ready.Status != status || ready.Reason != reason {
		t.Fatalf("Ready = %s %s (%q), want %s %s", ready.Status, ready.Reason, ready.Message, status, reason)
	}
	if message != "" && ready.Message != message {
		t.Fatalf("Ready message = %q, want %q", ready.Message, message)
	}
}
