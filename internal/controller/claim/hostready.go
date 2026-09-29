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
	"regexp"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/controller/claimscope"
	"github.com/phoban01/battery-operator/internal/execagent"
	"github.com/phoban01/battery-operator/internal/reconcile"
)

// conditionReason is what the API server accepts as a condition's reason
// (metav1.Condition), and maxConditionReason and maxConditionMessage are
// the longest reason and message it accepts.
var conditionReason = regexp.MustCompile(`^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$`)

const (
	maxConditionReason  = 1024
	maxConditionMessage = 32768
)

// HostReady mirrors the readiness in the Node report of a Bound claim's
// Host into the claim's HostReady condition, so that a Holder learns that
// its Host went bad without reading Nodes (#186). It never changes the
// phase: the Lease is battery's.
//
// HostReady reads the Node from the manager's cache and never calls
// battery. The controller watches Nodes (ClaimsOnNode, NodeReportChanged),
// so a change to the report reaches every claim bound on the Host.
type HostReady struct{}

var _ reconcile.SubReconciler[*claimscope.Scope] = HostReady{}

// Reconcile implements reconcile.SubReconciler[*claimscope.Scope].
func (HostReady) Reconcile(ctx context.Context, s *claimscope.Scope) (reconcile.Result, error) {
	st := &s.Object.Status
	if st.Phase != batteryv1alpha1.MicroVMClaimBound || st.Host == nil || st.Host.NodeName == "" {
		return reconcile.Result{}, nil
	}

	nodeName := st.Host.NodeName
	cond := metav1.Condition{
		Type:               batteryv1alpha1.ConditionHostReady,
		ObservedGeneration: s.Object.Generation,
		LastTransitionTime: metav1.NewTime(s.Clock.Now()),
	}
	node := &corev1.Node{}
	switch err := s.Client.Get(ctx, client.ObjectKey{Name: nodeName}, node); {
	case apierrors.IsNotFound(err):
		//= docs/requirements/02-claims.md#host-readiness
		//# If a Bound claim's Host has no Node, or its Node carries no
		//# Node report, then the Claim Controller SHALL keep the claim Bound and set
		//# the condition `HostReady` false with the reason `NodeNotFound` or
		//# `NoNodeReport`.
		cond.Status = metav1.ConditionFalse
		cond.Reason = batteryv1alpha1.ReasonNodeNotFound
		cond.Message = fmt.Sprintf("the Host's Node %s does not exist", nodeName)
	case err != nil:
		return reconcile.Result{}, fmt.Errorf("reading Node %s for its Host's readiness: %w", nodeName, err)
	default:
		cond.Status, cond.Reason, cond.Message = reportedReadiness(node)
	}

	if cond.Status == metav1.ConditionFalse && !meta.IsStatusConditionFalse(st.Conditions, batteryv1alpha1.ConditionHostReady) {
		s.Log.Info("Found the Host of MicroVMClaim not ready", "node", nodeName, "reason", cond.Reason, "message", cond.Message)
	}
	meta.SetStatusCondition(&st.Conditions, cond)
	return reconcile.Result{}, nil
}

// reportedReadiness is the HostReady condition's status, reason and message
// from a Node's report.
func reportedReadiness(node *corev1.Node) (metav1.ConditionStatus, string, string) {
	ready, ok := node.Annotations[execagent.AnnotationReady]
	if !ok {
		return metav1.ConditionFalse, batteryv1alpha1.ReasonNoNodeReport,
			fmt.Sprintf("Node %s carries no Node report in %s", node.Name, execagent.AnnotationReady)
	}
	reason := node.Annotations[execagent.AnnotationReason]
	message := node.Annotations[execagent.AnnotationMessage]
	if len(message) > maxConditionMessage {
		message = message[:maxConditionMessage]
	}

	// Only "true" is ready, as for the Inventory Controller (IN-001).
	if ready == "true" {
		//= docs/requirements/02-claims.md#host-readiness
		//# While a claim is Bound and the Node report of its Host says
		//# the Host is ready, the Claim Controller SHALL set the claim's condition
		//# `HostReady` true with the reason and message of the Node report.
		if !validReason(reason) {
			reason = execagent.ReasonReady
		}
		return metav1.ConditionTrue, reason, message
	}

	//= docs/requirements/02-claims.md#host-readiness
	//# While a claim is Bound and the Node report of its Host says
	//# the Host is not ready, the Claim Controller SHALL keep the claim Bound and
	//# set the condition `HostReady` false with the reason and message of the
	//# Node report.
	if !validReason(reason) {
		reason = batteryv1alpha1.ReasonHostNotReady
	}
	return metav1.ConditionFalse, reason, message
}

// validReason reports whether the API server accepts r as a condition's
// reason.
func validReason(r string) bool {
	return len(r) <= maxConditionReason && conditionReason.MatchString(r)
}
