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
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
)

// The claim token the unit tests' callers carry.
const (
	testJTI       = "jti-1"
	testSecretUID = "secret-uid-1"
	testOther     = "other"
)

// testClaimToken is shaped as the API server shapes a claim token: the
// Holder's, bound to the Secret of claim testClaimName. Its signature is
// not one, which only a real API server would notice.
var testClaimToken = fakeJWT(tokenClaims(testCaller, testJTI, testClaimName+ExecSecretSuffix, testSecretUID))

// tokenClaims are the claims of a ServiceAccount token bound to a Secret,
// in testNamespace, as the API server writes them; an empty secretName
// binds it to nothing.
func tokenClaims(sub, jti, secretName, secretUID string) map[string]any {
	k := map[string]any{
		"namespace":      testNamespace,
		"serviceaccount": map[string]any{"name": testHolder, "uid": "sa-uid"},
	}
	if secretName != "" {
		k["secret"] = map[string]any{"name": secretName, "uid": secretUID}
	}
	return map[string]any{"aud": []string{DefaultTokenAudience}, "sub": sub, "jti": jti, "kubernetes.io": k}
}

// fakeJWT encodes claims as a JWT with a meaningless signature.
func fakeJWT(claims map[string]any) string {
	payload, err := json.Marshal(claims)
	if err != nil {
		panic(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256","kid":"test"}`)) + "." + enc(payload) + "." + enc([]byte("signature"))
}

// holderUser is the UserInfo a TokenReview reports for testClaimToken.
func holderUser() authenticationv1.UserInfo {
	return authenticationv1.UserInfo{
		Username: testCaller,
		Extra:    map[string]authenticationv1.ExtraValue{credentialIDExtra: {"JTI=" + testJTI}},
	}
}

//= docs/requirements/05-exec-agent.md#authorization
//= type=test
//# The Exec Agent SHALL authenticate every request with a
//# TokenReview of the bearer token it carries, requesting the Exec Agent's
//# audience, and SHALL refuse a request that does not authenticate.

// TestAuthenticateReviewsForTheAgentsAudience checks that every request is
// reviewed, for the agent's audience, and that a request is refused
// unless the review authenticates it for that audience. The envtest suite
// checks the same against a real API server.
func TestAuthenticateReviewsForTheAgentsAudience(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		status authenticationv1.TokenReviewStatus
		ok     bool
	}{
		"authenticated for the audience": {ok: true, status: authenticationv1.TokenReviewStatus{
			Authenticated: true, User: holderUser(), Audiences: []string{DefaultTokenAudience},
		}},
		"not authenticated": {status: authenticationv1.TokenReviewStatus{Error: "invalid bearer token"}},
		"authenticated for another audience": {status: authenticationv1.TokenReviewStatus{
			Authenticated: true, User: holderUser(), Audiences: []string{"https://kubernetes.default.svc"},
		}},
		"authenticated as nobody": {status: authenticationv1.TokenReviewStatus{
			Authenticated: true, Audiences: []string{DefaultTokenAudience},
		}},
	} {
		var asked []string
		reviews := 0
		kube := kubefake.NewClientset()
		kube.PrependReactor("create", "tokenreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
			reviews++
			r, _ := a.(k8stesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
			asked = r.Spec.Audiences
			return true, &authenticationv1.TokenReview{Status: tc.status}, nil
		})
		a := &authenticator{reviews: kube.AuthenticationV1().TokenReviews(), audiences: []string{DefaultTokenAudience}, timeout: time.Second}
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+testClaimToken))
		caller, err := a.authenticate(ctx)
		if reviews != 1 || len(asked) != 1 || asked[0] != DefaultTokenAudience {
			t.Errorf("%s: %d reviews for audiences %v, want one for %q", name, reviews, asked, DefaultTokenAudience)
		}
		switch {
		case tc.ok && (err != nil || caller.User != testCaller):
			t.Errorf("%s: authenticate = (%+v, %v), want %s", name, caller, err, testCaller)
		case !tc.ok && !errors.Is(err, errUnauthenticated):
			t.Errorf("%s: authenticate = (%+v, %v), want it refused", name, caller, err)
		}
	}

	// Without an audience the agent does not review at all: a review
	// without one would accept the API server's own tokens.
	a := &authenticator{reviews: kubefake.NewClientset().AuthenticationV1().TokenReviews(), timeout: time.Second}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+testClaimToken))
	if _, err := a.authenticate(ctx); err == nil {
		t.Error("an agent with no audience authenticated a request")
	}
}

// TestBoundSecretIsReadFromTheReviewedToken checks that the Secret a token
// is bound to is read from the token's claims only when they are the
// claims of the token the review authenticated.
func TestBoundSecretIsReadFromTheReviewedToken(t *testing.T) {
	t.Parallel()
	want := BoundSecret{Namespace: testNamespace, Name: testClaimName + ExecSecretSuffix, UID: testSecretUID}
	if got := boundSecret(testClaimToken, holderUser()); got == nil || *got != want {
		t.Fatalf("boundSecret = %+v, want %+v", got, want)
	}
	noID := holderUser()
	noID.Extra = nil
	if got := boundSecret(testClaimToken, noID); got == nil || *got != want {
		t.Errorf("boundSecret with a review that reports no token id = %+v, want %+v", got, want)
	}
	parts := strings.Split(testClaimToken, ".")
	for name, tc := range map[string]struct {
		token string
		user  authenticationv1.UserInfo
	}{
		"another subject":      {fakeJWT(tokenClaims("system:serviceaccount:unit:other", testJTI, testClaimName+ExecSecretSuffix, testSecretUID)), holderUser()},
		"another token id":     {fakeJWT(tokenClaims(testCaller, "jti-2", testClaimName+ExecSecretSuffix, testSecretUID)), holderUser()},
		"bound to no secret":   {fakeJWT(tokenClaims(testCaller, testJTI, "", "")), holderUser()},
		"a secret with no uid": {fakeJWT(tokenClaims(testCaller, testJTI, testClaimName+ExecSecretSuffix, "")), holderUser()},
		"not a JWT":            {"opaque-token", holderUser()},
		"claims not base64":    {parts[0] + ".!!!." + parts[2], holderUser()},
		"claims not JSON":      {parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte("[")) + "." + parts[2], holderUser()},
	} {
		if got := boundSecret(tc.token, tc.user); got != nil {
			t.Errorf("%s: boundSecret = %+v, want none", name, got)
		}
	}
}

// TestAuthorize checks each of EA-011, EA-012 and EA-013 failing on its
// own, and that a claim which records nothing about a condition fails it.
func TestAuthorize(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	good := Claim{
		Namespace: testNamespace, Name: testClaimName, ServiceAccountName: testHolder,
		Phase: batteryv1alpha1.MicroVMClaimBound, VMUID: "vm", HostNode: testHost, ExpiresAt: now.Add(time.Minute),
	}
	holder := Caller{User: testCaller, BoundSecret: &BoundSecret{Namespace: testNamespace, Name: testClaimName + ExecSecretSuffix, UID: testSecretUID}}
	if err := Authorize(good, holder, "vm", testHost, now); err != nil {
		t.Fatalf("a good claim: %v", err)
	}
	refused := func(t *testing.T, cases map[string]func(*Claim, *Caller)) {
		t.Helper()
		for name, mutate := range cases {
			c, caller := good, holder
			s := *holder.BoundSecret
			caller.BoundSecret = &s
			mutate(&c, &caller)
			if err := Authorize(c, caller, "vm", testHost, now); !errors.Is(err, errNotAuthorized) {
				t.Errorf("%s: Authorize = %v, want a refusal", name, err)
			}
		}
	}

	t.Run("EA-011", func(t *testing.T) {
		t.Parallel()
		//= docs/requirements/05-exec-agent.md#authorization
		//= type=test
		//# The Exec Agent SHALL run a command in a MicroVM only when the
		//# request's token belongs to the Holder of a claim whose namespace and
		//# `spec.serviceAccountName` it names.
		refused(t, map[string]func(*Claim, *Caller){
			"another service account":         func(_ *Claim, k *Caller) { k.User = "system:serviceaccount:unit:other" },
			"the holder's name in another ns": func(_ *Claim, k *Caller) { k.User = "system:serviceaccount:other:" + testHolder },
			"not a service account":           func(_ *Claim, k *Caller) { k.User = testHolder },
			"a claim that names no holder":    func(c *Claim, _ *Caller) { c.ServiceAccountName = "" },
			"another holder named":            func(c *Claim, _ *Caller) { c.ServiceAccountName = testOther },
		})
	})
	t.Run("EA-012", func(t *testing.T) {
		t.Parallel()
		//= docs/requirements/05-exec-agent.md#authorization
		//= type=test
		//# The Exec Agent SHALL run a command in a MicroVM only when the
		//# request's token is bound to that claim's Secret `<claim name>-exec`, by
		//# both the Secret's name and its uid.
		refused(t, map[string]func(*Claim, *Caller){
			"a token bound to nothing":        func(_ *Claim, k *Caller) { k.BoundSecret = nil },
			"bound to another claim's secret": func(_ *Claim, k *Caller) { k.BoundSecret.Name = testOther + ExecSecretSuffix },
			"bound to the claim's own name":   func(_ *Claim, k *Caller) { k.BoundSecret.Name = testClaimName },
			"bound to a secret in another ns": func(_ *Claim, k *Caller) { k.BoundSecret.Namespace = testOther },
			"bound to a secret by name alone": func(_ *Claim, k *Caller) { k.BoundSecret.UID = "" },
		})
	})
	t.Run("EA-013", func(t *testing.T) {
		t.Parallel()
		//= docs/requirements/05-exec-agent.md#authorization
		//= type=test
		//# The Exec Agent SHALL run a command in a MicroVM only when that
		//# claim is Bound, its Lease has not expired, and it names that MicroVM's uid
		//# and this Host.
		refused(t, map[string]func(*Claim, *Caller){
			"pending":            func(c *Claim, _ *Caller) { c.Phase = batteryv1alpha1.MicroVMClaimPending },
			"expired phase":      func(c *Claim, _ *Caller) { c.Phase = batteryv1alpha1.MicroVMClaimExpired },
			"no phase":           func(c *Claim, _ *Caller) { c.Phase = "" },
			"lease ran out":      func(c *Claim, _ *Caller) { c.ExpiresAt = now },
			"no expiry recorded": func(c *Claim, _ *Caller) { c.ExpiresAt = time.Time{} },
			"another microvm":    func(c *Claim, _ *Caller) { c.VMUID = testOther },
			"no microvm":         func(c *Claim, _ *Caller) { c.VMUID = "" },
			"another host":       func(c *Claim, _ *Caller) { c.HostNode = testOther },
			"no host":            func(c *Claim, _ *Caller) { c.HostNode = "" },
		})
	})
}

// TestAuthorizeClaimReadsTheClaimTheTokenNames checks that the claim a
// request is authorized against is the one its token's Secret names, read
// through the lookup, and that a token whose Secret names no claim, or a
// claim that does not exist, is refused without an error of the lookup.
func TestAuthorizeClaimReadsTheClaimTheTokenNames(t *testing.T) {
	t.Parallel()
	now := time.Now()
	lookup := &stubClaims{claims: []Claim{boundClaim("vm")}}
	holder := Caller{User: testCaller, BoundSecret: &BoundSecret{Namespace: testNamespace, Name: testClaimName + ExecSecretSuffix, UID: testSecretUID}}
	if err := authorizeClaim(context.Background(), lookup, holder, "vm", testHost, now); err != nil {
		t.Fatalf("authorizeClaim = %v", err)
	}
	for name, s := range map[string]*BoundSecret{
		"no secret":              nil,
		"a secret of no claim":   {Namespace: testNamespace, Name: "unrelated", UID: "u"},
		"a secret named -exec":   {Namespace: testNamespace, Name: ExecSecretSuffix, UID: "u"},
		"a claim that is absent": {Namespace: testNamespace, Name: "gone" + ExecSecretSuffix, UID: "u"},
	} {
		caller := holder
		caller.BoundSecret = s
		err := authorizeClaim(context.Background(), lookup, caller, "vm", testHost, now)
		var lookupErr *lookupError
		if !errors.Is(err, errNotAuthorized) || errors.As(err, &lookupErr) {
			t.Errorf("%s: authorizeClaim = %v, want a refusal", name, err)
		}
	}
}
