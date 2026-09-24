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
	"maps"
	"slices"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
)

// poolRef names a Pool in battery: the Pool's own namespace and name.
func poolRef(pool *batteryv1alpha1.Pool) battery.PoolRef {
	return battery.PoolRef{Name: pool.Name, Namespace: pool.Namespace}
}

// poolSpecToBattery converts a Pool to battery v0.1.0's PoolSpec, field by
// field as the doc comment of batteryv1alpha1.PoolSpec maps them.
//
// Two fields have no counterpart in the Pool's spec:
//
//   - The template's flintlock namespace is the Pool's own namespace, so
//     that a MicroVM in flintlock shows which Pool's namespace it serves.
//     Its id stays empty: flintlock generates one per MicroVM.
//   - flintlock_hosts is left empty. Resolving spec.placement.nodeSelector
//     to Host names is #20's job (PO-010). battery v0.1.0 does not validate
//     flintlock_hosts, so an empty list is a valid PoolSpec: battery holds
//     the Pool and places no MicroVM until #20 fills the list in.
//
// An enumeration value the CRD does not define becomes battery's
// UNSPECIFIED, which battery refuses (PO-004).
func poolSpecToBattery(pool *batteryv1alpha1.Pool) battery.PoolSpec {
	spec := pool.Spec
	out := battery.PoolSpec{
		Ref:      poolRef(pool),
		Template: templateToFlintlock(spec.Template, pool.Namespace),
		Size:     spec.Size,
		// TODO(#20): the names of the Hosts that spec.placement.nodeSelector
		// selects (PO-010). Empty is valid in battery v0.1.0.
		FlintlockHosts: []string{},
		Replenishment: battery.ReplenishmentStrategy{
			Type:    replenishmentToBattery(spec.Replenishment.Type),
			MinSize: clonePtr(spec.Replenishment.MinSize),
		},
		CreateCommands:    slices.Clone(spec.Hooks.Create),
		PreLeaseCommands:  slices.Clone(spec.Hooks.PreLease),
		HookFailurePolicy: hookFailurePolicyToBattery(spec.Hooks.FailurePolicy),
	}
	if d := spec.Lease.HeartbeatInterval; d != nil {
		out.HeartbeatInterval = d.Duration
	}
	if d := spec.Lease.ExpiryThreshold; d != nil {
		out.HeartbeatExpiryThreshold = d.Duration
	}
	return out
}

func replenishmentToBattery(t batteryv1alpha1.ReplenishmentStrategyType) battery.ReplenishmentStrategyType {
	switch t {
	case batteryv1alpha1.ReplenishImmediateOnLease:
		return poolmgrv1.ReplenishmentStrategyType_IMMEDIATE_ON_LEASE
	case batteryv1alpha1.ReplenishMinSizeThreshold:
		return poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD
	case batteryv1alpha1.ReplenishReplaceOnDelete:
		return poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE
	default:
		return poolmgrv1.ReplenishmentStrategyType_REPLENISHMENT_STRATEGY_TYPE_UNSPECIFIED
	}
}

func hookFailurePolicyToBattery(p batteryv1alpha1.HookFailurePolicy) battery.HookFailurePolicy {
	switch p {
	case batteryv1alpha1.HookFailureDeleteAndReplace:
		return poolmgrv1.HookFailurePolicy_DELETE_AND_REPLACE
	case batteryv1alpha1.HookFailureQuarantine:
		return poolmgrv1.HookFailurePolicy_QUARANTINE
	default:
		return poolmgrv1.HookFailurePolicy_HOOK_FAILURE_POLICY_UNSPECIFIED
	}
}

// templateToFlintlock converts a MicroVMTemplate to flintlock's MicroVMSpec
// in the given flintlock namespace. allow_guest_agent is left for battery,
// which forces it true.
func templateToFlintlock(t batteryv1alpha1.MicroVMTemplate, namespace string) *types.MicroVMSpec {
	out := &types.MicroVMSpec{
		Namespace:  namespace,
		Labels:     maps.Clone(t.Labels),
		Vcpu:       t.VCPU,
		MemoryInMb: t.MemoryInMb,
		Kernel: &types.Kernel{
			Image:            t.Kernel.Image,
			Cmdline:          maps.Clone(t.Kernel.Cmdline),
			Filename:         clonePtr(t.Kernel.Filename),
			AddNetworkConfig: t.Kernel.AddNetworkConfig,
		},
		RootVolume: volumeToFlintlock(t.RootVolume),
		Metadata:   maps.Clone(t.Metadata),
		Provider:   clonePtr(t.Provider),
	}
	if t.Initrd != nil {
		out.Initrd = &types.Initrd{Image: t.Initrd.Image, Filename: clonePtr(t.Initrd.Filename)}
	}
	for _, v := range t.AdditionalVolumes {
		out.AdditionalVolumes = append(out.AdditionalVolumes, volumeToFlintlock(v))
	}
	for _, iface := range t.Interfaces {
		out.Interfaces = append(out.Interfaces, interfaceToFlintlock(iface))
	}
	if c := t.CPUConfig; c != nil {
		out.CpuConfig = &types.CPUConfig{
			FeaturesToEnable:         slices.Clone(c.FeaturesToEnable),
			KvmCapabilitiesToDisable: slices.Clone(c.KVMCapabilitiesToDisable),
		}
	}
	return out
}

func volumeToFlintlock(v batteryv1alpha1.Volume) *types.Volume {
	return &types.Volume{
		Id:          v.ID,
		IsReadOnly:  v.ReadOnly,
		MountPoint:  clonePtr(v.MountPoint),
		PartitionId: clonePtr(v.PartitionID),
		SizeInMb:    clonePtr(v.SizeInMb),
		Source: &types.VolumeSource{
			ContainerSource: clonePtr(v.ContainerSource),
			VirtiofsSource:  clonePtr(v.VirtiofsSource),
		},
	}
}

func interfaceToFlintlock(n batteryv1alpha1.NetworkInterface) *types.NetworkInterface {
	out := &types.NetworkInterface{
		DeviceId: n.DeviceID,
		Type:     types.NetworkInterface_MACVTAP,
		GuestMac: clonePtr(n.GuestMAC),
	}
	if n.Type == batteryv1alpha1.NetworkInterfaceTap {
		out.Type = types.NetworkInterface_TAP
	}
	if a := n.Address; a != nil {
		out.Address = &types.StaticAddress{
			Address:     a.Address,
			Gateway:     clonePtr(a.Gateway),
			Nameservers: slices.Clone(a.Nameservers),
		}
	}
	if o := n.Overrides; o != nil {
		out.Overrides = &types.NetworkOverrides{BridgeName: clonePtr(o.BridgeName)}
	}
	return out
}

// clonePtr copies the value p points to, so that the converted spec shares
// no memory with the Pool.
func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
