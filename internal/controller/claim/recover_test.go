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

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/controller/claimscope"
)

// recovered is a RecoveredLeases in which the recovery saw lease-1 recorded
// by a claim, and battery listed listed.
func recovered(listed ...*battery.LeaseRecord) *RecoveredLeases {
	r := &RecoveredLeases{}
	r.Record([]string{testLease}, listed)
	return r
}

//= docs/requirements/02-claims.md#recovery
//= type=test
//# When the Claim Controller starts, and when its connection to
//# battery is restored, the Claim Controller SHALL reconcile every Bound claim against
//# battery's Leases.

//= docs/requirements/02-claims.md#renewal
//= type=test
//# If battery answers a `Heartbeat` for a claim's Lease with
//# `NOT_FOUND`, or leaves the claim's Lease out of its answer to
//# `ListLeases`, then the Claim Controller SHALL set the claim's phase to
//# `Expired` and its condition `Bound` false with the reason `LeaseExpired`.

// TestRecoverExpiresAClaimWhoseLeaseBatteryLost covers CL-030 with CL-012:
// the recovery's ListLeases left out the claim's Lease, so the claim goes
// Expired without a call of its own, although its status still has an
// expiry to come and a renewal is pending. The recovery's answer counts as
// battery's, so Synced goes true.
func TestRecoverExpiresAClaimWhoseLeaseBatteryLost(t *testing.T) {
	// A leaseStub with no functions panics if it is called.
	s, c := newScope(t, renewed(), &leaseStub{})

	res, err := Recover{Leases: recovered(another)}.Reconcile(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stop {
		t.Errorf("result = %+v, want the chain stopped", res)
	}
	if _, err := (Synced{}).Reconcile(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	got := patched(t, s, c)
	wantExpired(t, got)
	if !meta.IsStatusConditionTrue(got.Status.Conditions, batteryv1alpha1.ConditionSynced) {
		t.Errorf("Synced = %+v, want true", meta.FindStatusCondition(got.Status.Conditions, batteryv1alpha1.ConditionSynced))
	}
}

//= docs/requirements/02-claims.md#renewal
//= type=test
//# The Claim Controller SHALL take a claim's Lease expiry time only
//# from battery's answer to `Heartbeat` or `ListLeases`, and SHALL NOT
//# compute it.

// TestRecoverKeepsAClaimWhoseLeaseBatteryHolds covers CL-030 for a Lease
// battery listed: the claim stays Bound, and takes battery's expiry only
// when it is later than the status's, as after a Heartbeat answer lost in
// a crash (#57). An earlier one is older than the status and is ignored,
// even when it has passed: CheckExpiry decides that on a fresh read.
func TestRecoverKeepsAClaimWhoseLeaseBatteryHolds(t *testing.T) {
	statusExpiry := start.Add(30 * time.Second) // boundClaim's
	for name, tc := range map[string]struct {
		listed time.Time
		want   time.Time
	}{
		"battery's is later":   {listed: start.Add(time.Minute), want: start.Add(time.Minute)},
		"battery's is earlier": {listed: start.Add(-time.Minute), want: statusExpiry},
	} {
		t.Run(name, func(t *testing.T) {
			s, c := newScope(t, boundClaim(), &leaseStub{})

			res, err := Recover{Leases: recovered(record(tc.listed))}.Reconcile(context.Background(), s)
			if err != nil {
				t.Fatal(err)
			}
			if res.Stop {
				t.Errorf("result = %+v, want the chain to go on", res)
			}
			got := patched(t, s, c)
			wantBound(t, got)
			if got.Status.LeaseExpiresAt == nil || !got.Status.LeaseExpiresAt.Time.Equal(tc.want) {
				t.Errorf("leaseExpiresAt = %v, want %v", got.Status.LeaseExpiresAt, tc.want)
			}
		})
	}
}

// TestRecoverHandsOutAListedRecordOnce: a Lease battery listed is used by
// the first reconcile after the recovery only, since later ones would
// take a stale answer as battery's. A Lease battery did not list stays
// ended.
func TestRecoverHandsOutAListedRecordOnce(t *testing.T) {
	r := recovered(record(start.Add(time.Minute)))
	if rec, known := r.lookup(testLease); !known || rec == nil {
		t.Fatalf("first lookup = %v, %t, want the record", rec, known)
	}
	if rec, known := r.lookup(testLease); known {
		t.Errorf("second lookup = %v, %t, want nothing", rec, known)
	}

	gone := recovered()
	for range 2 {
		if rec, known := gone.lookup(testLease); !known || rec != nil {
			t.Errorf("lookup of a Lease battery did not list = %v, %t, want known and not held", rec, known)
		}
	}
}

// TestRecoverLeavesAClaimTheRecoveryDidNotSee: a claim bound after the
// recovery began, or with no recovery yet, is left to the later steps,
// and so is a claim that is not Bound.
func TestRecoverLeavesAClaimTheRecoveryDidNotSee(t *testing.T) {
	other := &RecoveredLeases{}
	other.Record([]string{another.LeaseID}, nil)
	pending := aClaim(batteryv1alpha1.ReleaseFinalizer)
	for name, tc := range map[string]struct {
		leases *RecoveredLeases
		claim  *batteryv1alpha1.MicroVMClaim
	}{
		"no recovery":       {leases: nil, claim: boundClaim()},
		"bound after it":    {leases: other, claim: boundClaim()},
		"not bound":         {leases: recovered(), claim: pending},
		"nothing recovered": {leases: &RecoveredLeases{}, claim: boundClaim()},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := newScope(t, tc.claim, &leaseStub{})
			before := s.Claim.Status.DeepCopy()

			res, err := Recover{Leases: tc.leases}.Reconcile(context.Background(), s)
			if err != nil {
				t.Fatal(err)
			}
			if res != (claimscope.Result{}) {
				t.Errorf("result = %+v, want the chain to go on untouched", res)
			}
			if s.BatteryCall != nil {
				t.Errorf("BatteryCall = %+v, want none", s.BatteryCall)
			}
			if !apiequality.Semantic.DeepEqual(before, &s.Claim.Status) {
				t.Errorf("status changed to %+v", s.Claim.Status)
			}
		})
	}
}

//= docs/requirements/02-claims.md#recovery
//= type=test
//# The Claim Controller SHALL NOT release a Lease that battery
//# holds and no claim records.

// TestRecoveredLeasesIgnoresALeaseNoClaimRecords covers CL-031 in the
// record: a Lease battery lists that no claim recorded, such as the orphan
// of a lost ClaimVM answer, is not kept, so nothing can act on it.
// TestMicroVMClaimRecoveryAgainstTheFakeBattery, in package controller,
// shows battery still holds it after a recovery.
func TestRecoveredLeasesIgnoresALeaseNoClaimRecords(t *testing.T) {
	orphan := &battery.LeaseRecord{LeaseID: "orphan", VMUID: "vm-9", ExpiresAt: start.Add(time.Hour)}
	r := recovered(record(start.Add(time.Minute)), orphan)
	if rec, known := r.lookup("orphan"); known {
		t.Errorf("lookup(orphan) = %v, %t, want nothing", rec, known)
	}
	if len(r.leases) != 1 {
		t.Errorf("leases = %v, want lease-1 alone", r.leases)
	}
}
