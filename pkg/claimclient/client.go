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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/clock"
)

const (
	// defaultTokenExpiration is the lifetime of a claim token when the
	// Config names none: ten minutes, the shortest the API server grants.
	defaultTokenExpiration = 10 * time.Minute
	// defaultHeartbeatInterval is the Pool's heartbeat interval when its
	// spec names none, the CRD's default.
	defaultHeartbeatInterval = 10 * time.Second
	// pollInterval is how often Claim reads a claim that is not yet Bound.
	pollInterval = time.Second
	// deleteTimeout bounds the deletion of a claim the Client gives up on,
	// which runs after the caller's context has ended.
	deleteTimeout = 30 * time.Second
)

var (
	// ErrPoolExhausted is wrapped by the error of a Claim that stopped
	// waiting while its Pool had no MicroVM to give (CC-004).
	ErrPoolExhausted = errors.New("the pool is exhausted")
	// ErrLeaseLost is wrapped by Claim.Err once a held claim's Lease is lost
	// (CC-012), and by the error of a Claim whose claim was deleted or
	// expired before it was returned.
	ErrLeaseLost = errors.New("the lease is lost")
	// ErrNoAgentAddress is wrapped by the error of Dial when the claim's
	// status names no Exec Agent address yet.
	ErrNoAgentAddress = errors.New("the claim names no exec agent address")
)

// Config configures a Client.
type Config struct {
	// Client reaches the API server. Its scheme has to know client-go's
	// types and this project's v1alpha1.
	Client client.Client
	// Namespace is the namespace of the claims, their Pools, their Secrets
	// and their Holders.
	Namespace string
	// ServingCA verifies the Exec Agent's serving certificate: the
	// Operator's serving CA (ADR 0004).
	ServingCA *x509.CertPool
	// TokenExpiration is the lifetime requested for each claim token. Zero
	// is ten minutes, the shortest the API server grants.
	TokenExpiration time.Duration
}

// Client claims MicroVMs for a Consumer. Build it with New. It is safe for
// concurrent use.
type Client struct {
	cfg Config
	// clk and poll are the time source and the polling interval; tests
	// replace them.
	clk  clock.Clock
	poll time.Duration
}

// New returns a Client for cfg.
func New(cfg Config) (*Client, error) {
	switch {
	case cfg.Client == nil:
		return nil, errors.New("claimclient: Config.Client is required")
	case cfg.Namespace == "":
		return nil, errors.New("claimclient: Config.Namespace is required")
	case cfg.ServingCA == nil:
		return nil, errors.New("claimclient: Config.ServingCA is required")
	case cfg.TokenExpiration < 0:
		return nil, errors.New("claimclient: Config.TokenExpiration is negative")
	}
	if cfg.TokenExpiration == 0 {
		cfg.TokenExpiration = defaultTokenExpiration
	}
	return &Client{cfg: cfg, clk: clock.Real{}, poll: pollInterval}, nil
}

// Request is one claim a Consumer makes.
type Request struct {
	// Pool names the Pool, in the Client's namespace, to claim a MicroVM
	// from. It is required.
	Pool string
	// ServiceAccountName names the Holder: the ServiceAccount, in the
	// Client's namespace, that may use the MicroVM and that claim tokens
	// are requested for. It is required.
	ServiceAccountName string
	// Name is the claim's name. When it is empty the API server generates
	// one from GenerateName, or from the Pool's name when that is empty
	// too.
	Name         string
	GenerateName string
	// Labels are put on the claim.
	Labels map[string]string
	// OnPending, when set, is called from Claim each time the reason a
	// claim is still Pending changes.
	OnPending func(Pending)
}

// Pending is why a claim is not yet Bound: the reason and message of its
// Bound condition.
type Pending struct {
	Reason  string
	Message string
}

// PoolExhausted reports whether the claim waits because its Pool has no
// MicroVM to give. It binds once one is available.
func (p Pending) PoolExhausted() bool { return p.Reason == batteryv1alpha1.ReasonPoolExhausted }

// Claim claims a MicroVM for req and returns the claim once it is Bound. It
// creates the MicroVMClaim and its Secret, waits for the claim to bind,
// reports why it is still Pending through req.OnPending, and requests the
// first claim token. From then on the returned Claim renews itself until
// it is released or its Lease is lost.
//
// When ctx ends before the claim binds, or anything fails, Claim deletes
// the claim and returns an error, which wraps ErrPoolExhausted if the claim
// was then waiting for its exhausted Pool.
func (c *Client) Claim(ctx context.Context, req Request) (*Claim, error) {
	if req.Pool == "" || req.ServiceAccountName == "" {
		return nil, errors.New("claimclient: Request.Pool and Request.ServiceAccountName are required")
	}

	//= docs/requirements/07-client.md#claiming
	//# The Client Library SHALL create a claim with the Pool and
	//# Holder the Consumer names, and then an empty Secret `<claim name>-exec`
	//# whose owner reference is the claim.
	obj := &batteryv1alpha1.MicroVMClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:    c.cfg.Namespace,
			Name:         req.Name,
			GenerateName: req.GenerateName,
			Labels:       req.Labels,
		},
		Spec: batteryv1alpha1.MicroVMClaimSpec{
			PoolRef:            batteryv1alpha1.PoolReference{Name: req.Pool},
			ServiceAccountName: req.ServiceAccountName,
		},
	}
	if obj.Name == "" && obj.GenerateName == "" {
		obj.GenerateName = req.Pool + "-"
	}
	if err := c.cfg.Client.Create(ctx, obj); err != nil {
		return nil, fmt.Errorf("creating a claim on pool %s: %w", req.Pool, err)
	}
	cl, err := c.bind(ctx, obj, req)
	if err != nil {
		if derr := c.deleteClaim(ctx, obj.Name, obj.UID); derr != nil {
			err = errors.Join(err, derr)
		}
		return nil, err
	}
	return cl, nil
}

// bind takes a newly created claim to Bound and starts holding it.
func (c *Client) bind(ctx context.Context, obj *batteryv1alpha1.MicroVMClaim, req Request) (*Claim, error) {
	if obj.UID == "" {
		return nil, fmt.Errorf("the API server gave claim %s no uid", obj.Name)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: obj.Namespace,
			Name:      batteryv1alpha1.ExecSecretName(obj.Name),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: batteryv1alpha1.GroupVersion.String(),
				Kind:       "MicroVMClaim",
				Name:       obj.Name,
				UID:        obj.UID,
			}},
		},
		Type: corev1.SecretTypeOpaque,
	}
	if err := c.cfg.Client.Create(ctx, secret); err != nil {
		return nil, fmt.Errorf("creating the Secret of claim %s: %w", obj.Name, err)
	}
	if secret.UID == "" {
		return nil, fmt.Errorf("the API server gave Secret %s no uid", secret.Name)
	}

	bound, err := c.waitBound(ctx, obj.Name, obj.UID, req.OnPending)
	if err != nil {
		return nil, err
	}
	interval, err := c.heartbeatInterval(ctx, req.Pool)
	if err != nil {
		return nil, err
	}
	cl := newClaim(c, bound, secret, interval)
	tokenWait, err := cl.refreshToken(ctx)
	if err != nil {
		return nil, err
	}
	go cl.hold(tokenWait)
	return cl, nil
}

// waitBound polls the claim until it is Bound.
func (c *Client) waitBound(
	ctx context.Context, name string, uid types.UID, onPending func(Pending),
) (*batteryv1alpha1.MicroVMClaim, error) {
	var last *Pending
	for {
		obj := &batteryv1alpha1.MicroVMClaim{}
		err := c.cfg.Client.Get(ctx, client.ObjectKey{Namespace: c.cfg.Namespace, Name: name}, obj)
		switch {
		case apierrors.IsNotFound(err) || (err == nil && obj.UID != uid):
			return nil, fmt.Errorf("%w: claim %s was deleted before it was bound", ErrLeaseLost, name)
		case err == nil && obj.DeletionTimestamp != nil:
			return nil, fmt.Errorf("%w: claim %s is being deleted before it was bound", ErrLeaseLost, name)
		case err == nil && obj.Status.Phase == batteryv1alpha1.MicroVMClaimExpired:
			return nil, fmt.Errorf("%w: claim %s is Expired", ErrLeaseLost, name)
		case err == nil && obj.Status.Phase == batteryv1alpha1.MicroVMClaimBound:
			//= docs/requirements/07-client.md#claiming
			//# The Client Library SHALL return a claim to the Consumer only once
			//# the claim's phase is `Bound`, together with the MicroVM's uid, the
			//# Host's node name and the Exec Agent's address from the claim's status.
			return obj, nil
		case err == nil:
			p := pendingOf(obj)
			//= docs/requirements/07-client.md#claiming
			//# While a claim waits with the reason `PoolExhausted`, the Client
			//# Library SHALL report the Pool as exhausted to the Consumer.
			if p != (Pending{}) && (last == nil || *last != p) {
				last = &p
				if onPending != nil {
					onPending(p)
				}
			}
		}
		// A failed read is retried: the claim may bind meanwhile, and the
		// caller's context bounds the wait.

		//= docs/requirements/07-client.md#claiming
		//# When the Consumer stops waiting for a claim to bind, the Client
		//# Library SHALL delete the claim.
		//
		// Claim deletes it when this returns an error.
		timer := c.clk.NewTimer(c.poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			if last != nil && last.PoolExhausted() {
				return nil, fmt.Errorf("claim %s did not bind: %w: %w", name, ErrPoolExhausted, ctx.Err())
			}
			return nil, fmt.Errorf("claim %s did not bind: %w", name, ctx.Err())
		case <-timer.C():
		}
	}
}

// pendingOf reads why a claim is Pending from its Bound condition.
func pendingOf(obj *batteryv1alpha1.MicroVMClaim) Pending {
	cond := meta.FindStatusCondition(obj.Status.Conditions, batteryv1alpha1.ConditionBound)
	if cond == nil {
		return Pending{}
	}
	return Pending{Reason: cond.Reason, Message: cond.Message}
}

// heartbeatInterval reads the heartbeat interval of the Pool pool.
func (c *Client) heartbeatInterval(ctx context.Context, pool string) (time.Duration, error) {
	p := &batteryv1alpha1.Pool{}
	if err := c.cfg.Client.Get(ctx, client.ObjectKey{Namespace: c.cfg.Namespace, Name: pool}, p); err != nil {
		return 0, fmt.Errorf("reading the heartbeat interval of pool %s: %w", pool, err)
	}
	if d := p.Spec.Lease.HeartbeatInterval; d != nil && d.Duration > 0 {
		return d.Duration, nil
	}
	return defaultHeartbeatInterval, nil
}

// deleteClaim deletes the claim name if it still has uid. It runs even
// after ctx has ended, because it is how the Client cleans up after a
// caller that gave up.
func (c *Client) deleteClaim(ctx context.Context, name string, uid types.UID) error {
	if name == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deleteTimeout)
	defer cancel()
	obj := &batteryv1alpha1.MicroVMClaim{ObjectMeta: metav1.ObjectMeta{Namespace: c.cfg.Namespace, Name: name}}
	opts := []client.DeleteOption{}
	if uid != "" {
		opts = append(opts, client.Preconditions{UID: &uid})
	}
	if err := c.cfg.Client.Delete(ctx, obj, opts...); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting claim %s: %w", name, err)
	}
	return nil
}
