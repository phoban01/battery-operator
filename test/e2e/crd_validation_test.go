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
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
)

// TestCRDValidation: the CRDs, as the Manifests install them, define the
// API and refuse what it says is invalid. Most assessments are dry runs:
// the API server's answer is the point, and nothing reaches the Operator's
// controllers. The status subresource needs objects that exist, so those
// assessments create them, and write their status only in dry runs, which
// no write of the controllers' can conflict with.
func TestCRDValidation(t *testing.T) {
	//= docs/requirements/08-test-doubles.md#test-environments
	//= type=test
	//# The e2e suite SHALL test the behaviour that depends on the
	//# Kubernetes API server, including CRD validation, admission policies,
	//# TokenReview, TokenRequest and `CertificateSigningRequest`s, in the kind
	//# cluster.
	poolNS, claimNS := envconf.RandomName("e2e-pools", 16), envconf.RandomName("e2e-claims", 16)
	testenv.Test(t, apiGroupFeature(), poolFeature(poolNS), claimFeature(claimNS))
}

// apiGroupFeature: the API server serves the two resources, and no third.
func apiGroupFeature() features.Feature {
	return features.New("API group").
		Assess("Pool and MicroVMClaim are namespaced in battery.liquidmetal-x.dev/v1alpha1", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/01-resources.md#api-group
			//= type=test
			//# The CRDs SHALL define `Pool` and `MicroVMClaim` as namespaced
			//# resources in the API group `battery.liquidmetal-x.dev` at version
			//# `v1alpha1`.
			c := mustClient(t, cfg)
			for kind, resource := range map[string]string{"Pool": "pools", "MicroVMClaim": "microvmclaims"} {
				gk := schema.GroupKind{Group: "battery.liquidmetal-x.dev", Kind: kind}
				mapping, err := c.RESTMapper().RESTMapping(gk, "v1alpha1")
				if err != nil {
					t.Fatalf("the API server has no %s in battery.liquidmetal-x.dev/v1alpha1: %v", kind, err)
				}
				if mapping.Scope.Name() != meta.RESTScopeNameNamespace {
					t.Errorf("%s scope = %q, want %q", kind, mapping.Scope.Name(), meta.RESTScopeNameNamespace)
				}
				if mapping.Resource.Resource != resource {
					t.Errorf("%s resource = %q, want %s", kind, mapping.Resource.Resource, resource)
				}
			}
			return ctx
		}).
		Assess("there is no MicroVM resource", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/01-resources.md#api-group
			//= type=test
			//# The CRDs SHALL define no `MicroVM` resource.
			gk := schema.GroupKind{Group: "battery.liquidmetal-x.dev", Kind: "MicroVM"}
			if _, err := mustClient(t, cfg).RESTMapper().RESTMapping(gk); !meta.IsNoMatchError(err) {
				t.Errorf("RESTMapping(MicroVM) error = %v, want a no-match error", err)
			}
			return ctx
		}).
		Feature()
}

// poolFeature: what the Pool CRD enforces, in the namespace poolNS.
func poolFeature(poolNS string) features.Feature {
	return features.New("Pool").
		Setup(createNamespace(poolNS)).
		Assess("a Pool carries every PoolSpec field", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/01-resources.md#pool
			//= type=test
			//# The `Pool` resource SHALL carry in its `spec` one field for
			//# each of the `PoolSpec` fields `microvm_template`, `size`,
			//# `replenishment_strategy`, `create_commands`, `pre_lease_commands`,
			//# `hook_failure_policy`, `heartbeat_interval` and
			//# `heartbeat_expiry_threshold` of battery v0.3.3.
			pool := everyFieldPool(poolNS)
			want := pool.Spec.DeepCopy()
			if err := mustClient(t, cfg).Create(ctx, pool, client.DryRunAll); err != nil {
				t.Fatalf("creating a Pool with every field: %v", err)
			}
			if !reflect.DeepEqual(&pool.Spec, want) {
				t.Errorf("stored spec differs from the one applied:\n got %+v\nwant %+v", pool.Spec, *want)
			}
			return ctx
		}).
		Assess("a Pool gets battery's defaults", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			pool := validPool(poolNS, "defaults")
			pool.Spec.Template.Interfaces = []batteryv1alpha1.NetworkInterface{{DeviceID: deviceID}}
			if err := mustClient(t, cfg).Create(ctx, pool, client.DryRunAll); err != nil {
				t.Fatalf("creating a minimal Pool: %v", err)
			}
			s := pool.Spec
			if s.Replenishment.Type != batteryv1alpha1.ReplenishImmediateOnLease {
				t.Errorf("replenishment.type = %q, want ImmediateOnLease", s.Replenishment.Type)
			}
			if s.Hooks.FailurePolicy != batteryv1alpha1.HookFailureDeleteAndReplace {
				t.Errorf("hooks.failurePolicy = %q, want DeleteAndReplace", s.Hooks.FailurePolicy)
			}
			if s.Lease.HeartbeatInterval == nil || s.Lease.HeartbeatInterval.Duration != 10*time.Second {
				t.Errorf("lease.heartbeatInterval = %v, want 10s", s.Lease.HeartbeatInterval)
			}
			if s.Lease.ExpiryThreshold == nil || s.Lease.ExpiryThreshold.Duration != 30*time.Second {
				t.Errorf("lease.expiryThreshold = %v, want 30s", s.Lease.ExpiryThreshold)
			}
			if s.Template.Interfaces[0].Type != batteryv1alpha1.NetworkInterfaceMacvtap {
				t.Errorf("interfaces[0].type = %q, want Macvtap", s.Template.Interfaces[0].Type)
			}
			return ctx
		}).
		Assess("a Pool selects its Hosts by Node labels, and has no flintlock_hosts", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/01-resources.md#pool
			//= type=test
			//# The `Pool` resource SHALL select the Hosts its MicroVMs may run
			//# on with a Node label selector in `spec.placement.nodeSelector`, in place of
			//# battery's `flintlock_hosts`.
			c := mustClient(t, cfg)
			pool := validPool(poolNS, "placement")
			selector := map[string]string{hostLabel: isTrue, "kubernetes.io/arch": "amd64"}
			pool.Spec.Placement.NodeSelector = selector
			if err := c.Create(ctx, pool, client.DryRunAll); err != nil {
				t.Fatalf("creating a Pool with a nodeSelector: %v", err)
			}
			if !reflect.DeepEqual(pool.Spec.Placement.NodeSelector, selector) {
				t.Errorf("placement.nodeSelector = %v, want %v", pool.Spec.Placement.NodeSelector, selector)
			}

			// flintlock_hosts and the proposal's placement.strategy have no
			// field: the API server prunes them.
			raw := unstructuredPool(poolNS, "placement-pruned", map[string]any{
				"size": int64(1),
				"template": map[string]any{
					"vcpu": int64(1), "memoryInMb": int64(512),
					"kernel":     map[string]any{"image": "k"},
					"rootVolume": map[string]any{"containerSource": "r"},
				},
				"flintlockHosts": []any{"host-a"},
				"placement":      map[string]any{"nodeSelector": map[string]any{"a": "b"}, "strategy": "RoundRobin"},
			})
			if err := c.Create(ctx, raw, client.DryRunAll); err != nil {
				t.Fatalf("creating a Pool with unknown fields: %v", err)
			}
			spec, _ := raw.Object["spec"].(map[string]any)
			if _, ok := spec["flintlockHosts"]; ok {
				t.Error("spec.flintlockHosts was kept, want it pruned")
			}
			placement, _ := spec["placement"].(map[string]any)
			if _, ok := placement["strategy"]; ok {
				t.Error("spec.placement.strategy was kept, want it pruned")
			}
			return ctx
		}).
		Assess("an invalid Pool is refused", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/01-resources.md#pool
			//= type=test
			//# The CRDs SHALL reject a `Pool` whose `spec.size` is negative or
			//# whose enumerated fields hold a value that battery v0.3.3 does not define.
			c := mustClient(t, cfg)
			for _, tc := range invalidPools {
				t.Run(tc.name, func(t *testing.T) {
					p := validPool(poolNS, "invalid")
					tc.mutate(p)
					wantInvalid(t, c.Create(ctx, p, client.DryRunAll), tc.field)
				})
			}
			t.Run("no template", func(t *testing.T) {
				raw := unstructuredPool(poolNS, "no-template", map[string]any{"size": int64(1)})
				wantInvalid(t, c.Create(ctx, raw, client.DryRunAll), "spec.template")
			})
			return ctx
		}).
		Assess("a Pool's status is a subresource", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/01-resources.md#pool
			//= type=test
			//# The `Pool` resource SHALL have a status subresource that carries
			//# `observedGeneration`, the counts of available, leased, provisioning and
			//# quarantined MicroVMs, and the conditions `Ready` and `Exhausted`.
			c := mustClient(t, cfg)
			// Size 0, so that battery creates no MicroVMs for it.
			pool := validPool(poolNS, "status")
			pool.Spec.Size = 0
			if err := c.Create(ctx, pool); err != nil {
				t.Fatalf("creating a Pool: %v", err)
			}

			now := metav1.Now()
			got, before := getObject(ctx, t, c, pool)
			got.Status = batteryv1alpha1.PoolStatus{
				ObservedGeneration: got.Generation,
				Available:          1,
				Leased:             2,
				Provisioning:       3,
				Quarantined:        4,
				Conditions: []metav1.Condition{
					{Type: batteryv1alpha1.PoolConditionReady, Status: metav1.ConditionTrue, Reason: "AtTargetSize", LastTransitionTime: now},
					{Type: batteryv1alpha1.PoolConditionExhausted, Status: metav1.ConditionFalse, Reason: "VMsAvailable", LastTransitionTime: now},
				},
			}
			if err := c.Status().Patch(ctx, got, client.MergeFrom(before), client.DryRunAll); err != nil {
				t.Fatalf("patching the status subresource: %v", err)
			}
			s := got.Status
			if s.ObservedGeneration != got.Generation || s.Available != 1 || s.Leased != 2 ||
				s.Provisioning != 3 || s.Quarantined != 4 {
				t.Errorf("status = %+v, want the counts 1, 2, 3, 4 at generation %d", s, got.Generation)
			}
			for _, ct := range []string{batteryv1alpha1.PoolConditionReady, batteryv1alpha1.PoolConditionExhausted} {
				if meta.FindStatusCondition(s.Conditions, ct) == nil {
					t.Errorf("condition %s missing from status", ct)
				}
			}

			// A write to the main resource changes spec and leaves status
			// alone.
			got, before = getObject(ctx, t, c, pool)
			got.Spec.Size = 3
			got.Status.Available = 99
			if err := c.Patch(ctx, got, client.MergeFrom(before), client.DryRunAll); err != nil {
				t.Fatalf("patching the Pool: %v", err)
			}
			if got.Status.Available == 99 {
				t.Error("a write to the main resource set status.available")
			}
			if got.Spec.Size != 3 {
				t.Errorf("spec.size = %d, want 3", got.Spec.Size)
			}
			return ctx
		}).
		Teardown(deleteNamespace(poolNS)).
		Feature()
}

// claimFeature: what the MicroVMClaim CRD enforces, in the namespace claimNS.
func claimFeature(claimNS string) features.Feature {
	return features.New("MicroVMClaim").
		Setup(createNamespace(claimNS)).
		Assess("a claim names its Pool", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/01-resources.md#microvmclaim
			//= type=test
			//# The `MicroVMClaim` resource SHALL carry `spec.poolRef.name`,
			//# which names a `Pool` in the claim's own namespace.
			c := mustClient(t, cfg)
			claim := newClaim(claimNS, "poolref")
			if err := c.Create(ctx, claim, client.DryRunAll); err != nil {
				t.Fatalf("creating a claim: %v", err)
			}
			if claim.Spec.PoolRef.Name != "builders" {
				t.Errorf("spec.poolRef.name = %q, want builders", claim.Spec.PoolRef.Name)
			}
			noPool := newClaim(claimNS, "poolref-empty")
			noPool.Spec.PoolRef.Name = ""
			wantInvalid(t, c.Create(ctx, noPool, client.DryRunAll), "spec.poolRef.name")
			return ctx
		}).
		Assess("a claim names its ServiceAccount", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/01-resources.md#microvmclaim
			//= type=test
			//# The `MicroVMClaim` resource SHALL carry
			//# `spec.serviceAccountName`, which names the ServiceAccount in the claim's
			//# namespace that may use the claimed MicroVM.
			c := mustClient(t, cfg)
			claim := newClaim(claimNS, "sa")
			if err := c.Create(ctx, claim, client.DryRunAll); err != nil {
				t.Fatalf("creating a claim: %v", err)
			}
			if claim.Spec.ServiceAccountName != "runner" {
				t.Errorf("spec.serviceAccountName = %q, want runner", claim.Spec.ServiceAccountName)
			}
			noSA := newClaim(claimNS, "sa-empty")
			noSA.Spec.ServiceAccountName = ""
			wantInvalid(t, c.Create(ctx, noSA, client.DryRunAll), "spec.serviceAccountName")
			return ctx
		}).
		Assess("a claim's poolRef and serviceAccountName cannot change", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/01-resources.md#microvmclaim
			//= type=test
			//# The CRDs SHALL reject an update that changes a claim's
			//# `spec.serviceAccountName` or `spec.poolRef`.
			c := mustClient(t, cfg)
			claim := newClaim(claimNS, "immutable")
			if err := c.Create(ctx, claim); err != nil {
				t.Fatalf("creating a claim: %v", err)
			}
			for _, tc := range []struct {
				name   string
				mutate func(*batteryv1alpha1.MicroVMClaim)
				field  string
			}{
				{"poolRef", func(c *batteryv1alpha1.MicroVMClaim) { c.Spec.PoolRef.Name = "others" }, "spec.poolRef is immutable"},
				{"serviceAccountName", func(c *batteryv1alpha1.MicroVMClaim) { c.Spec.ServiceAccountName = "someone-else" }, "spec.serviceAccountName is immutable"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					// The Claim Controller writes the claim too, so the change
					// is a merge patch, which no write of its can conflict with.
					got, before := getObject(ctx, t, c, claim)
					tc.mutate(got)
					wantInvalid(t, c.Patch(ctx, got, client.MergeFrom(before), client.DryRunAll), tc.field)
				})
			}
			return ctx
		}).
		Assess("the Holder renews by writing spec.renewTime", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/01-resources.md#microvmclaim
			//= type=test
			//# The `MicroVMClaim` resource SHALL carry `spec.renewTime`, which
			//# the Holder sets to renew the claim's Lease.
			c := mustClient(t, cfg)
			// A merge patch on spec, which needs no access to the status
			// subresource: the rest of the spec stays mutable beside the
			// immutable fields.
			renew := time.Date(2026, 9, 22, 17, 4, 10, 123456000, time.UTC)
			got, before := getObject(ctx, t, c, newClaim(claimNS, "immutable"))
			rt := metav1.NewMicroTime(renew)
			got.Spec.RenewTime = &rt
			if err := c.Patch(ctx, got, client.MergeFrom(before), client.DryRunAll); err != nil {
				t.Fatalf("patching spec.renewTime: %v", err)
			}
			if got.Spec.RenewTime == nil || !got.Spec.RenewTime.Time.Equal(renew) {
				t.Errorf("spec.renewTime = %v, want %v, to the microsecond", got.Spec.RenewTime, renew)
			}
			return ctx
		}).
		Assess("a claim's status is a subresource", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/01-resources.md#microvmclaim
			//= type=test
			//# The `MicroVMClaim` resource SHALL have a status subresource that
			//# carries the phase, one of `Pending`, `Bound` and `Expired`,
			//# the lease id battery chose, the MicroVM's uid, the Host's node name, the
			//# Exec Agent's address, the time the claim was bound, the time its Lease
			//# expires, and the condition `Bound`.
			c := mustClient(t, cfg)
			existing := newClaim(claimNS, "immutable")

			bound := metav1.NewTime(time.Date(2026, 9, 22, 17, 3, 58, 0, time.UTC))
			expires := metav1.NewTime(time.Date(2026, 9, 22, 17, 4, 40, 0, time.UTC))
			got, before := getObject(ctx, t, c, existing)
			got.Status = batteryv1alpha1.MicroVMClaimStatus{
				Phase:          batteryv1alpha1.MicroVMClaimBound,
				LeaseID:        "lease-0001",
				MicroVM:        &batteryv1alpha1.MicroVMReference{UID: "01JB7ZK4QH3M8V6R2N5T9X0C1D"},
				Host:           &batteryv1alpha1.HostReference{NodeName: "node-a", AgentAddress: "10.0.1.23:10270"},
				BoundTime:      &bound,
				LeaseExpiresAt: &expires,
			}
			meta.SetStatusCondition(&got.Status.Conditions, metav1.Condition{
				Type:   batteryv1alpha1.ConditionBound,
				Status: metav1.ConditionTrue,
				Reason: batteryv1alpha1.ReasonBound,
			})
			if err := c.Status().Patch(ctx, got, client.MergeFrom(before), client.DryRunAll); err != nil {
				t.Fatalf("patching status: %v", err)
			}
			s := got.Status
			if s.Phase != batteryv1alpha1.MicroVMClaimBound || s.LeaseID != "lease-0001" {
				t.Errorf("status phase %q, lease %q, want Bound, lease-0001", s.Phase, s.LeaseID)
			}
			if s.MicroVM == nil || s.MicroVM.UID != "01JB7ZK4QH3M8V6R2N5T9X0C1D" {
				t.Errorf("status.microVM = %+v, want uid 01JB7ZK4QH3M8V6R2N5T9X0C1D", s.MicroVM)
			}
			if s.Host == nil || s.Host.NodeName != "node-a" || s.Host.AgentAddress != "10.0.1.23:10270" {
				t.Errorf("status.host = %+v, want node-a at 10.0.1.23:10270", s.Host)
			}
			if s.BoundTime == nil || !s.BoundTime.Equal(&bound) {
				t.Errorf("status.boundTime = %v, want %v", s.BoundTime, bound)
			}
			if s.LeaseExpiresAt == nil || !s.LeaseExpiresAt.Equal(&expires) {
				t.Errorf("status.leaseExpiresAt = %v, want %v", s.LeaseExpiresAt, expires)
			}
			if !meta.IsStatusConditionTrue(s.Conditions, batteryv1alpha1.ConditionBound) {
				t.Errorf("status.conditions = %+v, want Bound True", s.Conditions)
			}

			// A write to the main resource leaves status alone.
			got, before = getObject(ctx, t, c, existing)
			got.Status.LeaseID = "someone-elses"
			if err := c.Patch(ctx, got, client.MergeFrom(before), client.DryRunAll); err != nil {
				t.Fatalf("patching the claim: %v", err)
			}
			if got.Status.LeaseID == "someone-elses" {
				t.Error("a write to the main resource set status.leaseID")
			}

			// Every phase is accepted, and nothing else.
			for _, phase := range []batteryv1alpha1.MicroVMClaimPhase{
				batteryv1alpha1.MicroVMClaimPending,
				batteryv1alpha1.MicroVMClaimBound,
				batteryv1alpha1.MicroVMClaimExpired,
			} {
				got, before = getObject(ctx, t, c, existing)
				got.Status.Phase = phase
				if err := c.Status().Patch(ctx, got, client.MergeFrom(before), client.DryRunAll); err != nil {
					t.Errorf("setting status.phase to %s: %v", phase, err)
				}
			}
			got, before = getObject(ctx, t, c, existing)
			got.Status.Phase = "Released"
			wantInvalid(t, c.Status().Patch(ctx, got, client.MergeFrom(before), client.DryRunAll), "status.phase")
			return ctx
		}).
		Teardown(deleteNamespace(claimNS)).
		Feature()
}

// deviceID is the network interface the Pools in these tests name.
const deviceID = "eth1"

// invalidPools are changes to validPool that the CRD refuses, each with
// what the refusal names.
var invalidPools = []struct {
	name   string
	mutate func(*batteryv1alpha1.Pool)
	field  string
}{
	{"a negative size", func(p *batteryv1alpha1.Pool) { p.Spec.Size = -1 }, "spec.size"},
	{"an undefined replenishment strategy", func(p *batteryv1alpha1.Pool) {
		p.Spec.Replenishment.Type = "WhenItFeelsLikeIt"
	}, "spec.replenishment.type"},
	{"the unspecified replenishment strategy", func(p *batteryv1alpha1.Pool) {
		p.Spec.Replenishment.Type = "REPLENISHMENT_STRATEGY_TYPE_UNSPECIFIED"
	}, "spec.replenishment.type"},
	{"an undefined hook failure policy", func(p *batteryv1alpha1.Pool) {
		p.Spec.Hooks = batteryv1alpha1.PoolHooks{Create: []string{"true"}, FailurePolicy: "Ignore"}
	}, "spec.hooks.failurePolicy"},
	{"an undefined network interface type", func(p *batteryv1alpha1.Pool) {
		p.Spec.Template.Interfaces = []batteryv1alpha1.NetworkInterface{{DeviceID: deviceID, Type: "Bridge"}}
	}, "spec.template.interfaces[0].type"},
	{"MinSizeThreshold without minSize", func(p *batteryv1alpha1.Pool) {
		p.Spec.Replenishment.Type = batteryv1alpha1.ReplenishMinSizeThreshold
	}, "minSize is required"},
	{"MinSizeThreshold with a zero minSize", func(p *batteryv1alpha1.Pool) {
		p.Spec.Replenishment = batteryv1alpha1.ReplenishmentStrategy{
			Type: batteryv1alpha1.ReplenishMinSizeThreshold, MinSize: ptr.To[int32](0),
		}
	}, "spec.replenishment.minSize"},
	{"a zero heartbeat interval", func(p *batteryv1alpha1.Pool) {
		p.Spec.Lease.HeartbeatInterval = &metav1.Duration{}
	}, "heartbeatInterval must be positive"},
	{"a negative expiry threshold", func(p *batteryv1alpha1.Pool) {
		p.Spec.Lease.ExpiryThreshold = &metav1.Duration{Duration: -time.Second}
	}, "expiryThreshold must be positive"},
	{"a template without a kernel image", func(p *batteryv1alpha1.Pool) {
		p.Spec.Template.Kernel.Image = ""
	}, "spec.template.kernel.image"},
	{"a root volume without a source", func(p *batteryv1alpha1.Pool) {
		p.Spec.Template.RootVolume = batteryv1alpha1.Volume{}
	}, "exactly one of containerSource and virtiofsSource"},
	{"a root volume with two sources", func(p *batteryv1alpha1.Pool) {
		p.Spec.Template.RootVolume.VirtiofsSource = ptr.To("/srv")
	}, "exactly one of containerSource and virtiofsSource"},
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

// everyFieldPool is a Pool with every field of its spec set.
func everyFieldPool(ns string) *batteryv1alpha1.Pool {
	pool := validPool(ns, "every-field")
	pool.Spec.Template = batteryv1alpha1.MicroVMTemplate{
		Provider:   ptr.To("firecracker"),
		VCPU:       4,
		MemoryInMb: 4096,
		Kernel: batteryv1alpha1.Kernel{
			Image:            "ghcr.io/example/kernel:6.6",
			Cmdline:          map[string]string{"console": "ttyS0"},
			Filename:         ptr.To("boot/vmlinux"),
			AddNetworkConfig: true,
		},
		Initrd:     &batteryv1alpha1.Initrd{Image: "ghcr.io/example/initrd:1", Filename: ptr.To("initrd")},
		RootVolume: batteryv1alpha1.Volume{ContainerSource: ptr.To("ghcr.io/example/root:24.04")},
		AdditionalVolumes: []batteryv1alpha1.Volume{{
			ID:             "cache",
			ReadOnly:       true,
			MountPoint:     ptr.To("/cache"),
			PartitionID:    ptr.To("b3c1"),
			SizeInMb:       ptr.To[int32](1024),
			VirtiofsSource: ptr.To("/var/cache/ci"),
		}},
		Interfaces: []batteryv1alpha1.NetworkInterface{{
			DeviceID: deviceID,
			Type:     batteryv1alpha1.NetworkInterfaceTap,
			GuestMAC: ptr.To("aa:bb:cc:dd:ee:ff"),
			Address: &batteryv1alpha1.StaticAddress{
				Address:     "10.0.0.5/24",
				Gateway:     ptr.To("10.0.0.1"),
				Nameservers: []string{"10.0.0.2"},
			},
			Overrides: &batteryv1alpha1.NetworkOverrides{BridgeName: ptr.To("br0")},
		}},
		Metadata:  map[string]string{"user-data": "I2Nsb3VkLWNvbmZpZw=="},
		Labels:    map[string]string{"profile": "ci"},
		CPUConfig: &batteryv1alpha1.CPUConfig{FeaturesToEnable: []string{"amx"}, KVMCapabilitiesToDisable: []string{"1"}},
	}
	pool.Spec.Size = 4
	pool.Spec.Replenishment = batteryv1alpha1.ReplenishmentStrategy{
		Type:    batteryv1alpha1.ReplenishMinSizeThreshold,
		MinSize: ptr.To[int32](2),
	}
	pool.Spec.Hooks = batteryv1alpha1.PoolHooks{
		Create:        []string{"systemctl is-active --quiet ready"},
		PreLease:      []string{"true"},
		FailurePolicy: batteryv1alpha1.HookFailureQuarantine,
	}
	pool.Spec.Lease = batteryv1alpha1.PoolLease{
		HeartbeatInterval: &metav1.Duration{Duration: 5 * time.Second},
		ExpiryThreshold:   &metav1.Duration{Duration: time.Minute},
	}
	return pool
}

// unstructuredPool is a Pool with spec as given, for fields the typed
// Pool cannot hold.
func unstructuredPool(ns, name string, spec map[string]any) *unstructured.Unstructured {
	raw := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	raw.SetGroupVersionKind(batteryv1alpha1.GroupVersion.WithKind("Pool"))
	raw.SetNamespace(ns)
	raw.SetName(name)
	return raw
}

// newClaim is a claim the CRD accepts, for the Pool "builders" and the
// ServiceAccount "runner".
func newClaim(ns, name string) *batteryv1alpha1.MicroVMClaim {
	return &batteryv1alpha1.MicroVMClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: batteryv1alpha1.MicroVMClaimSpec{
			PoolRef:            batteryv1alpha1.PoolReference{Name: "builders"},
			ServiceAccountName: "runner",
		},
	}
}

// getObject reads obj as stored, and returns it twice: one copy to change
// and one to patch from.
func getObject[T client.Object](ctx context.Context, t *testing.T, c client.Client, obj T) (T, T) {
	t.Helper()
	got := obj.DeepCopyObject().(T)
	if err := c.Get(ctx, client.ObjectKeyFromObject(obj), got); err != nil {
		t.Fatalf("getting %s: %v", obj.GetName(), err)
	}
	return got, got.DeepCopyObject().(T)
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
// of a namespace of its own. deleteNamespace waits for the namespace to go,
// which it does only once the Operator has deleted its Pools from battery
// (PO-003) and released its claims, so a Pool or claim the Operator cannot
// delete fails the feature.
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
		c := mustClient(t, cfg)
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if err := c.Delete(ctx, ns); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("deleting the namespace %s: %v", name, err)
			return ctx
		}
		err := wait.PollUntilContextTimeout(ctx, 2*time.Second, placementTimeout, true, func(ctx context.Context) (bool, error) {
			return apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(ns), ns)), nil
		})
		if err != nil {
			var pools batteryv1alpha1.PoolList
			_ = c.List(ctx, &pools, client.InNamespace(name))
			var claims batteryv1alpha1.MicroVMClaimList
			_ = c.List(ctx, &claims, client.InNamespace(name))
			left := make([]string, 0, len(pools.Items)+len(claims.Items))
			for _, p := range pools.Items {
				left = append(left, "Pool "+p.Name+" "+describe(p.Status))
			}
			for _, cl := range claims.Items {
				left = append(left, "MicroVMClaim "+cl.Name+" "+describe(cl.Status))
			}
			t.Errorf("waiting for the namespace %s to go: %v; left in it: %v", name, err, left)
		}
		return ctx
	}
}
