//go:build e2e

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

// Package e2e is the end-to-end suite (ADR 0006, test/e2e/README.md): a
// kind cluster running the Manifests, with battery's real poolmgrd as the
// Operator's sidecar, the Exec Agent, and the fake flintlockd on every
// Host. It is written with sigs.k8s.io/e2e-framework and runs with
// `make test-e2e`.
package e2e

import (
	"os"
	"testing"

	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
	"sigs.k8s.io/e2e-framework/support/kind"
)

// testenv is the suite's environment: TestMain sets up the cluster once,
// and every test runs its features against it.
var testenv env.Environment

// TestMain creates the kind cluster, loads the images the suite built,
// installs cert-manager, deploys the Manifests through the overlay in
// config/ and waits for them, runs the tests, and deletes the cluster.
func TestMain(m *testing.M) {
	//= docs/requirements/08-test-doubles.md#test-environments
	//= type=test
	//# The e2e suite SHALL run the Operator with battery's `poolmgrd`
	//# as its sidecar, the Exec Agent, and the fake `flintlockd` as each Host's
	//# `flintlockd`, in a kind cluster, from the Manifests.
	s := settingsFromEnv()
	testenv = env.NewWithConfig(envconf.New())
	testenv.Setup(
		envfuncs.CreateClusterWithConfig(kind.NewProvider(), s.cluster, "kind-config.yaml"),
		envfuncs.LoadImageToCluster(s.cluster, s.operatorImage),
		envfuncs.LoadImageToCluster(s.cluster, s.execAgentImage),
		envfuncs.LoadImageToCluster(s.cluster, s.fakeFlintlockdImage),
		installCertManager,
		deploy(s),
		waitForDeployment,
	)
	if s.logsDir != "" {
		testenv.Finish(envfuncs.ExportClusterLogs(s.cluster, s.logsDir))
	}
	if !s.keepCluster {
		testenv.Finish(envfuncs.DestroyCluster(s.cluster))
	}
	os.Exit(testenv.Run(m))
}
