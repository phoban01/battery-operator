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

//= docs/requirements/10-battery.md#seeding
//= type=test
//# When a Pool's reconciler starts, and on each of its ticks
//# after that, battery SHALL provision for the Pool its size less its
//# available, claiming and provisioning MicroVMs for `IMMEDIATE_ON_LEASE`,
//# and its size less its available, claiming, leased and provisioning
//# MicroVMs for `REPLACE_ON_DELETE`.

// TestTickTopsUpAnEventDrivenPool covers BA-075: an event-driven Pool
// whose seed fails is filled by the next tick, as
// battery v0.4.0's reconciler provisions the shortfall on every tick, and
// a Pool at its size gets nothing more.
func TestTickTopsUpAnEventDrivenPool(t *testing.T) {
	for _, typ := range []poolmgrv1.ReplenishmentStrategyType{
		poolmgrv1.ReplenishmentStrategyType_IMMEDIATE_ON_LEASE,
		poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE,
	} {
		t.Run(typ.String(), func(t *testing.T) {
			h := newHarness(t, Config{}, hostA)
			host := h.stubs[hostA]
			spec := h.spec("pool", 2, hostA)
			spec.Replenishment = replenishment{Type: typ}
			h.failSeed(host, spec)

			h.tick()
			for range spec.Size {
				h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
			}
			if st := h.pool("pool").Status; st.Available != 2 {
				t.Fatalf("status after the tick = %+v, want 2 available", st)
			}
			h.tick()
			if created, _ := host.counts(); created != 2 {
				t.Fatalf("host created %d microvms after a tick at size, want 2", created)
			}
		})
	}
}
