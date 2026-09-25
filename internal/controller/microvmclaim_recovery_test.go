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
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/controller/claim"
	"github.com/phoban01/battery-operator/internal/fakebattery"
)

// releaseCounter is the Operator's battery client, counting ReleaseVM
// calls.
type releaseCounter struct {
	battery.Client
	releases atomic.Int32
}

func (r *releaseCounter) ReleaseVM(ctx context.Context, leaseID string) error {
	r.releases.Add(1)
	return r.Client.ReleaseVM(ctx, leaseID)
}

// recoveredClaim is testClaim named name, Bound to battery's claim l, with
// no renewal pending and expires as the Lease expiry in its status.
func recoveredClaim(name string, l *battery.Claim, expires time.Time) *batteryv1alpha1.MicroVMClaim {
	bound := metav1.NewTime(time.Now().Add(-time.Minute))
	renew := metav1.NewMicroTime(bound.Time)
	exp := metav1.NewTime(expires)
	c := testClaim()
	c.Name = name
	c.Generation = 1
	c.Finalizers = []string{batteryv1alpha1.ReleaseFinalizer}
	c.Spec.RenewTime = &renew
	c.Status = batteryv1alpha1.MicroVMClaimStatus{
		Phase:             batteryv1alpha1.MicroVMClaimBound,
		LeaseID:           l.LeaseID,
		MicroVM:           &batteryv1alpha1.MicroVMReference{UID: l.VMUID},
		Host:              &batteryv1alpha1.HostReference{NodeName: l.Host.Name},
		BoundTime:         &bound,
		LeaseExpiresAt:    &exp,
		ObservedRenewTime: &renew,
		Conditions: []metav1.Condition{{
			Type:               batteryv1alpha1.ConditionBound,
			Status:             metav1.ConditionTrue,
			Reason:             batteryv1alpha1.ReasonBound,
			LastTransitionTime: bound,
		}},
	}
	return c
}

//= docs/requirements/02-claims.md#recovery
//= type=test
//# When the Claim Controller starts, and when its connection to
//# battery is restored, the Claim Controller SHALL reconcile every Bound claim that is not being deleted against
//# battery's Leases.

//= docs/requirements/02-claims.md#recovery
//= type=test
//# The Claim Controller SHALL NOT release a Lease that battery
//# holds and no claim records.

// TestMicroVMClaimRecoveryAgainstTheFakeBattery is #12's "done when": the
// Claim Controller starts against a fake battery whose state has moved on
// while the Operator was away, and the claims converge.
//
//   - "kept" records lease 1, which battery holds with a later expiry than
//     the claim's status (a Heartbeat answer lost in the crash, #57): it
//     stays Bound with battery's expiry.
//   - "lost" records lease 2, which battery no longer holds: it goes
//     Expired, although its status has an hour to run.
//   - Lease 3 is an orphan that no claim records: it is left alone, and
//     nothing calls ReleaseVM (CL-031).
//
// Then battery goes away and comes back having lost lease 1 from its
// answers: the watcher's new subscription recovers again, and "kept" goes
// Expired too. The claims' MicroVM deletions that battery's Events stream
// reports are not wired into the reconciler, so recovery alone expires
// them.
func TestMicroVMClaimRecoveryAgainstTheFakeBattery(t *testing.T) {
	fb, conn := startClaimFakeBattery(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	ref := battery.PoolRef{Namespace: testClaimKey.Namespace, Name: testClaim().Spec.PoolRef.Name}
	if _, err := conn.CreatePool(ctx, battery.PoolSpec{
		Ref:                      ref,
		Template:                 &types.MicroVMSpec{Namespace: "ci", Vcpu: 1, MemoryInMb: 256},
		Size:                     3,
		FlintlockHosts:           []string{testHostA},
		Replenishment:            battery.ReplenishmentStrategy{Type: poolmgrv1.ReplenishmentStrategyType_IMMEDIATE_ON_LEASE},
		HookFailurePolicy:        poolmgrv1.HookFailurePolicy_DELETE_AND_REPLACE,
		HeartbeatExpiryThreshold: time.Hour,
	}); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	var leases [3]*battery.Claim
	for i := range leases {
		waitForClaimTest(t, "a MicroVM AVAILABLE in the Pool", func() bool {
			p, err := conn.GetPool(ctx, ref)
			return err == nil && p.Status.Available > 0
		})
		l, err := conn.ClaimVM(ctx, ref)
		if err != nil {
			t.Fatalf("ClaimVM: %v", err)
		}
		leases[i] = l
	}
	kept, lost, orphan := leases[0], leases[1], leases[2]

	// What happened while the Operator was away: lease 1 was renewed and
	// the answer lost, and lease 2 was released.
	renewed, err := conn.Heartbeat(ctx, kept.LeaseID)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	waitForClaimTest(t, "battery to release lease 2", func() bool {
		return errors.Is(conn.ReleaseVM(ctx, lost.LeaseID), battery.ErrNotFound)
	})

	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := batteryv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(
			recoveredClaim("kept", kept, renewed.Add(-10*time.Minute)),
			recoveredClaim("lost", lost, time.Now().Add(time.Hour)),
		).
		WithStatusSubresource(&batteryv1alpha1.MicroVMClaim{}).
		WithIndex(&batteryv1alpha1.MicroVMClaim{}, claim.MicroVMUIDIndex, func(o client.Object) []string {
			return claim.MicroVMUID(o.(*batteryv1alpha1.MicroVMClaim))
		}).
		Build()

	// The Operator starts: a new reconciler and watcher.
	bc := &releaseCounter{Client: conn}
	r := &MicroVMClaimReconciler{
		Client:    c,
		Scheme:    s,
		APIReader: c,
		Battery:   bc,
		Clock:     clock.Real{},
		Backoff:   claim.Backoff{Min: time.Second, Max: time.Minute},
		recovered: &claim.RecoveredLeases{},
	}
	out := make(chan event.GenericEvent, 16)
	w := &claimEvents{
		Reader: c,
		// Not the reconciler's: see the test's comment.
		Deleted: &claim.DeletedVMs{},
		Out:     out,
		Recovery: &claimRecovery{
			Battery: bc,
			Reader:  c,
			Leases:  r.recovered,
			Out:     out,
			Log:     testr.New(t),
		},
		Log: testr.New(t),
	}
	wctx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- claimEventsOn(bc, w).Start(wctx) }()
	defer func() {
		stop()
		if err := <-done; err != nil {
			t.Errorf("Start = %v", err)
		}
	}()

	// reconcileSent reconciles each claim the watcher sends until every one
	// of want has come, as the controller's channel source would.
	reconcileSent := func(want ...string) {
		t.Helper()
		missing := map[string]bool{}
		for _, n := range want {
			missing[n] = true
		}
		timeout := time.After(time.Minute)
		for len(missing) > 0 {
			select {
			case e := <-out:
				if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(e.Object)}); err != nil {
					t.Fatalf("Reconcile %s: %v", e.Object.GetName(), err)
				}
				delete(missing, e.Object.GetName())
			case <-timeout:
				t.Fatalf("the watcher did not send %v", missing)
			}
		}
	}
	get := func(name string) *batteryv1alpha1.MicroVMClaim {
		t.Helper()
		got := &batteryv1alpha1.MicroVMClaim{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: "ci", Name: name}, got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	holds := func(id string) bool {
		for _, l := range fb.Leases() {
			if l.LeaseID == id {
				return true
			}
		}
		return false
	}

	reconcileSent("kept", "lost")
	got := get("kept")
	if got.Status.Phase != batteryv1alpha1.MicroVMClaimBound {
		t.Errorf("kept: phase = %q, want Bound", got.Status.Phase)
	}
	if exp := got.Status.LeaseExpiresAt; exp == nil || exp.Unix() != renewed.Unix() {
		t.Errorf("kept: leaseExpiresAt = %v, want battery's %v", exp, renewed)
	}
	if got := get("lost"); got.Status.Phase != batteryv1alpha1.MicroVMClaimExpired {
		t.Errorf("lost: phase = %q, want Expired", got.Status.Phase)
	}
	if !holds(orphan.LeaseID) {
		t.Error("battery no longer holds the orphan, want it left alone")
	}

	// battery goes away, and comes back no longer listing lease 1.
	for len(out) > 0 {
		<-out
	}
	fb.SetFaults(fakebattery.Faults{UnavailableFor: 300 * time.Millisecond, DropEventsStream: true, RefuseHeartbeats: true})
	reconcileSent("kept")
	if got := get("kept"); got.Status.Phase != batteryv1alpha1.MicroVMClaimExpired {
		t.Errorf("kept after battery came back: phase = %q, want Expired", got.Status.Phase)
	}

	if !holds(orphan.LeaseID) {
		t.Error("battery no longer holds the orphan after the second recovery, want it left alone")
	}
	if n := bc.releases.Load(); n != 0 {
		t.Errorf("ReleaseVM called %d times, want none", n)
	}
}

// flakyLeases is a battery.Client whose Subscribe always gives a stream
// that ends at once, and whose first ListLeases fails in transit. It counts
// both.
type flakyLeases struct {
	battery.Client
	subscribes, lists atomic.Int32
}

func (f *flakyLeases) Subscribe(context.Context, battery.EventFilter) (battery.EventStream, error) {
	f.subscribes.Add(1)
	return &scriptedStream{}, nil
}

func (f *flakyLeases) ListLeases(context.Context, *battery.PoolRef) ([]*battery.LeaseRecord, error) {
	if f.lists.Add(1) == 1 {
		return nil, battery.ErrUnavailable
	}
	return nil, nil
}

// TestClaimEventsRecoversAfterEachSubscription: the watcher recovers after
// every subscription, and a recovery that fails drops the subscription and
// is made again with the next one, so that a connection is never followed
// unrecovered (CL-030).
func TestClaimEventsRecoversAfterEachSubscription(t *testing.T) {
	s := runtime.NewScheme()
	if err := batteryv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(claimOn(testClaimName, testClaimVM)).Build()
	b := &flakyLeases{}
	out := make(chan event.GenericEvent, 16)
	leases := &claim.RecoveredLeases{}
	w := &claimEvents{
		Reader:   c,
		Deleted:  &claim.DeletedVMs{},
		Out:      out,
		Recovery: &claimRecovery{Battery: b, Reader: c, Leases: leases, Out: out, Log: testr.New(t)},
		Log:      testr.New(t),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- claimEventsOn(b, w).Start(ctx) }()

	select {
	case e := <-out:
		if e.Object.GetName() != testClaimName {
			t.Errorf("sent %s, want %s", e.Object.GetName(), testClaimName)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no claim was sent by a recovery")
	}
	if n := b.lists.Load(); n < 2 {
		t.Errorf("ListLeases called %d times before the claim was sent, want the failed one and another", n)
	}
	waitForClaimTest(t, "a third subscription", func() bool { return b.subscribes.Load() >= 3 })
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Start = %v", err)
	}
}
