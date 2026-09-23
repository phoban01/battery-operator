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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/metadata"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authenticationv1client "k8s.io/client-go/kubernetes/typed/authentication/v1"
)

// DefaultTokenAudience is the Exec Agent's audience: the audience a claim
// token is requested with (CC-002) and the one the agent's TokenReviews ask
// for (EA-010). A token for the API server's own audience, which every pod
// of the namespace may hold, is not accepted.
const DefaultTokenAudience = "battery.liquidmetal-x.dev/exec-agent"

// credentialIDExtra is the key under which a TokenReview reports the id of
// a ServiceAccount token, as "JTI=<the token's jti>".
const credentialIDExtra = "authentication.kubernetes.io/credential-id"

// errUnauthenticated is wrapped by every request that does not authenticate.
var errUnauthenticated = errors.New("the request does not authenticate")

// reviewError is a TokenReview that could not be completed (EA-014).
type reviewError struct{ err error }

func (e *reviewError) Error() string { return "reviewing the bearer token: " + e.err.Error() }
func (e *reviewError) Unwrap() error { return e.err }

// Caller is who a request authenticated as, and what its token is bound to.
type Caller struct {
	// User is the user name the TokenReview authenticated the token as.
	User string
	// BoundSecret is the Secret the token is bound to, when it is a
	// ServiceAccount token bound to one; nil otherwise.
	BoundSecret *BoundSecret
}

// BoundSecret is the Secret a ServiceAccount token is bound to, as the
// token's own claims name it.
type BoundSecret struct {
	// Namespace is the Secret's namespace, which is the ServiceAccount's:
	// TokenRequest binds a token only to an object in the namespace of the
	// ServiceAccount the token is for.
	Namespace string
	Name      string
	UID       string
}

// authenticator authenticates requests by their bearer token.
type authenticator struct {
	reviews   authenticationv1client.TokenReviewInterface
	audiences []string
	timeout   time.Duration
}

//= docs/requirements/05-exec-agent.md#authorization
//# The Exec Agent SHALL authenticate every request with a
//# TokenReview of the bearer token it carries, requesting the Exec Agent's
//# audience, and SHALL refuse a request that does not authenticate.

//= docs/requirements/05-exec-agent.md#authorization
//# If the TokenReview or the claim lookup of a request cannot be
//# completed, then the Exec Agent SHALL refuse the request.

// authenticate returns who a request's bearer token belongs to, asking the
// API server with a TokenReview for the agent's audiences every time: the
// agent keeps no cache, so a token that stops being valid stops working as
// soon as the API server says so. A request with no bearer token, a token
// the review does not authenticate, or one the review does not report for
// every audience is refused as unauthenticated; a review that cannot be
// made is refused as well, as a reviewError, because a request the agent
// cannot vouch for is not run.
//
// What the token is bound to is read afterwards, by boundSecret.
func (a *authenticator) authenticate(ctx context.Context) (Caller, error) {
	token, err := bearerToken(ctx)
	if err != nil {
		return Caller{}, err
	}
	if len(a.audiences) == 0 {
		// Without an audience the review would accept a token for the API
		// server itself; the configuration never allows it, and this does
		// not either.
		return Caller{}, &reviewError{err: errors.New("the exec agent has no token audience configured")}
	}
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	review, err := a.reviews.Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token, Audiences: a.audiences},
	}, metav1.CreateOptions{})
	if err != nil {
		return Caller{}, &reviewError{err: err}
	}
	st := review.Status
	switch {
	case st.Error != "" && !st.Authenticated:
		return Caller{}, fmt.Errorf("%w: %s", errUnauthenticated, st.Error)
	case !st.Authenticated:
		return Caller{}, fmt.Errorf("%w: the token is not valid", errUnauthenticated)
	case st.User.Username == "":
		return Caller{}, fmt.Errorf("%w: the token names no user", errUnauthenticated)
	}
	for _, want := range a.audiences {
		if !slices.Contains(st.Audiences, want) {
			return Caller{}, fmt.Errorf("%w: the token is not for audience %q", errUnauthenticated, want)
		}
	}
	return Caller{User: st.User.Username, BoundSecret: boundSecret(token, st.User)}, nil
}

// serviceAccountClaims are the claims of a ServiceAccount token that
// boundSecret reads.
type serviceAccountClaims struct {
	Subject    string `json:"sub"`
	ID         string `json:"jti"`
	Kubernetes struct {
		Namespace string `json:"namespace"`
		Secret    *struct {
			Name string `json:"name"`
			UID  string `json:"uid"`
		} `json:"secret"`
	} `json:"kubernetes.io"`
}

// boundSecret reads the Secret a ServiceAccount token is bound to from the
// token's own claims. It is called only for a token that a TokenReview has
// just authenticated as user.
//
// A TokenReview does not report the object a token is bound to. For a
// token bound to a Pod it reports the Pod in the user's extra, but for one
// bound to a Secret it reports only the token's id
// (`authentication.kubernetes.io/credential-id`, "JTI=<jti>"); the Secret
// is named only in the token's claims, `kubernetes.io.secret.name` and
// `.uid`. The claims are read without checking the signature, which is
// sound because the API server has just checked it: the TokenReview
// authenticated this very string, and a JWT's signature covers its claims.
// Still, the claims have to be those of the token the review vouched for:
// `sub` has to be the user the review authenticated, and when the review
// reports the token's id, the claims' `jti` has to be it. A token that is
// not such a JWT, or is bound to no Secret, is bound to nothing (nil), and
// authorizes no MicroVM.
//
// The API server checks the uid itself. The ServiceAccount authenticator
// refuses a token bound to a Secret that no longer exists, or whose uid is
// no longer the one in the token, so a Secret deleted and made again under
// the same name does not revive the tokens bound to the old one. It caches
// an authenticated token for up to ten seconds, and a claim released in
// that time is refused by EA-013 regardless.
func boundSecret(token string, user authenticationv1.UserInfo) *BoundSecret {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims serviceAccountClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil
	}
	if claims.Subject == "" || claims.Subject != user.Username {
		return nil
	}
	if ids := user.Extra[credentialIDExtra]; len(ids) > 0 && !slices.Contains(ids, "JTI="+claims.ID) {
		return nil
	}
	s := claims.Kubernetes.Secret
	if s == nil || s.Name == "" || s.UID == "" || claims.Kubernetes.Namespace == "" {
		return nil
	}
	return &BoundSecret{Namespace: claims.Kubernetes.Namespace, Name: s.Name, UID: s.UID}
}

// bearerToken reads the token of an `authorization: Bearer <token>` header.
func bearerToken(ctx context.Context) (string, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get("authorization")
	if len(values) == 0 {
		return "", fmt.Errorf("%w: no bearer token", errUnauthenticated)
	}
	if len(values) > 1 {
		return "", fmt.Errorf("%w: more than one authorization header", errUnauthenticated)
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	token = strings.TrimSpace(token)
	if !ok || !strings.EqualFold(scheme, "bearer") || token == "" {
		return "", fmt.Errorf("%w: the authorization header is not a bearer token", errUnauthenticated)
	}
	return token, nil
}
