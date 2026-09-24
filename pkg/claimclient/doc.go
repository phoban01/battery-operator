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

// Package claimclient is the Client Library (docs/requirements/07-client.md):
// what a Consumer uses to claim a MicroVM from a Pool, hold it, run commands
// in it through the Exec Agent on its Host, and release it.
//
// A Consumer makes one Client and, for each MicroVM it needs, follows this
// sequence:
//
//	cl, err := claimclient.New(claimclient.Config{
//		Client:    kubeClient, // a controller-runtime client
//		Namespace: "ci",
//		ServingCA: servingCA,  // the Operator's serving CA (ADR 0004)
//	})
//
//	// Claim creates the MicroVMClaim and its Secret, and blocks until the
//	// claim is Bound. Cancelling ctx gives up and deletes the claim.
//	claim, err := cl.Claim(ctx, claimclient.Request{
//		Pool:               "small",
//		ServiceAccountName: "runner",
//		OnPending: func(p claimclient.Pending) {
//			if p.PoolExhausted() {
//				// tell the user the Pool has no MicroVM free
//			}
//		},
//	})
//	if errors.Is(err, claimclient.ErrPoolExhausted) {
//		// gave up while the Pool was exhausted
//	}
//	defer claim.Release(context.WithoutCancel(ctx))
//
//	// Dial reaches the Exec Agent at the claim's agent address over TLS,
//	// and sends the claim token with every call. The connection speaks
//	// flintlock's MicroVMExec service (EA-003) for claim.MicroVMUID().
//	conn, err := claim.Dial(ctx)
//	defer conn.Close()
//	exec := execv1.NewMicroVMExecClient(conn)
//
//	// While the command runs, watch for the Lease being lost.
//	select {
//	case <-done:
//	case <-claim.Lost():
//		return claim.Err() // wraps ErrLeaseLost
//	}
//
// Between Claim and Release, the Client renews the claim's Lease by setting
// spec.renewTime at the Pool's heartbeat interval, and requests a new claim
// token when two thirds of the current one's lifetime have passed. It
// reports the Lease as lost, through Lost and Err, when the claim becomes
// Expired or when someone else deletes or replaces it.
//
// # Tokens
//
// Each claim has an empty Secret `<claim name>-exec`, which the Client
// creates with the claim as its owner. The claim token is a ServiceAccount
// token for the claim's Holder, requested with TokenRequest for the Exec
// Agent's audience (v1alpha1.ExecAgentTokenAudience) and bound to that
// Secret by name and uid. It opens the claim's MicroVM and nothing else, and
// dies with the claim: deleting the claim garbage-collects the Secret, and
// a token bound to a deleted Secret no longer authenticates
// (05-exec-agent.md#authorization).
//
// # Permissions
//
// The identity the Client's Kubernetes client runs as needs, in the
// namespace: create, get, patch and delete on microvmclaims; get on pools;
// create on secrets; and create on serviceaccounts/token for each Holder.
// Its scheme has to know client-go's types and this project's v1alpha1.
package claimclient
