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
	"errors"
	"fmt"
	"time"
)

// ClaimPhase is the phase a claim's status reports.
type ClaimPhase string

// The phases of a claim.
const (
	ClaimPending  ClaimPhase = "Pending"
	ClaimBound    ClaimPhase = "Bound"
	ClaimExpired  ClaimPhase = "Expired"
	ClaimReleased ClaimPhase = "Released"
)

// Claim is what the Exec Agent reads of a claim: exactly the facts the
// authorization and the drain guard (EA-040) turn on, and nothing that
// depends on how the resource spells them.
type Claim struct {
	// Namespace and Name identify the claim, for messages.
	Namespace string
	Name      string
	// Phase is the claim's phase.
	Phase ClaimPhase
	// VMUID is the flintlock uid of the MicroVM the claim is bound to.
	VMUID string
	// HostNode is the name of the Node of the Host the MicroVM runs on.
	HostNode string
	// ExpiresAt is when the claim's lease runs out. The zero time means the
	// claim records none, which is never taken to mean "never".
	ExpiresAt time.Time
	// Creator is the user name of the identity that created the claim, as
	// the API server authenticated it. Empty means the claim records none.
	Creator string
}

// String names the claim.
func (c Claim) String() string { return c.Namespace + "/" + c.Name }

// ClaimLookup reads claims. The provisional implementation, DynamicClaims,
// reads a test definition of the resource; #16 replaces it with this
// project's MicroVMClaim, and nothing else in the agent changes.
type ClaimLookup interface {
	// ClaimsForVM returns every claim that names the MicroVM uid, read
	// fresh enough that a claim deleted or changed before the call is not
	// returned as it was. An error means the lookup could not be completed,
	// and the caller refuses (EA-014).
	ClaimsForVM(ctx context.Context, vmUID string) ([]Claim, error)
	// BoundOnHost returns the claims whose phase is Bound and whose Host is
	// hostNode (EA-040).
	BoundOnHost(ctx context.Context, hostNode string) ([]Claim, error)
}

// errNotAuthorized is wrapped by every refusal of Authorize.
var errNotAuthorized = errors.New("no Bound claim of the caller names this microvm on this host")

// Authorize decides whether one claim lets caller use the MicroVM vmUID on
// the Host whose Node is hostNode at now. Every condition is checked and a
// claim that says nothing about one of them fails it: no recorded creator,
// no recorded expiry, no Host. The error says which condition failed, for
// the agent's log; the caller is told only that it was refused.
//
// This is flintlock-runner's check, kept until #16 replaces it with
// EA-011 to EA-013.
func Authorize(c Claim, caller, vmUID, hostNode string, now time.Time) error {
	switch {
	case c.Phase != ClaimBound:
		return fmt.Errorf("claim %s is %q, not Bound: %w", c, c.Phase, errNotAuthorized)
	case c.VMUID == "" || c.VMUID != vmUID:
		return fmt.Errorf("claim %s names microvm %q, not %q: %w", c, c.VMUID, vmUID, errNotAuthorized)
	case c.HostNode == "" || c.HostNode != hostNode:
		return fmt.Errorf("claim %s names host %q, not %q: %w", c, c.HostNode, hostNode, errNotAuthorized)
	case c.ExpiresAt.IsZero():
		return fmt.Errorf("claim %s records no lease expiry: %w", c, errNotAuthorized)
	case !now.Before(c.ExpiresAt):
		return fmt.Errorf("claim %s expired at %s: %w", c, c.ExpiresAt.UTC().Format(time.RFC3339), errNotAuthorized)
	case c.Creator == "":
		return fmt.Errorf("claim %s records no creator: %w", c, errNotAuthorized)
	case c.Creator != caller:
		return fmt.Errorf("claim %s was created by %q, not %q: %w", c, c.Creator, caller, errNotAuthorized)
	}
	return nil
}

//= docs/requirements/05-exec-agent.md#authorization
//# If the TokenReview or the claim lookup of a request cannot be
//# completed, then the Exec Agent SHALL refuse the request.

// authorizeClaims looks up the claims that name vmUID and grants the
// request when one of them authorizes caller. A lookup that fails refuses,
// wrapped so that the caller can tell it from a plain refusal: the first is
// reported as unavailable, the second as permission denied, and neither
// runs anything.
func authorizeClaims(ctx context.Context, claims ClaimLookup, caller, vmUID, hostNode string, now time.Time) error {
	if vmUID == "" {
		return fmt.Errorf("the request names no microvm: %w", errNotAuthorized)
	}
	found, err := claims.ClaimsForVM(ctx, vmUID)
	if err != nil {
		return &lookupError{err: err}
	}
	if len(found) == 0 {
		return fmt.Errorf("no claim names microvm %s: %w", vmUID, errNotAuthorized)
	}
	refusals := make([]error, 0, len(found))
	for _, c := range found {
		err := Authorize(c, caller, vmUID, hostNode, now)
		if err == nil {
			return nil
		}
		refusals = append(refusals, err)
	}
	return errors.Join(refusals...)
}

// lookupError is a claim lookup that could not be completed (EA-014).
type lookupError struct{ err error }

func (e *lookupError) Error() string { return "looking up the claims: " + e.err.Error() }
func (e *lookupError) Unwrap() error { return e.err }
