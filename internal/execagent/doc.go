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
//   - claims.go and dynclaims.go: every request that names a MicroVM
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
//   - node.go and annotations.go: the readiness check, with the Host
//     Image's not ready reasons (EA-033), and the Node report (EA-034).
//   - drain.go: the drain guard (EA-040).
//   - identity.go: the check, at start, that the agent's identity names its
//     Host (EA-050), which config/exec-agent/admission-policy.yaml relies on
//     (EA-051).
//
// # The claim check is provisional
//
// The claim checks of 05-exec-agent.md#authorization (EA-010 to EA-013) are
// issue #16's. Until it lands, the agent keeps flintlock-runner's check:
// the caller has to be the identity that created a Bound, unexpired claim
// naming the MicroVM and this Host. Claims are read through ClaimLookup,
// and the one implementation here, DynamicClaims, reads a test definition
// of the resource: group `claims.test.battery.liquidmetal-x.dev`, version
// `v1alpha1`, resource `microvmclaims`, in
// internal/execagent/testdata/crds/microvmclaims.yaml. Each fact the checks
// turn on is one field of it, named in ProvisionalClaimResource:
//
//   - phase: `status.phase`, one of Pending, Bound, Expired and Released;
//   - MicroVM uid: `status.microVM.uid`;
//   - Host: `status.host.nodeName`, the node name of the Host's Node;
//   - expiry: `status.leaseExpiresAt`, an RFC 3339 time;
//   - creator identity: the annotation
//     `claims.test.battery.liquidmetal-x.dev/creator`, the user name of the
//     identity that created the claim.
//
// An annotation is not a trustworthy record of who created an object,
// because whoever may update the claim may write it. #16 replaces the
// creator check with the Holder and bound-token checks of EA-011 and
// EA-012, and the test resource with this project's MicroVMClaim.
package execagent
