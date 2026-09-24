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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

//= docs/requirements/01-resources.md#pool
//# The `Pool` resource SHALL carry in its `spec` one field for
//# each of the `PoolSpec` fields `microvm_template`, `size`,
//# `replenishment_strategy`, `create_commands`, `pre_lease_commands`,
//# `hook_failure_policy`, `heartbeat_interval` and
//# `heartbeat_expiry_threshold` of battery v0.1.0.

// PoolSpec is battery v0.3.3's PoolSpec
// (api/proto/poolmgr/v1alpha1/types.proto in github.com/liquidmetal-dev/battery)
// as the spec of a namespaced resource. Each PoolSpec field maps to a Pool as
// follows:
//
//	PoolSpec field                   Pool field
//	-------------------------------  ---------------------------------------
//	name                             metadata.name
//	namespace                        metadata.namespace
//	microvm_template                 spec.template (see MicroVMTemplate)
//	size                             spec.size
//	flintlock_hosts                  spec.placement.nodeSelector, resolved
//	replenishment_strategy.type      spec.replenishment.type
//	replenishment_strategy.min_size  spec.replenishment.minSize
//	create_commands                  spec.hooks.create
//	pre_lease_commands               spec.hooks.preLease
//	hook_failure_policy              spec.hooks.failurePolicy
//	heartbeat_interval               spec.lease.heartbeatInterval
//	heartbeat_expiry_threshold       spec.lease.expiryThreshold
//
// The enumerations map one to one. Their UNSPECIFIED zero values, which
// battery refuses, have no counterpart:
//
//	ReplenishmentStrategyType  spec.replenishment.type
//	IMMEDIATE_ON_LEASE         ImmediateOnLease
//	MIN_SIZE_THRESHOLD         MinSizeThreshold
//	REPLACE_ON_DELETE          ReplaceOnDelete
//
//	HookFailurePolicy          spec.hooks.failurePolicy
//	DELETE_AND_REPLACE         DeleteAndReplace
//	QUARANTINE                 Quarantine
//
// battery's PoolStatus maps to status.available (available_count),
// status.leased (leased_count), status.provisioning (provisioning_count) and
// status.quarantined (quarantined_count).
//
// Left out, and why:
//
//   - flintlock_hosts: a Pool selects Nodes by label instead, and the Pool
//     Controller resolves the selector to Host names.
//   - placement.strategy, from the proposal on battery#46: v0.3.3's PoolSpec
//     has no such field, and battery places a MicroVM on the Host with the
//     fewest MicroVMs.
//   - MicroVMSpec's id, namespace, uid, created_at, updated_at and
//     deleted_at: they identify one MicroVM rather than describe a template.
//     battery v0.3.3 gives each MicroVM its own id, the Pool's name and a
//     random suffix, and the Pool's namespace unless the template has one;
//     flintlock sets the rest.
//   - MicroVMSpec's allow_guest_agent: battery forces it true for every
//     MicroVM in a pool.
//
// What battery refuses in CreatePool and UpdatePool, the CRD refuses at
// admission: a replenishment type or hook failure policy battery does not
// define, a MinSizeThreshold strategy without a positive minSize, and a
// missing template. battery also refuses a missing heartbeat interval or
// expiry threshold; here both are defaulted, and have to be positive. A
// negative size is refused as well.
type PoolSpec struct {
	// template is the flintlock MicroVM spec every MicroVM in the Pool is
	// created from (PoolSpec.microvm_template).
	// +required
	Template MicroVMTemplate `json:"template"`

	//= docs/requirements/01-resources.md#pool
	//# The CRDs SHALL reject a `Pool` whose `spec.size` is negative or
	//# whose enumerated fields hold a value that battery v0.1.0 does not define.

	// size is the number of MicroVMs the Pool keeps (PoolSpec.size).
	// +required
	// +kubebuilder:validation:Minimum=0
	Size int32 `json:"size"`

	// placement says which Hosts the Pool's MicroVMs may run on.
	// +optional
	Placement PoolPlacement `json:"placement,omitempty"`

	// replenishment is how the Pool tops itself up
	// (PoolSpec.replenishment_strategy).
	// +optional
	// +kubebuilder:default={}
	Replenishment ReplenishmentStrategy `json:"replenishment,omitempty"`

	// hooks are the commands run in each MicroVM, and what happens when they
	// fail.
	// +optional
	// +kubebuilder:default={}
	Hooks PoolHooks `json:"hooks,omitempty"`

	// lease sets how often a Holder renews its Lease, and how long a Lease
	// lives without a renewal.
	// +optional
	// +kubebuilder:default={}
	Lease PoolLease `json:"lease,omitempty"`
}

// PoolPlacement selects the Hosts a Pool's MicroVMs may run on.
type PoolPlacement struct {
	//= docs/requirements/01-resources.md#pool
	//# The `Pool` resource SHALL select the Hosts its MicroVMs may run
	//# on with a Node label selector in `spec.placement.nodeSelector`, in place of
	//# battery's `flintlock_hosts`.

	// nodeSelector selects, by label, the Nodes whose Hosts may run the
	// Pool's MicroVMs, as a Pod's nodeSelector does. It replaces
	// PoolSpec.flintlock_hosts. Empty selects every Host.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
}

// ReplenishmentStrategyType names how a Pool tops itself up.
// +kubebuilder:validation:Enum=ImmediateOnLease;MinSizeThreshold;ReplaceOnDelete
type ReplenishmentStrategyType string

const (
	// ReplenishImmediateOnLease starts one new MicroVM on every claim
	// (IMMEDIATE_ON_LEASE).
	ReplenishImmediateOnLease ReplenishmentStrategyType = "ImmediateOnLease"
	// ReplenishMinSizeThreshold tops the Pool up to its size whenever fewer
	// than minSize MicroVMs are available (MIN_SIZE_THRESHOLD).
	ReplenishMinSizeThreshold ReplenishmentStrategyType = "MinSizeThreshold"
	// ReplenishReplaceOnDelete starts one new MicroVM for every one deleted
	// (REPLACE_ON_DELETE).
	ReplenishReplaceOnDelete ReplenishmentStrategyType = "ReplaceOnDelete"
)

// ReplenishmentStrategy is battery's ReplenishmentStrategy.
// +kubebuilder:validation:XValidation:rule="!has(self.type) || self.type != 'MinSizeThreshold' || has(self.minSize)",message="minSize is required when type is MinSizeThreshold"
type ReplenishmentStrategy struct {
	// type is the strategy.
	// +optional
	// +kubebuilder:default=ImmediateOnLease
	Type ReplenishmentStrategyType `json:"type,omitempty"`

	// minSize is the available count below which a MinSizeThreshold Pool
	// tops itself up. battery reads it only for MinSizeThreshold.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MinSize *int32 `json:"minSize,omitempty"`
}

// HookFailurePolicy decides what happens to a MicroVM whose create or
// pre-lease hook fails.
// +kubebuilder:validation:Enum=DeleteAndReplace;Quarantine
type HookFailurePolicy string

const (
	// HookFailureDeleteAndReplace deletes the MicroVM and lets replenishment
	// replace it (DELETE_AND_REPLACE).
	HookFailureDeleteAndReplace HookFailurePolicy = "DeleteAndReplace"
	// HookFailureQuarantine keeps the MicroVM, marked unhealthy, for
	// inspection (QUARANTINE).
	HookFailureQuarantine HookFailurePolicy = "Quarantine"
)

// PoolHooks are the commands battery runs in each of a Pool's MicroVMs.
type PoolHooks struct {
	// create runs once in each MicroVM, after it boots and its guest agent
	// is ready (PoolSpec.create_commands).
	// +optional
	// +listType=atomic
	Create []string `json:"create,omitempty"`

	// preLease runs in a MicroVM just before it is handed to a Holder
	// (PoolSpec.pre_lease_commands).
	// +optional
	// +listType=atomic
	PreLease []string `json:"preLease,omitempty"`

	// failurePolicy decides what happens to a MicroVM whose hook fails
	// (PoolSpec.hook_failure_policy).
	// +optional
	// +kubebuilder:default=DeleteAndReplace
	FailurePolicy HookFailurePolicy `json:"failurePolicy,omitempty"`
}

// PoolLease holds the heartbeat timings of a Pool's Leases.
type PoolLease struct {
	// heartbeatInterval is how often a Holder is expected to renew its Lease
	// (PoolSpec.heartbeat_interval).
	// +optional
	// +kubebuilder:default="10s"
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="heartbeatInterval must be positive"
	HeartbeatInterval *metav1.Duration `json:"heartbeatInterval,omitempty"`

	// expiryThreshold is how long a Lease lives without a renewal
	// (PoolSpec.heartbeat_expiry_threshold).
	// +optional
	// +kubebuilder:default="30s"
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="expiryThreshold must be positive"
	ExpiryThreshold *metav1.Duration `json:"expiryThreshold,omitempty"`
}

// MicroVMTemplate is flintlock's MicroVMSpec without the fields that identify
// one MicroVM (id, namespace, uid and the timestamps) and without
// allow_guest_agent, which battery forces true.
type MicroVMTemplate struct {
	// provider names the flintlock MicroVM provider. Empty uses flintlock's
	// default.
	// +optional
	Provider *string `json:"provider,omitempty"`

	// vcpu is the number of virtual CPUs.
	// +required
	// +kubebuilder:validation:Minimum=1
	VCPU int32 `json:"vcpu"`

	// memoryInMb is the memory, in megabytes.
	// +required
	// +kubebuilder:validation:Minimum=1
	MemoryInMb int32 `json:"memoryInMb"`

	// kernel is the kernel to boot.
	// +required
	Kernel Kernel `json:"kernel"`

	// initrd is the initial ramdisk, if any.
	// +optional
	Initrd *Initrd `json:"initrd,omitempty"`

	// rootVolume is the MicroVM's root volume.
	// +required
	RootVolume Volume `json:"rootVolume"`

	// additionalVolumes are attached after the root volume.
	// +optional
	// +listType=atomic
	AdditionalVolumes []Volume `json:"additionalVolumes,omitempty"`

	// interfaces are the network interfaces, which appear in the guest as
	// eth1, eth2 and so on, in order.
	// +optional
	// +listType=atomic
	Interfaces []NetworkInterface `json:"interfaces,omitempty"`

	// metadata is served by the metadata service, for cloud-init. Each
	// value is base64 encoded.
	// +optional
	Metadata map[string]string `json:"metadata,omitempty"`

	// labels are flintlock's labels on each MicroVM.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// cpuConfig sets the CPU features presented to the guest.
	// +optional
	CPUConfig *CPUConfig `json:"cpuConfig,omitempty"`
}

// Kernel is flintlock's Kernel.
type Kernel struct {
	// image is the container image holding the kernel.
	// +required
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// cmdline adds kernel command line arguments to the provider's own.
	// +optional
	Cmdline map[string]string `json:"cmdline,omitempty"`

	// filename is the kernel's path in the image.
	// +optional
	Filename *string `json:"filename,omitempty"`

	// addNetworkConfig generates the network-config kernel argument.
	// +optional
	AddNetworkConfig bool `json:"addNetworkConfig,omitempty"`
}

// Initrd is flintlock's Initrd.
type Initrd struct {
	// image is the container image holding the initial ramdisk.
	// +required
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// filename is the ramdisk's path in the image.
	// +optional
	Filename *string `json:"filename,omitempty"`
}

// Volume is flintlock's Volume, with its VolumeSource inlined.
// +kubebuilder:validation:XValidation:rule="has(self.containerSource) != has(self.virtiofsSource)",message="exactly one of containerSource and virtiofsSource is required"
type Volume struct {
	// id identifies the volume.
	// +optional
	ID string `json:"id,omitempty"`

	// readOnly mounts the volume read-only (flintlock's is_read_only).
	// +optional
	ReadOnly bool `json:"readOnly,omitempty"`

	// mountPoint is where cloud-init mounts an additional volume.
	// +optional
	MountPoint *string `json:"mountPoint,omitempty"`

	// partitionId is the uuid of the boot partition.
	// +optional
	PartitionID *string `json:"partitionId,omitempty"`

	// sizeInMb resizes the volume.
	// +optional
	// +kubebuilder:validation:Minimum=1
	SizeInMb *int32 `json:"sizeInMb,omitempty"`

	// containerSource is a container image to take the volume from.
	// +optional
	ContainerSource *string `json:"containerSource,omitempty"`

	// virtiofsSource is a Host path to pass through with virtiofs.
	// +optional
	VirtiofsSource *string `json:"virtiofsSource,omitempty"`
}

// NetworkInterfaceType is flintlock's NetworkInterface.IfaceType.
// +kubebuilder:validation:Enum=Macvtap;Tap
type NetworkInterfaceType string

const (
	// NetworkInterfaceMacvtap is a macvtap interface (MACVTAP).
	NetworkInterfaceMacvtap NetworkInterfaceType = "Macvtap"
	// NetworkInterfaceTap is a tap interface (TAP).
	NetworkInterfaceTap NetworkInterfaceType = "Tap"
)

// NetworkInterface is flintlock's NetworkInterface.
type NetworkInterface struct {
	// deviceId names the interface. Without guestMac it is also the
	// interface's name in the guest. flintlock reserves eth0.
	// +required
	// +kubebuilder:validation:MinLength=1
	DeviceID string `json:"deviceId"`

	// type is the kind of interface. It defaults to Macvtap, flintlock's
	// zero value.
	// +optional
	// +kubebuilder:default=Macvtap
	Type NetworkInterfaceType `json:"type,omitempty"`

	// guestMac is the interface's MAC address. Empty generates one.
	// +optional
	GuestMAC *string `json:"guestMac,omitempty"`

	// address is a static address. Empty uses DHCP.
	// +optional
	Address *StaticAddress `json:"address,omitempty"`

	// overrides override flintlock's network settings for this interface.
	// +optional
	Overrides *NetworkOverrides `json:"overrides,omitempty"`
}

// StaticAddress is flintlock's StaticAddress.
type StaticAddress struct {
	// address is an IPv4 or IPv6 address in CIDR notation.
	// +required
	// +kubebuilder:validation:MinLength=1
	Address string `json:"address"`

	// gateway is the default gateway.
	// +optional
	Gateway *string `json:"gateway,omitempty"`

	// nameservers are the interface's nameservers.
	// +optional
	// +listType=atomic
	Nameservers []string `json:"nameservers,omitempty"`
}

// NetworkOverrides is flintlock's NetworkOverrides.
type NetworkOverrides struct {
	// bridgeName is the Linux bridge to attach a tap interface to.
	// +optional
	BridgeName *string `json:"bridgeName,omitempty"`
}

// CPUConfig is flintlock's CPUConfig.
type CPUConfig struct {
	// featuresToEnable are CPU features to enable. Their names depend on the
	// provider.
	// +optional
	// +listType=atomic
	FeaturesToEnable []string `json:"featuresToEnable,omitempty"`

	// kvmCapabilitiesToDisable are KVM capability ids to disable, where the
	// provider supports it.
	// +optional
	// +listType=atomic
	KVMCapabilitiesToDisable []string `json:"kvmCapabilitiesToDisable,omitempty"`
}

// The condition types of a Pool.
const (
	// PoolConditionReady is true while battery holds the Pool at its size.
	PoolConditionReady = "Ready"
	// PoolConditionExhausted is true while the Pool has no available
	// MicroVM.
	PoolConditionExhausted = "Exhausted"
)

//= docs/requirements/01-resources.md#pool
//# The `Pool` resource SHALL have a status subresource that carries
//# `observedGeneration`, the counts of available, leased, provisioning and
//# quarantined MicroVMs, and the conditions `Ready` and `Exhausted`.

// PoolStatus is the Pool as battery reports it.
type PoolStatus struct {
	// observedGeneration is the generation battery last accepted.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// available is the number of MicroVMs ready to claim
	// (PoolStatus.available_count).
	// +optional
	Available int32 `json:"available,omitempty"`

	// leased is the number of claimed MicroVMs (PoolStatus.leased_count).
	// +optional
	Leased int32 `json:"leased,omitempty"`

	// provisioning is the number of MicroVMs being created
	// (PoolStatus.provisioning_count).
	// +optional
	Provisioning int32 `json:"provisioning,omitempty"`

	// quarantined is the number of MicroVMs kept after a hook failed
	// (PoolStatus.quarantined_count).
	// +optional
	Quarantined int32 `json:"quarantined,omitempty"`

	// conditions are Ready and Exhausted.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//= docs/requirements/01-resources.md#api-group
//# The CRDs SHALL define `Pool` and `MicroVMClaim` as namespaced
//# resources in the API group `battery.liquidmetal-x.dev` at version
//# `v1alpha1`.

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Size",type=integer,JSONPath=`.spec.size`
// +kubebuilder:printcolumn:name="Available",type=integer,JSONPath=`.status.available`
// +kubebuilder:printcolumn:name="Leased",type=integer,JSONPath=`.status.leased`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Pool is a warm pool of flintlock MicroVMs that battery keeps and leases
// out. PoolSpec says how it maps to battery's own PoolSpec.
type Pool struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata. Its name and namespace are the
	// Pool's name and namespace in battery.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Pool
	// +required
	Spec PoolSpec `json:"spec"`

	// status defines the observed state of Pool
	// +optional
	Status PoolStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// PoolList contains a list of Pool
type PoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Pool `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Pool{}, &PoolList{})
		return nil
	})
}
