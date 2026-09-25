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
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/controller/claimscope"
)

// record is battery's record of lease-1, expiring at expires.
func record(expires time.Time) *battery.LeaseRecord {
	return &battery.LeaseRecord{
		LeaseID:   testLease,
		VMUID:     testVM,
		Pool:      battery.PoolRef{Name: testPool, Namespace: "ci"},
		ExpiresAt: expires,
	}
}

// another is another claim's Lease in the same Pool.
var another = &battery.LeaseRecord{LeaseID: "lease-2", VMUID: "vm-2", ExpiresAt: start.Add(time.Hour)}

// passedClaim is boundClaim with its status expiry passed: the fake clock
// is at start, and the status says start less 5s.
func passedClaim() *batteryv1alpha1.MicroVMClaim {
	c := boundClaim()
	exp := metav1.NewTime(start.Add(-5 * time.Second))
	c.Status.LeaseExpiresAt = &exp
	return c
}

//= docs/requirements/02-claims.md#recovery
//= type=test
//# While a claim is `Bound` and the Lease expiry time in its
//# status has not passed, the Claim Controller SHALL reconcile the claim
//# again once that time has passed.

// TestCheckExpiryWaitsForTheExpiry covers CL-032: while the expiry in the
// status is to come, nothing is called and the claim is reconciled when it
// passes.
func TestCheckExpiryWaitsForTheExpiry(t *testing.T) {
	b := lists(nil)
	s, _ := newScope(t, boundClaim(), b)

	res, err := CheckExpiry{Backoff: boundBackoff}.Reconcile(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stop || res.RequeueAfter != 30*time.Second {
		t.Errorf("result = %+v, want the chain to go on with a requeue at the expiry, 30s", res)
	}
	if _, n := b.calls(); n != 0 {
		t.Errorf("ListLeases called %d times, want none", n)
	}
}

//= docs/requirements/02-claims.md#renewal
//= type=test
//# When a Bound claim that is not being deleted has no pending renewal, and its status has
//# no Lease expiry time or one that has passed, the Claim Controller SHALL
//# read the claim's Lease with battery's `ListLeases`, and SHALL keep the
//# claim `Bound` and write the expiry time battery lists if that time has
//# not passed.

//= docs/requirements/02-claims.md#renewal
//= type=test
//# The Claim Controller SHALL take a claim's Lease expiry time only
//# from battery's answer to `Heartbeat` or `ListLeases`, and SHALL NOT
//# compute it.

// TestCheckExpiryKeepsALeaseBatteryStillHolds covers CL-016 and CL-011: a
// claim just bound, with no expiry, and a claim whose status is behind
// battery's (a Heartbeat answer lost, #57) both read battery's expiry with
// ListLeases for the claim's Pool, and stay Bound with it.
func TestCheckExpiryKeepsALeaseBatteryStillHolds(t *testing.T) {
	justBound := boundClaim()
	justBound.Status.LeaseExpiresAt = nil
	for name, cl := range map[string]*batteryv1alpha1.MicroVMClaim{
		"just bound":    justBound,
		"status behind": passedClaim(),
	} {
		t.Run(name, func(t *testing.T) {
			expires := start.Add(23 * time.Second)
			b := lists(nil, another, record(expires))
			s, c := newScope(t, cl, b)

			res, err := CheckExpiry{Backoff: boundBackoff}.Reconcile(context.Background(), s)
			if err != nil {
				t.Fatal(err)
			}
			if res.Stop || res.RequeueAfter != 23*time.Second {
				t.Errorf("result = %+v, want the chain to go on, with a requeue at battery's expiry, 23s", res)
			}
			if len(b.lists) != 1 || b.lists[0] != (battery.PoolRef{Name: testPool, Namespace: "ci"}) {
				t.Errorf("ListLeases calls = %v, want one for ci/small", b.lists)
			}
			got := patched(t, s, c)
			wantBound(t, got)
			if got.Status.LeaseExpiresAt == nil || !got.Status.LeaseExpiresAt.Time.Equal(expires) {
				t.Errorf("leaseExpiresAt = %v, want battery's %v", got.Status.LeaseExpiresAt, expires)
			}
		})
	}
}

//= docs/requirements/02-claims.md#renewal
//= type=test
//# When battery lists the Lease of a Bound claim that is not being
//# deleted in its answer to
//# `ListLeases` with an expiry time that has passed, and the claim has no
//# pending renewal, the Claim Controller SHALL set the claim's phase to
//# `Expired` and its condition `Bound` false with the reason `LeaseExpired`.

// TestCheckExpiryExpiresALeaseThatRanOut covers CL-014: battery still lists
// the Lease, before its sweep, but with an expiry at or before now.
func TestCheckExpiryExpiresALeaseThatRanOut(t *testing.T) {
	for name, expires := range map[string]time.Time{
		"passed":      start.Add(-2 * time.Second),
		"exactly now": start,
	} {
		t.Run(name, func(t *testing.T) {
			b := lists(nil, record(expires))
			s, c := newScope(t, passedClaim(), b)

			res, err := CheckExpiry{Backoff: boundBackoff}.Reconcile(context.Background(), s)
			if err != nil {
				t.Fatal(err)
			}
			if !res.Stop {
				t.Errorf("result = %+v, want the chain stopped", res)
			}
			wantExpired(t, patched(t, s, c))
		})
	}
}

//= docs/requirements/02-claims.md#renewal
//= type=test
//# If battery answers a `Heartbeat` for a claim's Lease with
//# `NOT_FOUND`, or leaves the claim's Lease out of its answer to
//# `ListLeases`, then the Claim Controller SHALL set the claim's phase to
//# `Expired` and its condition `Bound` false with the reason `LeaseExpired`.

// TestCheckExpiryExpiresALeaseBatteryDoesNotList covers CL-012 from
// ListLeases' side: another claim's Lease is listed, this one's is not.
func TestCheckExpiryExpiresALeaseBatteryDoesNotList(t *testing.T) {
	b := lists(nil, another)
	s, c := newScope(t, passedClaim(), b)

	if _, err := (CheckExpiry{Backoff: boundBackoff}).Reconcile(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	wantExpired(t, patched(t, s, c))
	if s.BatteryCall == nil || s.BatteryCall.Method != methodListLeases || s.BatteryCall.Err != nil {
		t.Errorf("BatteryCall = %+v, want a successful ListLeases", s.BatteryCall)
	}
}

//= docs/requirements/02-claims.md#renewal
//= type=test
//# When battery lists the Lease of a Bound claim that is not being
//# deleted in its answer to
//# `ListLeases` with an expiry time that has passed, and the claim has no
//# pending renewal, the Claim Controller SHALL set the claim's phase to
//# `Expired` and its condition `Bound` false with the reason `LeaseExpired`.

// TestCheckExpiryWaitsForAPendingRenewal is #85: the Holder renewed, the
// controller has not relayed it, and the expiry has passed. battery would
// still renew the Lease until its sweep, so the claim is not expired and
// ListLeases is not called; Renew goes first.
func TestCheckExpiryWaitsForAPendingRenewal(t *testing.T) {
	cl := passedClaim()
	cl.Spec.RenewTime = micro(start.Add(-6 * time.Second))
	b := lists(nil, record(start.Add(-5*time.Second)))
	s, _ := newScope(t, cl, b)

	res, err := CheckExpiry{Backoff: boundBackoff}.Reconcile(context.Background(), s)
	if err != nil || res != (claimscope.Result{}) {
		t.Errorf("Reconcile = %+v, %v, want nothing done", res, err)
	}
	if _, n := b.calls(); n != 0 {
		t.Errorf("ListLeases called %d times, want none", n)
	}
	if s.Claim.Status.Phase != batteryv1alpha1.MicroVMClaimBound {
		t.Errorf("phase = %q, want Bound", s.Claim.Status.Phase)
	}
}

//= docs/requirements/02-claims.md#renewal
//= type=test
//# If the `ListLeases` call of CL-016 fails in transit, then the
//# Claim Controller SHALL keep the claim `Bound` and retry the call with
//# backoff.

// TestCheckExpiryKeepsTheClaimWhenListLeasesFailsInTransit covers CL-017.
func TestCheckExpiryKeepsTheClaimWhenListLeasesFailsInTransit(t *testing.T) {
	cl := passedClaim()
	cl.Status.Conditions = append(cl.Status.Conditions, metav1.Condition{
		Type:               batteryv1alpha1.ConditionSynced,
		Status:             metav1.ConditionFalse,
		Reason:             batteryv1alpha1.ReasonBatteryUnavailable,
		LastTransitionTime: metav1.NewTime(start.Add(-4 * time.Second)),
	})
	b := lists(battery.ErrUnavailable)
	s, c := newScope(t, cl, b)

	res, err := CheckExpiry{Backoff: boundBackoff}.Reconcile(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stop || res.RequeueAfter != 4*time.Second {
		t.Errorf("result = %+v, want the chain to go on, with a retry after 4s", res)
	}
	got := patched(t, s, c)
	wantBound(t, got)
	if !got.Status.LeaseExpiresAt.Time.Equal(start.Add(-5 * time.Second)) {
		t.Errorf("leaseExpiresAt = %v, want the old one kept", got.Status.LeaseExpiresAt)
	}
}

// TestCheckExpiryReturnsOtherFailures: CL-041 from ListLeases' side.
func TestCheckExpiryReturnsOtherFailures(t *testing.T) {
	b := lists(battery.ErrInvalid)
	s, c := newScope(t, passedClaim(), b)

	if _, err := (CheckExpiry{Backoff: boundBackoff}).Reconcile(context.Background(), s); !errors.Is(err, battery.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	wantBound(t, patched(t, s, c))
}

// TestUntilIsAtLeastASecond: a requeue at an expiry that has just passed
// still waits a moment.
func TestUntilIsAtLeastASecond(t *testing.T) {
	s, _ := newScope(t, boundClaim(), nil)
	s.Clock = clock.NewFake(start)
	if got := until(s, start.Add(-time.Minute)); got != time.Second {
		t.Errorf("until a passed time = %s, want 1s", got)
	}
}
