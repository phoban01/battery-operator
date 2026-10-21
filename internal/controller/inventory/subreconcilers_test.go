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

package inventory

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/execagent"
)

func TestReportAnnotationsMatchTheExecAgent(t *testing.T) {
	if AnnotationReady != execagent.AnnotationReady || AnnotationFlintlockdAddress != execagent.AnnotationFlintlockdAddress {
		t.Error("the Inventory Controller reads other annotations than the Exec Agent writes")
	}
}

//= docs/requirements/04-inventory.md#admission
//= type=test
//# The Inventory Controller SHALL treat a Node as a Host only while
//# the Node's Node report says the Host is ready and the Node is schedulable.

//= docs/requirements/04-inventory.md#admission
//= type=test
//# The Inventory Controller SHALL give battery each Host under the
//# Node's name, at the `flintlockd` address the Host's Node report gives.

// TestAdmit checks which Nodes are Hosts, and at which address.
func TestAdmit(t *testing.T) {
	deleting := host("node-d", "10.0.0.4:9090")
	deleting.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	deleting.Finalizers = []string{"example.com/hold"}
	nodes := []*corev1.Node{
		host(nodeA, addrA),
		host(nodeB, addrB),
		host("node-c", addrC),
		deleting,
		host("node-e", ""),
		{ObjectMeta: metav1.ObjectMeta{Name: "node-f"}},
	}
	cordon(nodes[1])
	notReady(nodes[2])
	h := newHarness(t, nodes...)
	s := h.scope()
	if _, err := (Admit{}).Reconcile(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if want := (Hosts{nodeA: addrA}); !s.Observed.Equal(want) {
		t.Errorf("Observed = %v, want %v", s.Observed, want)
	}
}

//= docs/requirements/04-inventory.md#applying
//= type=test
//# While a Node is a Host, the Inventory Controller SHALL keep a
//# Host of the Node's name in battery, uncordoned, at the Node's `flintlockd`
//# address and with the TLS settings of IN-004, adding it with `AddHost`,
//# and changing its address or TLS settings with `UpdateHost` and uncordoning
//# it with `UncordonHost` wherever battery's Host differs.

//= docs/requirements/04-inventory.md#admission
//= type=test
//# The Inventory Controller SHALL configure battery to reach every
//# Host's `flintlockd` over TLS, verifying the serving certificate against
//# the configured certificate authority and presenting the Operator's client
//# certificate for `flintlockd`.

//= docs/requirements/04-inventory.md#applying
//= type=test
//# The Inventory Controller SHALL act on a change in whether a Node
//# is a Host only after the change has held for the configured settle time.

// TestAddsAHostOnceSettled checks that a Node becoming a Host reaches
// battery through AddHost once the settle time has passed, with mutual TLS
// to its flintlockd and battery's check that it can reach it.
func TestAddsAHostOnceSettled(t *testing.T) {
	h := newHarness(t, host(nodeA, addrA))
	res := h.mustReconcile()
	if res.RequeueAfter != settle {
		t.Errorf("RequeueAfter = %s, want the settle time", res.RequeueAfter)
	}
	if _, ok := h.admin.held(nodeA); ok {
		t.Fatal("battery holds the Host before the settle time")
	}
	h.advance(settle - time.Second)
	if _, ok := h.admin.held(nodeA); ok {
		t.Fatal("battery holds the Host a second before the settle time")
	}
	h.advance(time.Second)
	got, ok := h.admin.held(nodeA)
	if !ok || got.Address != addrA || got.Cordoned {
		t.Fatalf("battery holds %+v (%v), want node-a uncordoned at %s", got, ok, addrA)
	}
	if got.TLS.Insecure || got.TLS.CAFile == "" || got.TLS.CertFile == "" || got.TLS.KeyFile == "" || got.TLS != TLS {
		t.Errorf("battery reaches node-a with %+v, want mutual TLS %+v", got.TLS, TLS)
	}
	for _, c := range h.admin.calls {
		if c.op == opAddHost && c.skipValidation {
			t.Error("AddHost skipped battery's check that it can reach the Host")
		}
	}
}

// TestKeepsAHostInStep checks the Host of a Node that is a Host: an address
// that moved and settled is updated, a Host battery holds cordoned is
// uncordoned, and a Host already in step is left alone.
func TestKeepsAHostInStep(t *testing.T) {
	t.Run("in step", func(t *testing.T) {
		h := newHarness(t, host(nodeA, addrA))
		h.admin.put(heldHost(nodeA, addrA))
		h.mustReconcile()
		h.advance(settle)
		if ops := h.admin.ops(); len(ops) != 0 {
			t.Errorf("calls %v, want none", ops)
		}
	})
	t.Run("address moved", func(t *testing.T) {
		h := newHarness(t, host(nodeA, addrA))
		h.admin.put(heldHost(nodeA, addrA))
		h.mustReconcile()
		h.changeA(moveTo(addrC))
		h.settled()
		if got, _ := h.admin.held(nodeA); got.Address != addrC {
			t.Errorf("battery reaches node-a at %s, want %s", got.Address, addrC)
		}
		if ops := h.admin.ops(); !slices.Equal(ops, []string{opUpdateHost + " " + nodeA}) {
			t.Errorf("calls %v, want one UpdateHost", ops)
		}
	})
	t.Run("cordoned in battery", func(t *testing.T) {
		h := newHarness(t, host(nodeA, addrA))
		held := heldHost(nodeA, addrA)
		held.Cordoned = true
		h.admin.put(held)
		h.settled()
		if got, _ := h.admin.held(nodeA); got.Cordoned {
			t.Error("battery's Host stays cordoned")
		}
		if ops := h.admin.ops(); !slices.Equal(ops, []string{opUncordonHost + " " + nodeA}) {
			t.Errorf("calls %v, want one UncordonHost", ops)
		}
	})
}

//= docs/requirements/04-inventory.md#applying
//= type=test
//# If `AddHost` answers that battery already holds a Host of that
//# name, then the Inventory Controller SHALL read the Host with `GetHost` and
//# bring it in step as IN-017 says, without failing the reconcile.

// TestAddHostThatBatteryHolds checks the Host that ListHosts did not show
// and AddHost finds: as after an upgrade, battery holds it with no TLS
// settings, and the Inventory Controller updates it.
func TestAddHostThatBatteryHolds(t *testing.T) {
	h := newHarness(t, host(nodeA, addrA))
	h.admin.put(battery.Host{Name: nodeA, Address: addrA})
	h.admin.hidden[nodeA] = true
	h.mustReconcile()
	if _, err := h.reconcile(); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(settle)
	if _, err := h.reconcile(); err != nil {
		t.Fatalf("the reconcile failed for a Host battery already holds: %v", err)
	}
	want := []string{opAddHost + " " + nodeA, opGetHost + " " + nodeA, opUpdateHost + " " + nodeA}
	if ops := h.admin.ops(); !slices.Equal(ops, want) {
		t.Errorf("calls %v, want %v", ops, want)
	}
	if got, _ := h.admin.held(nodeA); got.TLS != TLS {
		t.Errorf("battery reaches node-a with %+v, want %+v", got.TLS, TLS)
	}
}

//= docs/requirements/04-inventory.md#admission
//= type=test
//# When a Node that is a Host is cordoned, or its Node report
//# says the Host is not ready or gives no `flintlockd` address, the
//# Inventory Controller SHALL cordon the Host in battery with `CordonHost`,
//# and SHALL keep it in battery's Hosts.

// TestCordonsAHostThatStopsBeingOne checks each way a Node can stop being a
// Host while it exists: battery cordons its Host once the change has
// settled, keeps it, and is never asked to remove it; and the Host is
// uncordoned when the Node is a Host again.
func TestCordonsAHostThatStopsBeingOne(t *testing.T) {
	for name, tc := range map[string]struct {
		change, undo func(*corev1.Node)
		reason       string
	}{
		"cordoned":   {cordon, uncordon, "Node cordoned"},
		"not ready":  {notReady, func(n *corev1.Node) { n.Annotations[AnnotationReady] = annotationTrue }, "Host not ready"},
		"no address": {noAddress, moveTo(addrA), "Node report gives no flintlockd address"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, host(nodeA, addrA))
			h.admin.put(heldHost(nodeA, addrA))
			h.mustReconcile()
			h.changeA(tc.change)
			h.mustReconcile()
			if got, _ := h.admin.held(nodeA); got.Cordoned {
				t.Fatal("battery cordoned the Host before the settle time")
			}
			h.advance(settle)
			got, ok := h.admin.held(nodeA)
			if !ok || !got.Cordoned || got.CordonedReason != tc.reason {
				t.Fatalf("battery holds %+v (%v), want node-a cordoned for %q", got, ok, tc.reason)
			}
			h.advance(removeRetry)
			if slices.Contains(h.admin.ops(), opRemoveHost+" "+nodeA) {
				t.Error("battery was asked to remove the Host of a Node that exists")
			}
			if _, ok := h.admin.held(nodeA); !ok {
				t.Fatal("battery no longer holds the Host")
			}

			h.changeA(tc.undo)
			h.settled()
			if got, _ := h.admin.held(nodeA); got.Cordoned || got.Address != addrA {
				t.Errorf("battery holds %+v, want node-a uncordoned at %s", got, addrA)
			}
			if !h.published().Equal(Hosts{nodeA: addrA}) {
				t.Errorf("published %v, want node-a back", h.published())
			}
		})
	}
}

// TestAFlappingHostReachesBatteryNotAtAll checks that a change that flaps
// back within the settle time calls battery for nothing.
func TestAFlappingHostReachesBatteryNotAtAll(t *testing.T) {
	h := newHarness(t, host(nodeA, addrA))
	h.admin.put(heldHost(nodeA, addrA))
	h.mustReconcile()
	h.changeA(notReady)
	h.advance(settle / 2)
	h.changeA(func(n *corev1.Node) { n.Annotations[AnnotationReady] = annotationTrue })
	h.advance(settle / 2)
	h.advance(settle)
	if ops := h.admin.ops(); len(ops) != 0 {
		t.Errorf("calls %v, want none", ops)
	}
}

//= docs/requirements/04-inventory.md#admission
//= type=test
//# When battery holds a Host whose Node does not exist or is being
//# deleted, the Inventory Controller SHALL cordon the Host and remove it with
//# `RemoveHost`, and SHALL try again while battery refuses because a Pool
//# names the Host or a MicroVM is still on it.

// TestRemovesTheHostOfADeletedNode checks that the Host of a deleted Node
// is cordoned, then removed, and that battery's refusals, while a Pool
// names it and while a MicroVM is on it, are retried after the remove
// retry without failing the reconcile.
func TestRemovesTheHostOfADeletedNode(t *testing.T) {
	h := newHarness(t, host(nodeA, addrA), host(nodeB, addrB))
	h.admin.put(heldHost(nodeA, addrA))
	h.admin.put(heldHost(nodeB, addrB))
	h.admin.naming[nodeA] = []string{"ci/pool"}
	h.admin.vms[nodeA] = 1
	h.mustReconcile()
	h.remove(nodeA)
	h.settled()

	got, ok := h.admin.held(nodeA)
	if !ok || !got.Cordoned || got.CordonedReason != "Node deleted" {
		t.Fatalf("battery holds %+v (%v), want node-a cordoned as deleted", got, ok)
	}
	if !h.published().Equal(Hosts{nodeB: addrB}) {
		t.Errorf("published %v, want node-b alone", h.published())
	}
	ops := h.admin.ops()
	if i, j := slices.Index(ops, opCordonHost+" "+nodeA), slices.Index(ops, opRemoveHost+" "+nodeA); i < 0 || j < i {
		t.Errorf("calls %v, want CordonHost before RemoveHost", ops)
	}

	// A Pool still names it.
	if res := h.advance(time.Second); res.RequeueAfter != removeRetry {
		t.Errorf("RequeueAfter = %s, want the remove retry", res.RequeueAfter)
	}
	// The Pool Controller dropped it; a MicroVM is still on it.
	h.admin.naming[nodeA] = nil
	if res := h.advance(removeRetry); res.RequeueAfter != removeRetry {
		t.Errorf("RequeueAfter = %s, want the remove retry", res.RequeueAfter)
	}
	if _, ok := h.admin.held(nodeA); !ok {
		t.Fatal("battery removed a Host with a MicroVM on it")
	}
	// Its last Lease ended.
	h.admin.vms[nodeA] = 0
	h.advance(removeRetry)
	if _, ok := h.admin.held(nodeA); ok {
		t.Fatal("battery still holds the Host of a deleted Node")
	}
	if _, ok := h.admin.held(nodeB); !ok {
		t.Error("battery lost node-b")
	}
}

// TestRemovesAHostNoNodeNames checks a Host battery holds that no Node
// names, such as the placeholder an Operator before v0.5 gave battery: it
// is cordoned and removed at once, since there is no change to settle.
func TestRemovesAHostNoNodeNames(t *testing.T) {
	h := newHarness(t)
	h.admin.put(battery.Host{Name: "_no-hosts", Address: "no-hosts.invalid:9090"})
	h.mustReconcile()
	h.advance(settle)
	if _, ok := h.admin.held("_no-hosts"); ok {
		t.Errorf("battery still holds the placeholder; calls %v", h.admin.ops())
	}
}

// TestRemovesTheHostOfANodeBeingDeleted checks that a Node with a deletion
// timestamp counts as gone.
func TestRemovesTheHostOfANodeBeingDeleted(t *testing.T) {
	n := host(nodeA, addrA)
	n.Finalizers = []string{"example.com/hold"}
	h := newHarness(t, n)
	h.admin.put(heldHost(nodeA, addrA))
	h.mustReconcile()
	if err := h.c.Delete(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	h.settled()
	if _, ok := h.admin.held(nodeA); ok {
		t.Errorf("battery still holds the Host of a Node being deleted; calls %v", h.admin.ops())
	}
}

//= docs/requirements/04-inventory.md#applying
//= type=test
//# The Inventory Controller SHALL give the Pool Controller a Host
//# only once battery holds it uncordoned at the Node's address, and SHALL
//# take a Host from the Pool Controller before it cordons or removes the Host.

// TestPublishesOnlyHostsBatteryPlacesOn checks the HostSet's side of every
// change: a joining Host is published only after AddHost, and only if it
// succeeds; a leaving Host is gone from it before CordonHost and
// RemoveHost; a moving Host is gone from it before UpdateHost.
func TestPublishesOnlyHostsBatteryPlacesOn(t *testing.T) {
	h := newHarness(t, host(nodeA, addrA), host(nodeB, addrB))
	h.admin.unreachable[addrB] = true
	h.mustReconcile()
	if !h.published().Equal(Hosts{}) {
		t.Errorf("published %v before battery holds any Host", h.published())
	}
	h.clock.Advance(settle)
	if _, err := h.reconcile(); err == nil {
		t.Error("the reconcile did not fail for a Host battery cannot reach")
	}
	for _, c := range h.admin.calls {
		if c.op == opAddHost && len(c.published) != 0 {
			t.Errorf("%s came with %v published", c, c.published)
		}
	}
	if !h.published().Equal(Hosts{nodeA: addrA}) {
		t.Errorf("published %v, want node-a alone: battery refused node-b", h.published())
	}
	delete(h.admin.unreachable, addrB)
	h.mustReconcile()

	h.admin.reset()
	h.changeA(moveTo(addrC))
	h.settled()
	h.remove(nodeA)
	h.settled()
	for _, c := range h.admin.calls {
		if c.name == nodeA && (c.op == opUpdateHost || c.op == opCordonHost || c.op == opRemoveHost) {
			if _, ok := c.published[nodeA]; ok {
				t.Errorf("%s came while node-a was still published: %v", c, c.published)
			}
		}
	}
}

// TestOneHostFailingDoesNotHoldBackTheOthers checks that a call that fails
// for one Host leaves the others to go ahead, and fails the reconcile.
func TestOneHostFailingDoesNotHoldBackTheOthers(t *testing.T) {
	h := newHarness(t, host(nodeA, addrA), host(nodeB, addrB))
	h.admin.fail[opAddHost+" "+nodeA] = battery.ErrUnavailable
	h.mustReconcile()
	h.clock.Advance(settle)
	if _, err := h.reconcile(); !errors.Is(err, battery.ErrUnavailable) {
		t.Errorf("reconcile: %v, want the failure of node-a", err)
	}
	if _, ok := h.admin.held(nodeB); !ok {
		t.Error("node-b was held back by node-a")
	}
	delete(h.admin.fail, opAddHost+" "+nodeA)
	h.mustReconcile()
	if !h.published().Equal(Hosts{nodeA: addrA, nodeB: addrB}) {
		t.Errorf("published %v, want both", h.published())
	}
}

//= docs/requirements/04-inventory.md#upgrade
//= type=test
//# When battery holds a Host whose TLS settings are not those of
//# IN-004, the Inventory Controller SHALL update the Host with `UpdateHost` to
//# those settings, keeping its address.

// TestGivesEveryHostItsTLSSettings checks the Hosts battery v0.4 copied
// from its configuration file, which have no TLS settings: a Node's Host is
// updated and validated, and a cordoned one is updated at its address
// without the check, since its flintlockd may be down.
func TestGivesEveryHostItsTLSSettings(t *testing.T) {
	n := host(nodeB, addrB)
	cordon(n)
	h := newHarness(t, host(nodeA, addrA), n)
	h.admin.put(battery.Host{Name: nodeA, Address: addrA})
	h.admin.put(battery.Host{Name: nodeB, Address: addrB, Cordoned: true})
	h.mustReconcile()
	for _, name := range []string{nodeA, nodeB} {
		if got, _ := h.admin.held(name); got.TLS != TLS || got.Address != map[string]string{nodeA: addrA, nodeB: addrB}[name] {
			t.Errorf("battery holds %+v, want its address and %+v", got, TLS)
		}
	}
	for _, c := range h.admin.calls {
		if c.op == opUpdateHost && c.skipValidation != (c.name == nodeB) {
			t.Errorf("%s: skipValidation %v", c, c.skipValidation)
		}
	}
	if b, _ := h.admin.held(nodeB); !b.Cordoned {
		t.Error("node-b was uncordoned")
	}
	if !h.published().Equal(Hosts{nodeA: addrA}) {
		t.Errorf("published %v, want node-a alone", h.published())
	}
}

//= docs/requirements/06-deployment.md#battery-sidecar
//= type=test
//# When the client certificate in the Secret of DP-005 changes,
//# and once after the Operator starts, the Operator SHALL call `UpdateHost`
//# for every Host battery holds, with the Host's address and TLS settings
//# unchanged, once the Operator's own mount of that Secret holds the
//# certificate the Secret holds.

// TestReloadsTheClientCertificate checks that battery reconnects to every
// Host once after the Operator starts and once after each renewal, only
// once the mount holds the Secret's certificate, with each Host's settings
// unchanged and without the reachability check.
func TestReloadsTheClientCertificate(t *testing.T) {
	n := host(nodeB, addrB)
	cordon(n)
	h := newHarness(t, host(nodeA, addrA), n)
	h.admin.put(heldHost(nodeA, addrA))
	b := heldHost(nodeB, addrB)
	b.Cordoned = true
	h.admin.put(b)
	h.state = NewState() // the Operator has just started
	updates := func() []string {
		var out []string
		for _, c := range h.admin.calls {
			if c.op == opUpdateHost {
				if !c.skipValidation {
					t.Errorf("%s checked the Host", c)
				}
				out = append(out, c.name)
			}
		}
		h.admin.reset()
		return out
	}

	// The Operator has just started; the mount is still swapping.
	h.mount.set(nil)
	if res := h.mustReconcile(); res.RequeueAfter != mountPoll {
		t.Errorf("RequeueAfter = %s, want the mount poll", res.RequeueAfter)
	}
	if got := updates(); len(got) != 0 {
		t.Fatalf("updated %v before the mount held the certificate", got)
	}
	h.mount.set(certificate1)
	h.advance(mountPoll)
	if got := updates(); !slices.Equal(got, []string{nodeA, nodeB}) {
		t.Fatalf("updated %v after the start, want every Host", got)
	}
	h.advance(time.Minute)
	if got := updates(); len(got) != 0 {
		t.Fatalf("updated %v again with nothing renewed", got)
	}

	// cert-manager renews; the kubelet updates the volume later.
	h.cert = certificate2
	h.mustReconcile()
	if got := updates(); len(got) != 0 {
		t.Fatalf("updated %v before the mount held the renewal", got)
	}
	h.mount.set(certificate2)
	h.advance(mountPoll)
	if got := updates(); !slices.Equal(got, []string{nodeA, nodeB}) {
		t.Fatalf("updated %v after the renewal, want every Host", got)
	}
	for name, addr := range map[string]string{nodeA: addrA, nodeB: addrB} {
		if got, _ := h.admin.held(name); got.Address != addr || got.TLS != TLS {
			t.Errorf("the reload changed %s to %+v", name, got)
		}
	}
	if b, _ := h.admin.held(nodeB); !b.Cordoned {
		t.Error("the reload uncordoned node-b")
	}
}

// TestReloadRetriesAFailure checks that a reload that fails for one Host
// is tried again at the next reconcile, for every Host.
func TestReloadRetriesAFailure(t *testing.T) {
	h := newHarness(t, host(nodeA, addrA))
	h.admin.put(heldHost(nodeA, addrA))
	h.state = NewState() // the Operator has just started
	h.admin.fail[opUpdateHost+" "+nodeA] = battery.ErrUnavailable
	if _, err := h.reconcile(); err == nil {
		t.Fatal("the reconcile did not fail")
	}
	delete(h.admin.fail, opUpdateHost+" "+nodeA)
	h.admin.reset()
	h.mustReconcile()
	if ops := h.admin.ops(); !slices.Equal(ops, []string{opUpdateHost + " " + nodeA}) {
		t.Errorf("calls %v, want the reload again", ops)
	}
}

// TestNoSecretNoReload checks that without the client certificate's
// Secret, nothing is reloaded.
func TestNoSecretNoReload(t *testing.T) {
	h := newHarness(t, host(nodeA, addrA))
	h.admin.put(heldHost(nodeA, addrA))
	h.cert = nil
	h.mustReconcile()
	if ops := h.admin.ops(); len(ops) != 0 {
		t.Errorf("calls %v, want none", ops)
	}
}

// TestOptionsValidate checks the timings the controller refuses.
func TestOptionsValidate(t *testing.T) {
	for _, o := range []Options{{SettleTime: -1}, {RemoveRetry: -1}, {MountPoll: -1}} {
		if o.Validate() == nil {
			t.Errorf("%+v is valid", o)
		}
	}
	if (Options{}).Validate() != nil {
		t.Error("the zero Options are not valid")
	}
}
