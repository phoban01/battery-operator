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

// Package layers checks that the tests keep to their two layers (ADR 0006,
// docs/requirements/08-test-doubles.md#test-environments): unit tests with
// fakes, and the e2e suite on kind, with envtest only where a file says why.
package layers

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const (
	// EnvtestPackage starts a kube-apiserver and etcd in the test process.
	EnvtestPackage = "sigs.k8s.io/controller-runtime/pkg/envtest"
	// TD027 is the citation a file that imports envtest carries, beside the
	// reason it cannot use a fake or the kind cluster instead.
	TD027 = "//" + "# The unit tests and the e2e suite SHALL use envtest only where"
)

// skipDirs hold no sources of this project's.
var skipDirs = []string{".git", ".gopath", ".devbox", ".run", "bin", "node_modules", "vendor"}

//= docs/requirements/08-test-doubles.md#test-environments
//# The unit tests and the e2e suite SHALL use envtest only where
//# neither a fake nor the kind cluster can exercise a behaviour, and SHALL
//# say why beside that use.

// EnvtestUsers returns the Go files under root that import envtest, relative
// to root, and of those, the ones without the TD-027 citation. No test of
// this repository uses envtest now; a use that returns has to cite TD-027
// and say why, or TestEnvtestOnlyWhereAFileSaysWhy fails.
func EnvtestUsers(root string) (users, unexplained []string, err error) {
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && slices.Contains(skipDirs, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p != EnvtestPackage {
				continue
			}
			rel, _ := filepath.Rel(root, path)
			users = append(users, rel)
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !strings.Contains(string(src), TD027) {
				unexplained = append(unexplained, rel)
			}
		}
		return nil
	})
	return users, unexplained, err
}
