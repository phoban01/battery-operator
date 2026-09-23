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

// ReleaseFinalizer is the finalizer the Claim Controller puts on a
// MicroVMClaim. It keeps the claim until battery has released its Lease and
// deleted its MicroVM.
const ReleaseFinalizer = "battery.liquidmetal-x.dev/release"

// MicroVMClaimPhase is where a MicroVMClaim is in its life.
// +kubebuilder:validation:Enum=Pending;Bound;Expired
type MicroVMClaimPhase string

const (
	// MicroVMClaimPending is a claim that has no Lease yet: it waits for its
	// Pool to have a MicroVM to give.
	MicroVMClaimPending MicroVMClaimPhase = "Pending"
	// MicroVMClaimBound is a claim that holds a Lease on a MicroVM.
	MicroVMClaimBound MicroVMClaimPhase = "Bound"
	// MicroVMClaimExpired is a claim whose Lease lapsed because it was not
	// renewed in time. battery deletes the MicroVM.
	MicroVMClaimExpired MicroVMClaimPhase = "Expired"
)

// ConditionBound is the condition type that says whether a MicroVMClaim
// holds a Lease on a MicroVM.
const ConditionBound = "Bound"

// Reasons for the Bound condition.
const (
	// ReasonBound means the claim holds a Lease.
	ReasonBound = "Bound"
	// ReasonPoolExhausted means the Pool has no available MicroVM to give;
	// the claim stays Pending and binds once one is available.
	ReasonPoolExhausted = "PoolExhausted"
	// ReasonPoolNotFound means the Pool the claim names does not exist.
	ReasonPoolNotFound = "PoolNotFound"
	// ReasonNoEligibleHost means no Host can run a MicroVM for the Pool.
	ReasonNoEligibleHost = "NoEligibleHost"
	// ReasonLeaseExpired means the Holder stopped renewing and the Lease
	// lapsed.
	ReasonLeaseExpired = "LeaseExpired"
)

// PoolReference names a Pool in the claim's own namespace.
type PoolReference struct {
	// name is the name of the Pool.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// MicroVMClaimSpec is what a consumer asks for: a MicroVM from a Pool, for a
// ServiceAccount.
type MicroVMClaimSpec struct {
	//= docs/requirements/01-resources.md#microvmclaim
	//# The `MicroVMClaim` resource SHALL carry `spec.poolRef.name`,
	//# which names a `Pool` in the claim's own namespace.

	//= docs/requirements/01-resources.md#microvmclaim
	//# The CRDs SHALL reject an update that changes a claim's
	//# `spec.serviceAccountName` or `spec.poolRef`.

	// poolRef names the Pool, in the claim's namespace, to claim a MicroVM
	// from. It is immutable.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.poolRef is immutable"
	PoolRef PoolReference `json:"poolRef"`

	//= docs/requirements/01-resources.md#microvmclaim
	//# The `MicroVMClaim` resource SHALL carry `spec.renewTime`, which
	//# the Holder sets to renew the claim's Lease.

	// renewTime is the last time the Holder renewed the claim's Lease. The
	// Holder patches it, as the holder of a coordination.k8s.io Lease does,
	// and the Claim Controller turns each change into a battery Heartbeat.
	// +optional
	RenewTime *metav1.MicroTime `json:"renewTime,omitempty"`

	//= docs/requirements/01-resources.md#microvmclaim
	//# The `MicroVMClaim` resource SHALL carry
	//# `spec.serviceAccountName`, which names the ServiceAccount in the claim's
	//# namespace that may use the claimed MicroVM.

	//= docs/requirements/01-resources.md#microvmclaim
	//# The CRDs SHALL reject an update that changes a claim's
	//# `spec.serviceAccountName` or `spec.poolRef`.

	// serviceAccountName names the ServiceAccount, in the claim's namespace,
	// that may use the claimed MicroVM: the Holder. It is immutable, so a
	// claimed MicroVM can never be handed to another account.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.serviceAccountName is immutable"
	ServiceAccountName string `json:"serviceAccountName"`
}

// MicroVMReference identifies the claimed MicroVM.
type MicroVMReference struct {
	// uid is the MicroVM's uid in battery and flintlock.
	// +required
	UID string `json:"uid"`
}

// HostReference identifies the Host the claimed MicroVM runs on, and where
// its Exec Agent listens.
type HostReference struct {
	// nodeName is the name of the Host's Node.
	// +optional
	NodeName string `json:"nodeName,omitempty"`

	// agentAddress is the host:port of the Exec Agent on the Host.
	// +optional
	AgentAddress string `json:"agentAddress,omitempty"`
}

//= docs/requirements/01-resources.md#microvmclaim
//# The `MicroVMClaim` resource SHALL have a status subresource that
//# carries the phase, one of `Pending`, `Bound` and `Expired`,
//# the lease id battery chose, the MicroVM's uid, the Host's node name, the
//# Exec Agent's address, the time the claim was bound, the time its Lease
//# expires, and the condition `Bound`.

// MicroVMClaimStatus mirrors battery's answer to the claim. Only the Claim
// Controller writes it.
type MicroVMClaimStatus struct {
	// phase is where the claim is in its life.
	// +optional
	Phase MicroVMClaimPhase `json:"phase,omitempty"`

	// leaseID is the id of the claim's Lease, which battery chose when the
	// claim was bound.
	// +optional
	LeaseID string `json:"leaseID,omitempty"`

	// microVM is the claimed MicroVM.
	// +optional
	MicroVM *MicroVMReference `json:"microVM,omitempty"`

	// host is the Host the claimed MicroVM runs on.
	// +optional
	Host *HostReference `json:"host,omitempty"`

	// boundTime is when the claim was bound.
	// +optional
	BoundTime *metav1.Time `json:"boundTime,omitempty"`

	// leaseExpiresAt is when the claim's Lease expires unless the Holder
	// renews it. It is battery's expiry, not one the Operator works out.
	// +optional
	LeaseExpiresAt *metav1.Time `json:"leaseExpiresAt,omitempty"`

	// conditions hold the claim's Bound condition.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.status.host.nodeName`
// +kubebuilder:printcolumn:name="Expires",type=date,JSONPath=`.status.leaseExpiresAt`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// MicroVMClaim is one consumer's hold on one warm MicroVM from a Pool:
// battery's ClaimVM, Heartbeat and ReleaseVM as a resource. Creating it
// claims a MicroVM, patching spec.renewTime renews the Lease, and deleting it
// releases the MicroVM.
//
// The proposal on battery#46 made the claim's name its lease id. battery
// v0.1.0 chooses the lease id itself in ClaimVM, so the id is recorded in
// status.leaseID instead (ADR 0001, consequence 2).
type MicroVMClaim struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec is the claim the consumer makes.
	// +required
	Spec MicroVMClaimSpec `json:"spec"`

	// status is battery's answer to the claim, which only the Claim
	// Controller writes.
	// +optional
	Status MicroVMClaimStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// MicroVMClaimList contains a list of MicroVMClaim
type MicroVMClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []MicroVMClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &MicroVMClaim{}, &MicroVMClaimList{})
		return nil
	})
}
