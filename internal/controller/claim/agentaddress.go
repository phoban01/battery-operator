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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/controller/claimscope"
	"github.com/phoban01/battery-operator/internal/execagent"
)

// AgentAddress copies the Exec Agent's address from the Node report of a
// Bound claim's Host into the claim's status.host.agentAddress, and says in
// the AgentAvailable condition whether there was one.
//
// battery's HostInfo.address is where battery reaches the Host's
// flintlockd, not where a Holder reaches the Exec Agent, so the address
// comes from the annotation the agent publishes on its Node (EA-034). A
// Node with no such annotation, or no Node at all, leaves the claim Bound:
// the Lease is battery's, and the address only says where to reach the
// MicroVM. The address is cleared then, so a Holder never dials an agent
// that no longer reports one.
//
// AgentAddress reads the Node from the manager's cache and never calls
// battery. The controller watches Nodes (ClaimsOnNode), so a change to the
// report reaches every claim bound on the Host.
type AgentAddress struct{}

var _ claimscope.Subreconciler = AgentAddress{}

// Reconcile implements claimscope.Subreconciler.
func (AgentAddress) Reconcile(ctx context.Context, s *claimscope.Scope) (claimscope.Result, error) {
	st := &s.Claim.Status
	if st.Phase != batteryv1alpha1.MicroVMClaimBound || st.Host == nil || st.Host.NodeName == "" {
		return claimscope.Result{}, nil
	}

	nodeName := st.Host.NodeName
	node := &corev1.Node{}
	var address string
	switch err := s.Client.Get(ctx, client.ObjectKey{Name: nodeName}, node); {
	case apierrors.IsNotFound(err):
		// A Host with no Node has no Node report, and so no address.
	case err != nil:
		return claimscope.Result{}, fmt.Errorf("reading Node %s for the Exec Agent's address: %w", nodeName, err)
	default:
		address = node.Annotations[execagent.AnnotationAddress]
	}

	cond := metav1.Condition{
		Type:               batteryv1alpha1.ConditionAgentAvailable,
		ObservedGeneration: s.Claim.Generation,
		LastTransitionTime: metav1.NewTime(s.Clock.Now()),
	}
	if address == "" {
		//= docs/requirements/02-claims.md#binding
		//# If the Node report of a Bound claim's Host carries no Exec Agent
		//# address, then the Claim Controller SHALL keep the claim Bound and set the
		//# condition `AgentAvailable` false with the reason `NoAgentAddress`.
		st.Host.AgentAddress = ""
		cond.Status = metav1.ConditionFalse
		cond.Reason = batteryv1alpha1.ReasonNoAgentAddress
		cond.Message = fmt.Sprintf("Node %s reports no Exec Agent address in %s", nodeName, execagent.AnnotationAddress)
		if !meta.IsStatusConditionFalse(st.Conditions, batteryv1alpha1.ConditionAgentAvailable) {
			s.Log.Info("Found no Exec Agent address for MicroVMClaim", "node", nodeName)
		}
		meta.SetStatusCondition(&st.Conditions, cond)
		return claimscope.Result{}, nil
	}

	//= docs/requirements/02-claims.md#binding
	//# When a claim is Bound, the Claim Controller SHALL set the claim's
	//# Exec Agent address from the Node report of the claim's Host.
	if st.Host.AgentAddress != address {
		s.Log.Info("Set the Exec Agent address of MicroVMClaim", "node", nodeName, "agentAddress", address)
	}
	st.Host.AgentAddress = address
	cond.Status = metav1.ConditionTrue
	cond.Reason = batteryv1alpha1.ReasonAgentAddressPublished
	cond.Message = fmt.Sprintf("Node %s reports the Exec Agent at %s", nodeName, address)
	meta.SetStatusCondition(&st.Conditions, cond)
	return claimscope.Result{}, nil
}
