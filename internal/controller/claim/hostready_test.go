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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/execagent"
	"github.com/phoban01/battery-operator/internal/reconcile"
)

// reportNotReady is the ready annotation of a Node report that says not
// ready.
const reportNotReady = "false"

// reportedNode is the Node nodeA with a Node report of ready, reason and
// message.
func reportedNode(ready, reason, message string) *corev1.Node {
	n := aNode(agentAddr)
	n.Annotations[execagent.AnnotationReady] = ready
	n.Annotations[execagent.AnnotationReason] = reason
	n.Annotations[execagent.AnnotationMessage] = message
	return n
}

// reconcileHostReady runs HostReady on a scope for the claim at claimKey
// in c, with a battery that fails the test if it is called, and patches
// the claim as the controller would. It returns the claim as stored.
func reconcileHostReady(t *testing.T, c client.Client) *batteryv1alpha1.MicroVMClaim {
	t.Helper()
	// Every method of the zero stubBattery panics: HostReady must not
	// call battery.
	s := scopeFor(t, c, claimKey, &stubBattery{})
	res, err := HostReady{}.Reconcile(context.Background(), s)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res != (reconcile.Result{}) {
		t.Errorf("result = %+v, want the chain to go on", res)
	}
	if err := s.Patch(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := &batteryv1alpha1.MicroVMClaim{}
	if err := c.Get(context.Background(), claimKey, got); err != nil {
		t.Fatal(err)
	}
	return got
}

// wantHostReady fails the test unless got is still Bound with its binding,
// and its HostReady condition has status, reason and message.
func wantHostReady(t *testing.T, got *batteryv1alpha1.MicroVMClaim, status metav1.ConditionStatus, reason, message string) {
	t.Helper()
	st := got.Status
	if st.Phase != batteryv1alpha1.MicroVMClaimBound {
		t.Errorf("phase = %q, want the claim kept Bound", st.Phase)
	}
	if !meta.IsStatusConditionTrue(st.Conditions, batteryv1alpha1.ConditionBound) {
		t.Errorf("Bound condition = %+v, want it kept true", meta.FindStatusCondition(st.Conditions, batteryv1alpha1.ConditionBound))
	}
	if st.LeaseID != "lease-"+claimKey.Name || st.Host == nil || st.Host.NodeName != nodeA {
		t.Errorf("status = %+v, want the binding kept", st)
	}
	cond := meta.FindStatusCondition(st.Conditions, batteryv1alpha1.ConditionHostReady)
	if cond == nil {
		t.Fatalf("no HostReady condition, want %s/%s", status, reason)
	}
	if cond.Status != status || cond.Reason != reason {
		t.Errorf("HostReady = %s/%s, want %s/%s", cond.Status, cond.Reason, status, reason)
	}
	if message != "" && cond.Message != message {
		t.Errorf("HostReady message = %q, want %q", cond.Message, message)
	}
	if cond.ObservedGeneration != got.Generation {
		t.Errorf("HostReady observedGeneration = %d, want %d", cond.ObservedGeneration, got.Generation)
	}
}

//= docs/requirements/02-claims.md#host-readiness
//= type=test
//# While a claim is Bound and the Node report of its Host says
//# the Host is ready, the Claim Controller SHALL set the claim's condition
//# `HostReady` true with the reason and message of the Node report.

func TestHostReadyIsTrueWhileTheReportSaysReady(t *testing.T) {
	c := newFakeClient(t, aBoundClaim(claimKey.Name, nodeA, agentAddr),
		reportedNode("true", execagent.ReasonReady, "the Host has a KVM device"))
	got := reconcileHostReady(t, c)
	wantHostReady(t, got, metav1.ConditionTrue, execagent.ReasonReady, "the Host has a KVM device")
}

//= docs/requirements/02-claims.md#host-readiness
//= type=test
//# While a claim is Bound and the Node report of its Host says
//# the Host is not ready, the Claim Controller SHALL keep the claim Bound and
//# set the condition `HostReady` false with the reason and message of the
//# Node report.

func TestHostReadyIsFalseWhileTheReportSaysNotReady(t *testing.T) {
	for _, tc := range []struct {
		name, ready, reason, wantReason string
	}{
		{"KVM gone", reportNotReady, execagent.ReasonKVMUnavailable, execagent.ReasonKVMUnavailable},
		{"flintlockd not answering", reportNotReady, execagent.ReasonFlintlockdNotReady, execagent.ReasonFlintlockdNotReady},
		{"a Host Image reason", reportNotReady, execagent.ReasonHostImageNotReady, execagent.ReasonHostImageNotReady},
		// Only "true" is ready, as for the Inventory Controller.
		{"not a boolean", "yes", execagent.ReasonThinPoolMissing, execagent.ReasonThinPoolMissing},
		// A reason the API server would refuse becomes HostNotReady.
		{"no reason", "false", "", batteryv1alpha1.ReasonHostNotReady},
		{"an invalid reason", "false", "not a reason!", batteryv1alpha1.ReasonHostNotReady},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The claim was ready before.
			claim := aBoundClaim(claimKey.Name, nodeA, agentAddr)
			meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
				Type: batteryv1alpha1.ConditionHostReady, Status: metav1.ConditionTrue,
				Reason: execagent.ReasonReady, LastTransitionTime: metav1.NewTime(start),
			})
			c := newFakeClient(t, claim, reportedNode(tc.ready, tc.reason, "flintlock-kvm-check.service: /dev/kvm is absent"))
			got := reconcileHostReady(t, c)
			wantHostReady(t, got, metav1.ConditionFalse, tc.wantReason, "flintlock-kvm-check.service: /dev/kvm is absent")
			if got.Status.Host.AgentAddress != agentAddr {
				t.Errorf("host.agentAddress = %q, want it kept", got.Status.Host.AgentAddress)
			}
		})
	}
}

func TestHostReadyCutsALongMessage(t *testing.T) {
	long := strings.Repeat("m", maxConditionMessage+10)
	c := newFakeClient(t, aBoundClaim(claimKey.Name, nodeA, agentAddr),
		reportedNode(reportNotReady, execagent.ReasonKVMUnavailable, long))
	got := reconcileHostReady(t, c)
	wantHostReady(t, got, metav1.ConditionFalse, execagent.ReasonKVMUnavailable, long[:maxConditionMessage])
}

//= docs/requirements/02-claims.md#host-readiness
//= type=test
//# If a Bound claim's Host has no Node, or its Node carries no
//# Node report, then the Claim Controller SHALL keep the claim Bound and set
//# the condition `HostReady` false with the reason `NodeNotFound` or
//# `NoNodeReport`.

func TestHostReadyIsFalseWithoutANodeReport(t *testing.T) {
	noReport := aNode(agentAddr)
	noReport.Annotations = nil
	for _, tc := range []struct {
		name       string
		objs       []client.Object
		wantReason string
	}{
		{"no Node", []client.Object{aBoundClaim(claimKey.Name, nodeA, agentAddr)}, batteryv1alpha1.ReasonNodeNotFound},
		{"a Node with no report", []client.Object{aBoundClaim(claimKey.Name, nodeA, agentAddr), noReport}, batteryv1alpha1.ReasonNoNodeReport},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := reconcileHostReady(t, newFakeClient(t, tc.objs...))
			wantHostReady(t, got, metav1.ConditionFalse, tc.wantReason, "")
		})
	}
}

func TestHostReadyFollowsTheReportBackToReady(t *testing.T) {
	claim := aBoundClaim(claimKey.Name, nodeA, agentAddr)
	meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
		Type: batteryv1alpha1.ConditionHostReady, Status: metav1.ConditionFalse,
		Reason: execagent.ReasonKVMUnavailable, LastTransitionTime: metav1.NewTime(start),
	})
	c := newFakeClient(t, claim, reportedNode("true", execagent.ReasonReady, "ready"))
	got := reconcileHostReady(t, c)
	wantHostReady(t, got, metav1.ConditionTrue, execagent.ReasonReady, "ready")
}

func TestHostReadyLeavesAnUnboundClaimAlone(t *testing.T) {
	expired := aBoundClaim(claimKey.Name, nodeA, agentAddr)
	expired.Status.Phase = batteryv1alpha1.MicroVMClaimExpired
	for name, obj := range map[string]*batteryv1alpha1.MicroVMClaim{
		"pending": aClaim(batteryv1alpha1.ReleaseFinalizer),
		"expired": expired,
	} {
		t.Run(name, func(t *testing.T) {
			c := newFakeClient(t, obj, reportedNode(reportNotReady, execagent.ReasonKVMUnavailable, "gone"))
			s := scopeFor(t, c, claimKey, &stubBattery{})
			if _, err := (HostReady{}).Reconcile(context.Background(), s); err != nil {
				t.Fatal(err)
			}
			if cond := meta.FindStatusCondition(s.Object.Status.Conditions, batteryv1alpha1.ConditionHostReady); cond != nil {
				t.Errorf("HostReady = %+v, want no condition on a claim that is not Bound", cond)
			}
		})
	}
}

func TestHostReadyReturnsAFailedNodeRead(t *testing.T) {
	boom := errors.New("boom")
	c := newFakeClient(t, aBoundClaim(claimKey.Name, nodeA, agentAddr), aNode(agentAddr))
	failing := interceptor.NewClient(c, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Node); ok {
				return boom
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	})
	s := scopeFor(t, failing, claimKey, &stubBattery{})
	if _, err := (HostReady{}).Reconcile(context.Background(), s); !errors.Is(err, boom) {
		t.Fatalf("Reconcile error = %v, want the Node read's", err)
	}
	if cond := meta.FindStatusCondition(s.Object.Status.Conditions, batteryv1alpha1.ConditionHostReady); cond != nil {
		t.Errorf("HostReady = %+v, want it left as it was", cond)
	}
}
