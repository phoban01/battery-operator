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
	"errors"
	"testing"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/controller/claim"
	"github.com/phoban01/battery-operator/internal/fakebattery"
	"github.com/phoban01/battery-operator/internal/fakeflintlock"
)

// TestMicroVMClaimReleaseAgainstTheFakeBattery binds a claim to a MicroVM
// of the fake battery, on a fake flintlockd, and deletes it: battery
// releases the Lease and deletes the MicroVM, and the claim goes away
// (CL-020). The requirement's cases are tested in package claim.
func TestMicroVMClaimReleaseAgainstTheFakeBattery(t *testing.T) {
	ctx := context.Background()
	fb, bc := startClaimFakeBattery(t)
	ref := battery.PoolRef{Namespace: testClaimKey.Namespace, Name: testClaim().Spec.PoolRef.Name}
	if _, err := bc.CreatePool(ctx, battery.PoolSpec{
		Ref:                      ref,
		Template:                 &types.MicroVMSpec{Namespace: "ci", Vcpu: 1, MemoryInMb: 256},
		Size:                     1,
		FlintlockHosts:           []string{"host-a"},
		Replenishment:            battery.ReplenishmentStrategy{Type: poolmgrv1.ReplenishmentStrategyType_IMMEDIATE_ON_LEASE},
		HookFailurePolicy:        poolmgrv1.HookFailurePolicy_DELETE_AND_REPLACE,
		HeartbeatExpiryThreshold: time.Hour,
	}); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	waitForClaimTest(t, "a MicroVM AVAILABLE in the Pool", func() bool {
		p, err := bc.GetPool(ctx, ref)
		return err == nil && p.Status.Available > 0
	})

	s, c := newClaimFakeClient(t, testClaim())
	r := &MicroVMClaimReconciler{
		Client:    c,
		Scheme:    s,
		APIReader: c,
		Battery:   bc,
		Clock:     clock.NewFake(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)),
		Backoff:   claim.Backoff{Min: time.Second, Max: time.Minute},
	}
	key := testClaimKey
	req := ctrl.Request{NamespacedName: key}
	got := &batteryv1alpha1.MicroVMClaim{}

	// The finalizer, then the bind.
	for range 2 {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	if err := c.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != batteryv1alpha1.MicroVMClaimBound || got.Status.LeaseID == "" {
		t.Fatalf("status = %+v, want Bound", got.Status)
	}
	lease := got.Status.LeaseID
	if leases := fb.Leases(); len(leases) != 1 || leases[0].LeaseID != lease {
		t.Fatalf("battery holds %+v, want the one Lease %s", leases, lease)
	}

	// Deleting the claim releases the Lease.
	if err := c.Delete(ctx, got); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile after the deletion: %v", err)
	}
	if err := c.Get(ctx, key, got); !apierrors.IsNotFound(err) {
		t.Errorf("Get = %v, want the claim gone", err)
	}
	if leases := fb.Leases(); len(leases) != 0 {
		t.Errorf("battery still holds %+v, want the Lease released", leases)
	}
	if err := bc.ReleaseVM(ctx, lease); !errors.Is(err, battery.ErrNotFound) {
		t.Errorf("ReleaseVM again = %v, want the Lease unknown", err)
	}
}

// startClaimFakeBattery serves the fake battery, with one fake flintlockd
// called host-a, until the test ends, and returns it with the Operator's
// client for it.
func startClaimFakeBattery(t *testing.T) (*fakebattery.Battery, battery.Client) {
	t.Helper()
	fl := fakeflintlock.New(fakeflintlock.Config{Name: "host-a"})
	flConn, err := fl.Conn()
	if err != nil {
		t.Fatalf("connecting to the fake flintlockd: %v", err)
	}
	hosts := fakebattery.NewHosts()
	hosts.Add("host-a", "", flConn)
	b := fakebattery.New(fakebattery.Config{Hosts: hosts, ReconcileInterval: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("fake battery: Serve: %v", err)
		}
		_ = flConn.Close()
		_ = fl.Close()
	})
	select {
	case <-b.Ready():
	case err := <-done:
		t.Fatalf("fake battery: Serve: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("fake battery did not start listening")
	}
	conn, err := battery.Dial(battery.Config{Address: b.Addr()})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return b, conn
}

// waitForClaimTest polls cond until it holds, or fails the test after 30 seconds.
func waitForClaimTest(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
