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

package layers

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// repoRoot is the repository, from this package's directory.
const repoRoot = "../.."

// envtestUsers is EnvtestUsers, failing t when the walk fails.
func envtestUsers(t *testing.T, root string) (users, unexplained []string) {
	t.Helper()
	users, unexplained, err := EnvtestUsers(root)
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return users, unexplained
}

func TestEnvtestOnlyWhereAFileSaysWhy(t *testing.T) {
	//= docs/requirements/08-test-doubles.md#test-environments
	//= type=test
	//# The unit tests and the e2e suite SHALL use envtest only where
	//# neither a fake nor the kind cluster can exercise a behaviour, and SHALL
	//# say why beside that use.

	//= docs/requirements/08-test-doubles.md#test-environments
	//= type=test
	//# The unit tests SHALL test each subreconciler, and the rest of
	//# the logic of the controllers and the Exec Agent, against the fake battery,
	//# the fake `flintlockd` and a fake Kubernetes client, without a Kubernetes
	//# API server.

	users, unexplained := envtestUsers(t, repoRoot)
	for _, f := range unexplained {
		t.Errorf("%s imports envtest without saying why: use fakes or the e2e suite, or cite TD-027 and say why", f)
	}
	for _, f := range users {
		if strings.HasPrefix(filepath.ToSlash(f), "test/e2e/") {
			t.Errorf("%s: the e2e suite runs against the kind cluster, not envtest", f)
		}
	}
	t.Logf("envtest users: %v", users)
}

func TestEnvtestUsersAreFound(t *testing.T) {
	root := t.TempDir()
	write := func(name, src string) {
		t.Helper()
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a/plain_test.go", "package a\n\nimport \"testing\"\n")
	envtestImport := "import \"sigs.k8s.io/controller-runtime/pkg/envtest\"\n\n"
	write("b/unexplained_test.go", "package b\n\n"+envtestImport+"var _ envtest.Environment\n")
	write("c/explained_test.go", "package c\n\n"+envtestImport+
		"//"+"= docs/requirements/08-test-doubles.md#test-environments\n"+TD027+"\n// Why.\nvar _ envtest.Environment\n")
	write("bin/ignored.go", "package bin\n\n"+envtestImport)

	users, unexplained := envtestUsers(t, root)
	if want := []string{"b/unexplained_test.go", "c/explained_test.go"}; !slices.Equal(users, want) {
		t.Errorf("users = %v, want %v", users, want)
	}
	if want := []string{"b/unexplained_test.go"}; !slices.Equal(unexplained, want) {
		t.Errorf("unexplained = %v, want %v", unexplained, want)
	}
}
