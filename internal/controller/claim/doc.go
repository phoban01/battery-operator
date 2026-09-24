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

// Package claim holds the Claim Controller's subreconcilers
// (docs/requirements/02-claims.md). Each is one step of a MicroVMClaim's
// life and works on a claimscope.Scope; the controller in
// internal/controller runs them as a chain and patches the claim once at
// the end.
//
// Release comes first. For a claim being deleted it calls battery's
// ReleaseVM for the claim's Lease, if it has one, removes the release
// finalizer once the Lease is gone, and stops the chain (CL-020, CL-021,
// CL-022).
//
// Binding is four steps, in this order:
//
//   - EnsureFinalizer puts the release finalizer on the claim, and stops
//     the chain until that is written (CL-001).
//   - Bind calls battery's ClaimVM for a claim that has no Lease, and
//     records a successful answer in the status (CL-002).
//   - AgentAddress copies the Exec Agent's address from the Node report of
//     a Bound claim's Host, or says there is none (CL-005, CL-006). The
//     controller watches Nodes, so a change to the report reaches the
//     claims bound on the Host (ClaimsOnNode).
//   - Pending records why a claim could not be bound, and asks for a retry
//     with backoff (CL-003, CL-004, CL-007).
//
// A Bound claim then has three, in this order, each of which stops the
// chain once it has called battery or changed the phase:
//
//   - ExpireDeleted expires the claim once battery's Events stream has
//     reported its MicroVM deleted (CL-013).
//   - Renew relays a pending renewal as a Heartbeat, and records battery's
//     expiry with the renewTime it relayed (CL-010, CL-011, CL-012,
//     CL-015).
//   - CheckExpiry reads the Lease with ListLeases once the expiry in the
//     status has passed, or when there is none yet, and keeps or expires
//     the claim by battery's record (CL-012, CL-014, CL-016, CL-017). It
//     waits while a renewal is pending, so Renew always goes first (#85).
//
// Bind calls ClaimVM only while it holds one of BindSlots' slots, so that
// renewals never wait behind a slow ClaimVM (CL-018).
//
// After them, however they ended, Synced sets the Synced condition from
// the last call to battery (CL-040, CL-041, CL-042).
package claim
