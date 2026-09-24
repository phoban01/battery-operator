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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/controller/claimscope"
	"github.com/phoban01/battery-operator/internal/execagent"
)

const (
	// nodeA is the Node of the Host the tests' claims are bound on.
	nodeA = "node-a"
	// agentAddr is the Exec Agent address in nodeA's report.
	agentAddr = "10.0.0.7:7443"
)

// aBoundClaim is aClaim bound on the Node nodeName, with the Exec Agent
// address agentAddress already in its status.
func aBoundClaim(name, nodeName, agentAddress string) *batteryv1alpha1.MicroVMClaim {
	c := aClaim(batteryv1alpha1.ReleaseFinalizer)
	c.Name = name
	c.Status = batteryv1alpha1.MicroVMClaimStatus{
		Phase:   batteryv1alpha1.MicroVMClaimBound,
		LeaseID: "lease-" + name,
		MicroVM: &batteryv1alpha1.MicroVMReference{UID: "vm-" + name},
		Host:    &batteryv1alpha1.HostReference{NodeName: nodeName, AgentAddress: agentAddress},
		Conditions: []metav1.Condition{{
			Type:               batteryv1alpha1.ConditionBound,
			Status:             metav1.ConditionTrue,
			Reason:             batteryv1alpha1.ReasonBound,
			LastTransitionTime: metav1.NewTime(start),
		}},
	}
	return c
}

// aNode is the Node nodeA, whose Node report carries the Exec Agent address
// agentAddress, or none if it is empty.
func aNode(agentAddress string) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: nodeA,
		Annotations: map[string]string{
			execagent.AnnotationReady:  "true",
			execagent.AnnotationReason: execagent.ReasonReady,
		},
	}}
	if agentAddress != "" {
		n.Annotations[execagent.AnnotationAddress] = agentAddress
	}
	return n
}

// reconcileAgentAddress runs AgentAddress on a scope for the claim at
// claimKey in c, with a battery that fails the test if it is called.
func reconcileAgentAddress(t *testing.T, c client.Client) *claimscope.Scope {
	t.Helper()
	// Every method of the zero stubBattery panics: AgentAddress must not
	// call battery.
	s := scopeFor(t, c, claimKey, &stubBattery{})
	res, err := AgentAddress{}.Reconcile(context.Background(), s)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res != (claimscope.Result{}) {
		t.Errorf("result = %+v, want the chain to go on", res)
	}
	return s
}

func TestAgentAddressComesFromTheNodeReport(t *testing.T) {
	//= docs/requirements/02-claims.md#binding
	//= type=test
	//# When a claim is Bound, the Claim Controller SHALL set the claim's
	//# Exec Agent address from the Node report of the claim's Host.
	c := newFakeClient(t, aBoundClaim(claimKey.Name, nodeA, ""), aNode(agentAddr))
	s := reconcileAgentAddress(t, c)

	st := s.Claim.Status
	if st.Host.AgentAddress != agentAddr {
		t.Errorf("host.agentAddress = %q, want the Node report's 10.0.0.7:7443", st.Host.AgentAddress)
	}
	if st.Phase != batteryv1alpha1.MicroVMClaimBound {
		t.Errorf("phase = %q, want Bound", st.Phase)
	}
	cond := meta.FindStatusCondition(st.Conditions, batteryv1alpha1.ConditionAgentAvailable)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != batteryv1alpha1.ReasonAgentAddressPublished {
		t.Errorf("AgentAvailable = %+v, want True/AgentAddressPublished", cond)
	}

	// The controller's patch writes it.
	if err := s.Patch(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := &batteryv1alpha1.MicroVMClaim{}
	if err := c.Get(context.Background(), claimKey, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Host.AgentAddress != agentAddr {
		t.Errorf("stored host.agentAddress = %q, want 10.0.0.7:7443", got.Status.Host.AgentAddress)
	}
	if got.Status.Host.NodeName != nodeA || got.Status.LeaseID != "lease-"+claimKey.Name {
		t.Errorf("stored status = %+v, want the binding kept", got.Status)
	}
}

func TestAgentAddressFollowsAChangedNodeReport(t *testing.T) {
	c := newFakeClient(t, aBoundClaim(claimKey.Name, nodeA, agentAddr), aNode("10.0.0.8:7443"))
	s := reconcileAgentAddress(t, c)
	if got := s.Claim.Status.Host.AgentAddress; got != "10.0.0.8:7443" {
		t.Errorf("host.agentAddress = %q, want the Node report's new 10.0.0.8:7443", got)
	}
}

func TestAgentAddressMissingKeepsTheClaimBound(t *testing.T) {
	//= docs/requirements/02-claims.md#binding
	//= type=test
	//# If the Node report of a Bound claim's Host carries no Exec Agent
	//# address, then the Claim Controller SHALL keep the claim Bound and set the
	//# condition `AgentAvailable` false with the reason `NoAgentAddress`.
	for _, tc := range []struct {
		name string
		objs []client.Object
	}{
		{
			name: "a Node report with no address",
			objs: []client.Object{aBoundClaim(claimKey.Name, nodeA, ""), aNode("")},
		},
		{
			name: "a report that dropped its address",
			objs: []client.Object{aBoundClaim(claimKey.Name, nodeA, agentAddr), aNode("")},
		},
		{
			name: "no Node",
			objs: []client.Object{aBoundClaim(claimKey.Name, nodeA, "")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeClient(t, tc.objs...)
			s := reconcileAgentAddress(t, c)
			if err := s.Patch(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := &batteryv1alpha1.MicroVMClaim{}
			if err := c.Get(context.Background(), claimKey, got); err != nil {
				t.Fatal(err)
			}

			st := got.Status
			if st.Phase != batteryv1alpha1.MicroVMClaimBound {
				t.Errorf("phase = %q, want the claim kept Bound", st.Phase)
			}
			if !meta.IsStatusConditionTrue(st.Conditions, batteryv1alpha1.ConditionBound) {
				t.Errorf("Bound condition = %+v, want it kept true", meta.FindStatusCondition(st.Conditions, batteryv1alpha1.ConditionBound))
			}
			if st.LeaseID != "lease-"+claimKey.Name || st.Host.NodeName != nodeA {
				t.Errorf("status = %+v, want the binding kept", st)
			}
			if st.Host.AgentAddress != "" {
				t.Errorf("host.agentAddress = %q, want none", st.Host.AgentAddress)
			}
			cond := meta.FindStatusCondition(st.Conditions, batteryv1alpha1.ConditionAgentAvailable)
			if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != batteryv1alpha1.ReasonNoAgentAddress {
				t.Errorf("AgentAvailable = %+v, want False/NoAgentAddress", cond)
			}
		})
	}
}

func TestAgentAddressLeavesAnUnboundClaimAlone(t *testing.T) {
	c := newFakeClient(t, aClaim(batteryv1alpha1.ReleaseFinalizer), aNode(agentAddr))
	s := reconcileAgentAddress(t, c)
	if s.Claim.Status.Host != nil {
		t.Errorf("host = %+v, want none on an unbound claim", s.Claim.Status.Host)
	}
	if cond := meta.FindStatusCondition(s.Claim.Status.Conditions, batteryv1alpha1.ConditionAgentAvailable); cond != nil {
		t.Errorf("AgentAvailable = %+v, want no condition on an unbound claim", cond)
	}
}

func TestAgentAddressReturnsAFailedNodeRead(t *testing.T) {
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
	if _, err := (AgentAddress{}).Reconcile(context.Background(), s); !errors.Is(err, boom) {
		t.Fatalf("Reconcile error = %v, want the Node read's", err)
	}
	if got := s.Claim.Status.Host.AgentAddress; got != agentAddr {
		t.Errorf("host.agentAddress = %q, want it left as it was", got)
	}
}

func TestClaimsOnNodeMapsANodeToTheClaimsBoundOnIt(t *testing.T) {
	c := newFakeClient(t,
		aBoundClaim("bound-1", nodeA, ""),
		aBoundClaim("bound-2", nodeA, ""),
		aBoundClaim("bound-3", "node-b", ""),
		aClaim(), // runner-1, not bound
	)
	got := ClaimsOnNode(c)(context.Background(), aNode(agentAddr))
	want := map[reconcile.Request]bool{
		{NamespacedName: client.ObjectKey{Namespace: "ci", Name: "bound-1"}}: true,
		{NamespacedName: client.ObjectKey{Namespace: "ci", Name: "bound-2"}}: true,
	}
	if len(got) != len(want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	for _, r := range got {
		if !want[r] {
			t.Errorf("request %v is not for a claim on node-a", r)
		}
	}
}

func TestAgentAddressChangedPassesOnlyReportAddressChanges(t *testing.T) {
	p := AgentAddressChanged()
	relabelled := aNode(agentAddr)
	relabelled.Labels = map[string]string{"x": "y"}
	for _, tc := range []struct {
		name     string
		old, new *corev1.Node
		want     bool
	}{
		{"address published", aNode(""), aNode(agentAddr), true},
		{"address changed", aNode(agentAddr), aNode("10.0.0.8:7443"), true},
		{"address dropped", aNode(agentAddr), aNode(""), true},
		{"something else changed", aNode(agentAddr), relabelled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.Update(event.UpdateEvent{ObjectOld: tc.old, ObjectNew: tc.new}); got != tc.want {
				t.Errorf("Update = %v, want %v", got, tc.want)
			}
		})
	}
	if !p.Create(event.CreateEvent{Object: aNode("")}) {
		t.Error("Create = false, want a new Node to reach its claims")
	}
	if !p.Delete(event.DeleteEvent{Object: aNode("")}) {
		t.Error("Delete = false, want a deleted Node to reach its claims")
	}
}
