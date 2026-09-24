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

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/controller/claimscope"
)

//= docs/requirements/02-claims.md#renewal
//= type=test
//# While battery has not yet answered `ClaimVM` calls for other
//# claims, the Claim Controller SHALL still reconcile a Bound claim that has
//# a pending renewal.

// TestRenewalDoesNotWaitBehindAClaimVM covers CL-018 (#89). With two
// concurrent reconciles, one ClaimVM may be in flight. While battery sits
// on it, a second claim's Bind calls nothing and asks for a retry, and a
// Bound claim's renewal is relayed at once. Once the ClaimVM is answered,
// the slot is free again.
func TestRenewalDoesNotWaitBehindAClaimVM(t *testing.T) {
	ctx := context.Background()
	slots, err := NewBindSlots(2)
	if err != nil {
		t.Fatal(err)
	}
	bind := Bind{Slots: slots, SlotWait: 3 * time.Second}

	inClaimVM := make(chan struct{})
	answer := make(chan struct{})
	slow := &stubBattery{claimVM: func(battery.PoolRef) (*battery.Claim, error) {
		close(inClaimVM)
		<-answer
		return leased, nil
	}}
	first, _ := newScope(t, aClaim(batteryv1alpha1.ReleaseFinalizer), slow)
	done := make(chan error, 1)
	go func() {
		_, err := bind.Reconcile(ctx, first)
		done <- err
	}()
	<-inClaimVM

	// A second claim finds no slot: no ClaimVM, a retry after SlotWait.
	other := answers(leased, nil)
	second, _ := newScope(t, aClaim(batteryv1alpha1.ReleaseFinalizer), other)
	res, err := bind.Reconcile(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stop || res.RequeueAfter != 3*time.Second {
		t.Errorf("Bind without a slot = %+v, want the chain stopped and a retry after 3s", res)
	}
	if n := len(other.claimCalls()); n != 0 {
		t.Errorf("ClaimVM called %d times without a slot, want none", n)
	}
	if second.BatteryCall != nil {
		t.Errorf("BatteryCall = %+v, want none, so Synced and Pending are left alone", second.BatteryCall)
	}

	// A renewal goes through while the ClaimVM is still unanswered.
	hb := heartbeatAnswers(start.Add(30*time.Second), nil)
	renewing, _ := newScope(t, renewed(), hb)
	renewDone := make(chan error, 1)
	go func() {
		_, err := Renew{Backoff: boundBackoff}.Reconcile(ctx, renewing)
		renewDone <- err
	}()
	select {
	case err := <-renewDone:
		if err != nil {
			t.Fatalf("Renew: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Renew waited for the ClaimVM of another claim")
	}
	if n, _ := hb.calls(); n != 1 {
		t.Errorf("Heartbeat calls = %d, want 1", n)
	}

	close(answer)
	if err := <-done; err != nil {
		t.Fatalf("Bind: %v", err)
	}
	third, _ := newScope(t, aClaim(batteryv1alpha1.ReleaseFinalizer), other)
	if _, err := bind.Reconcile(ctx, third); err != nil {
		t.Fatal(err)
	}
	if n := len(other.claimCalls()); n != 1 {
		t.Errorf("ClaimVM calls once the slot was free = %d, want 1", n)
	}
	if third.Claim.Status.Phase != batteryv1alpha1.MicroVMClaimBound {
		t.Errorf("phase = %q, want Bound", third.Claim.Status.Phase)
	}
}

// TestNewBindSlotsNeedsTwoReconciles: with one reconcile, keeping one from
// ClaimVM would leave none to bind.
func TestNewBindSlotsNeedsTwoReconciles(t *testing.T) {
	if _, err := NewBindSlots(1); err == nil {
		t.Error("NewBindSlots(1) succeeded, want an error")
	}
}

// TestBindRecordsTheRenewTimeAsRelayed: binding records the renewTime the
// claim had, and no expiry, which CheckExpiry reads next (CL-016).
func TestBindRecordsTheRenewTimeAsRelayed(t *testing.T) {
	cl := aClaim(batteryv1alpha1.ReleaseFinalizer)
	cl.Spec.RenewTime = micro(start.Add(-time.Minute))
	s, c := newScope(t, cl, answers(leased, nil))
	if err := (claimscope.Chain{Steps: []claimscope.Subreconciler{Bind{}}}).Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	got := patched(t, s, c)
	if !got.Status.ObservedRenewTime.Equal(cl.Spec.RenewTime) {
		t.Errorf("observedRenewTime = %v, want %v", got.Status.ObservedRenewTime, cl.Spec.RenewTime)
	}
	if got.Status.LeaseExpiresAt != nil {
		t.Errorf("leaseExpiresAt = %v, want none: ClaimVM's answer has no expiry", got.Status.LeaseExpiresAt)
	}
	if pendingRenewal(got) {
		t.Error("a claim just bound has a pending renewal")
	}
}
