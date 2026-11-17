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
	"encoding/hex"
	"strings"
	"testing"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
)

// TestPlacementByLeastVMCount fills Pools of several sizes over several
// Hosts and checks where each MicroVM landed, on the fake's own record and
// on the Hosts themselves. Placement is over the Pool's flintlock_hosts, so
// a Host the fake knows but the Pool does not name gets nothing, and ties
// are broken by the order of flintlock_hosts as battery's PlaceVM does.
func TestPlacementByLeastVMCount(t *testing.T) {
	tests := []struct {
		name  string
		known []string
		pool  []string
		size  int32
		want  map[string]int
	}{
		{
			name:  "one host takes everything",
			known: []string{hostA},
			pool:  []string{hostA},
			size:  3,
			want:  map[string]int{hostA: 3},
		},
		{
			name:  "an exact multiple spreads evenly",
			known: []string{hostA, hostB, hostC},
			pool:  []string{hostA, hostB, hostC},
			size:  6,
			want:  map[string]int{hostA: 2, hostB: 2, hostC: 2},
		},
		{
			name:  "the remainder goes to the first hosts",
			known: []string{hostA, hostB, hostC},
			pool:  []string{hostA, hostB, hostC},
			size:  4,
			want:  map[string]int{hostA: 2, hostB: 1, hostC: 1},
		},
		{
			name:  "hosts outside flintlock_hosts are not placed on",
			known: []string{hostA, hostB, hostC},
			pool:  []string{hostB, hostC},
			size:  4,
			want:  map[string]int{hostB: 2, hostC: 2},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, Config{}, tt.known...)
			h.createPool(h.spec("pool", tt.size, tt.pool...))

			if got := h.vmHosts(); !sameCounts(got, tt.want) {
				t.Fatalf("placement = %v, want %v", got, tt.want)
			}
			for _, name := range tt.known {
				created, _ := h.stubs[name].counts()
				if created != tt.want[name] {
					t.Fatalf("host %s created %d microvms, want %d", name, created, tt.want[name])
				}
			}
		})
	}
}

// TestPlacementFillsTheEmptiestHost makes the second Host of a balanced Pool
// lose a MicroVM and asserts that the replacement goes back to it, because
// it is the Host with the fewest. Round-robin placement, the fake's other
// mode, would hand out the first Host instead, and the test asserts that
// difference so that a least-VM-count placement cannot be mistaken for a
// cycle that happens to look balanced.
func TestPlacementFillsTheEmptiestHost(t *testing.T) {
	tests := []struct {
		placement Placement
		want      map[string]int
	}{
		{placement: PlacementLeastVMs, want: map[string]int{hostA: 2, hostB: 2}},
		{placement: PlacementRoundRobin, want: map[string]int{hostA: 3, hostB: 1}},
	}
	for _, tt := range tests {
		t.Run(string(tt.placement), func(t *testing.T) {
			h := newHarness(t, Config{Placement: tt.placement}, hostA, hostB)
			spec := h.spec("pool", 4, hostA, hostB)
			spec.Replenishment = replenishment{Type: poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE}
			h.createPool(spec)
			if got := h.vmHosts(); !sameCounts(got, map[string]int{hostA: 2, hostB: 2}) {
				t.Fatalf("initial placement = %v, want two on each host", got)
			}

			// Claims go to the longest-available MicroVM, so the first is the
			// one on host-a and the second the one on host-b. Releasing only
			// the second leaves host-b one short: a leased MicroVM still
			// counts towards its Host.
			first := h.claim("pool")
			second := h.claim("pool")
			if host := hostOfUID(t, h.b.VMs(), first.VMUID); host != hostA {
				t.Fatalf("first claim is on %q, want host-a", host)
			}
			if host := hostOfUID(t, h.b.VMs(), second.VMUID); host != hostB {
				t.Fatalf("second claim is on %q, want host-b", host)
			}
			h.release(second.LeaseID)
			h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)

			if got := h.vmHosts(); !sameCounts(got, tt.want) {
				t.Fatalf("placement after the replacement = %v, want %v", got, tt.want)
			}
		})
	}
}

// hostOfUID returns the Host a MicroVM is placed on.
func hostOfUID(t *testing.T, records []VMRecord, uid string) string {
	t.Helper()
	for _, r := range records {
		if r.UID == uid {
			return r.Host
		}
	}
	t.Fatalf("no microvm %q among %d records", uid, len(records))
	return ""
}

// sameCounts compares two placement maps, treating a missing key as zero.
func sameCounts(got, want map[string]int) bool {
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	for k, v := range got {
		if want[k] != v {
			return false
		}
	}
	return true
}

// TestMicroVMIDs checks that each MicroVM is created with an id of its own,
// the Pool's name and eight hex digits, and in the Pool's namespace when the
// template names none, as battery v0.3.3 creates them.
func TestMicroVMIDs(t *testing.T) {
	h := newHarness(t, Config{}, hostA)
	spec := h.spec("ids", 3, hostA)
	spec.Template.Namespace = ""
	h.createPool(spec)

	seen := map[string]bool{}
	for _, vm := range h.stubs[hostA].fl.MicroVMs() {
		s := vm.GetSpec()
		id, ok := strings.CutPrefix(s.GetId(), "ids-")
		if _, err := hex.DecodeString(id); !ok || len(id) != 8 || err != nil {
			t.Errorf("microvm id %q, want ids- and eight hex digits", s.GetId())
		}
		if s.GetNamespace() != testNamespace {
			t.Errorf("microvm %s in namespace %q, want the Pool's %q", s.GetId(), s.GetNamespace(), testNamespace)
		}
		if seen[s.GetId()] {
			t.Errorf("microvm id %q given twice", s.GetId())
		}
		seen[s.GetId()] = true
	}
	if len(seen) != 3 {
		t.Fatalf("%d microvms on the Host, want 3", len(seen))
	}
}

//= docs/requirements/10-battery.md#hosts
//= type=test
//# battery SHALL place each new MicroVM of a Pool on the Host, of
//# those in the Pool's `flintlock_hosts` that it holds and has not cordoned,
//# with the least memory reserved, summing the template `memory_in_mb` of
//# every Pool's MicroVMs on the Host that are not `DELETING` or `FAILED` and
//# of every placement it has reserved there, and SHALL give a tie to the Host
//# with the fewest of the Pool's own MicroVMs and then to the Host that comes
//# first in `flintlock_hosts`.

// TestBatchSpreadsOverHosts: a Pool of 8 on two empty Hosts is provisioned
// in one batch, and the batch is split 4 and 4. Grown to 10 with a third,
// empty Host, the 2 new MicroVMs both go to it. A tie goes to the Host
// listed first.
func TestBatchSpreadsOverHosts(t *testing.T) {
	h := newHarness(t, Config{}, hostA, hostB, hostC)
	h.createPool(h.spec("pool", 8, hostA, hostB))
	if got, want := h.vmHosts(), map[string]int{hostA: 4, hostB: 4}; !sameCounts(got, want) {
		t.Fatalf("placement of a batch of 8 = %v, want %v", got, want)
	}

	if _, err := h.client.UpdatePool(h.ctx, h.spec("pool", 10, hostA, hostB, hostC)); err != nil {
		t.Fatalf("UpdatePool: %v", err)
	}
	h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
	h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
	if got, want := h.vmHosts(), map[string]int{hostA: 4, hostB: 4, hostC: 2}; !sameCounts(got, want) {
		t.Fatalf("placement after growing onto host-c = %v, want %v", got, want)
	}

	h.createPool(h.spec("other", 1, hostC, hostA))
	for _, r := range h.b.VMs() {
		if r.Pool.Name == "other" && r.Host != hostC {
			t.Fatalf("other's microvm on %s, want host-c: it has the least memory reserved", r.Host)
		}
	}
}

//= docs/requirements/10-battery.md#hosts
//= type=test
//# battery SHALL place each new MicroVM of a Pool on the Host, of
//# those in the Pool's `flintlock_hosts` that it holds and has not cordoned,
//# with the least memory reserved, summing the template `memory_in_mb` of
//# every Pool's MicroVMs on the Host that are not `DELETING` or `FAILED` and
//# of every placement it has reserved there, and SHALL give a tie to the Host
//# with the fewest of the Pool's own MicroVMs and then to the Host that comes
//# first in `flintlock_hosts`.

// TestPlacementByMemoryAcrossPools: one 2048 MiB MicroVM of "big" lands on
// host-a, the first of two empty Hosts. Four 256 MiB MicroVMs of "small"
// then all go to host-b, which has less memory reserved until it holds 1024
// MiB, though host-a has none of small's own. By count alone they would
// split 2 and 2.
func TestPlacementByMemoryAcrossPools(t *testing.T) {
	h := newHarness(t, Config{}, hostA, hostB)
	big := h.spec("big", 1, hostA, hostB)
	big.Template.MemoryInMb = 2048
	h.createPool(big)
	h.createPool(h.spec("small", 4, hostA, hostB))

	got := map[string]int{}
	for _, r := range h.b.VMs() {
		if r.Pool.Name == "small" {
			got[r.Host]++
		}
	}
	if want := map[string]int{hostB: 4}; !sameCounts(got, want) {
		t.Fatalf("small's placement = %v, want %v", got, want)
	}
}

//= docs/requirements/10-battery.md#balloon
//= type=test
//# battery SHALL send each MicroVM of a Pool to `flintlockd` with
//# the `balloon` of the Pool's `microvm_template`, as the template sets it.

// TestBalloonReachesFlintlockd: a Pool whose template has a balloon creates
// each MicroVM on its Host with it.
func TestBalloonReachesFlintlockd(t *testing.T) {
	h := newHarness(t, Config{}, hostA)
	spec := h.spec("pool", 2, hostA)
	spec.Template.Balloon = &types.Balloon{FreePageReporting: true}
	h.createPool(spec)

	vms := h.stubs[hostA].fl.MicroVMs()
	if len(vms) != 2 {
		t.Fatalf("host-a holds %d microvms, want 2", len(vms))
	}
	for _, vm := range vms {
		if !vm.GetSpec().GetBalloon().GetFreePageReporting() {
			t.Fatalf("microvm %s has balloon %v, want free_page_reporting", vm.GetSpec().GetUid(), vm.GetSpec().GetBalloon())
		}
	}
}

//= docs/requirements/10-battery.md#balloon
//= type=test
//# If `flintlockd` answers a `CreateMicroVM` whose spec has a
//# `balloon` with a spec that has none, then battery SHALL delete that
//# MicroVM and fail its provision.

// TestBalloonDroppedByFlintlockd: a Host whose flintlockd drops the balloon
// gets the MicroVM deleted again, and the Pool gets no available MicroVM.
func TestBalloonDroppedByFlintlockd(t *testing.T) {
	h := newHarness(t, Config{}, hostA)
	host := h.stubs[hostA]
	host.mu.Lock()
	host.dropBalloon = true
	host.mu.Unlock()

	spec := h.spec("pool", 1, hostA)
	spec.Template.Balloon = &types.Balloon{FreePageReporting: true}
	if _, err := h.client.CreatePool(h.ctx, spec); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}

	select {
	case uid := <-host.deletions:
		created, _ := host.counts()
		if created < 1 {
			t.Fatalf("host-a deleted %s before it created anything", uid)
		}
	case <-h.ctx.Done():
		t.Fatal("host-a never deleted the microvm without its balloon")
	}
	for _, r := range h.b.VMs() {
		if r.Phase == poolmgrv1.VMPhase_AVAILABLE {
			t.Fatalf("microvm %s is available, want none while the host drops the balloon", r.UID)
		}
	}
}
