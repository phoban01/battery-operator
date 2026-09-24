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
	"sync"
	"testing"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"google.golang.org/grpc/codes"
)

// TestClaimVMNeverLeasesAMicroVMTwice races more ClaimVM calls than a Pool
// has MicroVMs, and checks that each MicroVM goes to one of them at most.
// It then releases every Lease, waits for the replacements, and claims
// again: none of the MicroVMs leased before is leased a second time.
func TestClaimVMNeverLeasesAMicroVMTwice(t *testing.T) {
	//= docs/requirements/10-battery.md#claiming
	//= type=test
	//# battery SHALL move a MicroVM out of the phase `AVAILABLE` in
	//# the same transaction in which `ClaimVM` selects it, so that two `ClaimVM`
	//# calls never lease the same MicroVM.

	//= docs/requirements/10-battery.md#claiming
	//= type=test
	//# battery SHALL set a MicroVM's phase to `AVAILABLE` only when it
	//# finishes provisioning the MicroVM, so that a MicroVM that has been leased
	//# is never leased again.
	const size = 3
	h := newHarness(t, Config{}, hostA)
	spec := h.spec("pool", size, hostA)
	// REPLACE_ON_DELETE does not replenish on a claim, so the race is over
	// exactly the MicroVMs the Pool was filled with.
	spec.Replenishment = replenishment{Type: poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE}
	h.createPool(spec)

	type result struct {
		c   *claimed
		err error
	}
	results := make(chan result, size+1)
	var wg sync.WaitGroup
	for range size + 1 {
		wg.Go(func() {
			c, err := h.client.ClaimVM(h.ctx, h.ref("pool"))
			results <- result{c, err}
		})
	}
	wg.Wait()
	close(results)

	leased := make(map[string]string) // MicroVM uid to lease id
	exhausted := 0
	for r := range results {
		if r.err != nil {
			if code := statusCode(t, r.err); code != codes.ResourceExhausted {
				t.Fatalf("ClaimVM: code %v, want success or RESOURCE_EXHAUSTED", code)
			}
			exhausted++
			continue
		}
		if other, ok := leased[r.c.VMUID]; ok {
			t.Fatalf("MicroVM %s leased twice, to %s and %s", r.c.VMUID, other, r.c.LeaseID)
		}
		leased[r.c.VMUID] = r.c.LeaseID
	}
	if len(leased) != size || exhausted != 1 {
		t.Fatalf("%d MicroVMs leased and %d claims exhausted, want %d and 1", len(leased), exhausted, size)
	}

	// Release every Lease; each release starts a replacement.
	for _, lease := range leased {
		h.release(lease)
	}
	for h.seen[poolmgrv1.EventType_VM_AVAILABLE] < 2*size {
		h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
	}

	for range size {
		c := h.claim("pool")
		if _, ok := leased[c.VMUID]; ok {
			t.Fatalf("ClaimVM leased MicroVM %s again after its release", c.VMUID)
		}
	}
	for _, l := range h.b.Leases() {
		if _, ok := leased[l.VMUID]; ok {
			t.Fatalf("Lease %s names MicroVM %s, released before", l.LeaseID, l.VMUID)
		}
	}
}
