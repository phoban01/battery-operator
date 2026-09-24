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

package controller

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/protobuf/testing/protocmp"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
)

// TestPoolSpecToBattery checks each field of the mapping in
// batteryv1alpha1.PoolSpec's doc comment.
func TestPoolSpecToBattery(t *testing.T) {
	pool := testPool()
	pool.Spec.Replenishment = batteryv1alpha1.ReplenishmentStrategy{Type: batteryv1alpha1.ReplenishMinSizeThreshold, MinSize: ptrTo[int32](2)}
	pool.Spec.Hooks = batteryv1alpha1.PoolHooks{Create: []string{"setup"}, PreLease: []string{"check"}, FailurePolicy: batteryv1alpha1.HookFailureQuarantine}
	tmpl := &pool.Spec.Template
	tmpl.Provider = ptrTo("firecracker")
	tmpl.Kernel.Cmdline = map[string]string{"console": "ttyS0"}
	tmpl.Kernel.Filename = ptrTo("vmlinux")
	tmpl.Kernel.AddNetworkConfig = true
	tmpl.Initrd = &batteryv1alpha1.Initrd{Image: "initrd:1", Filename: ptrTo("initrd.img")}
	tmpl.RootVolume.ReadOnly = true
	tmpl.AdditionalVolumes = []batteryv1alpha1.Volume{{ID: "cache", MountPoint: ptrTo("/cache"), SizeInMb: ptrTo[int32](512), VirtiofsSource: ptrTo("/srv/cache")}}
	tmpl.Interfaces = []batteryv1alpha1.NetworkInterface{{
		DeviceID: "eth1", Type: batteryv1alpha1.NetworkInterfaceTap, GuestMAC: ptrTo("aa:bb:cc:dd:ee:ff"),
		Address:   &batteryv1alpha1.StaticAddress{Address: "10.0.0.2/24", Gateway: ptrTo("10.0.0.1"), Nameservers: []string{"1.1.1.1"}},
		Overrides: &batteryv1alpha1.NetworkOverrides{BridgeName: ptrTo("br0")},
	}}
	tmpl.Metadata = map[string]string{"user-data": "e30="}
	tmpl.Labels = map[string]string{"team": "ci"}
	tmpl.CPUConfig = &batteryv1alpha1.CPUConfig{FeaturesToEnable: []string{"amx"}, KVMCapabilitiesToDisable: []string{"7"}}

	want := battery.PoolSpec{
		Ref: battery.PoolRef{Name: testPoolName, Namespace: testPoolNamespace},
		Template: &types.MicroVMSpec{
			Namespace:  "ci",
			Labels:     map[string]string{"team": "ci"},
			Vcpu:       2,
			MemoryInMb: 2048,
			Kernel:     &types.Kernel{Image: "ghcr.io/example/kernel:6.1", Cmdline: map[string]string{"console": "ttyS0"}, Filename: ptrTo("vmlinux"), AddNetworkConfig: true},
			Initrd:     &types.Initrd{Image: "initrd:1", Filename: ptrTo("initrd.img")},
			RootVolume: &types.Volume{Id: "root", IsReadOnly: true, Source: &types.VolumeSource{ContainerSource: ptrTo("ghcr.io/example/root:24.04")}},
			AdditionalVolumes: []*types.Volume{{
				Id: "cache", MountPoint: ptrTo("/cache"), SizeInMb: ptrTo[int32](512),
				Source: &types.VolumeSource{VirtiofsSource: ptrTo("/srv/cache")},
			}},
			Interfaces: []*types.NetworkInterface{{
				DeviceId: "eth1", Type: types.NetworkInterface_TAP, GuestMac: ptrTo("aa:bb:cc:dd:ee:ff"),
				Address:   &types.StaticAddress{Address: "10.0.0.2/24", Gateway: ptrTo("10.0.0.1"), Nameservers: []string{"1.1.1.1"}},
				Overrides: &types.NetworkOverrides{BridgeName: ptrTo("br0")},
			}},
			Metadata:  map[string]string{"user-data": "e30="},
			Provider:  ptrTo("firecracker"),
			CpuConfig: &types.CPUConfig{FeaturesToEnable: []string{"amx"}, KvmCapabilitiesToDisable: []string{"7"}},
		},
		Size:                     3,
		FlintlockHosts:           []string{},
		Replenishment:            battery.ReplenishmentStrategy{Type: poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD, MinSize: ptrTo[int32](2)},
		CreateCommands:           []string{"setup"},
		PreLeaseCommands:         []string{"check"},
		HookFailurePolicy:        poolmgrv1.HookFailurePolicy_QUARANTINE,
		HeartbeatInterval:        10 * time.Second,
		HeartbeatExpiryThreshold: 30 * time.Second,
	}
	if diff := cmp.Diff(want, poolSpecToBattery(pool), protocmp.Transform()); diff != "" {
		t.Errorf("poolSpecToBattery (-want +got):\n%s", diff)
	}
}

// TestPoolEnumsWithoutCounterpartAreUnspecified: a value the CRD does not
// define becomes battery's UNSPECIFIED, which battery refuses.
func TestPoolEnumsWithoutCounterpartAreUnspecified(t *testing.T) {
	pool := testPool()
	pool.Spec.Replenishment.Type = "Sometimes"
	pool.Spec.Hooks.FailurePolicy = ""
	got := poolSpecToBattery(pool)
	if got.Replenishment.Type != poolmgrv1.ReplenishmentStrategyType_REPLENISHMENT_STRATEGY_TYPE_UNSPECIFIED ||
		got.HookFailurePolicy != poolmgrv1.HookFailurePolicy_HOOK_FAILURE_POLICY_UNSPECIFIED {
		t.Errorf("enums = %v, %v; want UNSPECIFIED", got.Replenishment.Type, got.HookFailurePolicy)
	}
}
