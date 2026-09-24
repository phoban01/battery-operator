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
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/controller/claimscope"
)

// boundBackoff is the Backoff of the renewal and expiry tests.
var boundBackoff = Backoff{Min: time.Second, Max: 30 * time.Second}

// leaseStub is a battery.Client whose Heartbeat and ListLeases answer with
// the functions set on it, and which counts its calls. Every other method
// panics through the nil embedded Client.
type leaseStub struct {
	battery.Client

	mu         sync.Mutex
	heartbeat  func(leaseID string) (time.Time, error)
	listLeases func(pool *battery.PoolRef) ([]*battery.LeaseRecord, error)
	heartbeats []string
	lists      []battery.PoolRef
}

func (b *leaseStub) Heartbeat(_ context.Context, leaseID string) (time.Time, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.heartbeats = append(b.heartbeats, leaseID)
	return b.heartbeat(leaseID)
}

func (b *leaseStub) ListLeases(_ context.Context, pool *battery.PoolRef) ([]*battery.LeaseRecord, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lists = append(b.lists, *pool)
	return b.listLeases(pool)
}

func (b *leaseStub) calls() (heartbeats, lists int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.heartbeats), len(b.lists)
}

// heartbeatAnswers makes a leaseStub whose Heartbeat answers t and err.
func heartbeatAnswers(t time.Time, err error) *leaseStub {
	return &leaseStub{heartbeat: func(string) (time.Time, error) { return t, err }}
}

// lists makes a leaseStub whose ListLeases answers recs and err.
func lists(err error, recs ...*battery.LeaseRecord) *leaseStub {
	return &leaseStub{listLeases: func(*battery.PoolRef) ([]*battery.LeaseRecord, error) { return recs, err }}
}

// micro is t as a MicroTime.
func micro(t time.Time) *metav1.MicroTime {
	m := metav1.NewMicroTime(t)
	return &m
}

// boundClaim is aClaim Bound to lease-1 on vm-1, with the Lease expiring
// at start plus 30s and the renewTime of its binding recorded as relayed.
func boundClaim() *batteryv1alpha1.MicroVMClaim {
	c := aClaim(batteryv1alpha1.ReleaseFinalizer)
	c.Spec.RenewTime = micro(start.Add(-10 * time.Second))
	exp := metav1.NewTime(start.Add(30 * time.Second))
	bound := metav1.NewTime(start.Add(-10 * time.Second))
	c.Status = batteryv1alpha1.MicroVMClaimStatus{
		Phase:             batteryv1alpha1.MicroVMClaimBound,
		LeaseID:           "lease-1",
		MicroVM:           &batteryv1alpha1.MicroVMReference{UID: "vm-1"},
		Host:              &batteryv1alpha1.HostReference{NodeName: "node-a"},
		BoundTime:         &bound,
		LeaseExpiresAt:    &exp,
		ObservedRenewTime: micro(start.Add(-10 * time.Second)),
		Conditions: []metav1.Condition{{
			Type:               batteryv1alpha1.ConditionBound,
			Status:             metav1.ConditionTrue,
			Reason:             batteryv1alpha1.ReasonBound,
			LastTransitionTime: bound,
		}},
	}
	return c
}

// renewed is boundClaim after its Holder renewed at start.
func renewed() *batteryv1alpha1.MicroVMClaim {
	c := boundClaim()
	c.Spec.RenewTime = micro(start)
	return c
}

// patched patches s and reads the claim back.
func patched(t *testing.T, s *claimscope.Scope, c client.Client) *batteryv1alpha1.MicroVMClaim {
	t.Helper()
	ctx := context.Background()
	if err := s.Patch(ctx); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	got := &batteryv1alpha1.MicroVMClaim{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(s.Claim), got); err != nil {
		t.Fatal(err)
	}
	return got
}

// wantExpired fails t unless got is Expired with Bound false and the reason
// LeaseExpired.
func wantExpired(t *testing.T, got *batteryv1alpha1.MicroVMClaim) {
	t.Helper()
	if got.Status.Phase != batteryv1alpha1.MicroVMClaimExpired {
		t.Errorf("phase = %q, want Expired", got.Status.Phase)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, batteryv1alpha1.ConditionBound)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != batteryv1alpha1.ReasonLeaseExpired {
		t.Errorf("Bound condition = %+v, want false with reason LeaseExpired", cond)
	}
	if got.Status.LeaseID == "" {
		t.Error("leaseID is gone, want it kept for the release")
	}
}

// wantBound fails t unless got is still Bound with the Bound condition
// true.
func wantBound(t *testing.T, got *batteryv1alpha1.MicroVMClaim) {
	t.Helper()
	if got.Status.Phase != batteryv1alpha1.MicroVMClaimBound {
		t.Errorf("phase = %q, want Bound", got.Status.Phase)
	}
	if !meta.IsStatusConditionTrue(got.Status.Conditions, batteryv1alpha1.ConditionBound) {
		t.Errorf("Bound condition = %+v, want true", got.Status.Conditions)
	}
}

//= docs/requirements/02-claims.md#renewal
//= type=test
//# While the `spec.renewTime` of a Bound claim differs from the
//# last `renewTime` the Claim Controller relayed for it, the Claim Controller
//# SHALL call battery's `Heartbeat` for the claim's Lease, and SHALL write
//# the expiry time battery returns and the `renewTime` it relayed to the
//# claim's status in one write.

//= docs/requirements/02-claims.md#renewal
//= type=test
//# The Claim Controller SHALL take a claim's Lease expiry time only
//# from battery's answer to `Heartbeat` or `ListLeases`, and SHALL NOT
//# compute it.

// TestRenewRelaysAPendingRenewal covers CL-010 and CL-011: a changed
// renewTime becomes one Heartbeat for the claim's Lease, and battery's
// expiry, however odd, is written with the relayed renewTime in one patch.
func TestRenewRelaysAPendingRenewal(t *testing.T) {
	// Not start plus any threshold: the expiry is battery's, not worked out.
	expires := start.Add(47 * time.Second)
	b := heartbeatAnswers(expires, nil)
	s, c := newScope(t, renewed(), b)

	res, err := Renew{Backoff: boundBackoff}.Reconcile(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stop || res.RequeueAfter != 47*time.Second {
		t.Errorf("result = %+v, want the chain stopped and a requeue at the expiry, 47s", res)
	}
	if len(b.heartbeats) != 1 || b.heartbeats[0] != "lease-1" {
		t.Errorf("Heartbeat calls = %v, want one for lease-1", b.heartbeats)
	}
	got := patched(t, s, c)
	wantBound(t, got)
	if got.Status.LeaseExpiresAt == nil || !got.Status.LeaseExpiresAt.Time.Equal(expires) {
		t.Errorf("leaseExpiresAt = %v, want battery's %v", got.Status.LeaseExpiresAt, expires)
	}
	if !got.Status.ObservedRenewTime.Equal(micro(start)) {
		t.Errorf("observedRenewTime = %v, want the relayed %v", got.Status.ObservedRenewTime, start)
	}
	if s.BatteryCall == nil || s.BatteryCall.Method != methodHeartbeat || s.BatteryCall.Err != nil {
		t.Errorf("BatteryCall = %+v, want a successful Heartbeat", s.BatteryCall)
	}
}

// TestRenewLeavesAClaimWithNoPendingRenewal: no changed renewTime, no
// Heartbeat; nor for a claim that is not Bound or is being deleted.
func TestRenewLeavesAClaimWithNoPendingRenewal(t *testing.T) {
	pending := renewed()
	pending.Status.Phase = batteryv1alpha1.MicroVMClaimPending
	expired := renewed()
	expired.Status.Phase = batteryv1alpha1.MicroVMClaimExpired
	for name, cl := range map[string]*batteryv1alpha1.MicroVMClaim{
		"no pending renewal": boundClaim(),
		"Pending":            pending,
		"Expired":            expired,
	} {
		t.Run(name, func(t *testing.T) {
			b := heartbeatAnswers(start, nil)
			s, _ := newScope(t, cl, b)
			res, err := Renew{Backoff: boundBackoff}.Reconcile(context.Background(), s)
			if err != nil || res != (claimscope.Result{}) {
				t.Errorf("Reconcile = %+v, %v, want the chain to go on", res, err)
			}
			if n, _ := b.calls(); n != 0 {
				t.Errorf("Heartbeat called %d times, want none", n)
			}
		})
	}
}

//= docs/requirements/02-claims.md#renewal
//= type=test
//# If battery answers a `Heartbeat` for a claim's Lease with
//# `NOT_FOUND`, or leaves the claim's Lease out of its answer to
//# `ListLeases`, then the Claim Controller SHALL set the claim's phase to
//# `Expired` and its condition `Bound` false with the reason `LeaseExpired`.

// TestRenewExpiresOnNotFound covers CL-012 from Heartbeat's side.
func TestRenewExpiresOnNotFound(t *testing.T) {
	b := heartbeatAnswers(time.Time{}, battery.ErrNotFound)
	s, c := newScope(t, renewed(), b)

	res, err := Renew{Backoff: boundBackoff}.Reconcile(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stop {
		t.Errorf("result = %+v, want the chain stopped", res)
	}
	got := patched(t, s, c)
	wantExpired(t, got)
	if !got.Status.LeaseExpiresAt.Time.Equal(start.Add(30 * time.Second)) {
		t.Errorf("leaseExpiresAt = %v, want the old one kept", got.Status.LeaseExpiresAt)
	}
}

//= docs/requirements/02-claims.md#renewal
//= type=test
//# If battery's `Heartbeat` for a Bound claim fails in transit,
//# then the Claim Controller SHALL keep the claim `Bound` with the Lease
//# expiry time already in its status, and SHALL retry the `Heartbeat` with
//# backoff while the claim is Bound.

// TestRenewKeepsTheClaimWhenHeartbeatFailsInTransit covers CL-015: the
// claim keeps its phase, expiry and pending renewal, and the retry waits
// as long as the claim has been out of sync, from Min up to Max.
func TestRenewKeepsTheClaimWhenHeartbeatFailsInTransit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		unsynced time.Duration // how long Synced has been false; 0 is true
		want     time.Duration
	}{
		{"first failure", 0, time.Second},
		{"failing for 8s", 8 * time.Second, 8 * time.Second},
		{"failing for an hour", time.Hour, 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cl := renewed()
			if tc.unsynced > 0 {
				cl.Status.Conditions = append(cl.Status.Conditions, metav1.Condition{
					Type:               batteryv1alpha1.ConditionSynced,
					Status:             metav1.ConditionFalse,
					Reason:             batteryv1alpha1.ReasonBatteryUnavailable,
					LastTransitionTime: metav1.NewTime(start.Add(-tc.unsynced)),
				})
			}
			b := heartbeatAnswers(time.Time{}, battery.ErrUnavailable)
			s, c := newScope(t, cl, b)

			res, err := Renew{Backoff: boundBackoff}.Reconcile(context.Background(), s)
			if err != nil {
				t.Fatal(err)
			}
			if !res.Stop || res.RequeueAfter != tc.want {
				t.Errorf("result = %+v, want the chain stopped and a retry after %s", res, tc.want)
			}
			got := patched(t, s, c)
			wantBound(t, got)
			if !got.Status.LeaseExpiresAt.Time.Equal(start.Add(30 * time.Second)) {
				t.Errorf("leaseExpiresAt = %v, want the one already there", got.Status.LeaseExpiresAt)
			}
			if !pendingRenewal(got) {
				t.Error("the renewal is no longer pending, so the retry would not relay it")
			}
		})
	}
}

// TestRenewReturnsOtherFailures: an error no requirement names is returned
// for the rate limiter (CL-041), and the claim is kept as it was.
func TestRenewReturnsOtherFailures(t *testing.T) {
	b := heartbeatAnswers(time.Time{}, battery.ErrInvalid)
	s, c := newScope(t, renewed(), b)

	_, err := Renew{Backoff: boundBackoff}.Reconcile(context.Background(), s)
	if !errors.Is(err, battery.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	got := patched(t, s, c)
	wantBound(t, got)
	if !pendingRenewal(got) {
		t.Error("the renewal is no longer pending")
	}
}

// TestRetryAfterUsesTheSyncedCondition: a Synced condition that is true
// gives the shortest wait.
func TestRetryAfterUsesTheSyncedCondition(t *testing.T) {
	cl := boundClaim()
	cl.Status.Conditions = append(cl.Status.Conditions, metav1.Condition{
		Type: batteryv1alpha1.ConditionSynced, Status: metav1.ConditionTrue, Reason: batteryv1alpha1.ReasonSynced,
		LastTransitionTime: metav1.NewTime(start.Add(-time.Hour)),
	})
	s, _ := newScope(t, cl, nil)
	s.Clock = clock.NewFake(start)
	if got := retryAfter(s, boundBackoff); got != time.Second {
		t.Errorf("retryAfter = %s, want 1s", got)
	}
}
