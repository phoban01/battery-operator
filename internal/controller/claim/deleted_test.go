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

	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/controller/claimscope"
)

//= docs/requirements/02-claims.md#renewal
//= type=test
//# When battery's `Events` stream reports that the MicroVM of a
//# Bound claim was deleted, the Claim Controller SHALL set the claim's phase
//# to `Expired` and its condition `Bound` false with the reason
//# `LeaseExpired`.

// TestExpireDeletedExpiresTheClaimOfADeletedMicroVM covers CL-013: once
// the event watcher has recorded the claim's MicroVM as deleted, the claim
// goes Expired without a call to battery, even with a renewal pending,
// since battery deleted the Lease first.
func TestExpireDeletedExpiresTheClaimOfADeletedMicroVM(t *testing.T) {
	deleted := &DeletedVMs{Clock: clock.NewFake(start)}
	deleted.Add(testVM)
	// A leaseStub with no functions panics if it is called.
	s, c := newScope(t, renewed(), &leaseStub{})

	res, err := ExpireDeleted{Deleted: deleted}.Reconcile(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stop {
		t.Errorf("result = %+v, want the chain stopped", res)
	}
	if s.BatteryCall != nil {
		t.Errorf("BatteryCall = %+v, want none", s.BatteryCall)
	}
	wantExpired(t, patched(t, s, c))
}

// TestExpireDeletedLeavesOtherClaims: another MicroVM's deletion, or no
// set at all, changes nothing.
func TestExpireDeletedLeavesOtherClaims(t *testing.T) {
	deleted := &DeletedVMs{}
	deleted.Add("vm-2")
	for name, sub := range map[string]ExpireDeleted{
		"another MicroVM": {Deleted: deleted},
		"no set":          {},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := newScope(t, boundClaim(), nil)
			res, err := sub.Reconcile(context.Background(), s)
			if err != nil || res != (claimscope.Result{}) {
				t.Errorf("Reconcile = %+v, %v, want the chain to go on", res, err)
			}
			wantBound(t, s.Claim)
		})
	}
}

// TestDeletedVMsForgetsOldEntries: an entry older than the retention is
// dropped when a new one is added.
func TestDeletedVMsForgetsOldEntries(t *testing.T) {
	clk := clock.NewFake(start)
	d := &DeletedVMs{Retention: time.Minute, Clock: clk}
	d.Add(testVM)
	clk.Advance(2 * time.Minute)
	d.Add("vm-2")
	if d.Has(testVM) || !d.Has("vm-2") {
		t.Errorf("Has(vm-1), Has(vm-2) = %t, %t, want false, true", d.Has(testVM), d.Has("vm-2"))
	}
}

// TestReportsDeletion: only the two deletions of a leased MicroVM count.
func TestReportsDeletion(t *testing.T) {
	for typ, want := range map[poolmgrv1.EventType]bool{
		poolmgrv1.EventType_VM_DELETED_DUE_TO_EXPIRY: true,
		poolmgrv1.EventType_VM_DELETED_ON_RELEASE:    true,
		poolmgrv1.EventType_VM_EXPIRING_SOON:         false,
		poolmgrv1.EventType_VM_RELEASED:              false,
		poolmgrv1.EventType_VM_CLAIMED:               false,
	} {
		if got := ReportsDeletion(&battery.Event{Type: typ, VMUID: testVM}); got != want {
			t.Errorf("ReportsDeletion(%s) = %t, want %t", typ, got, want)
		}
	}
	if ReportsDeletion(&battery.Event{Type: poolmgrv1.EventType_VM_DELETED_ON_RELEASE}) {
		t.Error("ReportsDeletion of an event with no MicroVM = true, want false")
	}
}
