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

// Package execagent is the Exec Agent (docs/requirements/05-exec-agent.md),
// which cmd/exec-agent runs on every Host. It relays a claim's Holder's
// exec requests to the flintlockd on that Host, after checking the caller
// against the claims, and reports the Host's readiness on the Host's Node.
// It comes from flintlock-runner's internal/agent.
//
// The exec API is flintlock's own `microvmexec.services.api.v1alpha1`
// service, and the ServerInfo and GetMicroVM calls of its
// `microvm.services.api.v1alpha1` service, served over TLS on the Host's
// internal address (EA-002, EA-003). A consumer that already speaks to
// flintlockd's exec API speaks to the Exec Agent unchanged, with a bearer
// token added.
//
// The pieces are:
//
//   - auth.go: every request authenticated with a TokenReview of its
//     bearer token; a review that cannot be made refuses (EA-014).
//   - claims.go and kubeclaims.go: every request that names a MicroVM
//     authorized against the claims, through the ClaimLookup interface; a
//     lookup that cannot be made refuses (EA-014).
//   - server.go: the relay to flintlockd, whose response ends with
//     flintlockd's exit_code or with an error status and never with a clean
//     end that has no exit code (EA-020), and whose stream to flintlockd has
//     to be open within a deadline (EA-021).
//   - flintlockd.go: the connection to this Host's flintlockd, over mutual
//     TLS, refused for any endpoint that is not an address of this Host
//     (EA-001).
//   - tls.go: the serving certificate, which has to name the Host's
//     internal address (EA-002).
//   - node.go and annotations.go: the readiness check, of the Host
//     prerequisites in internal/hostcheck (EA-030 to EA-032) and the Host
//     Image's not ready reasons (EA-033), and the Node report (EA-034,
//     EA-035).
//   - drain.go: the drain guard (EA-040).
//   - identity.go: the check, at start, that the agent's identity names its
//     Host (EA-050), which config/exec-agent/admission-policy.yaml relies on
//     (EA-051).
//
// # The claim checks
//
// A request that names a MicroVM runs only when all four checks of
// 05-exec-agent.md#authorization pass:
//
//   - EA-010 (auth.go): the bearer token passes a TokenReview for the
//     agent's audience, DefaultTokenAudience unless configured otherwise.
//   - EA-011 (claims.go): the token's user is
//     `system:serviceaccount:<claim namespace>:<spec.serviceAccountName>`.
//   - EA-012 (claims.go): the token is bound to the claim's Secret
//     `<claim name>-exec`. A TokenReview does not report the object a
//     Secret-bound token is bound to, only the token's id
//     (`authentication.kubernetes.io/credential-id`); the agent reads the
//     Secret's name and uid from the token's own `kubernetes.io.secret`
//     claim once the review has vouched for the token, and checks that the
//     claim's `sub` and `jti` are the ones the review reported. The API
//     server compares the uid with the live Secret's during the review, so
//     the agent never reads Secrets.
//   - EA-013 (claims.go): the claim is Bound, its Lease has not expired,
//     and it names the MicroVM's uid and this Host.
//
// The claim is the one the token names: the Secret's name less `-exec`, in
// the Secret's namespace. KubeClaims (kubeclaims.go) reads it through the
// typed MicroVMClaim API, afresh from the API server for every request, and
// answers the drain guard from a cache of every claim, indexed by Host.
package execagent
