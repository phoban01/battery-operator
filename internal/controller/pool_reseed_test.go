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
	"reflect"
	"strings"
	"testing"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/fakebattery"
	"github.com/phoban01/battery-operator/internal/fakeflintlock"
)

// reseedHarness runs the Pool Controller's chain for one Pool again and
// again, as the workqueue would, against the stub battery, with one fake
// clock and one poolReseeds across the reconciles.
type reseedHarness struct {
	t       *testing.T
	b       *stubBattery
	clk     *clock.Fake
	reseeds *poolReseeds
	pool    *batteryv1alpha1.Pool
	ref     battery.PoolRef
}

func newReseedHarness(t *testing.T, pool *batteryv1alpha1.Pool) *reseedHarness {
	return &reseedHarness{
		t:       t,
		b:       newStubBattery(),
		clk:     clock.NewFake(poolTestEpoch),
		reseeds: newPoolReseeds(),
		pool:    pool,
		ref:     poolRef(pool),
	}
}

// reconcile runs the chain once and returns the Result it asks for.
func (h *reseedHarness) reconcile() ctrl.Result {
	h.t.Helper()
	s := newTestPoolScope(h.pool.DeepCopy(), h.b, testHostA)
	s.Clock = h.clk
	s.Reseeds = h.reseeds
	if err := runPoolChain(context.Background(), s, poolChain()); err != nil {
		h.t.Fatalf("chain: %v", err)
	}
	h.pool = s.Pool
	return s.Result
}

// setStatus sets the counts battery reports for the Pool.
func (h *reseedHarness) setStatus(st battery.PoolStatus) {
	h.b.mu.Lock()
	defer h.b.mu.Unlock()
	h.b.status[h.ref] = st
}

// updates counts the UpdatePool calls battery has had.
func (h *reseedHarness) updates() int {
	n := 0
	for _, c := range h.b.Calls() {
		if strings.HasPrefix(c, "UpdatePool ") {
			n++
		}
	}
	return n
}

// wantStep advances the clock by d, reconciles, and checks the UpdatePool
// calls so far and the requeue the reconcile asked for.
func (h *reseedHarness) wantStep(d time.Duration, updates int, requeue time.Duration) {
	h.t.Helper()
	h.clk.Advance(d)
	res := h.reconcile()
	if got := h.updates(); got != updates {
		h.t.Fatalf("after %s: %d UpdatePool calls, want %d", h.clk.Now().Sub(poolTestEpoch), got, updates)
	}
	if res.RequeueAfter != requeue {
		h.t.Fatalf("after %s: RequeueAfter = %s, want %s", h.clk.Now().Sub(poolTestEpoch), res.RequeueAfter, requeue)
	}
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
//# When a Pool has been stalled for its reseed wait, counted
//# from the later of the time the Pool Controller first saw it stalled and
//# the Pool Controller's last `UpdatePool` for it, the Pool Controller SHALL
//# send battery the Pool's unchanged spec with `UpdatePool`.

//= docs/requirements/03-pools.md#refill
//= type=test
//# While a Pool's shortfall is greater than zero and its
//# replenishment strategy is `ImmediateOnLease` or `ReplaceOnDelete`, the
//# Pool Controller SHALL reconcile the Pool again no later than the end of
//# its reseed wait.

// TestAStalledPoolIsSentToBatteryAgain covers PO-035, PO-036 and PO-038:
// a Pool battery holds with nothing in it and nothing provisioning is
// sent to battery again, unchanged, once it has been stalled for a
// minute, and each reconcile asks to come back when the wait ends.
func TestAStalledPoolIsSentToBatteryAgain(t *testing.T) {
	for _, typ := range []batteryv1alpha1.ReplenishmentStrategyType{
		batteryv1alpha1.ReplenishImmediateOnLease, batteryv1alpha1.ReplenishReplaceOnDelete,
	} {
		t.Run(string(typ), func(t *testing.T) {
			pool := finalizedPool()
			pool.Spec.Replenishment.Type = typ
			h := newReseedHarness(t, pool)

			// CreatePool starts battery's reconciler, so the wait counts
			// from it.
			h.wantStep(0, 0, time.Minute)
			created, _ := h.b.spec(h.ref)
			h.wantStep(59*time.Second, 0, time.Second)
			h.wantStep(time.Second, 1, 2*time.Minute)

			sent, _ := h.b.spec(h.ref)
			if !reflect.DeepEqual(sent, created) {
				t.Errorf("UpdatePool sent %+v, want the unchanged spec %+v", sent, created)
			}
			if h.pool.Status.ObservedGeneration != h.pool.Generation {
				t.Errorf("observedGeneration = %d, want %d", h.pool.Status.ObservedGeneration, h.pool.Generation)
			}
		})
	}
}

//= docs/requirements/03-pools.md#refill
//= type=test
//# The Pool Controller SHALL make a Pool's reseed wait one
//# minute, double it after each `UpdatePool` of PO-036 up to 30 minutes,
//# and set it back to one minute when the Pool's shortfall changes or
//# reaches zero.

// TestAStalledPoolBacksOff covers PO-037: the wait doubles after each
// UpdatePool up to 30 minutes, battery provisioning in between does not
// shorten it, and a change in the shortfall sets it back to a minute.
func TestAStalledPoolBacksOff(t *testing.T) {
	h := newReseedHarness(t, finalizedPool())
	h.wantStep(0, 0, time.Minute)

	waits := []time.Duration{
		time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
		16 * time.Minute, 30 * time.Minute, 30 * time.Minute,
	}
	for i, wait := range waits {
		next := 30 * time.Minute
		if i+1 < len(waits) {
			next = waits[i+1]
		}
		// Nothing a second before the wait ends; the reseed at its end.
		h.wantStep(wait-time.Second, i, time.Second)
		h.wantStep(time.Second, i+1, next)
	}

	// battery provisions after the reseed, and fails: the Pool is not
	// stalled while it provisions, and the wait counts from when it is
	// stalled again, not from the reseed.
	h.setStatus(battery.PoolStatus{Provisioning: 3})
	h.wantStep(10*time.Minute, len(waits), 30*time.Minute)
	h.setStatus(battery.PoolStatus{})
	h.wantStep(time.Minute, len(waits), 30*time.Minute)
	h.wantStep(30*time.Minute-time.Second, len(waits), time.Second)
	h.wantStep(time.Second, len(waits)+1, 30*time.Minute)

	// One MicroVM became available: the shortfall changed, and the wait is
	// back to a minute.
	h.setStatus(battery.PoolStatus{Available: 1})
	h.wantStep(time.Second, len(waits)+1, time.Minute)
	h.wantStep(time.Minute, len(waits)+2, 2*time.Minute)

	// The Pool filled: nothing more to wait for.
	h.setStatus(battery.PoolStatus{Available: 3})
	h.wantStep(time.Hour, len(waits)+2, 0)
	h.reseeds.mu.Lock()
	_, remembered := h.reseeds.pools[client.ObjectKeyFromObject(h.pool)]
	h.reseeds.mu.Unlock()
	if remembered {
		t.Error("the Pool's wait is still remembered once the Pool is full")
	}
}

// TestAPoolThatIsNotStalledIsLeftAlone covers PO-035: a Pool that
// battery is filling, a full Pool, a MinSizeThreshold Pool, a
// ReplaceOnDelete Pool whose MicroVMs are leased, and a Pool battery
// refuses or that no Host can run, are never sent again.
func TestAPoolThatIsNotStalledIsLeftAlone(t *testing.T) {
	cases := []struct {
		name    string
		change  func(*batteryv1alpha1.Pool)
		status  battery.PoolStatus
		hosts   []string
		requeue time.Duration
	}{{
		name:    "provisioning",
		status:  battery.PoolStatus{Available: 1, Provisioning: 1},
		hosts:   []string{testHostA},
		requeue: time.Minute,
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
			h := newReseedHarness(t, pool)
			h.setStatus(tc.status)
			for range 3 {
				s := newTestPoolScope(h.pool.DeepCopy(), h.b, tc.hosts...)
				s.Clock = h.clk
				s.Reseeds = h.reseeds
				if err := runPoolChain(context.Background(), s, poolChain()); err != nil {
					t.Fatalf("chain: %v", err)
				}
				h.pool = s.Pool
				if s.Result.RequeueAfter != tc.requeue {
					t.Fatalf("RequeueAfter = %s, want %s", s.Result.RequeueAfter, tc.requeue)
				}
				h.clk.Advance(time.Hour)
			}
			if n := h.updates(); n != 0 {
				t.Errorf("%d UpdatePool calls, want none", n)
			}
		})
	}

	t.Run("refused", func(t *testing.T) {
		h := newReseedHarness(t, finalizedPool())
		h.wantStep(0, 0, time.Minute)
		h.b.mu.Lock()
		h.b.updateErr = fmt.Errorf("%w: bad template", battery.ErrInvalid)
		h.b.mu.Unlock()
		// A new generation that battery refuses: the one UpdatePool is
		// poolUpdate's, and no reseed follows it.
		h.pool.Generation = 2
		for range 3 {
			h.wantStep(time.Hour, 1, 0)
			h.b.mu.Lock()
			h.b.calls = nil
			h.b.mu.Unlock()
			h.pool.Status.ObservedGeneration = 1
		}
	})
}

// TestAPoolWhoseSeedFailsFillsAgainstTheFakeBattery drives the Pool
// Controller against the fake battery seeding once, as battery v0.3.3 does
// (TD-007): the Pool's first provisioning fails on the Host, the Pool
// stays empty, and the reseed of PO-036 fills it (PO-035 to PO-038).
func TestAPoolWhoseSeedFailsFillsAgainstTheFakeBattery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fl := fakeflintlock.New(fakeflintlock.Config{Name: testHostA})
	t.Cleanup(func() { _ = fl.Close() })
	flConn, err := fl.Conn()
	if err != nil {
		t.Fatalf("fake flintlockd: %v", err)
	}
	t.Cleanup(func() { _ = flConn.Close() })
	hosts := fakebattery.NewHosts()
	hosts.Add(testHostA, testHostA+":9090", flConn)
	bc, fb := startPoolFakeBatteryWith(t, fakebattery.Config{Hosts: hosts, SeedOnce: true})

	pool := finalizedPool()
	pool.Spec.Size = 2
	k8s := newPoolFakeClient(t, pool, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: testHostA}})
	clk := clock.NewFake(poolTestEpoch)
	r := &PoolReconciler{Client: k8s, Battery: bc, Hosts: newStubHosts(testHostA), Clock: clk}
	key := client.ObjectKeyFromObject(pool)
	ref := poolRef(pool)
	reconcile := func() ctrl.Result {
		t.Helper()
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		return res
	}
	events, err := bc.Subscribe(ctx, battery.EventFilter{Pool: &ref})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = events.Close() })

	// Every boot fails, so battery's seed fails; the fake seeds only once.
	fl.SetFaults(fakeflintlock.Faults{CreateFails: true})
	reconcile()
	for failed := int32(0); failed < pool.Spec.Size; {
		e, err := events.Recv(ctx)
		if err != nil {
			t.Fatalf("waiting for the seed to fail: %v", err)
		}
		if e.Type == poolmgrv1.EventType_VM_HOOK_FAILED {
			failed++
		}
	}
	eventually(t, "the failed MicroVMs deleted", func() (bool, string) {
		return len(fb.VMs()) == 0, fmt.Sprintf("%v", fb.VMs())
	})
	fl.SetFaults(fakeflintlock.Faults{})

	// The fake's ticks leave the Pool empty, and the wait is not over.
	time.Sleep(50 * time.Millisecond)
	clk.Advance(30 * time.Second)
	if res := reconcile(); res.RequeueAfter != 30*time.Second {
		t.Fatalf("RequeueAfter = %s, want the 30s left of the wait", res.RequeueAfter)
	}
	if held, err := bc.GetPool(ctx, ref); err != nil || held.Status.Available+held.Status.Provisioning != 0 {
		t.Fatalf("battery's Pool before the reseed = %+v, %v; want it empty", held, err)
	}

	// The wait ends: the reseed seeds the Pool again, and it fills.
	clk.Advance(30 * time.Second)
	if res := reconcile(); res.RequeueAfter != 2*time.Minute {
		t.Fatalf("RequeueAfter = %s, want the next wait of 2m", res.RequeueAfter)
	}
	eventually(t, "the Pool filled", func() (bool, string) {
		held, err := bc.GetPool(ctx, ref)
		if err != nil {
			return false, err.Error()
		}
		return held.Status.Available == pool.Spec.Size, fmt.Sprintf("%+v", held.Status)
	})
	if res := reconcile(); res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter for the full Pool = %s, want none", res.RequeueAfter)
	}
}
