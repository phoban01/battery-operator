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
	"os"
	"path/filepath"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	"github.com/phoban01/battery-operator/api/v1alpha1"
)

// The tests in this package read the CRDs as controller-gen writes them to
// config/crd/bases, and the scheme the Operator registers. What the API
// server enforces from those CRDs (their schemas, CEL rules, defaults,
// pruning and the status subresource at work) is tested against the kind
// cluster, in test/e2e/crd_validation_test.go.

const crdDir = "../../config/crd/bases"

// crds reads every CRD in config/crd/bases, by kind.
func crds(t *testing.T) map[string]*apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(crdDir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no CRDs in %s; run make manifests", crdDir)
	}
	byKind := map[string]*apiextensionsv1.CustomResourceDefinition{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.UnmarshalStrict(b, crd); err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		byKind[crd.Spec.Names.Kind] = crd
	}
	return byKind
}

// version returns crd's version name, failing t if it has none.
func version(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition, name string) *apiextensionsv1.CustomResourceDefinitionVersion {
	t.Helper()
	for i := range crd.Spec.Versions {
		if crd.Spec.Versions[i].Name == name {
			return &crd.Spec.Versions[i]
		}
	}
	t.Fatalf("the %s CRD has no version %s", crd.Spec.Names.Kind, name)
	return nil
}

func TestCRDsDefineNamespacedResourcesInTheBatteryGroup(t *testing.T) {
	//= docs/requirements/01-resources.md#api-group
	//= type=test
	//# The CRDs SHALL define `Pool` and `MicroVMClaim` as namespaced
	//# resources in the API group `battery.liquidmetal-x.dev` at version
	//# `v1alpha1`.

	all := crds(t)
	for kind, plural := range map[string]string{"Pool": "pools", "MicroVMClaim": "microvmclaims"} {
		t.Run(kind, func(t *testing.T) {
			crd, ok := all[kind]
			if !ok {
				t.Fatalf("no CRD defines %s", kind)
			}
			if crd.Spec.Group != "battery.liquidmetal-x.dev" {
				t.Errorf("group = %q, want battery.liquidmetal-x.dev", crd.Spec.Group)
			}
			if crd.Spec.Scope != apiextensionsv1.NamespaceScoped {
				t.Errorf("scope = %q, want Namespaced", crd.Spec.Scope)
			}
			if crd.Spec.Names.Plural != plural {
				t.Errorf("plural = %q, want %q", crd.Spec.Names.Plural, plural)
			}
			v := version(t, crd, "v1alpha1")
			if !v.Served || !v.Storage {
				t.Errorf("v1alpha1 served=%t storage=%t, want both", v.Served, v.Storage)
			}

			scheme := runtime.NewScheme()
			if err := v1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if !scheme.Recognizes(v1alpha1.GroupVersion.WithKind(kind)) {
				t.Errorf("the scheme does not register %s in %s", kind, v1alpha1.GroupVersion)
			}
		})
	}
}

func TestNoMicroVMResource(t *testing.T) {
	//= docs/requirements/01-resources.md#api-group
	//= type=test
	//# The CRDs SHALL define no `MicroVM` resource.

	all := crds(t)
	if _, ok := all["MicroVM"]; ok {
		t.Error("config/crd/bases defines a MicroVM CRD")
	}
	if len(all) != 2 {
		kinds := make([]string, 0, len(all))
		for k := range all {
			kinds = append(kinds, k)
		}
		t.Errorf("config/crd/bases defines %v, want only Pool and MicroVMClaim", kinds)
	}

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if scheme.Recognizes(v1alpha1.GroupVersion.WithKind("MicroVM")) {
		t.Error("the v1alpha1 scheme registers a MicroVM kind")
	}
}

func TestCRDsHaveAStatusSubresource(t *testing.T) {
	//= docs/requirements/01-resources.md#pool
	//= type=test
	//# The `Pool` resource SHALL have a status subresource that carries
	//# `observedGeneration`, the counts of available, leased, provisioning and
	//# quarantined MicroVMs, and the conditions `Ready` and `Exhausted`.

	//= docs/requirements/01-resources.md#microvmclaim
	//= type=test
	//# The `MicroVMClaim` resource SHALL have a status subresource that
	//# carries the phase, one of `Pending`, `Bound` and `Expired`,
	//# the lease id battery chose, the MicroVM's uid, the Host's node name, the
	//# Exec Agent's address, the time the claim was bound, the time its Lease
	//# expires, and the condition `Bound`.

	all := crds(t)
	for kind, fields := range map[string][]string{
		"Pool":         {"observedGeneration", "available", "leased", "provisioning", "quarantined", "conditions"},
		"MicroVMClaim": {"phase", "leaseID", "microVM", "host", "boundTime", "leaseExpiresAt", "conditions"},
	} {
		t.Run(kind, func(t *testing.T) {
			crd, ok := all[kind]
			if !ok {
				t.Fatalf("no CRD defines %s", kind)
			}
			v := version(t, crd, "v1alpha1")
			if v.Subresources == nil || v.Subresources.Status == nil {
				t.Fatal("v1alpha1 has no status subresource")
			}
			status, ok := v.Schema.OpenAPIV3Schema.Properties["status"]
			if !ok {
				t.Fatal("the schema has no status")
			}
			for _, f := range fields {
				if _, ok := status.Properties[f]; !ok {
					t.Errorf("status has no %s", f)
				}
			}
		})
	}

	phase := version(t, all["MicroVMClaim"], "v1alpha1").Schema.OpenAPIV3Schema.Properties["status"].Properties["phase"]
	phases := make([]string, 0, len(phase.Enum))
	for _, e := range phase.Enum {
		phases = append(phases, string(e.Raw))
	}
	want := []string{`"Pending"`, `"Bound"`, `"Expired"`}
	if len(phases) != len(want) {
		t.Fatalf("status.phase enum = %v, want %v", phases, want)
	}
	for i := range want {
		if phases[i] != want[i] {
			t.Errorf("status.phase enum = %v, want %v", phases, want)
			break
		}
	}
}
