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

package execagent_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/execagent"
	"github.com/phoban01/battery-operator/internal/execagent/execagenttest"
)

//= docs/requirements/08-test-doubles.md#test-environments
//= type=test
//# The Exec Agent SHALL be tested against envtest serving the
//# CRDs, and the fake `flintlockd`.

//= docs/requirements/05-exec-agent.md#authorization
//= type=test
//# The Exec Agent SHALL authenticate every request with a
//# TokenReview of the bearer token it carries, requesting the Exec Agent's
//# audience, and SHALL refuse a request that does not authenticate.

//= docs/requirements/05-exec-agent.md#authorization
//= type=test
//# The Exec Agent SHALL run a command in a MicroVM only when the
//# request's token belongs to the Holder of a claim whose namespace and
//# `spec.serviceAccountName` it names.

//= docs/requirements/05-exec-agent.md#authorization
//= type=test
//# The Exec Agent SHALL run a command in a MicroVM only when the
//# request's token is bound to that claim's Secret `<claim name>-exec`, by
//# both the Secret's name and its uid.

//= docs/requirements/05-exec-agent.md#authorization
//= type=test
//# The Exec Agent SHALL run a command in a MicroVM only when that
//# claim is Bound, its Lease has not expired, and it names that MicroVM's uid
//# and this Host.

// TestExecOnlyWithAClaimTokenOfABoundClaim tries to run a command once for
// each way one of the four checks of EA-010 to EA-013 can fail with the
// other three passing, and once with all four passing. Every token is a
// real ServiceAccount token from TokenRequest, which the agent reviews with
// a real TokenReview, every claim is a real MicroVMClaim, and every claim
// token is bound to a real Secret. Each refusal is checked to be the right
// status and never to have reached flintlockd; the Holder's claim token of
// a Bound, unexpired claim on this MicroVM and this Host runs its command.
func TestExecOnlyWithAClaimTokenOfABoundClaim(t *testing.T) {
	t.Parallel()
	f := newFixture(t, execagenttest.HostOptions{})
	other := newFixture(t, execagenttest.HostOptions{})
	stranger := env.ServiceAccountToken(t, f.ns, "stranger")
	hour := time.Now().Add(time.Hour)

	// claim writes a claim of the holder that passes EA-013 unless change
	// says otherwise, with its Secret.
	claim := func(name string, change func(*execagenttest.ClaimStatus)) {
		st := execagenttest.ClaimStatus{Phase: batteryv1alpha1.MicroVMClaimBound, VMUID: f.host.VMUID, HostNode: f.host.Node, ExpiresAt: hour}
		if change != nil {
			change(&st)
		}
		env.PutClaim(t, f.ns, name, f.holder.Name, st)
	}
	// claimToken is the holder's claim token for claim name.
	claimToken := func(name string) string {
		return env.ClaimToken(t, f.ns, f.holder.Name, name+execagent.ExecSecretSuffix)
	}
	// secret makes a Secret of the consumer's namespace.
	secret := func(name string) *corev1.Secret {
		s, err := env.Admin.CoreV1().Secrets(f.ns).Create(context.Background(),
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	bindTo := func(s *corev1.Secret) *authenticationv1.BoundObjectReference {
		return &authenticationv1.BoundObjectReference{Kind: "Secret", APIVersion: "v1", Name: s.Name, UID: s.UID}
	}
	agentAudience := []string{execagent.DefaultTokenAudience}

	cases := []struct {
		name string
		// token sets up the case for claim name and returns the token.
		token   func(name string) string
		want    codes.Code
		allowed bool
	}{
		// EA-010: the TokenReview, for the agent's audience.
		{name: "EA-010: no token", want: codes.Unauthenticated, token: func(name string) string {
			claim(name, nil)
			return ""
		}},
		{name: "EA-010: not a token", want: codes.Unauthenticated, token: func(name string) string {
			claim(name, nil)
			return "not-a-token"
		}},
		{name: "EA-010: a claim token for the API server's audience", want: codes.Unauthenticated, token: func(name string) string {
			claim(name, nil)
			s, err := env.Admin.CoreV1().Secrets(f.ns).Get(context.Background(), name+execagent.ExecSecretSuffix, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			return env.Token(t, f.ns, f.holder.Name, nil, bindTo(s))
		}},

		// EA-011: the token is the Holder's.
		{name: "EA-011: another account's token bound to the claim's secret", want: codes.PermissionDenied, token: func(name string) string {
			claim(name, nil)
			return env.ClaimToken(t, f.ns, stranger.Name, name+execagent.ExecSecretSuffix)
		}},

		// EA-012: the token is bound to the claim's Secret, by name and uid.
		{name: "EA-012: the holder's token bound to nothing", want: codes.PermissionDenied, token: func(name string) string {
			claim(name, nil)
			return env.Token(t, f.ns, f.holder.Name, agentAudience, nil)
		}},
		{name: "EA-012: the holder's token bound to a secret of another name", want: codes.PermissionDenied, token: func(name string) string {
			claim(name, nil)
			return env.Token(t, f.ns, f.holder.Name, agentAudience, bindTo(secret(name+"-other")))
		}},
		{name: "EA-012: a token bound to an earlier secret of the same name", want: codes.Unauthenticated, token: func(name string) string {
			claim(name, nil)
			tok := claimToken(name)
			// The Secret is made again: same name, another uid. The
			// token names the old uid, and the API server's review, which
			// has not seen this token yet, refuses it.
			if err := env.Admin.CoreV1().Secrets(f.ns).Delete(context.Background(), name+execagent.ExecSecretSuffix, metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
			secret(name + execagent.ExecSecretSuffix)
			return tok
		}},

		// EA-013: the claim is Bound, unexpired, and names this MicroVM and
		// this Host.
		{name: "EA-013: a pending claim", want: codes.PermissionDenied, token: func(name string) string {
			claim(name, func(st *execagenttest.ClaimStatus) {
				*st = execagenttest.ClaimStatus{Phase: batteryv1alpha1.MicroVMClaimPending}
			})
			return claimToken(name)
		}},
		{name: "EA-013: an expired claim", want: codes.PermissionDenied, token: func(name string) string {
			claim(name, func(st *execagenttest.ClaimStatus) { st.Phase = batteryv1alpha1.MicroVMClaimExpired })
			return claimToken(name)
		}},
		{name: "EA-013: a bound claim whose lease has run out", want: codes.PermissionDenied, token: func(name string) string {
			claim(name, func(st *execagenttest.ClaimStatus) { st.ExpiresAt = time.Now().Add(-time.Minute) })
			return claimToken(name)
		}},
		{name: "EA-013: a claim for another microvm", want: codes.PermissionDenied, token: func(name string) string {
			claim(name, func(st *execagenttest.ClaimStatus) { st.VMUID = other.host.VMUID })
			return claimToken(name)
		}},
		{name: "EA-013: a claim for another host", want: codes.PermissionDenied, token: func(name string) string {
			claim(name, func(st *execagenttest.ClaimStatus) { st.HostNode = other.host.Node })
			return claimToken(name)
		}},

		// All four pass.
		{name: "the holder's claim token of a bound claim", allowed: true, token: func(name string) string {
			claim(name, nil)
			return claimToken(name)
		}},
	}
	for i, tc := range cases {
		name := "claim-" + string(rune('a'+i))
		token := tc.token(name)
		cmd := "run-" + name
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		r := exchange(ctx, f.rawExec(token), f.start(cmd), "", nil)
		cancel()
		if tc.allowed {
			if r.err != nil || !r.gotExit || r.exitCode != 0 {
				t.Errorf("%s: exec = (exit %d, sent %v, %v), want the command run", tc.name, r.exitCode, r.gotExit, r.err)
			}
			if !f.ran(cmd) {
				t.Errorf("%s: the command did not reach flintlockd", tc.name)
			}
			continue
		}
		if got := status.Code(r.err); got != tc.want {
			t.Errorf("%s: exec ended with %v (%v), want %v", tc.name, got, r.err, tc.want)
		}
		if r.gotExit {
			t.Errorf("%s: a refused exec carried an exit code", tc.name)
		}
		if f.ran(cmd) {
			t.Errorf("%s: the refused command reached flintlockd", tc.name)
		}
	}

	// The claim is checked on every request, not once per connection: a
	// claim released while the caller stays connected stops working at
	// once, before its Secret is gone.
	f.bind("released-later")
	client := f.rawExec(f.holder.Token)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if r := exchange(ctx, client, f.start("before-release"), "", nil); r.err != nil || !r.gotExit {
		t.Fatalf("exec with a bound claim = (%v, %v)", r.gotExit, r.err)
	}
	env.DeleteClaim(t, f.ns, "released-later")
	if r := exchange(ctx, client, f.start("after-release"), "", nil); status.Code(r.err) != codes.PermissionDenied {
		t.Errorf("exec after the claim was deleted = %v, want permission denied", r.err)
	}
	if f.ran("after-release") {
		t.Error("a command reached flintlockd after its claim was deleted")
	}
}
