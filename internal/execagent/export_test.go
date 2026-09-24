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

import "sigs.k8s.io/controller-runtime/pkg/client"

// What the unit tests of package execagent_test reach inside the package.

// HostNodeIndex and ClaimHostNode index a fake client's claims as the
// claim cache does.
const HostNodeIndex = hostNodeIndex

// ClaimHostNode is the index function of HostNodeIndex.
var ClaimHostNode = claimHostNode

// NewKubeClaimsOver is the claim lookup over readers other than the API
// server's: controller-runtime's fake client, indexed by HostNodeIndex.
func NewKubeClaimsOver(reader, cached client.Reader) *KubeClaims {
	return newKubeClaims(reader, cached)
}
