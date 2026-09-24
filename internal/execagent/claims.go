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
	"strings"
	"time"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
)

// ExecSecretSuffix ends the name of a claim's Secret, `<claim name>-exec`,
// which each claim token is bound to (CC-001, CC-002, EA-012). It is
// defined in the API package, which the Client Library shares.
const ExecSecretSuffix = batteryv1alpha1.ExecSecretSuffix

// Claim is what the Exec Agent reads of a MicroVMClaim: exactly the facts
// the authorization and the drain guard (EA-040) turn on.
type Claim struct {
	// Namespace and Name identify the claim.
	Namespace string
	Name      string
	// ServiceAccountName is the claim's Holder, in its namespace.
	ServiceAccountName string
	// Phase is the claim's phase.
	Phase batteryv1alpha1.MicroVMClaimPhase
	// VMUID is the flintlock uid of the MicroVM the claim is bound to.
	VMUID string
	// HostNode is the name of the Node of the Host the MicroVM runs on.
	HostNode string
	// ExpiresAt is when the claim's Lease runs out. The zero time means the
	// claim records none, which is never taken to mean "never".
	ExpiresAt time.Time
}

// String names the claim.
func (c Claim) String() string { return c.Namespace + "/" + c.Name }

// claimOf reads the facts of a MicroVMClaim. A fact the claim does not
// record is left zero, which Authorize refuses.
func claimOf(o *batteryv1alpha1.MicroVMClaim) Claim {
	c := Claim{
		Namespace:          o.Namespace,
		Name:               o.Name,
		ServiceAccountName: o.Spec.ServiceAccountName,
		Phase:              o.Status.Phase,
	}
	if o.Status.MicroVM != nil {
		c.VMUID = o.Status.MicroVM.UID
	}
	if o.Status.Host != nil {
		c.HostNode = o.Status.Host.NodeName
	}
	if o.Status.LeaseExpiresAt != nil {
		c.ExpiresAt = o.Status.LeaseExpiresAt.Time
	}
	return c
}

// ClaimLookup reads claims; KubeClaims reads them from the API server.
type ClaimLookup interface {
	// Claim reads the claim namespace/name afresh from the API server, so
	// that a claim deleted or changed before the call is not returned as
	// it was. A claim that does not exist is (nil, nil). An error means the
	// lookup could not be completed, and the caller refuses (EA-014).
	Claim(ctx context.Context, namespace, name string) (*Claim, error)
	// BoundOnHost returns the claims whose phase is Bound and whose Host is
	// hostNode (EA-040).
	BoundOnHost(ctx context.Context, hostNode string) ([]Claim, error)
}

// errNotAuthorized is wrapped by every refusal of Authorize.
var errNotAuthorized = errors.New("the caller holds no Bound claim on this microvm on this host")

// serviceAccountUser is the user name of a ServiceAccount's tokens.
func serviceAccountUser(namespace, name string) string {
	return "system:serviceaccount:" + namespace + ":" + name
}

// Authorize decides whether claim c lets caller use the MicroVM vmUID on
// the Host whose Node is hostNode at now, with the three checks that
// follow the TokenReview (EA-010). Every condition is checked, and a claim
// that says nothing about one of them fails it: no Holder, no expiry, no
// Host. The error says which condition failed, for the agent's log; the
// caller is told only that it was refused.
func Authorize(c Claim, caller Caller, vmUID, hostNode string, now time.Time) error {
	//= docs/requirements/05-exec-agent.md#authorization
	//# The Exec Agent SHALL run a command in a MicroVM only when the
	//# request's token belongs to the Holder of a claim whose namespace and
	//# `spec.serviceAccountName` it names.
	if c.ServiceAccountName == "" {
		return fmt.Errorf("claim %s names no holder: %w", c, errNotAuthorized)
	}
	if holder := serviceAccountUser(c.Namespace, c.ServiceAccountName); caller.User != holder {
		return fmt.Errorf("claim %s is held by %q, not %q: %w", c, holder, caller.User, errNotAuthorized)
	}

	//= docs/requirements/05-exec-agent.md#authorization
	//# The Exec Agent SHALL run a command in a MicroVM only when the
	//# request's token is bound to that claim's Secret `<claim name>-exec`, by
	//# both the Secret's name and its uid.
	//
	// The name is checked here, and the token has to name a uid. That the
	// uid is the uid of the Secret of that name, as it is now, is checked
	// by the API server in the TokenReview (see boundSecret), which is why
	// the agent needs no permission to read Secrets.
	s := caller.BoundSecret
	switch {
	case s == nil:
		return fmt.Errorf("the token is bound to no Secret, so not to claim %s's: %w", c, errNotAuthorized)
	case s.Namespace != c.Namespace || s.Name != c.Name+ExecSecretSuffix:
		return fmt.Errorf("the token is bound to Secret %s/%s, not to claim %s's %s%s: %w",
			s.Namespace, s.Name, c, c.Name, ExecSecretSuffix, errNotAuthorized)
	case s.UID == "":
		return fmt.Errorf("the token names no uid for Secret %s/%s: %w", s.Namespace, s.Name, errNotAuthorized)
	}

	//= docs/requirements/05-exec-agent.md#authorization
	//# The Exec Agent SHALL run a command in a MicroVM only when that
	//# claim is Bound, its Lease has not expired, and it names that MicroVM's uid
	//# and this Host.
	switch {
	case c.Phase != batteryv1alpha1.MicroVMClaimBound:
		return fmt.Errorf("claim %s is %q, not Bound: %w", c, c.Phase, errNotAuthorized)
	case c.ExpiresAt.IsZero():
		return fmt.Errorf("claim %s records no lease expiry: %w", c, errNotAuthorized)
	case !now.Before(c.ExpiresAt):
		return fmt.Errorf("claim %s expired at %s: %w", c, c.ExpiresAt.UTC().Format(time.RFC3339), errNotAuthorized)
	case c.VMUID == "" || c.VMUID != vmUID:
		return fmt.Errorf("claim %s names microvm %q, not %q: %w", c, c.VMUID, vmUID, errNotAuthorized)
	case c.HostNode == "" || c.HostNode != hostNode:
		return fmt.Errorf("claim %s names host %q, not %q: %w", c, c.HostNode, hostNode, errNotAuthorized)
	}
	return nil
}

//= docs/requirements/05-exec-agent.md#authorization
//# If the TokenReview or the claim lookup of a request cannot be
//# completed, then the Exec Agent SHALL refuse the request.

// authorizeClaim finds the one claim that could let caller use vmUID and
// grants the request when it does. That claim is named by the token: a
// claim token is bound to `<claim name>-exec` in the claim's namespace
// (EA-012), so no other claim can pass Authorize, and the claim is read
// afresh from the API server for every request. A lookup that fails
// refuses, wrapped so that the caller can tell it from a plain refusal: the
// first is reported as unavailable, the second as permission denied, and
// neither runs anything.
func authorizeClaim(ctx context.Context, claims ClaimLookup, caller Caller, vmUID, hostNode string, now time.Time) error {
	if vmUID == "" {
		return fmt.Errorf("the request names no microvm: %w", errNotAuthorized)
	}
	s := caller.BoundSecret
	if s == nil {
		return fmt.Errorf("the token of %s is bound to no Secret, so to no claim's: %w", caller.User, errNotAuthorized)
	}
	name, ok := strings.CutSuffix(s.Name, ExecSecretSuffix)
	if !ok || name == "" {
		return fmt.Errorf("the token is bound to Secret %s/%s, which is no claim's: %w", s.Namespace, s.Name, errNotAuthorized)
	}
	c, err := claims.Claim(ctx, s.Namespace, name)
	if err != nil {
		return &lookupError{err: err}
	}
	if c == nil {
		return fmt.Errorf("the token is bound to Secret %s/%s, but claim %s/%s does not exist: %w",
			s.Namespace, s.Name, s.Namespace, name, errNotAuthorized)
	}
	return Authorize(*c, caller, vmUID, hostNode, now)
}

// lookupError is a claim lookup that could not be completed (EA-014).
type lookupError struct{ err error }

func (e *lookupError) Error() string { return "looking up the claim: " + e.err.Error() }
func (e *lookupError) Unwrap() error { return e.err }
