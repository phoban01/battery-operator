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
// does; battery v0.4.0's has no payload. A replayed event is not recorded
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

//= docs/requirements/03-pools.md#refill
//= type=test
//# While the Pool Controller has seen a Pool stalled with its
//# current shortfall at two reconciles at least one minute apart, and the
//# sum of the Pool's available, leased and provisioning MicroVMs is less
//# than its size, the Pool Controller SHALL set the Pool's condition `Ready`
//# false with the reason `NotFilling`.

// TestAPoolWhoseProvisioningKeepsFailingSaysSoAgainstTheFakeBattery drives
// the Pool Controller against the fake battery, whose tick provisions the
// Pool's shortfall again and again as battery v0.4.0 does (BA-075), with
// every create failing in the fake flintlockd: the failed hooks become
// Events on the Pool (PO-041), Ready says NotFilling once the Pool has
// been stalled for a minute (PO-045), and the Pool fills on battery's
// next tick once the Host creates again, with nothing sent by the
// Operator.
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
	bc, _ := startPoolFakeBatteryWith(t, fakebattery.Config{Hosts: hosts, ReconcileInterval: 20 * time.Millisecond})

	pool := finalizedPool()
	pool.Spec.Size = 2
	k8s := newPoolFakeClient(t, pool, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: testHostA}})
	clk := clock.NewFake(poolTestEpoch)
	r := &PoolReconciler{Client: k8s, Battery: bc, Hosts: newStubHosts(testHostA), Clock: clk}
	key := client.ObjectKeyFromObject(pool)
	reconcile := func() (*batteryv1alpha1.Pool, ctrl.Result) {
		t.Helper()
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		got := &batteryv1alpha1.Pool{}
		if err := k8s.Get(ctx, key, got); err != nil {
			t.Fatalf("Get: %v", err)
		}
		return got, res
	}

	// The Pool Controller's side of the Events stream, with a recorder.
	// The test reconciles by hand, on the fake clock, so the requests are
	// only drained.
	rec := events.NewFakeRecorder(1024)
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

	// Every create fails, and the fake's tick tries again and again.
	fl.SetFaults(fakeflintlock.Faults{CreateFails: true})
	reconcile()
	startBatteryEvents(t, stream)
	var hookEvents []string
	eventually(t, fmt.Sprintf("%d HookFailed Events", pool.Spec.Size), func() (bool, string) {
		hookEvents = append(hookEvents, recorded(rec)...)
		return len(hookEvents) >= int(pool.Spec.Size), fmt.Sprintf("%q", hookEvents)
	})
	for _, e := range hookEvents {
		if !strings.HasPrefix(e, "Warning HookFailed The create hook failed on MicroVM ") {
			t.Errorf("Event %q, want a Warning HookFailed for the create hook", e)
		}
	}

	// A reconcile that finds the Pool stalled asks to come back when the
	// minute ends. One that finds the fake provisioning, between a create
	// and its failure, does not, and does not count.
	eventually(t, "a reconcile that finds the Pool stalled", func() (bool, string) {
		got, res := reconcile()
		return res.RequeueAfter > 0, fmt.Sprintf("Ready = %+v", readyCondition(got))
	})
	got, _ := reconcile()
	if ready := readyCondition(got); ready.Reason == PoolReasonNotFilling {
		t.Fatalf("Ready = %+v before the minute has passed", ready)
	}

	// A minute later the Pool is still stalled, with the same shortfall.
	clk.Advance(time.Minute)
	eventually(t, "Ready says NotFilling", func() (bool, string) {
		got, _ = reconcile()
		return readyCondition(got).Reason == PoolReasonNotFilling, fmt.Sprintf("Ready = %+v", readyCondition(got))
	})
	if msg := readyCondition(got).Message; !strings.HasPrefix(msg, "The Pool has been 2 MicroVMs short since ") {
		t.Errorf("Ready message = %q", msg)
	}

	// The Host creates again: the fake's next tick fills the Pool.
	fl.SetFaults(fakeflintlock.Faults{})
	ref := poolRef(pool)
	eventually(t, "the Pool filled", func() (bool, string) {
		held, err := bc.GetPool(ctx, ref)
		if err != nil {
			return false, err.Error()
		}
		return held.Status.Available == pool.Spec.Size, fmt.Sprintf("%+v", held.Status)
	})
	got, res := reconcile()
	wantPoolReady(t, got, metav1.ConditionTrue, PoolReasonAtSize, "")
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter for the full Pool = %s, want none", res.RequeueAfter)
	}
}
