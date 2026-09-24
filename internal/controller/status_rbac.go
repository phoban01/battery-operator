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

//= docs/requirements/06-deployment.md#access
//# The Manifests SHALL grant write access to the status
//# subresources of `Pool` and `MicroVMClaim` to the Operator's identity and
//# to no other identity they create.

// The Operator alone writes the status of Pools and MicroVMClaims: this
// grant is in the manager-role, bound to the Operator's ServiceAccount
// only, and the Manifests grant these subresources to no other identity.
// +kubebuilder:rbac:groups=battery.liquidmetal-x.dev,resources=pools/status;microvmclaims/status,verbs=get;update;patch
