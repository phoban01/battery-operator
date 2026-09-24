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

// Package manifests holds the tests of the Manifests as kustomize renders
// them: config/default, the Operator with battery as its sidecar
// (docs/requirements/06-deployment.md), and config/exec-agent. The tests
// run `kustomize build` ($KUSTOMIZE, or bin/kustomize, which `make test`
// installs) and parse its output strictly, with no cluster: applying the
// Manifests to one is the e2e suite's job.
package manifests
