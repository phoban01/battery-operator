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
// Binding is three steps, in this order:
//
//   - EnsureFinalizer puts the release finalizer on the claim, and stops
//     the chain until that is written (CL-001).
//   - Bind calls battery's ClaimVM for a claim that has no Lease, and
//     records a successful answer in the status (CL-002).
//   - Pending records why a claim could not be bound, and asks for a retry
//     with backoff (CL-003, CL-004, CL-007).
//
// After them, however they ended, Synced sets the Synced condition from
// the last call to battery (CL-040, CL-041, CL-042).
package claim
