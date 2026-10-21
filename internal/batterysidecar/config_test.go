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

package batterysidecar_test

import (
	"strings"
	"testing"

	"github.com/phoban01/battery-operator/internal/batterysidecar"
)

//= docs/requirements/06-deployment.md#battery-sidecar
//= type=test
//# The Manifests SHALL supply battery's configuration from a
//# ConfigMap that names no Hosts.

// TestRender checks that battery's configuration names no Hosts, which
// battery v0.5.0 refuses (BA-087), and keeps battery's own API and metrics
// on loopback.
func TestRender(t *testing.T) {
	t.Parallel()
	out, err := batterysidecar.Render()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"hosts"`) {
		t.Errorf("the configuration names Hosts:\n%s", out)
	}
	f, err := batterysidecar.Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if f.APIServer == nil || f.APIServer.Addr != "127.0.0.1:50051" || f.MetricsAddr != "127.0.0.1:9090" {
		t.Errorf("battery listens on %+v and %q, want loopback", f.APIServer, f.MetricsAddr)
	}
}

func TestParseStrict(t *testing.T) {
	t.Parallel()
	for name, in := range map[string]string{
		"hosts":         `{"hosts": []}`,
		"unknown field": `{"metrics_addr": "127.0.0.1:9090", "host": []}`,
		"trailing data": `{"metrics_addr": "127.0.0.1:9090"} {}`,
	} {
		if _, err := batterysidecar.Parse([]byte(in)); err == nil {
			t.Errorf("%s: Parse accepted %s", name, in)
		}
	}
}
