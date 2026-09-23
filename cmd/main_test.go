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

package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// runMainEnv makes the test binary run main instead of the tests.
const runMainEnv = "BATTERY_OPERATOR_TEST_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(runMainEnv) == "1" {
		os.Args = append([]string{"manager"}, strings.Fields(os.Getenv(runMainEnv+"_ARGS"))...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestManagerRefusesToStartWithoutATrustDomain(t *testing.T) {
	//= docs/requirements/09-certificates.md#spiffe-identity
	//= type=test
	//# The Operator SHALL take its SPIFFE trust domain from its
	//# configuration, and SHALL refuse to start without one.
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		runMainEnv+"=1",
		runMainEnv+"_ARGS=--namespace=battery-operator-system",
		// Were it to get past the check, the manager would fail on this
		// instead, with another message.
		"KUBECONFIG=/nonexistent",
	)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("the manager exited with %v, want exit status 1; output:\n%s", err, out)
	}
	if !strings.Contains(string(out), "no SPIFFE trust domain is configured") {
		t.Errorf("the manager did not say why it refused to start; output:\n%s", out)
	}
}
