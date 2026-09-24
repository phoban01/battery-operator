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

package v1alpha1

// ExecAgentTokenAudience is the Exec Agent's audience: the audience a
// claim token is requested with (CC-002) and the one the Exec Agent's
// TokenReviews ask for (EA-010). It is here, in the API package, so that
// the Exec Agent and the Client Library share one definition.
const ExecAgentTokenAudience = "battery.liquidmetal-x.dev/exec-agent"

// ExecSecretSuffix ends the name of a claim's Secret, `<claim name>-exec`,
// which each claim token is bound to (CC-001, CC-002, EA-012).
const ExecSecretSuffix = "-exec"

// ExecSecretName is the name of the Secret of the claim named claimName.
func ExecSecretName(claimName string) string { return claimName + ExecSecretSuffix }
