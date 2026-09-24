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

package claimclient

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc/metadata"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/fakeflintlock"
)

const (
	testNamespace = "ci"
	testPool      = "small"
	testHolder    = "runner"
	testClaim     = "job-1"
	secondToken   = "Bearer token-2"
	testHeartbeat = 5 * time.Second
	testTimeout   = 30 * time.Second
)

var testEpoch = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

// tokenCall is one TokenRequest the fake API server answered or refused.
type tokenCall struct {
	serviceAccount k8stypes.NamespacedName
	spec           authenticationv1.TokenRequestSpec
	token          string
	err            error
}

// env is a Client against controller-runtime's fake client, which stands
// in for the API server, with a fake clock. TokenRequests are answered by
// an interceptor that records them.
type env struct {
	t    *testing.T
	kube client.WithWatch
	clk  *clock.Fake
	c    *Client

	mu       sync.Mutex
	uids     int
	tokens   []tokenCall
	tokenErr error
}

func newEnv(t *testing.T, ca *x509.CertPool) *env {
	t.Helper()
	scheme := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := batteryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, clk: clock.NewFake(testEpoch)}
	pool := &batteryv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testPool},
		Spec: batteryv1alpha1.PoolSpec{Lease: batteryv1alpha1.PoolLease{
			HeartbeatInterval: &metav1.Duration{Duration: testHeartbeat},
		}},
	}
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pool).
		WithStatusSubresource(&batteryv1alpha1.MicroVMClaim{}).
		Build()
	e.kube = interceptor.NewClient(base, interceptor.Funcs{
		// The fake client gives objects no uid; the API server does.
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetUID() == "" {
				e.mu.Lock()
				e.uids++
				obj.SetUID(k8stypes.UID(fmt.Sprintf("uid-%d", e.uids)))
				e.mu.Unlock()
			}
			return c.Create(ctx, obj, opts...)
		},
		SubResourceCreate: func(ctx context.Context, c client.Client, sub string, obj, subObj client.Object,
			opts ...client.SubResourceCreateOption) error {
			tr, ok := subObj.(*authenticationv1.TokenRequest)
			if sub != "token" || !ok {
				return c.SubResource(sub).Create(ctx, obj, subObj, opts...)
			}
			e.mu.Lock()
			defer e.mu.Unlock()
			call := tokenCall{serviceAccount: client.ObjectKeyFromObject(obj), spec: *tr.Spec.DeepCopy(), err: e.tokenErr}
			if call.err == nil {
				call.token = fmt.Sprintf("token-%d", len(e.tokens)+1)
				tr.Status.Token = call.token
				tr.Status.ExpirationTimestamp = metav1.NewTime(
					e.clk.Now().Add(time.Duration(*tr.Spec.ExpirationSeconds) * time.Second))
			}
			e.tokens = append(e.tokens, call)
			return call.err
		},
	})
	if ca == nil {
		ca = x509.NewCertPool()
	}
	c, err := New(Config{Client: e.kube, Namespace: testNamespace, ServingCA: ca})
	if err != nil {
		t.Fatal(err)
	}
	c.clk = e.clk
	e.c = c
	return e
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	t.Cleanup(cancel)
	return ctx
}

func (e *env) request() Request {
	return Request{Pool: testPool, ServiceAccountName: testHolder, Name: testClaim}
}

type claimResult struct {
	cl  *Claim
	err error
}

// claimAsync runs Claim in the background.
func (e *env) claimAsync(ctx context.Context, req Request) <-chan claimResult {
	out := make(chan claimResult, 1)
	go func() {
		cl, err := e.c.Claim(ctx, req)
		out <- claimResult{cl, err}
	}()
	return out
}

// waitTimers waits until the code under test has armed n timers.
func (e *env) waitTimers(n int) {
	e.t.Helper()
	if err := e.clk.BlockUntil(testCtx(e.t), n); err != nil {
		e.t.Fatalf("waiting for %d timers: %v", n, err)
	}
}

func (e *env) claim(name string) *batteryv1alpha1.MicroVMClaim {
	e.t.Helper()
	obj := &batteryv1alpha1.MicroVMClaim{}
	if err := e.kube.Get(testCtx(e.t), client.ObjectKey{Namespace: testNamespace, Name: name}, obj); err != nil {
		e.t.Fatalf("getting claim %s: %v", name, err)
	}
	return obj
}

func (e *env) claimGone(name string) bool {
	e.t.Helper()
	obj := &batteryv1alpha1.MicroVMClaim{}
	err := e.kube.Get(testCtx(e.t), client.ObjectKey{Namespace: testNamespace, Name: name}, obj)
	return apierrors.IsNotFound(err)
}

// setStatus plays the Claim Controller: it writes a claim's status.
func (e *env) setStatus(name string, mutate func(*batteryv1alpha1.MicroVMClaimStatus)) {
	e.t.Helper()
	obj := e.claim(name)
	mutate(&obj.Status)
	if err := e.kube.Status().Update(testCtx(e.t), obj); err != nil {
		e.t.Fatalf("updating the status of claim %s: %v", name, err)
	}
}

func (e *env) setPending(name, reason string) {
	e.setStatus(name, func(s *batteryv1alpha1.MicroVMClaimStatus) {
		s.Phase = batteryv1alpha1.MicroVMClaimPending
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{
			Type: batteryv1alpha1.ConditionBound, Status: metav1.ConditionFalse, Reason: reason, Message: reason + " now",
		})
	})
}

func (e *env) setBound(name, agentAddress string) {
	e.setStatus(name, func(s *batteryv1alpha1.MicroVMClaimStatus) {
		s.Phase = batteryv1alpha1.MicroVMClaimBound
		s.MicroVM = &batteryv1alpha1.MicroVMReference{UID: "vm-uid"}
		s.Host = &batteryv1alpha1.HostReference{NodeName: "host-a", AgentAddress: agentAddress}
		s.LeaseExpiresAt = &metav1.Time{Time: e.clk.Now().Add(30 * time.Second)}
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{
			Type: batteryv1alpha1.ConditionBound, Status: metav1.ConditionTrue, Reason: batteryv1alpha1.ReasonBound,
		})
	})
}

// held claims and binds job-1, and returns the Claim once its hold loop
// has armed its renewal and token timers.
func (e *env) held(agentAddress string) *Claim {
	e.t.Helper()
	res := e.claimAsync(testCtx(e.t), e.request())
	e.waitTimers(1)
	e.setBound(testClaim, agentAddress)
	e.clk.Advance(pollInterval)
	r := e.result(res)
	if r.err != nil {
		e.t.Fatalf("Claim: %v", r.err)
	}
	e.waitTimers(2)
	return r.cl
}

func (e *env) result(res <-chan claimResult) claimResult {
	e.t.Helper()
	select {
	case r := <-res:
		return r
	case <-time.After(testTimeout):
		e.t.Fatal("Claim did not return")
		return claimResult{}
	}
}

func (e *env) tokenCalls() []tokenCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]tokenCall(nil), e.tokens...)
}

func (e *env) currentToken(cl *Claim) string {
	e.t.Helper()
	md, err := bearer{cl}.GetRequestMetadata(testCtx(e.t))
	if err != nil {
		e.t.Fatalf("GetRequestMetadata: %v", err)
	}
	return md["authorization"]
}

func TestNewRequiresConfig(t *testing.T) {
	kube := fake.NewClientBuilder().Build()
	ca := x509.NewCertPool()
	for name, cfg := range map[string]Config{
		"no client":         {Namespace: "ns", ServingCA: ca},
		"no namespace":      {Client: kube, ServingCA: ca},
		"no serving CA":     {Client: kube, Namespace: "ns"},
		"negative lifetime": {Client: kube, Namespace: "ns", ServingCA: ca, TokenExpiration: -time.Second},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New succeeded", name)
		}
	}
}

//= docs/requirements/07-client.md#claiming
//= type=test
//# The Client Library SHALL create a claim with the Pool and
//# Holder the Consumer names, and then an empty Secret `<claim name>-exec`
//# whose owner reference is the claim.

func TestClaimCreatesClaimAndSecret(t *testing.T) {
	e := newEnv(t, nil)
	req := e.request()
	req.Labels = map[string]string{"job": "1"}
	_ = e.claimAsync(testCtx(t), req)
	e.waitTimers(1)

	obj := e.claim(testClaim)
	if obj.Spec.PoolRef.Name != testPool || obj.Spec.ServiceAccountName != testHolder {
		t.Errorf("claim spec = %+v, want pool %s and holder %s", obj.Spec, testPool, testHolder)
	}
	if obj.Labels["job"] != "1" {
		t.Errorf("claim labels = %v", obj.Labels)
	}
	secret := &corev1.Secret{}
	if err := e.kube.Get(testCtx(t), client.ObjectKey{Namespace: testNamespace, Name: "job-1-exec"}, secret); err != nil {
		t.Fatalf("getting the claim's Secret: %v", err)
	}
	if len(secret.Data) != 0 || len(secret.StringData) != 0 {
		t.Errorf("Secret holds data: %v", secret.Data)
	}
	want := metav1.OwnerReference{
		APIVersion: batteryv1alpha1.GroupVersion.String(), Kind: "MicroVMClaim", Name: testClaim, UID: obj.UID,
	}
	if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0] != want {
		t.Errorf("Secret owner references = %+v, want [%+v]", secret.OwnerReferences, want)
	}
}

func TestClaimGeneratesNameFromPool(t *testing.T) {
	e := newEnv(t, nil)
	req := e.request()
	req.Name = ""
	_ = e.claimAsync(testCtx(t), req)
	e.waitTimers(1)
	list := &batteryv1alpha1.MicroVMClaimList{}
	if err := e.kube.List(testCtx(t), list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].GenerateName != testPool+"-" {
		t.Fatalf("claims = %+v, want one generated from %q", list.Items, testPool+"-")
	}
}

//= docs/requirements/07-client.md#claiming
//= type=test
//# The Client Library SHALL request each claim token with
//# `TokenRequest` for the Holder, bound to the claim's Secret by name and uid,
//# with the Exec Agent's audience.

func TestClaimTokenIsBoundToTheSecret(t *testing.T) {
	e := newEnv(t, nil)
	cl := e.held("")
	secret := &corev1.Secret{}
	if err := e.kube.Get(testCtx(t), client.ObjectKey{Namespace: testNamespace, Name: "job-1-exec"}, secret); err != nil {
		t.Fatal(err)
	}
	calls := e.tokenCalls()
	if len(calls) != 1 {
		t.Fatalf("token requests = %d, want 1", len(calls))
	}
	call := calls[0]
	if want := (k8stypes.NamespacedName{Namespace: testNamespace, Name: testHolder}); call.serviceAccount != want {
		t.Errorf("token requested for %s, want %s", call.serviceAccount, want)
	}
	if len(call.spec.Audiences) != 1 || call.spec.Audiences[0] != batteryv1alpha1.ExecAgentTokenAudience {
		t.Errorf("audiences = %v, want [%s]", call.spec.Audiences, batteryv1alpha1.ExecAgentTokenAudience)
	}
	ref := call.spec.BoundObjectRef
	want := authenticationv1.BoundObjectReference{Kind: "Secret", APIVersion: "v1", Name: secret.Name, UID: secret.UID}
	if ref == nil || *ref != want {
		t.Errorf("bound object = %+v, want %+v", ref, want)
	}
	if got := e.currentToken(cl); got != "Bearer token-1" {
		t.Errorf("authorization = %q, want the token", got)
	}
}

//= docs/requirements/07-client.md#claiming
//= type=test
//# The Client Library SHALL return a claim to the Consumer only once
//# the claim's phase is `Bound`, together with the MicroVM's uid, the
//# Host's node name and the Exec Agent's address from the claim's status.

func TestClaimReturnsOnlyOnceBound(t *testing.T) {
	e := newEnv(t, nil)
	res := e.claimAsync(testCtx(t), e.request())
	e.waitTimers(1)
	for range 3 {
		e.setPending(testClaim, batteryv1alpha1.ReasonNoEligibleHost)
		e.clk.Advance(pollInterval)
		e.waitTimers(1)
		select {
		case r := <-res:
			t.Fatalf("Claim returned while Pending: %+v", r)
		default:
		}
	}
	e.setBound(testClaim, "10.0.0.1:9443")
	e.clk.Advance(pollInterval)
	r := e.result(res)
	if r.err != nil {
		t.Fatalf("Claim: %v", r.err)
	}
	t.Cleanup(func() { _ = r.cl.Release(context.Background()) })
	cl := r.cl
	if cl.Name() != testClaim || cl.MicroVMUID() != "vm-uid" || cl.NodeName() != "host-a" ||
		cl.AgentAddress() != "10.0.0.1:9443" {
		t.Errorf("claim = %s %s %s %s", cl.Name(), cl.MicroVMUID(), cl.NodeName(), cl.AgentAddress())
	}
}

//= docs/requirements/07-client.md#claiming
//= type=test
//# While a claim waits with the reason `PoolExhausted`, the Client
//# Library SHALL report the Pool as exhausted to the Consumer.

func TestClaimReportsPoolExhausted(t *testing.T) {
	e := newEnv(t, nil)
	ctx, cancel := context.WithCancel(testCtx(t))
	pending := make(chan Pending, 10)
	req := e.request()
	req.OnPending = func(p Pending) { pending <- p }
	res := e.claimAsync(ctx, req)
	e.waitTimers(1)

	e.setPending(testClaim, batteryv1alpha1.ReasonPoolExhausted)
	e.clk.Advance(pollInterval)
	e.waitTimers(1)
	// The same reason is reported once.
	e.clk.Advance(pollInterval)
	e.waitTimers(1)
	select {
	case p := <-pending:
		if !p.PoolExhausted() || p.Message != "PoolExhausted now" {
			t.Errorf("pending = %+v, want the Pool exhausted", p)
		}
	case <-time.After(testTimeout):
		t.Fatal("OnPending was not called")
	}
	if len(pending) != 0 {
		t.Errorf("OnPending called again for the same reason: %+v", <-pending)
	}

	cancel()
	r := e.result(res)
	if !errors.Is(r.err, ErrPoolExhausted) || !errors.Is(r.err, context.Canceled) {
		t.Errorf("Claim error = %v, want ErrPoolExhausted and context.Canceled", r.err)
	}
}

//= docs/requirements/07-client.md#claiming
//= type=test
//# When the Consumer stops waiting for a claim to bind, the Client
//# Library SHALL delete the claim.

func TestClaimDeletesClaimWhenConsumerStopsWaiting(t *testing.T) {
	e := newEnv(t, nil)
	ctx, cancel := context.WithCancel(testCtx(t))
	res := e.claimAsync(ctx, e.request())
	e.waitTimers(1)
	cancel()
	r := e.result(res)
	if !errors.Is(r.err, context.Canceled) || errors.Is(r.err, ErrPoolExhausted) {
		t.Errorf("Claim error = %v, want context.Canceled alone", r.err)
	}
	if !e.claimGone(testClaim) {
		t.Error("the claim was not deleted")
	}
}

func TestClaimDeletesClaimWhenTheTokenIsRefused(t *testing.T) {
	e := newEnv(t, nil)
	e.tokenErr = apierrors.NewForbidden(corev1.Resource("serviceaccounts/token"), testHolder, errors.New("no"))
	res := e.claimAsync(testCtx(t), e.request())
	e.waitTimers(1)
	e.setBound(testClaim, "")
	e.clk.Advance(pollInterval)
	if r := e.result(res); !apierrors.IsForbidden(r.err) {
		t.Errorf("Claim error = %v, want the refusal", r.err)
	}
	if !e.claimGone(testClaim) {
		t.Error("the claim was not deleted")
	}
}

func TestClaimFailsWhenDeletedBeforeBound(t *testing.T) {
	e := newEnv(t, nil)
	res := e.claimAsync(testCtx(t), e.request())
	e.waitTimers(1)
	if err := e.kube.Delete(testCtx(t), e.claim(testClaim)); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(pollInterval)
	if r := e.result(res); !errors.Is(r.err, ErrLeaseLost) {
		t.Errorf("Claim error = %v, want ErrLeaseLost", r.err)
	}
}

//= docs/requirements/07-client.md#holding
//= type=test
//# While the Consumer holds a Bound claim, the Client Library SHALL
//# set the claim's `spec.renewTime` at the Pool's heartbeat interval.

func TestHeldClaimIsRenewedAtTheHeartbeatInterval(t *testing.T) {
	e := newEnv(t, nil)
	cl := e.held("")
	t.Cleanup(func() { _ = cl.Release(context.Background()) })
	if rt := e.claim(testClaim).Spec.RenewTime; rt != nil {
		t.Fatalf("renewTime = %v before the first heartbeat", rt)
	}
	for range 3 {
		e.clk.Advance(testHeartbeat - time.Millisecond)
		if got := e.clk.Timers(); got != 2 {
			t.Fatalf("a timer fired before the heartbeat interval: %d armed", got)
		}
		e.clk.Advance(time.Millisecond)
		e.waitTimers(2)
		rt := e.claim(testClaim).Spec.RenewTime
		if rt == nil || !rt.Time.Equal(e.clk.Now()) {
			t.Fatalf("renewTime = %v, want %v", rt, e.clk.Now())
		}
	}
	if cl.Err() != nil {
		t.Errorf("Err = %v", cl.Err())
	}
}

//= docs/requirements/07-client.md#holding
//= type=test
//# While the Consumer holds a Bound claim, the Client Library SHALL
//# request a new claim token before the current one expires.

func TestHeldClaimGetsANewTokenBeforeExpiry(t *testing.T) {
	e := newEnv(t, nil)
	cl := e.held("")
	t.Cleanup(func() { _ = cl.Release(context.Background()) })

	// Tokens live ten minutes; the next is requested at two thirds.
	e.clk.Advance(400*time.Second - time.Millisecond)
	e.waitTimers(2)
	if n := len(e.tokenCalls()); n != 1 {
		t.Fatalf("token requests = %d before two thirds of the lifetime", n)
	}
	e.clk.Advance(time.Millisecond)
	e.waitTimers(2)
	if n := len(e.tokenCalls()); n != 2 {
		t.Fatalf("token requests = %d, want 2", n)
	}
	if got := e.currentToken(cl); got != secondToken {
		t.Errorf("authorization = %q, want the new token", got)
	}

	// A refused request keeps the current token and is retried at the
	// next heartbeat.
	e.mu.Lock()
	e.tokenErr = errors.New("unavailable")
	e.mu.Unlock()
	e.clk.Advance(400 * time.Second)
	e.waitTimers(2)
	if got := e.currentToken(cl); got != secondToken {
		t.Errorf("authorization = %q after a refused request, want the current token", got)
	}
	e.mu.Lock()
	e.tokenErr = nil
	e.mu.Unlock()
	e.clk.Advance(testHeartbeat)
	e.waitTimers(2)
	if got := e.currentToken(cl); got != "Bearer token-4" {
		t.Errorf("authorization = %q after the retry, want token-4", got)
	}
}

//= docs/requirements/07-client.md#holding
//= type=test
//# If a held claim's phase becomes `Expired`, or the claim is
//# deleted by anyone but the Client Library, then the Client Library SHALL
//# report the Lease as lost to the Consumer.

func TestHeldClaimReportsALostLease(t *testing.T) {
	for name, lose := range map[string]func(e *env){
		"expired": func(e *env) {
			e.setStatus(testClaim, func(s *batteryv1alpha1.MicroVMClaimStatus) { s.Phase = batteryv1alpha1.MicroVMClaimExpired })
		},
		"deleted": func(e *env) {
			if err := e.kube.Delete(testCtx(e.t), e.claim(testClaim)); err != nil {
				e.t.Fatal(err)
			}
		},
		"being deleted": func(e *env) {
			// The Claim Controller's finalizer keeps a deleted claim until
			// battery has released it.
			obj := e.claim(testClaim)
			obj.Finalizers = []string{batteryv1alpha1.ReleaseFinalizer}
			if err := e.kube.Update(testCtx(e.t), obj); err != nil {
				e.t.Fatal(err)
			}
			if err := e.kube.Delete(testCtx(e.t), obj); err != nil {
				e.t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, nil)
			cl := e.held("")
			lose(e)
			e.clk.Advance(testHeartbeat)
			select {
			case <-cl.Lost():
			case <-time.After(testTimeout):
				t.Fatal("the Lease was not reported lost")
			}
			if !errors.Is(cl.Err(), ErrLeaseLost) {
				t.Errorf("Err = %v, want ErrLeaseLost", cl.Err())
			}
			if _, err := cl.Dial(testCtx(t)); !errors.Is(err, ErrLeaseLost) {
				t.Errorf("Dial after the loss = %v, want ErrLeaseLost", err)
			}
			if err := cl.Release(testCtx(t)); err != nil {
				t.Errorf("Release: %v", err)
			}
		})
	}
}

//= docs/requirements/07-client.md#using
//= type=test
//# When the Consumer releases a claim, the Client Library SHALL
//# delete the claim.

func TestReleaseDeletesTheClaim(t *testing.T) {
	e := newEnv(t, nil)
	cl := e.held("")
	if err := cl.Release(testCtx(t)); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !e.claimGone(testClaim) {
		t.Error("the claim was not deleted")
	}
	// The Client's own deletion is not a lost Lease, and renewals stop.
	select {
	case <-cl.Lost():
		t.Errorf("Lost closed by Release: %v", cl.Err())
	default:
	}
	if n := e.clk.Timers(); n != 0 {
		t.Errorf("%d timers still armed after Release", n)
	}
	if err := cl.Release(testCtx(t)); err != nil {
		t.Errorf("a second Release: %v", err)
	}
}

// fakeAgent is the fake flintlockd serving flintlock's exec API over TLS on
// loopback, which is what the Exec Agent serves (EA-003), and records the
// authorization header of every exec.
type fakeAgent struct {
	srv   *fakeflintlock.Server
	vmUID string
	ca    *x509.CertPool

	mu    sync.Mutex
	auths []string
}

func newFakeAgent(t *testing.T) *fakeAgent {
	t.Helper()
	certs, err := fakeflintlock.WriteTestCerts(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &fakeAgent{srv: fakeflintlock.New(fakeflintlock.Config{TLS: certs.ServerTLS(false)})}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- a.srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-served
		_ = a.srv.Close()
	})
	select {
	case <-a.srv.Ready():
	case err := <-served:
		t.Fatalf("Serve: %v", err)
	}
	vm, err := a.srv.CreateMicroVM(&types.MicroVMSpec{Id: "vm", Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	a.vmUID = vm.GetSpec().GetUid()
	a.srv.SetExec(func(ctx context.Context, _ *fakeflintlock.Exec) (int32, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		a.mu.Lock()
		a.auths = append(a.auths, md.Get("authorization")...)
		a.mu.Unlock()
		return 0, nil
	})
	a.ca = caPool(t, certs.CAFile)
	return a
}

func caPool(t *testing.T, file string) *x509.CertPool {
	t.Helper()
	pem, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("no CA certificate")
	}
	return pool
}

func (a *fakeAgent) authorizations() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.auths...)
}

// exec runs one command over conn and returns its exit code, or the error
// the stream ended with.
func exec(ctx context.Context, conn execv1.MicroVMExecClient, uid string) (int32, error) {
	stream, err := conn.ExecCommand(ctx)
	if err != nil {
		return 0, err
	}
	start := &execv1.ExecStart{Uid: uid, Cmd: "true"}
	first := &execv1.ExecCommandRequest{Payload: &execv1.ExecCommandRequest_Start{Start: start}}
	if err := stream.Send(first); err != nil {
		_, err = stream.Recv()
		return 0, err
	}
	_ = stream.CloseSend()
	code := int32(-1)
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return code, nil
		}
		if err != nil {
			return 0, err
		}
		if p, ok := resp.GetPayload().(*execv1.ExecCommandResponse_ExitCode); ok {
			code = p.ExitCode
		}
	}
}

//= docs/requirements/07-client.md#using
//= type=test
//# The Client Library SHALL connect to the Exec Agent at the
//# address in the claim's status, verifying its serving certificate against
//# the certificate authority the Consumer configures, and SHALL send the
//# current claim token as the bearer token of every request.

func TestDialSendsTheCurrentTokenOverVerifiedTLS(t *testing.T) {
	agent := newFakeAgent(t)
	e := newEnv(t, agent.ca)
	cl := e.held(agent.srv.Addr())
	t.Cleanup(func() { _ = cl.Release(context.Background()) })

	conn, err := cl.Dial(testCtx(t))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	execs := execv1.NewMicroVMExecClient(conn)
	if code, err := exec(testCtx(t), execs, agent.vmUID); err != nil || code != 0 {
		t.Fatalf("exec = %d, %v", code, err)
	}

	// After a new token, the same connection sends it.
	e.clk.Advance(400 * time.Second)
	e.waitTimers(2)
	if code, err := exec(testCtx(t), execs, agent.vmUID); err != nil || code != 0 {
		t.Fatalf("exec = %d, %v", code, err)
	}
	got := agent.authorizations()
	if len(got) != 2 || got[0] != "Bearer token-1" || got[1] != secondToken {
		t.Errorf("authorization headers = %q, want token-1 then token-2", got)
	}
}

func TestDialRefusesAnAgentTheCADoesNotVouchFor(t *testing.T) {
	agent := newFakeAgent(t)
	other, err := fakeflintlock.WriteTestCerts(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, caPool(t, other.CAFile))
	cl := e.held(agent.srv.Addr())
	t.Cleanup(func() { _ = cl.Release(context.Background()) })

	conn, err := cl.Dial(testCtx(t))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := exec(testCtx(t), execv1.NewMicroVMExecClient(conn), agent.vmUID); err == nil {
		t.Error("exec succeeded against an agent the CA does not vouch for")
	}
	if got := agent.authorizations(); len(got) != 0 {
		t.Errorf("the agent received tokens: %q", got)
	}
}

func TestDialWaitsForAnAgentAddress(t *testing.T) {
	agent := newFakeAgent(t)
	e := newEnv(t, agent.ca)
	cl := e.held("")
	t.Cleanup(func() { _ = cl.Release(context.Background()) })

	if _, err := cl.Dial(testCtx(t)); !errors.Is(err, ErrNoAgentAddress) {
		t.Fatalf("Dial with no address = %v, want ErrNoAgentAddress", err)
	}
	// The address published later is read afresh.
	e.setStatus(testClaim, func(s *batteryv1alpha1.MicroVMClaimStatus) { s.Host.AgentAddress = agent.srv.Addr() })
	conn, err := cl.Dial(testCtx(t))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if code, err := exec(testCtx(t), execv1.NewMicroVMExecClient(conn), agent.vmUID); err != nil || code != 0 {
		t.Fatalf("exec = %d, %v", code, err)
	}
}
