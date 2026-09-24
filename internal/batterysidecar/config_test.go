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

// Two Hosts: their Nodes and their flintlockd addresses.
const (
	nodeA = "node-a"
	nodeB = "node-b"
	addrA = "10.0.0.1:9090"
	addrB = "10.0.0.2:9090"
)

//= docs/requirements/06-deployment.md#battery-sidecar
//= type=test
//# The Manifests SHALL give battery, from a Secret, a client
//# certificate and key for `flintlockd` and the certificate authority that
//# verifies the Hosts' `flintlockd` serving certificates.

// TestRenderHosts checks that every Host battery is given is reached over
// mutual TLS with the mounted client certificate and the serving CA, in the
// same order whatever order the Hosts come in, and that battery's own API
// and metrics stay on loopback.
func TestRenderHosts(t *testing.T) {
	t.Parallel()
	a, err := batterysidecar.Render([]batterysidecar.Host{
		{Name: nodeB, Address: addrB},
		{Name: nodeA, Address: addrA},
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := batterysidecar.Render([]batterysidecar.Host{
		{Name: nodeA, Address: addrA},
		{Name: nodeB, Address: addrB},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Errorf("the same Hosts in another order render differently:\n%s\n%s", a, b)
	}
	f, err := batterysidecar.Parse(a)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Hosts) != 2 || f.Hosts[0].Name != nodeA || f.Hosts[1].Name != nodeB {
		t.Fatalf("hosts %+v, want node-a then node-b", f.Hosts)
	}
	want := batterysidecar.HostTLS{
		CertFile: batterysidecar.ClientCertFile,
		KeyFile:  batterysidecar.ClientKeyFile,
		CAFile:   batterysidecar.ServingCAFile,
	}
	for _, h := range f.Hosts {
		if h.TLS != want {
			t.Errorf("host %s: TLS %+v, want %+v", h.Name, h.TLS, want)
		}
	}
	if f.APIServer == nil || f.APIServer.Addr != "127.0.0.1:50051" || f.MetricsAddr != "127.0.0.1:9090" {
		t.Errorf("battery listens on %+v and %q, want loopback", f.APIServer, f.MetricsAddr)
	}
}

// TestRenderPlaceholder checks that no Hosts render the one placeholder
// Host, since poolmgrd v0.3.3 refuses to start with none.
func TestRenderPlaceholder(t *testing.T) {
	t.Parallel()
	out, err := batterysidecar.Render(nil)
	if err != nil {
		t.Fatal(err)
	}
	f, err := batterysidecar.Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Hosts) != 1 || f.Hosts[0].Name != batterysidecar.PlaceholderHostName ||
		!strings.HasSuffix(strings.Split(f.Hosts[0].Address, ":")[0], ".invalid") {
		t.Errorf("hosts %+v, want the placeholder alone", f.Hosts)
	}
}

func TestRenderRejects(t *testing.T) {
	t.Parallel()
	for name, hosts := range map[string][]batterysidecar.Host{
		"duplicate":  {{Name: "a", Address: addrA}, {Name: "a", Address: addrB}},
		"no name":    {{Address: addrA}},
		"no address": {{Name: "a"}},
	} {
		if _, err := batterysidecar.Render(hosts); err == nil {
			t.Errorf("%s: Render accepted %+v", name, hosts)
		}
	}
}

func TestParseStrict(t *testing.T) {
	t.Parallel()
	for name, in := range map[string]string{
		"unknown field": `{"hosts": [], "host": []}`,
		"trailing data": `{"hosts": []} {}`,
	} {
		if _, err := batterysidecar.Parse([]byte(in)); err == nil {
			t.Errorf("%s: Parse accepted %s", name, in)
		}
	}
}
