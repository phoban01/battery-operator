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
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/fakebattery"
	"github.com/phoban01/battery-operator/internal/fakeflintlock"
)

// wantPoolReady checks the reason, and the message where want is not empty,
// of the Pool's Ready condition.
func wantPoolReady(t *testing.T, pool *batteryv1alpha1.Pool, status metav1.ConditionStatus, reason, message string) {
	t.Helper()
	ready := readyCondition(pool)
	if ready == nil {
		t.Fatalf("no Ready condition; want %s %s", status, reason)
	}
	if ready.Status != status || ready.Reason != reason {
		t.Fatalf("Ready = %s %s (%q), want %s %s", ready.Status, ready.Reason, ready.Message, status, reason)
	}
	if message != "" && ready.Message != message {
		t.Fatalf("Ready message = %q, want %q", ready.Message, message)
	}
}

//= docs/requirements/03-pools.md#refill
//= type=test
//# While a Pool's shortfall is unchanged since the Pool
//# Controller saw the Pool stalled after an `UpdatePool` of PO-036, and the
//# sum of its available, leased and provisioning MicroVMs is less than its
//# size, the Pool Controller SHALL set the Pool's condition `Ready` false
//# with the reason `NotFilling`.

//= docs/requirements/03-pools.md#refill
//= type=test
//# The Pool Controller SHALL give the condition `Ready` with the
//# reason `NotFilling` a message that gives the Pool's shortfall, the number
//# of `UpdatePool` calls of PO-036 since the shortfall last changed, the time
//# of the next one while the Pool is stalled, and the log of the battery
//# container as the place to find the cause.

// TestAPoolThatReseedsDoNotFillSaysSo covers PO-039 and PO-040: Ready
// says NotFilling once a reseed has gone by and the Pool is stalled again
// with the same shortfall, keeps it while battery provisions for the next
// reseed, and goes back to the reason of PO-022 when the shortfall changes
// or reaches zero.
func TestAPoolThatReseedsDoNotFillSaysSo(t *testing.T) {
	const cause = "battery does not report why provisioning fails: see the log of the battery container"
	h := newReseedHarness(t, finalizedPool())

	// Stalled, but no reseed has gone by: the usual reason.
	h.wantStep(0, 0, time.Minute)
	wantPoolReady(t, h.pool, metav1.ConditionFalse, PoolReasonBelowSize, "")

	// The first reseed: it has not yet had the chance to fail.
	h.wantStep(time.Minute, 1, 2*time.Minute)
	wantPoolReady(t, h.pool, metav1.ConditionFalse, PoolReasonBelowSize, "")

	// Stalled again with the same shortfall: the reseed made no
	// difference.
	h.wantStep(time.Second, 1, 2*time.Minute-time.Second)
	wantPoolReady(t, h.pool, metav1.ConditionFalse, PoolReasonNotFilling,
		"The Pool is 3 MicroVMs short after 1 reseed; the next is due at 2026-09-24T12:03:00Z. "+cause)

	// battery provisions for a reseed: the reason stays, with no due time.
	h.setStatus(battery.PoolStatus{Provisioning: 1})
	h.wantStep(time.Second, 1, 2*time.Minute)
	wantPoolReady(t, h.pool, metav1.ConditionFalse, PoolReasonNotFilling,
		"The Pool is 3 MicroVMs short after 1 reseed; battery is provisioning for the last one. "+cause)

	// Provisioning makes up the size: Ready is true, as PO-022 says.
	h.setStatus(battery.PoolStatus{Provisioning: 3})
	h.wantStep(time.Second, 1, 2*time.Minute)
	wantPoolReady(t, h.pool, metav1.ConditionTrue, PoolReasonAtSize, "")

	// The provisioning fails: stalled again, and not filling. The wait
	// counts from the new stall.
	h.setStatus(battery.PoolStatus{})
	h.wantStep(time.Second, 1, 2*time.Minute)
	wantPoolReady(t, h.pool, metav1.ConditionFalse, PoolReasonNotFilling,
		"The Pool is 3 MicroVMs short after 1 reseed; the next is due at 2026-09-24T12:03:04Z. "+cause)

	// The second reseed counts.
	h.wantStep(2*time.Minute, 2, 4*time.Minute)
	wantPoolReady(t, h.pool, metav1.ConditionFalse, PoolReasonNotFilling,
		"The Pool is 3 MicroVMs short after 2 reseeds; the next is due at 2026-09-24T12:07:04Z. "+cause)

	// The shortfall changes: back to BelowSize, and a reseed must go by
	// again before the Pool is reported as not filling.
	h.setStatus(battery.PoolStatus{Available: 1})
	h.wantStep(time.Second, 2, time.Minute)
	wantPoolReady(t, h.pool, metav1.ConditionFalse, PoolReasonBelowSize, "")
	h.wantStep(time.Minute, 3, 2*time.Minute)
	wantPoolReady(t, h.pool, metav1.ConditionFalse, PoolReasonBelowSize, "")
	h.wantStep(time.Second, 3, 2*time.Minute-time.Second)
	wantPoolReady(t, h.pool, metav1.ConditionFalse, PoolReasonNotFilling,
		"The Pool is 2 MicroVMs short after 1 reseed; the next is due at 2026-09-24T12:06:05Z. "+cause)

	// The shortfall reaches zero: at size.
	h.setStatus(battery.PoolStatus{Available: 3})
	h.wantStep(time.Second, 3, 0)
	wantPoolReady(t, h.pool, metav1.ConditionTrue, PoolReasonAtSize, "")
}

// hookFailedEvent is a VM_HOOK_FAILED for the test Pool.
func hookFailedEvent(t *testing.T, id int64, uid string, payload map[string]string) *battery.Event {
	t.Helper()
	ev := &battery.Event{
		ID:    id,
		Pool:  battery.PoolRef{Namespace: testPoolNamespace, Name: testPoolName},
		VMUID: uid,
		Type:  poolmgrv1.EventType_VM_HOOK_FAILED,
	}
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		ev.Payload = raw
	}
	return ev
}

// recorded drains the fake recorder's Events.
func recorded(r *events.FakeRecorder) []string {
	var got []string
	for {
		select {
		case e := <-r.Events:
			got = append(got, e)
		default:
			return got
		}
	}
}

//= docs/requirements/03-pools.md#pool-status
//= type=test
//# When battery's `Events` stream reports a `VM_HOOK_FAILED`
//# event for a Pool, the Pool Controller SHALL record on the Pool a
//# Kubernetes Event of type `Warning` with the reason `HookFailed`, whose
//# message names the MicroVM and, where the event's payload gives them, the
//# hook and its error.

// TestAFailedHookIsRecordedOnThePool covers PO-041: each VM_HOOK_FAILED
// becomes a Warning Event on the Pool that names the MicroVM, and the
// hook and its error when the payload gives them, as the fake battery's
// does; battery v0.3.3's has no payload. A replayed event is not recorded
// again, and other events, and events for a Pool the cluster does not
// hold, record nothing.
func TestAFailedHookIsRecordedOnThePool(t *testing.T) {
	ctx := context.Background()
	pool := finalizedPool()
	pool.UID = "pool-uid-1"
	rec := events.NewFakeRecorder(16)
	pe := newPoolEvents(newPoolFakeClient(t, pool), logr.Discard())
	pe.HookEvents = &poolHookEvents{Pools: pe.Pools, Recorder: rec, Log: logr.Discard()}
	handle := func(ev *battery.Event) {
		t.Helper()
		pe.handle(ctx, ev)
		<-pe.Requests()
	}

	handle(hookFailedEvent(t, 1, "vm-1", map[string]string{"hook": "create", "error": "boot failed"}))
	handle(hookFailedEvent(t, 2, "vm-2", nil))
	// battery replays its outbox on a new subscription.
	handle(hookFailedEvent(t, 1, "vm-1", map[string]string{"hook": "create", "error": "boot failed"}))
	handle(hookFailedEvent(t, 2, "vm-2", nil))
	handle(&battery.Event{ID: 3, Pool: battery.PoolRef{Namespace: testPoolNamespace, Name: testPoolName},
		VMUID: "vm-3", Type: poolmgrv1.EventType_VM_AVAILABLE})
	other := hookFailedEvent(t, 4, "vm-4", nil)
	other.Pool.Name = "not-in-the-cluster"
	handle(other)

	want := []string{
		"Warning HookFailed The create hook failed on MicroVM vm-1: boot failed",
		"Warning HookFailed A hook failed on MicroVM vm-2; battery does not report why: " +
			"see the log of the battery container",
	}
	got := recorded(rec)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("Events = %q, want %q", got, want)
	}

	// The same Pool deleted and created again starts again.
	pool2 := finalizedPool()
	pool2.UID = "pool-uid-2"
	pe.HookEvents.Pools = newPoolFakeClient(t, pool2)
	handle(hookFailedEvent(t, 1, "vm-5", nil))
	if got := recorded(rec); len(got) != 1 || !strings.Contains(got[0], "vm-5") {
		t.Errorf("Events for the new Pool = %q, want one for vm-5", got)
	}
}

// TestAPoolWhoseProvisioningKeepsFailingSaysSoAgainstTheFakeBattery drives
// the Pool Controller against the fake battery seeding once, as battery
// v0.3.3 does (TD-007), with every boot failing in the fake flintlockd:
// the failed hooks become Events on the Pool (PO-041), Ready says
// NotFilling once a reseed has failed too (PO-039, PO-040), and it goes
// once the Pool fills.
func TestAPoolWhoseProvisioningKeepsFailingSaysSoAgainstTheFakeBattery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fl := fakeflintlock.New(fakeflintlock.Config{Name: testHostA})
	t.Cleanup(func() { _ = fl.Close() })
	flConn, err := fl.Conn()
	if err != nil {
		t.Fatalf("fake flintlockd: %v", err)
	}
	t.Cleanup(func() { _ = flConn.Close() })
	hosts := fakebattery.NewHosts()
	hosts.Add(testHostA, testHostA+":9090", flConn)
	bc, fb := startPoolFakeBatteryWith(t, fakebattery.Config{Hosts: hosts, SeedOnce: true})

	pool := finalizedPool()
	pool.Spec.Size = 2
	k8s := newPoolFakeClient(t, pool, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: testHostA}})
	clk := clock.NewFake(poolTestEpoch)
	r := &PoolReconciler{Client: k8s, Battery: bc, Hosts: newStubHosts(testHostA), Clock: clk}
	key := client.ObjectKeyFromObject(pool)
	reconcile := func() *batteryv1alpha1.Pool {
		t.Helper()
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		got := &batteryv1alpha1.Pool{}
		if err := k8s.Get(ctx, key, got); err != nil {
			t.Fatalf("Get: %v", err)
		}
		return got
	}

	// The Pool Controller's side of the Events stream, with a recorder.
	// The test reconciles by hand, on the fake clock, so the requests are
	// only drained.
	rec := events.NewFakeRecorder(64)
	pe := newPoolEvents(k8s, logr.Discard())
	pe.HookEvents = &poolHookEvents{Pools: k8s, Recorder: rec, Log: logr.Discard()}
	stream := NewBatteryEvents(bc, time.Hour)
	stream.add(pe)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-pe.Requests():
			}
		}
	}()

	fl.SetFaults(fakeflintlock.Faults{CreateFails: true})
	reconcile()
	startBatteryEvents(t, stream)
	var hookEvents []string
	waitHookEvents := func(n int) {
		t.Helper()
		eventually(t, fmt.Sprintf("%d HookFailed Events", n), func() (bool, string) {
			hookEvents = append(hookEvents, recorded(rec)...)
			return len(hookEvents) >= n, fmt.Sprintf("%q", hookEvents)
		})
		eventually(t, "the failed MicroVMs deleted", func() (bool, string) {
			return len(fb.VMs()) == 0, fmt.Sprintf("%v", fb.VMs())
		})
	}
	waitHookEvents(int(pool.Spec.Size))
	for _, e := range hookEvents {
		if !strings.HasPrefix(e, "Warning HookFailed The create hook failed on MicroVM ") {
			t.Errorf("Event %q, want a Warning HookFailed for the create hook", e)
		}
	}

	// The seed failed; the first reseed fails too.
	clk.Advance(time.Minute)
	if ready := readyCondition(reconcile()); ready.Reason == PoolReasonNotFilling {
		t.Fatalf("Ready = %+v at the first reseed, before it has failed", ready)
	}
	waitHookEvents(2 * int(pool.Spec.Size))
	got := reconcile()
	wantPoolReady(t, got, metav1.ConditionFalse, PoolReasonNotFilling, "")
	if msg := readyCondition(got).Message; !strings.HasPrefix(msg, "The Pool is 2 MicroVMs short after 1 reseed; ") {
		t.Errorf("Ready message = %q", msg)
	}

	// The Hosts boot again: the next reseed fills the Pool.
	fl.SetFaults(fakeflintlock.Faults{})
	clk.Advance(2 * time.Minute)
	reconcile()
	ref := poolRef(pool)
	eventually(t, "the Pool filled", func() (bool, string) {
		held, err := bc.GetPool(ctx, ref)
		if err != nil {
			return false, err.Error()
		}
		return held.Status.Available == pool.Spec.Size, fmt.Sprintf("%+v", held.Status)
	})
	wantPoolReady(t, reconcile(), metav1.ConditionTrue, PoolReasonAtSize, "")
}
