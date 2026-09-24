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

package claim

import (
	"context"
	"testing"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/controller/claimscope"
	"github.com/phoban01/battery-operator/internal/fakebattery"
	"github.com/phoban01/battery-operator/internal/fakeflintlock"
)

// lifecycleTimeout bounds each wait of the lifecycle test.
const lifecycleTimeout = 30 * time.Second

// startFakeBattery serves the fake battery, on the wall clock, with one
// fake flintlockd, and dials it as the Operator does.
func startFakeBattery(t *testing.T) (*fakebattery.Battery, *battery.Connection) {
	t.Helper()
	fl := fakeflintlock.New(fakeflintlock.Config{Name: "node-a"})
	flConn, err := fl.Conn()
	if err != nil {
		t.Fatalf("connecting to the fake flintlockd: %v", err)
	}
	t.Cleanup(func() {
		_ = flConn.Close()
		_ = fl.Close()
	})
	hosts := fakebattery.NewHosts()
	hosts.Add("node-a", "node-a:9090", flConn)

	b := fakebattery.New(fakebattery.Config{Hosts: hosts, ReconcileInterval: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("fake battery: Serve: %v", err)
		}
	})
	select {
	case <-b.Ready():
	case err := <-done:
		t.Fatalf("fake battery: Serve: %v", err)
	case <-time.After(lifecycleTimeout):
		t.Fatal("the fake battery did not start listening")
	}
	conn, err := battery.Dial(battery.Config{Address: b.Addr()})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return b, conn
}

// eventually polls cond every 50ms until it holds or the timeout passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(lifecycleTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestRenewalAndExpiryAgainstTheFakeBattery is #10's "done when", end to
// end against the fake battery: a claim is bound, CheckExpiry reads its
// expiry (CL-016), renewing moves the expiry (CL-010), and once the Holder
// stops renewing and the fake expires the Lease, the claim goes Expired
// (CL-012, CL-014). The subreconcilers run in the controller's order on the
// wall clock, with a two-second expiry threshold.
func TestRenewalAndExpiryAgainstTheFakeBattery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*lifecycleTimeout)
	defer cancel()
	fake, conn := startFakeBattery(t)

	minSize := int32(1)
	if _, err := conn.CreatePool(ctx, battery.PoolSpec{
		Ref:            battery.PoolRef{Name: "small", Namespace: "ci"},
		Template:       &types.MicroVMSpec{Namespace: "ci", Vcpu: 1, MemoryInMb: 256},
		Size:           1,
		FlintlockHosts: []string{"node-a"},
		Replenishment: battery.ReplenishmentStrategy{
			Type:    poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD,
			MinSize: &minSize,
		},
		HookFailurePolicy:        poolmgrv1.HookFailurePolicy_DELETE_AND_REPLACE,
		HeartbeatInterval:        time.Second,
		HeartbeatExpiryThreshold: 2 * time.Second,
	}); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	eventually(t, "a warm MicroVM", func() bool {
		for _, vm := range fake.VMs() {
			if vm.Phase == poolmgrv1.VMPhase_AVAILABLE {
				return true
			}
		}
		return false
	})

	c := newFakeClient(t, aClaim(batteryv1alpha1.ReleaseFinalizer))
	chain := claimscope.Chain{
		Steps: []claimscope.Subreconciler{
			Bind{},
			ExpireDeleted{},
			Renew{Backoff: boundBackoff},
			CheckExpiry{Backoff: boundBackoff},
		},
		Finally: []claimscope.Subreconciler{Synced{}},
	}
	reconcile := func() *batteryv1alpha1.MicroVMClaim {
		t.Helper()
		s := scopeFor(t, c, claimKey, conn)
		s.Clock = clock.Real{}
		if err := chain.Run(ctx, s); err != nil {
			t.Fatalf("chain: %v", err)
		}
		return patched(t, s, c)
	}

	// Bound, then battery's expiry read with ListLeases.
	got := reconcile()
	wantBound(t, got)
	if got.Status.LeaseExpiresAt != nil {
		t.Fatalf("leaseExpiresAt = %v right after binding, want none", got.Status.LeaseExpiresAt)
	}
	got = reconcile()
	wantBound(t, got)
	if got.Status.LeaseExpiresAt == nil {
		t.Fatal("no leaseExpiresAt after CheckExpiry")
	}
	first := got.Status.LeaseExpiresAt.Time

	//= docs/requirements/02-claims.md#renewal
	//= type=test
	//# While the `spec.renewTime` of a Bound claim differs from the
	//# last `renewTime` the Claim Controller relayed for it, the Claim Controller
	//# SHALL call battery's `Heartbeat` for the claim's Lease, and SHALL write
	//# the expiry time battery returns and the `renewTime` it relayed to the
	//# claim's status in one write.
	time.Sleep(1100 * time.Millisecond)
	now := metav1.NewMicroTime(time.Now())
	got.Spec.RenewTime = &now
	if err := c.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	got = reconcile()
	wantBound(t, got)
	if !got.Status.LeaseExpiresAt.After(first) {
		t.Fatalf("leaseExpiresAt = %v after renewing, want later than %v", got.Status.LeaseExpiresAt, first)
	}
	if !got.Status.ObservedRenewTime.Equal(got.Spec.RenewTime) {
		t.Errorf("observedRenewTime = %v, want the relayed %v", got.Status.ObservedRenewTime, got.Spec.RenewTime)
	}

	// The Holder stops renewing. The claim stays Bound until battery's
	// expiry passes, and then goes Expired.
	renewedExpiry := got.Status.LeaseExpiresAt.Time
	eventually(t, "the claim to go Expired", func() bool {
		got = reconcile()
		return got.Status.Phase == batteryv1alpha1.MicroVMClaimExpired
	})
	if time.Now().Before(renewedExpiry) {
		t.Errorf("the claim went Expired before its renewed expiry %v", renewedExpiry)
	}
	wantExpired(t, got)
}
