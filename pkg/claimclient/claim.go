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
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
)

// minTokenRefresh is the shortest wait before a new claim token is
// requested, so that a token the API server issued with a very short life
// does not make the Client spin.
const minTokenRefresh = time.Second

// Claim is a Bound MicroVMClaim the Consumer holds. It renews itself until
// Release, or until its Lease is lost. Its methods are safe for concurrent
// use.
type Claim struct {
	c        *Client
	name     string
	uid      types.UID
	holder   string
	secret   types.NamespacedName
	secretID types.UID
	interval time.Duration

	// ctx ends when the claim is released; done closes when the hold loop
	// has returned.
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	// mu guards everything below it.
	mu     sync.Mutex
	status batteryv1alpha1.MicroVMClaimStatus
	token  string
	err    error
	lost   chan struct{}
}

func newClaim(c *Client, obj *batteryv1alpha1.MicroVMClaim, secret *corev1.Secret, interval time.Duration) *Claim {
	ctx, cancel := context.WithCancel(context.Background())
	return &Claim{
		c:        c,
		name:     obj.Name,
		uid:      obj.UID,
		holder:   obj.Spec.ServiceAccountName,
		secret:   types.NamespacedName{Namespace: secret.Namespace, Name: secret.Name},
		secretID: secret.UID,
		interval: interval,
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
		status:   *obj.Status.DeepCopy(),
		lost:     make(chan struct{}),
	}
}

// Name is the claim's name, in the Client's namespace.
func (cl *Claim) Name() string { return cl.name }

// MicroVMUID is the uid of the claimed MicroVM, which exec requests name.
func (cl *Claim) MicroVMUID() string {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if cl.status.MicroVM == nil {
		return ""
	}
	return cl.status.MicroVM.UID
}

// NodeName is the name of the Node of the Host the MicroVM runs on.
func (cl *Claim) NodeName() string {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if cl.status.Host == nil {
		return ""
	}
	return cl.status.Host.NodeName
}

// AgentAddress is the address of the Exec Agent on the MicroVM's Host, as
// the claim's status last named it. It may be empty while the Host's Node
// report names none.
func (cl *Claim) AgentAddress() string {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if cl.status.Host == nil {
		return ""
	}
	return cl.status.Host.AgentAddress
}

// Lost is closed when the claim's Lease is lost: the claim became Expired,
// or someone other than this Client deleted or replaced it. Err then says
// why. Release does not close it.
func (cl *Claim) Lost() <-chan struct{} { return cl.lost }

// Err is nil while the Lease is held, and wraps ErrLeaseLost once it is
// lost.
func (cl *Claim) Err() error {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	return cl.err
}

// Release stops renewing the claim and deletes it, which releases the
// MicroVM and, through the claim's Secret, every token of the claim. A
// claim already deleted is released without error. Release may be called
// more than once, and after the Lease is lost.
func (cl *Claim) Release(ctx context.Context) error {
	cl.cancel()
	<-cl.done

	//= docs/requirements/07-client.md#using
	//# When the Consumer releases a claim, the Client Library SHALL
	//# delete the claim.
	obj := &batteryv1alpha1.MicroVMClaim{ObjectMeta: metav1.ObjectMeta{Namespace: cl.c.cfg.Namespace, Name: cl.name}}
	err := cl.c.cfg.Client.Delete(ctx, obj, client.Preconditions{UID: &cl.uid})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("releasing claim %s: %w", cl.name, err)
	}
	return nil
}

// Dial connects to the Exec Agent at the claim's agent address over TLS,
// verifying the agent's serving certificate against Config.ServingCA, and
// sends the claim's current token as the bearer token of every call. When
// the address is not yet known, Dial reads the claim again first. opts are
// applied before the Client's own, which take precedence.
func (cl *Claim) Dial(ctx context.Context, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	if err := cl.Err(); err != nil {
		return nil, err
	}
	addr := cl.AgentAddress()
	if addr == "" {
		obj := &batteryv1alpha1.MicroVMClaim{}
		key := client.ObjectKey{Namespace: cl.c.cfg.Namespace, Name: cl.name}
		err := cl.c.cfg.Client.Get(ctx, key, obj)
		switch {
		case apierrors.IsNotFound(err):
			cl.lose("was deleted")
		case err != nil:
			return nil, fmt.Errorf("reading claim %s: %w", cl.name, err)
		default:
			cl.observe(obj)
		}
		if err := cl.Err(); err != nil {
			return nil, err
		}
		addr = cl.AgentAddress()
	}
	if addr == "" {
		return nil, fmt.Errorf("%w: claim %s", ErrNoAgentAddress, cl.name)
	}

	//= docs/requirements/07-client.md#using
	//# The Client Library SHALL connect to the Exec Agent at the
	//# address in the claim's status, verifying its serving certificate against
	//# the certificate authority the Consumer configures, and SHALL send the
	//# current claim token as the bearer token of every request.
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: cl.c.cfg.ServingCA}
	opts = append(opts,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		grpc.WithPerRPCCredentials(bearer{cl}),
	)
	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("dialling the exec agent of claim %s at %s: %w", cl.name, addr, err)
	}
	return conn, nil
}

// bearer sends a Claim's current token with every call.
type bearer struct{ cl *Claim }

// GetRequestMetadata implements credentials.PerRPCCredentials.
func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	b.cl.mu.Lock()
	token := b.cl.token
	b.cl.mu.Unlock()
	if token == "" {
		return nil, fmt.Errorf("claim %s has no token", b.cl.name)
	}
	return map[string]string{"authorization": "Bearer " + token}, nil
}

// RequireTransportSecurity implements credentials.PerRPCCredentials: a
// claim token is never sent in the clear.
func (bearer) RequireTransportSecurity() bool { return true }

// hold renews the claim and its token until the claim is released or its
// Lease is lost. The next token is due after tokenWait.
func (cl *Claim) hold(tokenWait time.Duration) {
	defer close(cl.done)
	renew := cl.c.clk.NewTimer(cl.interval)
	defer renew.Stop()
	token := cl.c.clk.NewTimer(tokenWait)
	defer token.Stop()
	for {
		select {
		case <-cl.ctx.Done():
			return
		case <-renew.C():
			if cl.renew() {
				return
			}
			renew.Reset(cl.interval)
		case <-token.C():
			wait, err := cl.refreshToken(cl.ctx)
			if err != nil {
				// Try again at the next heartbeat; the current token stays
				// in use until it expires.
				wait = cl.interval
			}
			token.Reset(wait)
		}
	}
}

// renew sets the claim's spec.renewTime to now, and reports whether the
// Lease has been lost.
func (cl *Claim) renew() bool {
	//= docs/requirements/07-client.md#holding
	//# While the Consumer holds a Bound claim, the Client Library SHALL
	//# set the claim's `spec.renewTime` at the Pool's heartbeat interval.
	//
	// The patch carries the claim's uid, which the API server refuses to
	// change, so a claim deleted and made again under the same name is not
	// renewed in its predecessor's place.
	ctx, cancel := context.WithTimeout(cl.ctx, cl.interval)
	defer cancel()
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"uid": cl.uid},
		"spec":     map[string]any{"renewTime": metav1.NewMicroTime(cl.c.clk.Now())},
	})
	if err != nil {
		return false
	}
	obj := &batteryv1alpha1.MicroVMClaim{ObjectMeta: metav1.ObjectMeta{Namespace: cl.c.cfg.Namespace, Name: cl.name}}
	err = cl.c.cfg.Client.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patch))
	switch {
	case apierrors.IsNotFound(err):
		cl.lose("was deleted")
	case err != nil && (apierrors.IsInvalid(err) || apierrors.IsConflict(err)):
		// The claim may have been replaced; find out.
		cur := &batteryv1alpha1.MicroVMClaim{}
		gerr := cl.c.cfg.Client.Get(ctx, client.ObjectKeyFromObject(obj), cur)
		switch {
		case apierrors.IsNotFound(gerr):
			cl.lose("was deleted")
		case gerr == nil:
			cl.observe(cur)
		}
	case err != nil:
		// A renewal that failed in transit is tried again at the next
		// heartbeat; the Lease outlives several of them.
	default:
		cl.observe(obj)
	}
	return cl.Err() != nil
}

// observe records the claim as last read, and loses the Lease if the claim
// is no longer this Client's Bound claim.
func (cl *Claim) observe(obj *batteryv1alpha1.MicroVMClaim) {
	//= docs/requirements/07-client.md#holding
	//# If a held claim's phase becomes `Expired`, or the claim is
	//# deleted by anyone but the Client Library, then the Client Library SHALL
	//# report the Lease as lost to the Consumer.
	//
	// The Client stops renewing before it deletes the claim itself, so any
	// deletion seen here is someone else's.
	switch {
	case obj.UID != cl.uid:
		cl.lose("was deleted and replaced")
	case obj.DeletionTimestamp != nil:
		cl.lose("is being deleted")
	case obj.Status.Phase == batteryv1alpha1.MicroVMClaimExpired:
		cl.lose("is Expired")
	default:
		cl.mu.Lock()
		cl.status = *obj.Status.DeepCopy()
		cl.mu.Unlock()
	}
}

// lose records the Lease as lost, once.
func (cl *Claim) lose(why string) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if cl.err != nil {
		return
	}
	cl.err = fmt.Errorf("%w: claim %s %s", ErrLeaseLost, cl.name, why)
	close(cl.lost)
}

// refreshToken requests a new claim token and returns how long to wait
// before requesting the next one.
func (cl *Claim) refreshToken(ctx context.Context) (time.Duration, error) {
	//= docs/requirements/07-client.md#claiming
	//# The Client Library SHALL request each claim token with
	//# `TokenRequest` for the Holder, bound to the claim's Secret by name and uid,
	//# with the Exec Agent's audience.
	now := cl.c.clk.Now()
	tr := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			Audiences:         []string{batteryv1alpha1.ExecAgentTokenAudience},
			ExpirationSeconds: ptr.To(int64(cl.c.cfg.TokenExpiration / time.Second)),
			BoundObjectRef: &authenticationv1.BoundObjectReference{
				APIVersion: "v1",
				Kind:       "Secret",
				Name:       cl.secret.Name,
				UID:        cl.secretID,
			},
		},
	}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: cl.secret.Namespace, Name: cl.holder}}
	if err := cl.c.cfg.Client.SubResource("token").Create(ctx, sa, tr); err != nil {
		return 0, fmt.Errorf("requesting a token of claim %s for %s: %w", cl.name, cl.holder, err)
	}
	if tr.Status.Token == "" {
		return 0, errors.New("the API server returned an empty claim token")
	}
	expires := tr.Status.ExpirationTimestamp.Time
	if expires.IsZero() {
		expires = now.Add(cl.c.cfg.TokenExpiration)
	}

	//= docs/requirements/07-client.md#holding
	//# While the Consumer holds a Bound claim, the Client Library SHALL
	//# request a new claim token before the current one expires.
	//
	// The next one is requested when two thirds of this one's life have
	// passed, which leaves a third for retries.
	wait := max(expires.Sub(now)*2/3, minTokenRefresh)
	cl.mu.Lock()
	cl.token = tr.Status.Token
	cl.mu.Unlock()
	return wait, nil
}
