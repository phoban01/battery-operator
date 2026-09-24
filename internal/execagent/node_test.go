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

package execagent_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/phoban01/battery-operator/internal/execagent"
	"github.com/phoban01/battery-operator/internal/fakeflintlock"
	"github.com/phoban01/battery-operator/internal/hostcert"
)

// annotation reads one annotation of the Host's Node.
func annotation(h *testHost, key string) string {
	return h.ReadNode().Annotations[key]
}

// waitReady waits until the agent reports want with reason.
func waitReady(t *testing.T, h *testHost, want bool, reason string) {
	t.Helper()
	eventually(t, fmt.Sprintf("the host reported ready=%v with reason %s", want, reason), func() bool {
		a := h.ReadNode().Annotations
		return a[execagent.AnnotationReady] == strconv.FormatBool(want) && a[execagent.AnnotationReason] == reason
	})
}

//= docs/requirements/05-exec-agent.md#host-checks
//= type=test
//# The Exec Agent SHALL report its Host not ready while any file
//# in the not ready reason directory names a reason, and SHALL report that
//# reason.

// TestReadiness walks one Host through the reasons to be not ready and
// back: a Host Image unit's reason, whose own words the message carries,
// KVM unavailable, the thin pool missing, and flintlockd not answering;
// and a second Host whose flintlockd has exec disabled.
func TestReadiness(t *testing.T) {
	t.Parallel()
	h := newHost(t, hostOptions{})
	waitReady(t, h, true, execagent.ReasonReady)

	if err := os.MkdirAll(h.NotReadyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	reasonFile := filepath.Join(h.NotReadyDir, "flintlock-kvm-check.service")
	if err := os.WriteFile(reasonFile, []byte("KVM is unavailable: /dev/kvm is absent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitReady(t, h, false, execagent.ReasonHostImageNotReady)
	if msg := annotation(h, execagent.AnnotationMessage); !strings.Contains(msg, "flintlock-kvm-check.service: KVM is unavailable: /dev/kvm is absent") {
		t.Errorf("message %q does not carry the unit's reason", msg)
	}
	if err := os.Remove(reasonFile); err != nil {
		t.Fatal(err)
	}
	waitReady(t, h, true, execagent.ReasonReady)

	if err := os.Rename(filepath.Join(h.KVMSysfsDir, "dev"), filepath.Join(h.KVMSysfsDir, "gone")); err != nil {
		t.Fatal(err)
	}
	waitReady(t, h, false, execagent.ReasonKVMUnavailable)
	if err := os.Rename(filepath.Join(h.KVMSysfsDir, "gone"), filepath.Join(h.KVMSysfsDir, "dev")); err != nil {
		t.Fatal(err)
	}
	waitReady(t, h, true, execagent.ReasonReady)

	pool, gone := filepath.Join(h.SysBlockDir, "dm-0"), filepath.Join(h.SysBlockDir, "removed")
	if err := os.Rename(pool, gone); err != nil {
		t.Fatal(err)
	}
	waitReady(t, h, false, execagent.ReasonThinPoolMissing)
	if err := os.Rename(gone, pool); err != nil {
		t.Fatal(err)
	}
	waitReady(t, h, true, execagent.ReasonReady)

	h.Fake.SetFaults(fakeflintlock.Faults{Unresponsive: true})
	waitReady(t, h, false, execagent.ReasonFlintlockdNotReady)
	h.Fake.SetFaults(fakeflintlock.Faults{})
	waitReady(t, h, true, execagent.ReasonReady)

	disabled := newHost(t, hostOptions{ExecDisabled: true})
	waitReady(t, disabled, false, execagent.ReasonExecDisabled)
}

//= docs/requirements/05-exec-agent.md#host-checks
//= type=test
//# The Exec Agent SHALL publish its Node report on its Host's Node
//# as the annotation `battery.liquidmetal-x.dev/exec-agent-ready` set to
//# `true` or `false`, with the reason and message of the last check in
//# `battery.liquidmetal-x.dev/exec-agent-reason` and
//# `battery.liquidmetal-x.dev/exec-agent-message`, and its own address in
//# `battery.liquidmetal-x.dev/exec-agent-address`.

//= docs/requirements/05-exec-agent.md#host-checks
//= type=test
//# The Exec Agent SHALL publish in its Node report the address
//# at which battery reaches the Host's `flintlockd`, as the annotation
//# `battery.liquidmetal-x.dev/flintlockd-address`.

// TestNodeReport checks the five annotations of the Node report by their
// literal keys, that the address is the one the agent serves on: the
// Host's internal address from its Node, and the agent's port; and that
// the flintlockd address is the Host's flintlockd endpoint.
func TestNodeReport(t *testing.T) {
	t.Parallel()
	h := newHost(t, hostOptions{})
	eventually(t, "the node report", func() bool {
		a := h.ReadNode().Annotations
		return a["battery.liquidmetal-x.dev/exec-agent-ready"] == "true" &&
			a["battery.liquidmetal-x.dev/exec-agent-reason"] == "Ready" &&
			a["battery.liquidmetal-x.dev/exec-agent-message"] != "" &&
			a["battery.liquidmetal-x.dev/exec-agent-address"] == h.Address &&
			a["battery.liquidmetal-x.dev/flintlockd-address"] == h.Fake.Addr()
	})
	if host, _, _ := strings.Cut(h.Address, ":"); host != hostAddress {
		t.Errorf("the agent serves on %s, want the Node's internal address %s", h.Address, hostAddress)
	}
}

//= docs/requirements/05-exec-agent.md#identity
//= type=test
//# When the Exec Agent starts, the Exec Agent SHALL confirm that
//# its identity names its own Host, and SHALL refuse to start otherwise.

// TestIdentityNamesItsHost checks the start-up check against a fake API
// server whose SelfSubjectReview names the agent's pod's node: an identity
// on the Host passes, one on another Node and one that names no node fail,
// and Run refuses to start with either of those, before it serves
// anything. Every other test's agent has passed the check to start at all.
// The e2e suite checks the same with real bound tokens (test/e2e,
// TestExecAgentAuthentication).
func TestIdentityNamesItsHost(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const host = "identity-a"
	for what, tc := range map[string]struct {
		identityNode string
		ok           bool
	}{
		"a pod on its own Host": {identityNode: host, ok: true},
		"a pod on another Node": {identityNode: "identity-b"},
		"a token of no pod":     {},
	} {
		t.Run(what, func(t *testing.T) {
			kube := newFakeCluster(t, host, tc.identityNode).Kube
			err := execagent.CheckIdentity(ctx, kube, host)
			if tc.ok {
				if err != nil {
					t.Errorf("CheckIdentity = %v, want it to pass", err)
				}
				return
			}
			if err == nil {
				t.Error("CheckIdentity passed")
			}

			cfg := &execagent.Config{
				HostNode: host, Flintlockd: net.JoinHostPort(hostAddress, "9090"), TrustDomain: trustDomain,
				FlintlockdCertDir: t.TempDir(), CABundle: execagent.CABundleRef{Namespace: agentNamespace, Name: hostcert.CABundleConfigMap},
				NotReadyDir: t.TempDir(), Guard: execagent.Guard{Namespace: agentNamespace},
			}
			cfg.KVMDevice, cfg.KVMSysfsDir, cfg.SysBlockDir = fakeHostPrerequisites(t)
			cfg.ApplyDefaults()
			certs := execagent.NewCertificates(kube, cfg, nil)
			fl, err := execagent.DialFlintlockd(cfg.Flintlockd, certs, []net.IP{net.ParseIP(hostAddress)})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = fl.Close() }()
			ready := make(chan struct{})
			runCtx, cancel := context.WithTimeout(ctx, testTimeout)
			defer cancel()
			err = execagent.Run(runCtx, execagent.Options{
				Config: cfg, Kube: kube, Claims: noClaims{}, Flintlockd: fl, Certificates: certs, Ready: ready,
				HostAddresses: []net.IP{net.ParseIP(hostAddress)},
			})
			if err == nil || !strings.Contains(err.Error(), "not on its Host") && !strings.Contains(err.Error(), "names no node") {
				t.Errorf("Run = %v, want it refused", err)
			}
			select {
			case <-ready:
				t.Error("Run served the exec API")
			default:
			}
		})
	}
}

// noClaims is a ClaimLookup that finds nothing.
type noClaims struct{}

func (noClaims) Claim(context.Context, string, string) (*execagent.Claim, error) { return nil, nil }
func (noClaims) BoundOnHost(context.Context, string) ([]execagent.Claim, error)  { return nil, nil }

//= docs/requirements/05-exec-agent.md#drain
//= type=test
//# While claims are Bound on its Host, the Exec Agent SHALL hold an
//# eviction-based drain of the Host's Node open, and SHALL let the drain
//# complete when none remain or when the configured drain timeout elapses.

//= docs/requirements/05-exec-agent.md#drain
//= type=test
//# The Exec Agent SHALL hold a drain open only with a guard pod
//# and a PodDisruptionBudget of its own, both bound to its own Host's Node.

// TestDrainGuard checks that a Bound claim on the Host puts a guard pod on
// the Host's Node under a budget that allows no disruption, and that the
// guard goes when the claim does. With a claim still Bound on a cordoned
// Node, the guard goes once the drain timeout has passed, and the
// abandoned claim is logged.
func TestDrainGuard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t, hostOptions{DrainTimeout: 2 * time.Second})
	pods := f.host.Kube.CoreV1().Pods(agentNamespace)
	pdbs := f.host.Kube.PolicyV1().PodDisruptionBudgets(agentNamespace)
	name := execagent.GuardName(f.host.Node)
	guarded := func() bool {
		_, podErr := pods.Get(ctx, name, metav1.GetOptions{})
		_, pdbErr := pdbs.Get(ctx, name, metav1.GetOptions{})
		return podErr == nil && pdbErr == nil
	}
	gone := func() bool {
		_, podErr := pods.Get(ctx, name, metav1.GetOptions{})
		_, pdbErr := pdbs.Get(ctx, name, metav1.GetOptions{})
		return apierrors.IsNotFound(podErr) && apierrors.IsNotFound(pdbErr)
	}

	time.Sleep(300 * time.Millisecond)
	if !gone() {
		t.Fatal("a guard was placed with no claim bound on the host")
	}
	f.bind("claim")
	eventually(t, "the guard to be placed for a bound claim", guarded)
	pod, err := pods.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pod.Spec.NodeName != f.host.Node || len(pod.OwnerReferences) != 1 || pod.OwnerReferences[0].Kind != "Node" {
		t.Errorf("guard pod = node %q, owners %v; want on the host's node, owned by it", pod.Spec.NodeName, pod.OwnerReferences)
	}
	pdb, err := pdbs.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.IntValue() != 1 {
		t.Errorf("guard budget = %v, want minAvailable 1", pdb.Spec)
	}
	// EA-041: the budget and the pod are the agent's own and bound to its
	// Host's Node: both owned by that Node, the budget selecting that Node's
	// guard, and that guard the only pod it selects.
	node := f.host.ReadNode()
	for what, owners := range map[string][]metav1.OwnerReference{"pod": pod.OwnerReferences, "budget": pdb.OwnerReferences} {
		if len(owners) != 1 || owners[0].Kind != "Node" || owners[0].Name != node.Name || owners[0].UID != node.UID {
			t.Errorf("guard %s owners = %v, want the Host's Node alone", what, owners)
		}
	}
	selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := pods.List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.Items) != 1 || selected.Items[0].Name != name || selected.Items[0].Spec.NodeName != f.host.Node {
		t.Errorf("the guard budget selects %d pods, want only the guard on %s", len(selected.Items), f.host.Node)
	}
	f.host.DeleteClaim(t, f.ns, "claim")
	eventually(t, "the guard to go when no claim is bound", gone)

	// A drain that outlasts the timeout is let through.
	f.bind("stuck")
	eventually(t, "the guard to be placed again", guarded)
	if _, err := f.host.Kube.CoreV1().Nodes().Patch(ctx, f.host.Node, k8stypes.MergePatchType,
		[]byte(`{"spec":{"unschedulable":true}}`), metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the drain start to be recorded", func() bool {
		return annotation(f.host, execagent.AnnotationDrainStarted) != ""
	})
	if !guarded() {
		t.Error("the guard went before the drain timeout, with a claim still bound")
	}
	eventually(t, "the guard to go when the drain timeout has passed", gone)
	if !strings.Contains(f.host.Log(), "Abandoned a Bound claim") {
		t.Errorf("the abandoned claim was not logged:\n%s", f.host.Log())
	}

	// Uncordoned, the drain is over and a bound claim is guarded again.
	if _, err := f.host.Kube.CoreV1().Nodes().Patch(ctx, f.host.Node, k8stypes.MergePatchType,
		[]byte(`{"spec":{"unschedulable":null}}`), metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the drain start to be cleared and the guard placed", func() bool {
		return annotation(f.host, execagent.AnnotationDrainStarted) == "" && guarded()
	})
}
