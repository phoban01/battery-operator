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

// ClaimsOnNode maps an event on a Node to a request for every MicroVMClaim
// bound on it, so that a change to the Node's report reaches AgentAddress.
// It needs the NodeNameField index on c.
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

// AgentAddressChanged passes a Node's creation and deletion, and an update
// only when it changes the Exec Agent's address in the Node's report. A
// Node's status changes far more often than the report, and none of those
// changes matter to a claim.
func AgentAddressChanged() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			return agentAddressOf(e.ObjectOld) != agentAddressOf(e.ObjectNew)
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// agentAddressOf is the Exec Agent's address in a Node's report.
func agentAddressOf(obj client.Object) string {
	return obj.GetAnnotations()[execagent.AnnotationAddress]
}
