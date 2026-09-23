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

	"github.com/phoban01/battery-operator/internal/execagent"
	"github.com/phoban01/battery-operator/internal/execagent/execagenttest"
)

//= docs/requirements/08-test-doubles.md#test-environments
//= type=test
//# The Exec Agent SHALL be tested against envtest serving the
//# CRDs, and the fake `flintlockd`.

// TestExecOnlyForTheCreatorOfABoundClaim tries to run a command once per
// way of not holding the claim, and once with it. Every caller is a real
// ServiceAccount token the agent reviews with a real TokenReview, and every
// claim is a real object of the claim resource's test definition. Each
// refusal is checked to be the right status and never to have reached
// flintlockd; the one caller that created a Bound, unexpired claim on this
// MicroVM and this Host runs its command.
//
// The check is flintlock-runner's provisional one, moved as it is; #16
// replaces it with EA-010 to EA-013 and this test with theirs.
func TestExecOnlyForTheCreatorOfABoundClaim(t *testing.T) {
	t.Parallel()
	f := newFixture(t, execagenttest.HostOptions{})
	other := newFixture(t, execagenttest.HostOptions{})
	stranger := env.ServiceAccountToken(t, f.ns, "stranger")
	hour := time.Now().Add(time.Hour)
	bound := func() execagenttest.ClaimStatus {
		return execagenttest.ClaimStatus{Phase: execagent.ClaimBound, VMUID: f.host.VMUID, HostNode: f.host.Node, ExpiresAt: hour}
	}

	cases := []struct {
		name    string
		token   string
		claim   func(name string)
		want    codes.Code
		allowed bool
	}{
		{name: "no token", token: "", want: codes.Unauthenticated},
		{name: "a bad token", token: "not-a-token", want: codes.Unauthenticated},
		{name: "a valid token with no claim", token: f.holder.Token, want: codes.PermissionDenied},
		{name: "a pending claim", token: f.holder.Token, want: codes.PermissionDenied, claim: func(name string) {
			env.PutClaim(t, f.ns, name, f.holder.User, execagenttest.ClaimStatus{Phase: execagent.ClaimPending, ExpiresAt: hour})
		}},
		{name: "an expired claim", token: f.holder.Token, want: codes.PermissionDenied, claim: func(name string) {
			st := bound()
			st.Phase = execagent.ClaimExpired
			env.PutClaim(t, f.ns, name, f.holder.User, st)
		}},
		{name: "a released claim", token: f.holder.Token, want: codes.PermissionDenied, claim: func(name string) {
			st := bound()
			st.Phase = execagent.ClaimReleased
			env.PutClaim(t, f.ns, name, f.holder.User, st)
		}},
		{name: "a bound claim whose lease has run out", token: f.holder.Token, want: codes.PermissionDenied, claim: func(name string) {
			st := bound()
			st.ExpiresAt = time.Now().Add(-time.Minute)
			env.PutClaim(t, f.ns, name, f.holder.User, st)
		}},
		{name: "a claim for another microvm", token: f.holder.Token, want: codes.PermissionDenied, claim: func(name string) {
			st := bound()
			st.VMUID = other.host.VMUID
			env.PutClaim(t, f.ns, name, f.holder.User, st)
		}},
		{name: "a claim for another host", token: f.holder.Token, want: codes.PermissionDenied, claim: func(name string) {
			st := bound()
			st.HostNode = other.host.Node
			env.PutClaim(t, f.ns, name, f.holder.User, st)
		}},
		{name: "a claim created by someone else", token: stranger.Token, want: codes.PermissionDenied, claim: func(name string) {
			env.PutClaim(t, f.ns, name, f.holder.User, bound())
		}},
		{name: "a claim that records no creator", token: f.holder.Token, want: codes.PermissionDenied, claim: func(name string) {
			env.PutClaim(t, f.ns, name, "", bound())
		}},
		{name: "a bound claim of the caller", token: f.holder.Token, allowed: true, claim: func(name string) {
			env.PutClaim(t, f.ns, name, f.holder.User, bound())
		}},
	}
	for i, tc := range cases {
		name := "claim-" + string(rune('a'+i))
		if tc.claim != nil {
			tc.claim(name)
		}
		cmd := "run-" + name
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		// The agent reads claims afresh for every request, but it learns
		// which claims to read from a cache; a refusal must not be the
		// cache being behind, so each case gives it a moment first.
		time.Sleep(200 * time.Millisecond)
		r := exchange(ctx, f.rawExec(tc.token), f.start(cmd), "", nil)
		cancel()
		if tc.allowed {
			if r.err != nil || !r.gotExit || r.exitCode != 0 {
				t.Errorf("%s: exec = (exit %d, sent %v, %v), want the command run", tc.name, r.exitCode, r.gotExit, r.err)
			}
			if !f.ran(cmd) {
				t.Errorf("%s: the command did not reach flintlockd", tc.name)
			}
		} else {
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
		env.DeleteClaim(t, f.ns, name)
	}

	// The claim is checked on every request, not once per connection: a
	// claim released while the caller stays connected stops working.
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
