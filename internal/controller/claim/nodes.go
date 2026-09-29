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

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/execagent"
)

// NodeNameField is the name of the cache index of MicroVMClaims by the
// node name in their status, which ClaimsOnNode looks claims up by.
const NodeNameField = "status.host.nodeName"

// NodeName is the indexer of NodeNameField: a claim's Host's node name, or
// nothing for a claim with no Host.
func NodeName(obj client.Object) []string {
	c, ok := obj.(*batteryv1alpha1.MicroVMClaim)
	if !ok || c.Status.Host == nil || c.Status.Host.NodeName == "" {
		return nil
	}
	return []string{c.Status.Host.NodeName}
}

//= docs/requirements/02-claims.md#host-readiness
//# When a Node is created or deleted, or the readiness, reason,
//# message or Exec Agent address in its Node report changes, the Claim
//# Controller SHALL reconcile every claim bound on that Node.

// ClaimsOnNode maps an event on a Node to a request for every MicroVMClaim
// bound on it, so that a change to the Node's report reaches AgentAddress
// and HostReady. It needs the NodeNameField index on c.
func ClaimsOnNode(c client.Reader) func(context.Context, client.Object) []reconcile.Request {
	return func(ctx context.Context, node client.Object) []reconcile.Request {
		var claims batteryv1alpha1.MicroVMClaimList
		if err := c.List(ctx, &claims, client.MatchingFields{NodeNameField: node.GetName()}); err != nil {
			// The claims are reconciled again on their own next change;
			// the Node's next change retries the lookup.
			logf.FromContext(ctx).Error(err, "Failed to list the MicroVMClaims on a Node", "node", node.GetName())
			return nil
		}
		reqs := make([]reconcile.Request, 0, len(claims.Items))
		for i := range claims.Items {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&claims.Items[i])})
		}
		return reqs
	}
}

// reportAnnotations are the annotations of a Node report that a claim
// mirrors: the Exec Agent's address (AgentAddress) and the Host's
// readiness (HostReady).
var reportAnnotations = []string{
	execagent.AnnotationAddress,
	execagent.AnnotationReady,
	execagent.AnnotationReason,
	execagent.AnnotationMessage,
}

// NodeReportChanged passes a Node's creation and deletion, and an update
// only when it changes the Exec Agent's address or the Host's readiness,
// reason or message in the Node's report. A Node's status changes far more
// often than the report, and none of those changes matter to a claim.
func NodeReportChanged() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			o, n := e.ObjectOld.GetAnnotations(), e.ObjectNew.GetAnnotations()
			for _, k := range reportAnnotations {
				ov, oldHas := o[k]
				nv, newHas := n[k]
				if ov != nv || oldHas != newHas {
					return true
				}
			}
			return false
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}
