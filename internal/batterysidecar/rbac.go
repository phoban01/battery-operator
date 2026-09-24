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

package batterysidecar

//= docs/requirements/06-deployment.md#battery-sidecar
//# The Manifests SHALL supply battery's configuration, its Hosts
//# included, from an object the Operator may write.

// The Operator reads and updates battery's ConfigMap, by name, in its own
// namespace (config/manager's battery-config, after kustomize's
// namePrefix). The namespace is config's placeholder, which kustomize
// replaces.
// +kubebuilder:rbac:groups="",namespace=system,resources=configmaps,verbs=get;list;watch;update;patch,resourceNames=battery-operator-battery-config
