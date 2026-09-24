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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
)

// hostNodeIndex indexes the claim cache by status.host.nodeName.
const hostNodeIndex = "status.host.nodeName"

// errNotSynced is a lookup made before the claims were first listed.
var errNotSynced = errors.New("the claims have not been listed yet")

// KubeClaims is the ClaimLookup of this project's MicroVMClaim, read
// through its typed API. Claim reads each claim from the API server, never
// from a cache, so that a claim released a moment ago is not authorized
// from a cache that has not heard yet; BoundOnHost answers from a cache of
// every claim of the cluster, indexed by Host. RBAC: get, list and watch
// on microvmclaims.
type KubeClaims struct {
	// reader reads a claim from the API server.
	reader client.Reader
	// cached lists the claims from the cache, by hostNodeIndex.
	cached client.Reader
	// start keeps the cache until its context ends.
	start func(context.Context) error
	// synced reports whether the cache has listed the claims once.
	synced func() bool
}

// claimHostNode is hostNodeIndex: the Node of the Host a claim is bound on.
func claimHostNode(o client.Object) []string {
	claim, ok := o.(*batteryv1alpha1.MicroVMClaim)
	if !ok || claim.Status.Host == nil || claim.Status.Host.NodeName == "" {
		return nil
	}
	return []string{claim.Status.Host.NodeName}
}

// NewKubeClaims builds the lookup from the agent's client configuration.
// Run starts its cache.
func NewKubeClaims(ctx context.Context, cfg *rest.Config, resync time.Duration) (*KubeClaims, error) {
	scheme := runtime.NewScheme()
	if err := batteryv1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	reader, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("execagent: building the claim client: %w", err)
	}
	c, err := cache.New(cfg, cache.Options{Scheme: scheme, SyncPeriod: &resync})
	if err != nil {
		return nil, fmt.Errorf("execagent: building the claim cache: %w", err)
	}
	if err := c.IndexField(ctx, &batteryv1alpha1.MicroVMClaim{}, hostNodeIndex, claimHostNode); err != nil {
		return nil, fmt.Errorf("execagent: indexing the claim cache: %w", err)
	}
	inf, err := c.GetInformer(ctx, &batteryv1alpha1.MicroVMClaim{})
	if err != nil {
		return nil, fmt.Errorf("execagent: building the claim informer: %w", err)
	}
	return &KubeClaims{reader: reader, cached: c, start: c.Start, synced: inf.HasSynced}, nil
}

// newKubeClaims is the lookup over reader for single claims and cached for
// the claims of a Host. cached has to be indexed by hostNodeIndex with
// claimHostNode, and is taken to be synced and to need no running. The
// unit tests pass controller-runtime's fake client as both.
func newKubeClaims(reader, cached client.Reader) *KubeClaims {
	return &KubeClaims{
		reader: reader, cached: cached,
		start:  func(ctx context.Context) error { <-ctx.Done(); return nil },
		synced: func() bool { return true },
	}
}

// Run keeps the cache until ctx ends.
func (k *KubeClaims) Run(ctx context.Context) error { return k.start(ctx) }

// HasSynced reports whether the claims have been listed once.
func (k *KubeClaims) HasSynced() bool { return k.synced() }

// Claim implements ClaimLookup with a read from the API server.
func (k *KubeClaims) Claim(ctx context.Context, namespace, name string) (*Claim, error) {
	var o batteryv1alpha1.MicroVMClaim
	err := k.reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &o)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading claim %s/%s: %w", namespace, name, err)
	}
	c := claimOf(&o)
	return &c, nil
}

// BoundOnHost implements ClaimLookup from the cache alone: the drain guard
// it serves is reconciled again a moment later, so a cache a moment behind
// costs nothing.
func (k *KubeClaims) BoundOnHost(ctx context.Context, hostNode string) ([]Claim, error) {
	if !k.HasSynced() {
		return nil, errNotSynced
	}
	var list batteryv1alpha1.MicroVMClaimList
	if err := k.cached.List(ctx, &list, client.MatchingFields{hostNodeIndex: hostNode}); err != nil {
		return nil, err
	}
	var out []Claim
	for i := range list.Items {
		if c := claimOf(&list.Items[i]); c.Phase == batteryv1alpha1.MicroVMClaimBound && c.HostNode == hostNode {
			out = append(out, c)
		}
	}
	return out, nil
}

var _ ClaimLookup = (*KubeClaims)(nil)
