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
	"slices"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/controller/inventory"
)

// knownHosts checks that no Pool in battery names a Host battery does not
// hold: poolsNameKnownHosts in specs/quint/pools.qnt (#73). battery
// refuses a Pool that names a Host it does not hold (BA-082), and a Host
// that a Pool names (BA-083), so this is what keeps the Pool Controller's
// UpdatePool and the Inventory Controller's RemoveHost from failing.
func knownHosts(ctx context.Context, t *testing.T, bc battery.Client, admin battery.HostAdmin) {
	t.Helper()
	held, err := admin.ListHosts(ctx)
	if err != nil {
		t.Fatalf("ListHosts: %v", err)
	}
	holds := map[string]bool{}
	for _, h := range held {
		holds[h.Host.Name] = true
	}
	pools, err := bc.ListPools(ctx, "")
	if err != nil {
		t.Fatalf("ListPools: %v", err)
	}
	for _, p := range pools {
		for _, h := range p.Spec.FlintlockHosts {
			if !holds[h] {
				t.Errorf("Pool %s names %s, which battery does not hold", p.Spec.Ref, h)
			}
		}
	}
}

//= docs/requirements/04-inventory.md#applying
//= type=test
//# The Inventory Controller SHALL give the Pool Controller a Host
//# only once battery holds it uncordoned at the Node's address, and SHALL
//# take a Host from the Pool Controller before it cordons or removes the Host.

//= docs/requirements/04-inventory.md#admission
//= type=test
//# When battery holds a Host whose Node does not exist or is being
//# deleted, the Inventory Controller SHALL cordon the Host and remove it with
//# `RemoveHost`, and SHALL try again while battery refuses because a Pool
//# names the Host or a MicroVM is still on it.

// TestPoolsNameOnlyHostsBatteryHolds runs the Inventory Controller and the
// Pool Controller together against the fake battery, through the same
// HostSet: two Hosts join, and the Pool names them only once battery holds
// them; one is cordoned, and the Pool drops it; its Node is deleted, and
// battery removes it once the Pool no longer names it.
func TestPoolsNameOnlyHostsBatteryHolds(t *testing.T) {
	ctx := context.Background()
	admin, _ := startHostAdminFakeBattery(t)
	var bc battery.Client = admin
	hostNode := func(name, addr string) *corev1.Node {
		n := labelledNode(name, zoneA)
		n.Annotations = map[string]string{
			inventory.AnnotationReady:             strconv.FormatBool(true),
			inventory.AnnotationFlintlockdAddress: addr,
		}
		return n
	}
	pool := zoneAPool()
	k8s := newPoolFakeClient(t, hostNode(testHostA, "10.0.0.1:9090"), hostNode(testHostB, "10.0.0.2:9090"), pool)
	clk := clock.NewFake(poolTestEpoch)
	hosts := inventory.NewHostSet()
	inv := &InventoryReconciler{
		Client:  k8s,
		Admin:   admin,
		Hosts:   hosts,
		Mount:   &mountedCertificate{},
		Options: inventory.Options{SettleTime: 30 * time.Second, RemoveRetry: 10 * time.Second},
		Clock:   clk,
		// No client certificate Secret: nothing is reloaded here.
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
		knownHosts(ctx, t, bc, admin)
		return res.RequeueAfter
	}

	// Both Nodes join. Until battery holds them, the Pool names no Host.
	requeue := inventoryAfter(0)
	f.reconcile()
	f.wantPlaced()
	inventoryAfter(requeue) // settled: battery holds both
	f.reconcile()
	f.wantPlaced(testHostA, testHostB)
	knownHosts(ctx, t, bc, admin)

	// host-a is cordoned: once settled, battery cordons it and the Pool
	// drops it.
	update := func(name string, change func(*corev1.Node)) {
		t.Helper()
		n := &corev1.Node{}
		if err := k8s.Get(ctx, client.ObjectKey{Name: name}, n); err != nil {
			t.Fatal(err)
		}
		change(n)
		if err := k8s.Update(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	update(testHostA, func(n *corev1.Node) { n.Spec.Unschedulable = true })
	requeue = inventoryAfter(0)
	inventoryAfter(requeue)
	if got, _ := hosts.Hosts(); !slices.Equal(got, []inventory.Host{{Name: testHostB, Address: "10.0.0.2:9090"}}) {
		t.Errorf("published %v, want host-b alone", got)
	}
	st, err := admin.GetHost(ctx, testHostA)
	if err != nil || !st.Host.Cordoned {
		t.Fatalf("battery holds host-a as %+v (%v), want it cordoned", st, err)
	}
	f.reconcile()
	f.wantPlaced(testHostB)
	knownHosts(ctx, t, bc, admin)

	// host-a's Node is deleted. No Pool names its Host any more, but the
	// MicroVMs the Pool made there are still on it, so battery refuses to
	// remove it, and the Inventory Controller asks again after the remove
	// retry.
	n := &corev1.Node{}
	if err := k8s.Get(ctx, client.ObjectKey{Name: testHostA}, n); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Delete(ctx, n); err != nil {
		t.Fatal(err)
	}
	st, err = admin.GetHost(ctx, testHostA)
	if err != nil {
		t.Fatal(err)
	}
	if st.VMCount > 0 {
		if requeue := inventoryAfter(0); requeue != 10*time.Second {
			t.Errorf("RequeueAfter = %s, want the remove retry", requeue)
		}
		if _, err := admin.GetHost(ctx, testHostA); err != nil {
			t.Fatalf("battery removed a Host with MicroVMs on it: %v", err)
		}
		// The Pool goes, and battery deletes its MicroVMs.
		if err := bc.DeletePool(ctx, battery.PoolRef{Namespace: f.key.Namespace, Name: f.key.Name}); err != nil {
			t.Fatalf("DeletePool: %v", err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		inventoryAfter(10 * time.Second)
		if _, err := admin.GetHost(ctx, testHostA); errors.Is(err, battery.ErrNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("battery still holds the Host of a deleted Node")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := admin.GetHost(ctx, testHostB); err != nil {
		t.Errorf("battery lost host-b: %v", err)
	}
}
