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

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"

	"github.com/phoban01/battery-operator/internal/execagent"
	"github.com/phoban01/battery-operator/internal/execagent/execagenttest"
	"github.com/phoban01/battery-operator/internal/fakeflintlock"
)

// annotation reads one annotation of the Host's Node.
func annotation(h *execagenttest.Host, key string) string {
	return h.ReadNode().Annotations[key]
}

// waitReady waits until the agent reports want with reason.
func waitReady(t *testing.T, h *execagenttest.Host, want bool, reason string) {
	t.Helper()
	execagenttest.Eventually(t, fmt.Sprintf("the host reported ready=%v with reason %s", want, reason), func() bool {
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
	needEnv(t)
	h := env.NewHost(t, execagenttest.HostOptions{})
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

	disabled := env.NewHost(t, execagenttest.HostOptions{ExecDisabled: true})
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
	needEnv(t)
	h := env.NewHost(t, execagenttest.HostOptions{})
	execagenttest.Eventually(t, "the node report", func() bool {
		a := h.ReadNode().Annotations
		return a["battery.liquidmetal-x.dev/exec-agent-ready"] == "true" &&
			a["battery.liquidmetal-x.dev/exec-agent-reason"] == "Ready" &&
			a["battery.liquidmetal-x.dev/exec-agent-message"] != "" &&
			a["battery.liquidmetal-x.dev/exec-agent-address"] == h.Address &&
			a["battery.liquidmetal-x.dev/flintlockd-address"] == h.Fake.Addr()
	})
	if host, _, _ := strings.Cut(h.Address, ":"); host != execagenttest.HostAddress {
		t.Errorf("the agent serves on %s, want the Node's internal address %s", h.Address, execagenttest.HostAddress)
	}
}

// agentClient is the Kubernetes client of the Exec Agent of hostNode, with
// the real bound token of an Exec Agent pod there.
func agentClient(t *testing.T, hostNode string) kubernetes.Interface {
	t.Helper()
	client, err := kubernetes.NewForConfig(env.AgentConfig(t, hostNode))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// unboundAgent is a client of the Exec Agent's ServiceAccount whose token is
// bound to no pod, so its identity names no Host.
func unboundAgent(t *testing.T) kubernetes.Interface {
	t.Helper()
	id := env.ServiceAccountToken(t, execagenttest.AgentNamespace, execagenttest.AgentServiceAccount)
	cfg := *env.Config
	cfg.CertData, cfg.KeyData, cfg.CertFile, cfg.KeyFile = nil, nil, "", ""
	cfg.BearerToken = id.Token
	client, err := kubernetes.NewForConfig(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

//= docs/requirements/05-exec-agent.md#identity
//= type=test
//# When the Exec Agent starts, the Exec Agent SHALL confirm that
//# its identity names its own Host, and SHALL refuse to start otherwise.

// TestIdentityNamesItsHost checks the start-up check with real tokens: the
// bound token of a pod on the Host passes, the bound token of a pod on
// another Node and a token bound to no pod fail, and Run refuses to start
// with either of those, before it serves anything. Every other test's agent
// has passed the check to start at all.
func TestIdentityNamesItsHost(t *testing.T) {
	t.Parallel()
	needEnv(t)
	ctx := context.Background()
	if err := execagent.CheckIdentity(ctx, agentClient(t, "identity-a"), "identity-a"); err != nil {
		t.Errorf("an agent on its own Host: %v", err)
	}
	if err := execagent.CheckIdentity(ctx, agentClient(t, "identity-b"), "identity-a"); err == nil {
		t.Error("an agent whose pod is on another Node passed the check")
	}
	if err := execagent.CheckIdentity(ctx, unboundAgent(t), "identity-a"); err == nil {
		t.Error("an agent whose token names no Node passed the check")
	}

	h := env.NewHost(t, execagenttest.HostOptions{})
	cfg := h.Config()
	for what, kube := range map[string]kubernetes.Interface{
		"a pod on another Node": agentClient(t, "identity-b"),
		"a token of no pod":     unboundAgent(t),
	} {
		certs := execagent.NewCertificates(kube, cfg, nil)
		fl, err := execagent.DialFlintlockd(cfg.Flintlockd, certs, []net.IP{net.ParseIP(execagenttest.HostAddress)})
		if err != nil {
			t.Fatal(err)
		}
		ready := make(chan struct{})
		runCtx, cancel := context.WithTimeout(ctx, testTimeout)
		err = execagent.Run(runCtx, execagent.Options{
			Config: cfg, Kube: kube, Claims: noClaims{}, Flintlockd: fl, Certificates: certs, Ready: ready,
			HostAddresses: []net.IP{net.ParseIP(execagenttest.HostAddress)},
		})
		cancel()
		_ = fl.Close()
		if err == nil || !strings.Contains(err.Error(), "not on its Host") && !strings.Contains(err.Error(), "names no node") {
			t.Errorf("Run with the identity of %s = %v, want it refused", what, err)
		}
		select {
		case <-ready:
			t.Errorf("Run with the identity of %s served the exec API", what)
		default:
		}
	}
}

// noClaims is a ClaimLookup that finds nothing.
type noClaims struct{}

func (noClaims) Claim(context.Context, string, string) (*execagent.Claim, error) { return nil, nil }
func (noClaims) BoundOnHost(context.Context, string) ([]execagent.Claim, error)  { return nil, nil }

//= docs/requirements/05-exec-agent.md#identity
//= type=test
//# The Manifests SHALL include a ValidatingAdmissionPolicy that
//# lets an Exec Agent's identity change only the annotations of its own
//# Host's Node under the prefix `battery.liquidmetal-x.dev/`, and nothing
//# else of any Node.

//= docs/requirements/05-exec-agent.md#identity
//= type=test
//# The ValidatingAdmissionPolicy of EA-051 SHALL let an Exec
//# Agent's identity create and delete guard pods and PodDisruptionBudgets
//# only for its own Host's Node.

// TestAdmissionPolicyKeepsEachAgentToItsOwnNode runs against the API server
// with config/exec-agent/admission-policy.yaml loaded unchanged, with the
// Exec Agents of two Hosts authenticating with real bound tokens of pods on
// their Nodes. Every change is one the shipped RBAC allows, so a refusal
// can only be the policy's, and each is checked to be.
func TestAdmissionPolicyKeepsEachAgentToItsOwnNode(t *testing.T) {
	t.Parallel()
	needEnv(t)
	ctx := context.Background()
	id := time.Now().UnixNano()
	hostA, hostB := fmt.Sprintf("policy-a-%d", id), fmt.Sprintf("policy-b-%d", id)
	for _, name := range []string{hostA, hostB} {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"keep": "me"},
			Annotations: map[string]string{"example.com/other": "x"}}}
		if _, err := env.Admin.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	a := agentClient(t, hostA)
	patch := func(client kubernetes.Interface, node, body string, sub ...string) error {
		_, err := client.CoreV1().Nodes().Patch(ctx, node, k8stypes.MergePatchType, []byte(body), metav1.PatchOptions{}, sub...)
		return err
	}
	ann := func(key, value string) string {
		return fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, key, value)
	}

	admitted := map[string]error{
		"a report annotation of its own Node": patch(a, hostA, ann(execagent.AnnotationReady, "true")),
		"removing a report annotation":        patch(a, hostA, `{"metadata":{"annotations":{"`+execagent.AnnotationReady+`":null}}}`),
	}
	for what, err := range admitted {
		if err != nil {
			t.Errorf("%s: %v, want admitted", what, err)
		}
	}

	refused := map[string]error{
		"a report annotation of another Host's Node":  patch(a, hostB, ann(execagent.AnnotationReady, "true")),
		"another prefix's annotation of its own Node": patch(a, hostA, ann("example.com/other", "changed")),
		"removing another prefix's annotation":        patch(a, hostA, `{"metadata":{"annotations":{"example.com/other":null}}}`),
		"a look-alike prefix":                         patch(a, hostA, ann("evilbattery.liquidmetal-x.dev/x", "y")),
		"a subdomain of the prefix":                   patch(a, hostA, ann("host-service.battery.liquidmetal-x.dev/x", "y")),
		"a label of its own Node":                     patch(a, hostA, `{"metadata":{"labels":{"keep":"changed"}}}`),
		"cordoning its own Node":                      patch(a, hostA, `{"spec":{"unschedulable":true}}`),
		"its own Node's status":                       patch(a, hostA, `{"status":{"conditions":[{"type":"Ready","status":"False"}]}}`, "status"),
		"a report annotation with a token bound to no pod": patch(unboundAgent(t), hostA,
			ann(execagent.AnnotationReady, "true")),
	}
	for what, err := range refused {
		if !execagenttest.IsPolicyDenial(err) && !apierrors.IsForbidden(err) {
			t.Errorf("%s: %v, want refused", what, err)
		}
		if err != nil && !execagenttest.IsPolicyDenial(err) && what != "its own Node's status" {
			t.Errorf("%s: refused by %v, want the admission policy's refusal", what, err)
		}
	}

	// The drain guard is the only pod and budget an agent makes.
	noToken := false
	guard := func(name, node string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: execagenttest.AgentNamespace},
			Spec: corev1.PodSpec{NodeName: node, AutomountServiceAccountToken: &noToken,
				Containers: []corev1.Container{{Name: "guard", Image: execagent.DefaultGuardImage}}},
		}
	}
	dryRun := metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}
	pods := a.CoreV1().Pods(execagenttest.AgentNamespace)
	if _, err := pods.Create(ctx, guard(execagent.GuardName(hostA), hostA), dryRun); err != nil {
		t.Errorf("its own guard pod: %v, want admitted", err)
	}
	withToken := guard(execagent.GuardName(hostA), hostA)
	withToken.Spec.AutomountServiceAccountToken = nil
	for what, pod := range map[string]*corev1.Pod{
		"another Host's guard pod":           guard(execagent.GuardName(hostB), hostB),
		"its guard pod on another Node":      guard(execagent.GuardName(hostA), hostB),
		"a pod of another name":              guard("not-the-guard", hostA),
		"its guard pod with a service token": withToken,
	} {
		if _, err := pods.Create(ctx, pod, dryRun); !execagenttest.IsPolicyDenial(err) {
			t.Errorf("%s: %v, want refused by the admission policy", what, err)
		}
	}

	// Budgets: its own Host's guard budget only.
	minAvailable := intstr.FromInt32(1)
	budget := func(name string) *policyv1.PodDisruptionBudget {
		return &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: execagenttest.AgentNamespace},
			Spec:       policyv1.PodDisruptionBudgetSpec{MinAvailable: &minAvailable},
		}
	}
	pdbs := a.PolicyV1().PodDisruptionBudgets(execagenttest.AgentNamespace)
	if _, err := pdbs.Create(ctx, budget(execagent.GuardName(hostA)), dryRun); err != nil {
		t.Errorf("its own guard budget: %v, want admitted", err)
	}
	for what, b := range map[string]*policyv1.PodDisruptionBudget{
		"another Host's guard budget": budget(execagent.GuardName(hostB)),
		"a budget of another name":    budget("not-the-guard"),
	} {
		if _, err := pdbs.Create(ctx, b, dryRun); !execagenttest.IsPolicyDenial(err) {
			t.Errorf("%s: %v, want refused by the admission policy", what, err)
		}
	}

	// Deleting: its own guard pod and budget, and not another Host's.
	adminPods := env.Admin.CoreV1().Pods(execagenttest.AgentNamespace)
	adminPDBs := env.Admin.PolicyV1().PodDisruptionBudgets(execagenttest.AgentNamespace)
	for _, host := range []string{hostA, hostB} {
		if _, err := adminPods.Create(ctx, guard(execagent.GuardName(host), host), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := adminPDBs.Create(ctx, budget(execagent.GuardName(host)), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	dryDelete := metav1.DeleteOptions{DryRun: []string{metav1.DryRunAll}}
	if err := pods.Delete(ctx, execagent.GuardName(hostA), dryDelete); err != nil {
		t.Errorf("deleting its own guard pod: %v, want admitted", err)
	}
	if err := pdbs.Delete(ctx, execagent.GuardName(hostA), dryDelete); err != nil {
		t.Errorf("deleting its own guard budget: %v, want admitted", err)
	}
	if err := pods.Delete(ctx, execagent.GuardName(hostB), dryDelete); !execagenttest.IsPolicyDenial(err) {
		t.Errorf("deleting another Host's guard pod: %v, want refused by the admission policy", err)
	}
	if err := pdbs.Delete(ctx, execagent.GuardName(hostB), dryDelete); !execagenttest.IsPolicyDenial(err) {
		t.Errorf("deleting another Host's guard budget: %v, want refused by the admission policy", err)
	}
}

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
	needEnv(t)
	ctx := context.Background()
	f := newFixture(t, execagenttest.HostOptions{DrainTimeout: 2 * time.Second})
	pods := env.Admin.CoreV1().Pods(execagenttest.AgentNamespace)
	pdbs := env.Admin.PolicyV1().PodDisruptionBudgets(execagenttest.AgentNamespace)
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
	execagenttest.Eventually(t, "the guard to be placed for a bound claim", guarded)
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
	env.DeleteClaim(t, f.ns, "claim")
	execagenttest.Eventually(t, "the guard to go when no claim is bound", gone)

	// A drain that outlasts the timeout is let through.
	f.bind("stuck")
	execagenttest.Eventually(t, "the guard to be placed again", guarded)
	if _, err := env.Admin.CoreV1().Nodes().Patch(ctx, f.host.Node, k8stypes.MergePatchType,
		[]byte(`{"spec":{"unschedulable":true}}`), metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	execagenttest.Eventually(t, "the drain start to be recorded", func() bool {
		return annotation(f.host, execagent.AnnotationDrainStarted) != ""
	})
	if !guarded() {
		t.Error("the guard went before the drain timeout, with a claim still bound")
	}
	execagenttest.Eventually(t, "the guard to go when the drain timeout has passed", gone)
	if !strings.Contains(f.host.Log(), "Abandoned a Bound claim") {
		t.Errorf("the abandoned claim was not logged:\n%s", f.host.Log())
	}

	// Uncordoned, the drain is over and a bound claim is guarded again.
	if _, err := env.Admin.CoreV1().Nodes().Patch(ctx, f.host.Node, k8stypes.MergePatchType,
		[]byte(`{"spec":{"unschedulable":null}}`), metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	execagenttest.Eventually(t, "the drain start to be cleared and the guard placed", func() bool {
		return annotation(f.host, execagent.AnnotationDrainStarted) == "" && guarded()
	})
}
