//go:build e2e

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

package e2e

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
)

// TestCRDValidation: the CRDs, as the Manifests install them, refuse what
// the API says is invalid.
func TestCRDValidation(t *testing.T) {
	//= docs/requirements/08-test-doubles.md#test-environments
	//= type=test
	//# The e2e suite SHALL test the behaviour that depends on the
	//# Kubernetes API server, including CRD validation, admission policies,
	//# TokenReview, TokenRequest and `CertificateSigningRequest`s, in the kind
	//# cluster.
	poolNS, claimNS := envconf.RandomName("e2e-pools", 16), envconf.RandomName("e2e-claims", 16)

	pools := features.New("Pool validation").
		Setup(createNamespace(poolNS)).
		Assess("a valid Pool is accepted", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c := mustClient(t, cfg)
			if err := c.Create(ctx, validPool(poolNS, "valid"), client.DryRunAll); err != nil {
				t.Fatalf("creating a valid Pool: %v", err)
			}
			return ctx
		}).
		Assess("an invalid Pool is refused", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/01-resources.md#pool
			//= type=test
			//# The CRDs SHALL reject a `Pool` whose `spec.size` is negative or
			//# whose enumerated fields hold a value that battery v0.3.3 does not define.
			c := mustClient(t, cfg)
			for _, tc := range []struct {
				name   string
				mutate func(*batteryv1alpha1.Pool)
				field  string
			}{
				{"a negative size", func(p *batteryv1alpha1.Pool) { p.Spec.Size = -1 }, "spec.size"},
				{"an undefined replenishment strategy", func(p *batteryv1alpha1.Pool) {
					p.Spec.Replenishment.Type = "WhenItFeelsLikeIt"
				}, "spec.replenishment.type"},
				{"an undefined hook failure policy", func(p *batteryv1alpha1.Pool) {
					p.Spec.Hooks = batteryv1alpha1.PoolHooks{Create: []string{"true"}, FailurePolicy: "Ignore"}
				}, "spec.hooks.failurePolicy"},
				{"an undefined network interface type", func(p *batteryv1alpha1.Pool) {
					p.Spec.Template.Interfaces = []batteryv1alpha1.NetworkInterface{{DeviceID: "eth1", Type: "Bridge"}}
				}, "spec.template.interfaces[0].type"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					p := validPool(poolNS, "invalid")
					tc.mutate(p)
					wantInvalid(t, c.Create(ctx, p, client.DryRunAll), tc.field)
				})
			}
			return ctx
		}).
		Teardown(deleteNamespace(poolNS)).
		Feature()

	claims := features.New("MicroVMClaim validation").
		Setup(createNamespace(claimNS)).
		Assess("a claim's poolRef and serviceAccountName cannot change", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/01-resources.md#microvmclaim
			//= type=test
			//# The CRDs SHALL reject an update that changes a claim's
			//# `spec.serviceAccountName` or `spec.poolRef`.
			c := mustClient(t, cfg)
			claim := &batteryv1alpha1.MicroVMClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "immutable", Namespace: claimNS},
				Spec: batteryv1alpha1.MicroVMClaimSpec{
					PoolRef:            batteryv1alpha1.PoolReference{Name: "builders"},
					ServiceAccountName: "runner",
				},
			}
			if err := c.Create(ctx, claim); err != nil {
				t.Fatalf("creating a claim: %v", err)
			}
			for _, tc := range []struct {
				name   string
				mutate func(*batteryv1alpha1.MicroVMClaim)
				field  string
			}{
				{"poolRef", func(c *batteryv1alpha1.MicroVMClaim) { c.Spec.PoolRef.Name = "others" }, "spec.poolRef"},
				{"serviceAccountName", func(c *batteryv1alpha1.MicroVMClaim) { c.Spec.ServiceAccountName = "someone-else" }, "spec.serviceAccountName"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					// The Claim Controller writes the claim too, so the change
					// is a merge patch, which no write of its can conflict with.
					var got batteryv1alpha1.MicroVMClaim
					if err := c.Get(ctx, client.ObjectKeyFromObject(claim), &got); err != nil {
						t.Fatal(err)
					}
					before := got.DeepCopy()
					tc.mutate(&got)
					wantInvalid(t, c.Patch(ctx, &got, client.MergeFrom(before), client.DryRunAll), tc.field)
				})
			}
			return ctx
		}).
		Teardown(deleteNamespace(claimNS)).
		Feature()

	testenv.Test(t, pools, claims)
}

// validPool is a Pool the CRD accepts.
func validPool(ns, name string) *batteryv1alpha1.Pool {
	return &batteryv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: batteryv1alpha1.PoolSpec{
			Size: 2,
			Template: batteryv1alpha1.MicroVMTemplate{
				VCPU:       2,
				MemoryInMb: 2048,
				Kernel:     batteryv1alpha1.Kernel{Image: "ghcr.io/example/kernel:6.6"},
				RootVolume: batteryv1alpha1.Volume{ContainerSource: ptr.To("ghcr.io/example/root:24.04")},
			},
		},
	}
}

// wantInvalid fails t unless err is the API server's Invalid error naming
// field.
func wantInvalid(t *testing.T, err error, field string) {
	t.Helper()
	if !apierrors.IsInvalid(err) {
		t.Fatalf("got error %v, want an Invalid error for %s", err, field)
	}
	if !strings.Contains(err.Error(), field) {
		t.Fatalf("got error %q, want it to name %s", err, field)
	}
}

// mustClient is newClient, failing t on an error.
func mustClient(t *testing.T, cfg *envconf.Config) client.Client {
	t.Helper()
	c, err := newClient(cfg)
	if err != nil {
		t.Fatalf("building a client: %v", err)
	}
	return c
}

// createNamespace and deleteNamespace are a feature's setup and teardown
// of a namespace of its own.
func createNamespace(name string) features.Func {
	return func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if err := mustClient(t, cfg).Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("creating the namespace %s: %v", name, err)
		}
		return ctx
	}
}

func deleteNamespace(name string) features.Func {
	return func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if err := mustClient(t, cfg).Delete(ctx, ns); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("deleting the namespace %s: %v", name, err)
		}
		return ctx
	}
}
