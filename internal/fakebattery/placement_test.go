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
	"testing"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
)

// TestPlacementByLeastVMCount fills Pools of several sizes over several
// Hosts and checks where each MicroVM landed, on the fake's own record and
// on the Hosts themselves. Placement is over the Pool's flintlock_hosts, so
// a Host the fake knows but the Pool does not name gets nothing, and ties
// are broken by the order of flintlock_hosts as battery's PickHost does.
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
