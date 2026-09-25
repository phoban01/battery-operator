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
	"errors"
	"testing"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
)

// failSeed declares spec while every create on host fails, waits until the
// seed's reservations are dropped, and then lets the Host create again.
func (h *harness) failSeed(host *testHost, spec poolSpec) {
	h.t.Helper()
	host.set(func(s *testHost) { s.createErr = errors.New("flintlockd refused the spec") })
	if _, err := h.client.CreatePool(h.ctx, spec); err != nil {
		h.t.Fatalf("CreatePool: %v", err)
	}
	h.waitEvent(poolmgrv1.EventType_POOL_REPLENISHING)
	for len(h.b.VMs()) != 0 {
		if h.ctx.Err() != nil {
			h.t.Fatalf("the reservations of the refused creates were never dropped: %v", h.b.VMs())
		}
		time.Sleep(time.Millisecond)
	}
	host.set(func(s *testHost) { s.createErr = nil })
}

//= docs/requirements/08-test-doubles.md#fake-battery
//= type=test
//# Where a test sets `SeedOnce`, the fake battery SHALL
//# provision for a Pool whose replenishment strategy is `IMMEDIATE_ON_LEASE`
//# or `REPLACE_ON_DELETE` on its tick only on the first tick after each
//# `CreatePool` or `UpdatePool` of the Pool, whether or not those provisions
//# succeed, as battery v0.3.3 seeds such a Pool (BA-075, BA-076).

//= docs/requirements/10-battery.md#seeding
//= type=test
//# When a Pool's reconciler starts, battery SHALL provision for
//# the Pool once, whether or not those provisions succeed, its size less its
//# available and provisioning MicroVMs for `IMMEDIATE_ON_LEASE`, and its size
//# less its available, leased and provisioning MicroVMs for
//# `REPLACE_ON_DELETE`.

//= docs/requirements/10-battery.md#seeding
//= type=test
//# While a Pool's replenishment strategy is `IMMEDIATE_ON_LEASE`
//# or `REPLACE_ON_DELETE`, battery SHALL provision a MicroVM for it only when
//# its reconciler starts, on a claim for `IMMEDIATE_ON_LEASE`, and on a
//# deletion for `REPLACE_ON_DELETE`.

// TestSeedOnce covers TD-007, BA-075 and BA-076: with Config.SeedOnce, an
// event-driven Pool whose seed fails stays short however many ticks pass,
// and UpdatePool, which starts a new reconciler, seeds it again. A
// MIN_SIZE_THRESHOLD Pool still tops up on the tick.
func TestSeedOnce(t *testing.T) {
	for _, typ := range []poolmgrv1.ReplenishmentStrategyType{
		poolmgrv1.ReplenishmentStrategyType_IMMEDIATE_ON_LEASE,
		poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE,
	} {
		t.Run(typ.String()+" stays short after a failed seed", func(t *testing.T) {
			h := newHarness(t, Config{SeedOnce: true}, hostA)
			host := h.stubs[hostA]
			spec := h.spec("pool", 2, hostA)
			spec.Replenishment = replenishment{Type: typ}
			h.failSeed(host, spec)
			if created, _ := host.counts(); created != 0 {
				t.Fatalf("%d MicroVMs created while the Host refused, want none", created)
			}

			for range 3 {
				h.advance(testInterval)
				if vms := h.b.VMs(); len(vms) != 0 {
					t.Fatalf("the tick provisioned %v for a seeded %s Pool, want nothing", vms, typ)
				}
			}
			if st := h.pool("pool").Status; st.Available != 0 || st.Provisioning != 0 {
				t.Fatalf("status after the ticks = %+v, want the Pool still empty", st)
			}

			// UpdatePool with the same spec starts a new reconciler, which
			// seeds the whole size again.
			if _, err := h.client.UpdatePool(h.ctx, spec); err != nil {
				t.Fatalf("UpdatePool: %v", err)
			}
			for range spec.Size {
				h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
			}
			if st := h.pool("pool").Status; st.Available != 2 {
				t.Fatalf("status after UpdatePool = %+v, want 2 available", st)
			}
		})
	}

	t.Run("the seed fills the Pool to its size", func(t *testing.T) {
		h := newHarness(t, Config{SeedOnce: true}, hostA)
		spec := h.spec("pool", 3, hostA)
		spec.Replenishment = replenishment{Type: poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE}
		h.createPool(spec)
		if st := h.pool("pool").Status; st.Available != 3 {
			t.Fatalf("status after the seed = %+v, want 3 available", st)
		}
	})

	t.Run("a claim still replenishes an IMMEDIATE_ON_LEASE Pool", func(t *testing.T) {
		h := newHarness(t, Config{SeedOnce: true}, hostA)
		h.createPool(h.spec("pool", 1, hostA))
		h.claim("pool")
		h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
		if st := h.pool("pool").Status; st.Available != 1 || st.Leased != 1 {
			t.Fatalf("status after the claim = %+v, want 1 available and 1 leased", st)
		}
	})

	t.Run("MIN_SIZE_THRESHOLD still tops up on the tick", func(t *testing.T) {
		h := newHarness(t, Config{SeedOnce: true}, hostA)
		host := h.stubs[hostA]
		spec := h.spec("pool", 1, hostA)
		spec.Replenishment = replenishment{
			Type:    poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD,
			MinSize: minSize(1),
		}
		h.failSeed(host, spec)
		h.tick()
		h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
		if st := h.pool("pool").Status; st.Available != 1 {
			t.Fatalf("status after the tick = %+v, want the Pool filled", st)
		}
	})
}
