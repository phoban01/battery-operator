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

package v1alpha1_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/phoban01/battery-operator/api/v1alpha1"
)

const deviceID = "eth1"

// validPool is the smallest Pool the CRD accepts.
func validPool(name string) *v1alpha1.Pool {
	return &v1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: v1alpha1.PoolSpec{
			Size: 2,
			Template: v1alpha1.MicroVMTemplate{
				VCPU:       2,
				MemoryInMb: 2048,
				Kernel:     v1alpha1.Kernel{Image: "ghcr.io/example/kernel:6.6"},
				RootVolume: v1alpha1.Volume{ContainerSource: ptr.To("ghcr.io/example/root:24.04")},
			},
		},
	}
}

func create(t *testing.T, obj client.Object) error {
	t.Helper()
	err := k8sClient.Create(context.Background(), obj)
	if err == nil {
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), obj) })
	}
	return err
}

//= docs/requirements/01-resources.md#api-group
//= type=test
//# The CRDs SHALL define `Pool` and `MicroVMClaim` as namespaced
//# resources in the API group `battery.liquidmetal-x.dev` at version
//# `v1alpha1`.

func TestPoolIsNamespacedInTheBatteryGroup(t *testing.T) {
	gk := schema.GroupKind{Group: "battery.liquidmetal-x.dev", Kind: "Pool"}
	mapping, err := k8sClient.RESTMapper().RESTMapping(gk, "v1alpha1")
	if err != nil {
		t.Fatalf("the API server has no Pool in battery.liquidmetal-x.dev/v1alpha1: %v", err)
	}
	if mapping.Scope.Name() != meta.RESTScopeNameNamespace {
		t.Errorf("Pool scope = %q, want %q", mapping.Scope.Name(), meta.RESTScopeNameNamespace)
	}
	if mapping.Resource.Resource != "pools" {
		t.Errorf("Pool resource = %q, want pools", mapping.Resource.Resource)
	}
}

//= docs/requirements/01-resources.md#api-group
//= type=test
//# The CRDs SHALL define no `MicroVM` resource.

func TestNoMicroVMResource(t *testing.T) {
	gk := schema.GroupKind{Group: "battery.liquidmetal-x.dev", Kind: "MicroVM"}
	if _, err := k8sClient.RESTMapper().RESTMapping(gk); !meta.IsNoMatchError(err) {
		t.Errorf("RESTMapping(MicroVM) error = %v, want a no-match error", err)
	}
	if k8sClient.Scheme().Recognizes(v1alpha1.GroupVersion.WithKind("MicroVM")) {
		t.Error("the v1alpha1 scheme registers a MicroVM kind")
	}
}

//= docs/requirements/01-resources.md#pool
//= type=test
//# The `Pool` resource SHALL carry in its `spec` one field for
//# each of the `PoolSpec` fields `microvm_template`, `size`,
//# `replenishment_strategy`, `create_commands`, `pre_lease_commands`,
//# `hook_failure_policy`, `heartbeat_interval` and
//# `heartbeat_expiry_threshold` of battery v0.1.0.

func TestPoolCarriesEveryPoolSpecField(t *testing.T) {
	pool := validPool("every-field")
	pool.Spec.Template = v1alpha1.MicroVMTemplate{
		Provider:   ptr.To("firecracker"),
		VCPU:       4,
		MemoryInMb: 4096,
		Kernel: v1alpha1.Kernel{
			Image:            "ghcr.io/example/kernel:6.6",
			Cmdline:          map[string]string{"console": "ttyS0"},
			Filename:         ptr.To("boot/vmlinux"),
			AddNetworkConfig: true,
		},
		Initrd:     &v1alpha1.Initrd{Image: "ghcr.io/example/initrd:1", Filename: ptr.To("initrd")},
		RootVolume: v1alpha1.Volume{ContainerSource: ptr.To("ghcr.io/example/root:24.04")},
		AdditionalVolumes: []v1alpha1.Volume{{
			ID:             "cache",
			ReadOnly:       true,
			MountPoint:     ptr.To("/cache"),
			PartitionID:    ptr.To("b3c1"),
			SizeInMb:       ptr.To[int32](1024),
			VirtiofsSource: ptr.To("/var/cache/ci"),
		}},
		Interfaces: []v1alpha1.NetworkInterface{{
			DeviceID: deviceID,
			Type:     v1alpha1.NetworkInterfaceTap,
			GuestMAC: ptr.To("aa:bb:cc:dd:ee:ff"),
			Address: &v1alpha1.StaticAddress{
				Address:     "10.0.0.5/24",
				Gateway:     ptr.To("10.0.0.1"),
				Nameservers: []string{"10.0.0.2"},
			},
			Overrides: &v1alpha1.NetworkOverrides{BridgeName: ptr.To("br0")},
		}},
		Metadata:  map[string]string{"user-data": "I2Nsb3VkLWNvbmZpZw=="},
		Labels:    map[string]string{"profile": "ci"},
		CPUConfig: &v1alpha1.CPUConfig{FeaturesToEnable: []string{"amx"}, KVMCapabilitiesToDisable: []string{"1"}},
	}
	pool.Spec.Size = 4
	pool.Spec.Replenishment = v1alpha1.ReplenishmentStrategy{
		Type:    v1alpha1.ReplenishMinSizeThreshold,
		MinSize: ptr.To[int32](2),
	}
	pool.Spec.Hooks = v1alpha1.PoolHooks{
		Create:        []string{"systemctl is-active --quiet ready"},
		PreLease:      []string{"true"},
		FailurePolicy: v1alpha1.HookFailureQuarantine,
	}
	pool.Spec.Lease = v1alpha1.PoolLease{
		HeartbeatInterval: &metav1.Duration{Duration: 5 * time.Second},
		ExpiryThreshold:   &metav1.Duration{Duration: time.Minute},
	}
	want := pool.Spec.DeepCopy()

	if err := create(t, pool); err != nil {
		t.Fatalf("creating a Pool with every field: %v", err)
	}
	got := &v1alpha1.Pool{}
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pool), got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(&got.Spec, want) {
		t.Errorf("stored spec differs from the one applied:\n got %+v\nwant %+v", got.Spec, *want)
	}
}

func TestPoolDefaults(t *testing.T) {
	pool := validPool("defaults")
	pool.Spec.Template.Interfaces = []v1alpha1.NetworkInterface{{DeviceID: deviceID}}
	if err := create(t, pool); err != nil {
		t.Fatalf("creating a minimal Pool: %v", err)
	}
	got := &v1alpha1.Pool{}
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pool), got); err != nil {
		t.Fatal(err)
	}
	s := got.Spec
	if s.Replenishment.Type != v1alpha1.ReplenishImmediateOnLease {
		t.Errorf("replenishment.type = %q, want ImmediateOnLease", s.Replenishment.Type)
	}
	if s.Hooks.FailurePolicy != v1alpha1.HookFailureDeleteAndReplace {
		t.Errorf("hooks.failurePolicy = %q, want DeleteAndReplace", s.Hooks.FailurePolicy)
	}
	if s.Lease.HeartbeatInterval == nil || s.Lease.HeartbeatInterval.Duration != 10*time.Second {
		t.Errorf("lease.heartbeatInterval = %v, want 10s", s.Lease.HeartbeatInterval)
	}
	if s.Lease.ExpiryThreshold == nil || s.Lease.ExpiryThreshold.Duration != 30*time.Second {
		t.Errorf("lease.expiryThreshold = %v, want 30s", s.Lease.ExpiryThreshold)
	}
	if s.Template.Interfaces[0].Type != v1alpha1.NetworkInterfaceMacvtap {
		t.Errorf("interfaces[0].type = %q, want Macvtap", s.Template.Interfaces[0].Type)
	}
}

//= docs/requirements/01-resources.md#pool
//= type=test
//# The `Pool` resource SHALL select the Hosts its MicroVMs may run
//# on with a Node label selector in `spec.placement.nodeSelector`, in place of
//# battery's `flintlock_hosts`.

func TestPoolPlacementNodeSelector(t *testing.T) {
	pool := validPool("placement")
	selector := map[string]string{"battery.liquidmetal-x.dev/host": "true", "kubernetes.io/arch": "amd64"}
	pool.Spec.Placement.NodeSelector = selector
	if err := create(t, pool); err != nil {
		t.Fatalf("creating a Pool with a nodeSelector: %v", err)
	}
	got := &v1alpha1.Pool{}
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pool), got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Spec.Placement.NodeSelector, selector) {
		t.Errorf("placement.nodeSelector = %v, want %v", got.Spec.Placement.NodeSelector, selector)
	}

	// flintlock_hosts and the proposal's placement.strategy have no field:
	// the API server prunes them.
	raw := &unstructured.Unstructured{Object: map[string]any{}}
	raw.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("Pool"))
	raw.SetNamespace(testNamespace)
	raw.SetName("placement-pruned")
	raw.Object["spec"] = map[string]any{
		"size": int64(1),
		"template": map[string]any{
			"vcpu": int64(1), "memoryInMb": int64(512),
			"kernel":     map[string]any{"image": "k"},
			"rootVolume": map[string]any{"containerSource": "r"},
		},
		"flintlockHosts": []any{"host-a"},
		"placement":      map[string]any{"nodeSelector": map[string]any{"a": "b"}, "strategy": "RoundRobin"},
	}
	if err := create(t, raw); err != nil {
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
}

//= docs/requirements/01-resources.md#pool
//= type=test
//# The CRDs SHALL reject a `Pool` whose `spec.size` is negative or
//# whose enumerated fields hold a value that battery v0.1.0 does not define.

func TestInvalidPoolsAreRejected(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*v1alpha1.Pool)
		want   string
	}{
		{
			name:   "negative size",
			mutate: func(p *v1alpha1.Pool) { p.Spec.Size = -1 },
			want:   "spec.size",
		},
		{
			name:   "unknown replenishment type",
			mutate: func(p *v1alpha1.Pool) { p.Spec.Replenishment.Type = "RoundRobin" },
			want:   "spec.replenishment.type",
		},
		{
			name: "unspecified replenishment type",
			mutate: func(p *v1alpha1.Pool) {
				p.Spec.Replenishment.Type = "REPLENISHMENT_STRATEGY_TYPE_UNSPECIFIED"
			},
			want: "spec.replenishment.type",
		},
		{
			name:   "unknown hook failure policy",
			mutate: func(p *v1alpha1.Pool) { p.Spec.Hooks.FailurePolicy = "Ignore" },
			want:   "spec.hooks.failurePolicy",
		},
		{
			name: "unknown network interface type",
			mutate: func(p *v1alpha1.Pool) {
				p.Spec.Template.Interfaces = []v1alpha1.NetworkInterface{{DeviceID: deviceID, Type: "Bridge"}}
			},
			want: "spec.template.interfaces[0].type",
		},
		{
			name: "MinSizeThreshold without minSize",
			mutate: func(p *v1alpha1.Pool) {
				p.Spec.Replenishment.Type = v1alpha1.ReplenishMinSizeThreshold
			},
			want: "minSize is required",
		},
		{
			name: "MinSizeThreshold with a zero minSize",
			mutate: func(p *v1alpha1.Pool) {
				p.Spec.Replenishment = v1alpha1.ReplenishmentStrategy{
					Type: v1alpha1.ReplenishMinSizeThreshold, MinSize: ptr.To[int32](0),
				}
			},
			want: "spec.replenishment.minSize",
		},
		{
			name: "zero heartbeat interval",
			mutate: func(p *v1alpha1.Pool) {
				p.Spec.Lease.HeartbeatInterval = &metav1.Duration{}
			},
			want: "heartbeatInterval must be positive",
		},
		{
			name: "negative expiry threshold",
			mutate: func(p *v1alpha1.Pool) {
				p.Spec.Lease.ExpiryThreshold = &metav1.Duration{Duration: -time.Second}
			},
			want: "expiryThreshold must be positive",
		},
		{
			name:   "template without a kernel image",
			mutate: func(p *v1alpha1.Pool) { p.Spec.Template.Kernel.Image = "" },
			want:   "spec.template.kernel.image",
		},
		{
			name:   "root volume without a source",
			mutate: func(p *v1alpha1.Pool) { p.Spec.Template.RootVolume = v1alpha1.Volume{} },
			want:   "exactly one of containerSource and virtiofsSource",
		},
		{
			name: "root volume with two sources",
			mutate: func(p *v1alpha1.Pool) {
				p.Spec.Template.RootVolume.VirtiofsSource = ptr.To("/srv")
			},
			want: "exactly one of containerSource and virtiofsSource",
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := validPool(fmt.Sprintf("invalid-%d", i))
			tc.mutate(pool)
			err := create(t, pool)
			if !apierrors.IsInvalid(err) {
				t.Fatalf("create error = %v, want Invalid", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("create error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestPoolWithoutTemplateIsRejected(t *testing.T) {
	raw := &unstructured.Unstructured{Object: map[string]any{}}
	raw.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("Pool"))
	raw.SetNamespace(testNamespace)
	raw.SetName("no-template")
	raw.Object["spec"] = map[string]any{"size": int64(1)}
	err := create(t, raw)
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "spec.template") {
		t.Errorf("create error = %v, want Invalid on spec.template", err)
	}
}

//= docs/requirements/01-resources.md#pool
//= type=test
//# The `Pool` resource SHALL have a status subresource that carries
//# `observedGeneration`, the counts of available, leased, provisioning and
//# quarantined MicroVMs, and the conditions `Ready` and `Exhausted`.

func TestPoolStatusSubresource(t *testing.T) {
	ctx := context.Background()
	pool := validPool("status")
	if err := create(t, pool); err != nil {
		t.Fatal(err)
	}

	now := metav1.Now()
	want := v1alpha1.PoolStatus{
		ObservedGeneration: pool.Generation,
		Available:          1,
		Leased:             2,
		Provisioning:       3,
		Quarantined:        4,
		Conditions: []metav1.Condition{
			{Type: v1alpha1.PoolConditionReady, Status: metav1.ConditionTrue, Reason: "AtTargetSize", LastTransitionTime: now},
			{Type: v1alpha1.PoolConditionExhausted, Status: metav1.ConditionFalse, Reason: "VMsAvailable", LastTransitionTime: now},
		},
	}
	pool.Status = want
	if err := k8sClient.Status().Update(ctx, pool); err != nil {
		t.Fatalf("updating the status subresource: %v", err)
	}

	got := &v1alpha1.Pool{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.ObservedGeneration != want.ObservedGeneration ||
		got.Status.Available != 1 || got.Status.Leased != 2 ||
		got.Status.Provisioning != 3 || got.Status.Quarantined != 4 {
		t.Errorf("status = %+v, want %+v", got.Status, want)
	}
	for _, ct := range []string{v1alpha1.PoolConditionReady, v1alpha1.PoolConditionExhausted} {
		if meta.FindStatusCondition(got.Status.Conditions, ct) == nil {
			t.Errorf("condition %s missing from status", ct)
		}
	}

	// With the subresource on, an update of the main resource leaves status
	// alone.
	got.Status.Available = 99
	got.Spec.Size = 3
	if err := k8sClient.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	after := &v1alpha1.Pool{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), after); err != nil {
		t.Fatal(err)
	}
	if after.Status.Available != 1 {
		t.Errorf("status.available = %d after a spec update, want 1", after.Status.Available)
	}
	if after.Spec.Size != 3 {
		t.Errorf("spec.size = %d, want 3", after.Spec.Size)
	}
}
