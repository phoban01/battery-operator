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
	"slices"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/batterysidecar"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/controller/inventory"
)

// knownHostsRestarter checks, at each restart of battery, that no Pool in
// battery names a Host the new configuration lacks: the property #73
// found missing, poolsNameKnownHosts in specs/quint/pools.qnt.
type knownHostsRestarter struct {
	t        *testing.T
	battery  battery.Client
	restarts int
}

func (r *knownHostsRestarter) Restart(ctx context.Context, want batterysidecar.Mounts) error {
	r.t.Helper()
	r.restarts++
	f, err := batterysidecar.Parse(want.Config)
	if err != nil {
		r.t.Fatalf("parsing battery's configuration: %v", err)
	}
	known := map[string]bool{}
	for _, h := range f.Hosts {
		known[h.Name] = true
	}
	pools, err := r.battery.ListPools(ctx, "")
	if err != nil {
		r.t.Fatalf("ListPools: %v", err)
	}
	for _, p := range pools {
		for _, h := range p.Spec.FlintlockHosts {
			if !known[h] {
				r.t.Errorf("restart %d: battery restarts without %s, which Pool %s still names", r.restarts, h, p.Spec.Ref)
			}
		}
	}
	return nil
}

//= docs/requirements/04-inventory.md#applying
//= type=test
//# When a restart of battery would remove a Host, the Inventory
//# Controller SHALL first give the Pool Controller the Hosts that remain,
//# and SHALL restart battery only once no Pool in battery names the removed
//# Host in its `flintlock_hosts` or the configured drain timeout has passed.

// TestPoolsNameOnlyHostsBatteryKnowsAcrossRestarts runs the Inventory
// Controller and the Pool Controller together against the fake battery,
// through the same HostSet (#73): two Hosts join, and the Pool names them
// only after the restart that gives battery them; one is cordoned, and the
// Pool drops it before the restart that takes it away.
func TestPoolsNameOnlyHostsBatteryKnowsAcrossRestarts(t *testing.T) {
	ctx := context.Background()
	bc := startPoolFakeBattery(t)
	raw, err := batterysidecar.Render(nil)
	if err != nil {
		t.Fatal(err)
	}
	cmKey := client.ObjectKey{Namespace: testNamespace, Name: batterysidecar.DefaultConfigMap}
	hostNode := func(name, addr string) *corev1.Node {
		n := labelledNode(name, zoneA)
		n.Annotations = map[string]string{
			inventory.AnnotationReady:             strconv.FormatBool(true),
			inventory.AnnotationFlintlockdAddress: addr,
		}
		return n
	}
	pool := zoneAPool()
	k8s := newPoolFakeClient(t,
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: cmKey.Namespace, Name: cmKey.Name},
			Data:       map[string]string{batterysidecar.ConfigKey: string(raw)},
		},
		hostNode(testHostA, "10.0.0.1:9090"), hostNode(testHostB, "10.0.0.2:9090"), pool)
	clk := clock.NewFake(poolTestEpoch)
	hosts := inventory.NewHostSet()
	restarter := &knownHostsRestarter{t: t, battery: bc}
	inv := &InventoryReconciler{
		Client:    k8s,
		Store:     inventory.ConfigMapStore{Reader: k8s, Writer: k8s, Key: cmKey},
		Restarter: restarter,
		Hosts:     hosts,
		Pools:     bc,
		Options:   inventory.Options{SettleTime: 30 * time.Second, RestartWindow: time.Minute, DrainTimeout: 10 * time.Second},
		Clock:     clk,
		// No client certificate Secret: nothing is renewed here.
		ClientSecret: batteryClientSecret,
		Secrets:      k8s,
	}
	pools := &PoolReconciler{Client: k8s, Battery: bc, Hosts: hosts, Clock: clk}
	f := &placementFixture{t: t, ctx: ctx, k8s: k8s, bc: bc, r: pools, key: client.ObjectKeyFromObject(pool)}

	// inventory reconciles once, advancing the clock to its requeue first,
	// and returns its requeue.
	inventoryAfter := func(d time.Duration) time.Duration {
		t.Helper()
		clk.Advance(d)
		res, err := inv.Reconcile(ctx, ctrl.Request{})
		if err != nil {
			t.Fatalf("inventory Reconcile: %v", err)
		}
		return res.RequeueAfter
	}

	// Both Nodes join. Until battery restarts with them, the Pool names no
	// Host.
	requeue := inventoryAfter(0)
	f.reconcile()
	f.wantPlaced()
	requeue = inventoryAfter(requeue) // settled: the window opens
	f.reconcile()
	f.wantPlaced()
	inventoryAfter(requeue) // the window closes: battery restarts
	if restarter.restarts != 1 {
		t.Fatalf("restarts = %d, want 1", restarter.restarts)
	}
	f.reconcile()
	f.wantPlaced(testHostA, testHostB)

	// host-a is cordoned. When the window closes, the restart waits for the
	// Pool to drop it.
	n := &corev1.Node{}
	if err := k8s.Get(ctx, client.ObjectKey{Name: testHostA}, n); err != nil {
		t.Fatal(err)
	}
	n.Spec.Unschedulable = true
	if err := k8s.Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	requeue = inventoryAfter(0)
	requeue = inventoryAfter(requeue)
	requeue = inventoryAfter(requeue)
	if restarter.restarts != 1 {
		t.Fatal("battery restarted without host-a while the Pool still names it")
	}
	f.reconcile()
	f.wantPlaced(testHostB)
	inventoryAfter(requeue)
	if restarter.restarts != 2 {
		t.Fatalf("restarts = %d, want 2", restarter.restarts)
	}
	if got, _ := hosts.Hosts(); !slices.Equal(got, []batterysidecar.Host{{Name: testHostB, Address: "10.0.0.2:9090"}}) {
		t.Errorf("published %v, want host-b alone", got)
	}
}
