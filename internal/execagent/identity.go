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

package execagent

import (
	"context"
	"fmt"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// NodeNameExtra is the key of the user information extra in which the API
// server records the node of the pod a bound ServiceAccount token was issued
// to. config/exec-agent/admission-policy.yaml tells one Host's Exec Agent
// from another's by it.
const NodeNameExtra = "authentication.kubernetes.io/node-name"

//= docs/requirements/05-exec-agent.md#identity
//# When the Exec Agent starts, the Exec Agent SHALL confirm that
//# its identity names its own Host, and SHALL refuse to start otherwise.

// CheckIdentity asks the API server who the agent is, with a
// SelfSubjectReview, and fails unless the answer names hostNode as the node
// of the agent's pod. That holds for the in-cluster configuration of a pod
// on the Host, whose projected ServiceAccount token is bound to the pod and
// carries its node. A kubeconfig with a static token, a legacy Secret token
// or a pod on another node does not name the Host, and the admission policy
// of EA-051 would refuse every change the agent makes to its Host's Node;
// failing here says why at once instead. A SelfSubjectReview needs no
// permission beyond being authenticated.
func CheckIdentity(ctx context.Context, kube kubernetes.Interface, hostNode string) error {
	review, err := kube.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("execagent: asking the API server who this agent is: %w", err)
	}
	info := review.Status.UserInfo
	nodes := info.Extra[NodeNameExtra]
	switch {
	case len(nodes) == 0:
		return fmt.Errorf("execagent: the exec agent authenticates as %q, whose identity names no node; "+
			"run it with the bound ServiceAccount token of a pod on %s (the in-cluster configuration), "+
			"or the admission policy refuses every change it makes to its Host's Node", info.Username, hostNode)
	case len(nodes) != 1 || nodes[0] != hostNode:
		return fmt.Errorf("execagent: the exec agent authenticates as a pod on %v, not on its Host %s", []string(nodes), hostNode)
	}
	return nil
}
