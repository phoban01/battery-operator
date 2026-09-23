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

package hostcheck

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestValidateHostEndpoint(t *testing.T) {
	t.Parallel()
	host := []net.IP{net.ParseIP("10.0.0.7"), net.ParseIP("127.0.0.1"), net.ParseIP("fd00::7")}
	for _, ok := range []string{"10.0.0.7:9090", "127.0.0.1:9090", "[fd00::7]:9090"} {
		if err := ValidateHostEndpoint(ok, host); err != nil {
			t.Errorf("ValidateHostEndpoint(%q) = %v, want accepted", ok, err)
		}
	}
	for _, bad := range []string{
		"", "10.0.0.8:9090", "localhost:9090", "flintlockd.example:9090", "dns:///10.0.0.7:9090",
		"unix:///run/flintlockd.sock", "https://10.0.0.7:9090", "0.0.0.0:9090", "[::]:9090", "10.0.0.7", "10.0.0.7:",
	} {
		if err := ValidateHostEndpoint(bad, host); err == nil {
			t.Errorf("ValidateHostEndpoint(%q) accepted, want refused", bad)
		}
	}
}

func TestHostAddressesIncludeLoopback(t *testing.T) {
	t.Parallel()
	addrs, err := HostAddresses()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(addrs, net.IPv4(127, 0, 0, 1).Equal) {
		t.Errorf("HostAddresses() = %v, want 127.0.0.1 among them", addrs)
	}
}

func TestReadNotReadyReasons(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if got, err := ReadNotReadyReasons(filepath.Join(dir, "absent")); err != nil || len(got) != 0 {
		t.Fatalf("an absent directory = (%v, %v), want nothing to report", got, err)
	}
	files := map[string]string{
		"b.service":  "KVM is unavailable: /dev/kvm is absent\nsecond line\n",
		"a.service":  "",
		".tmp-write": "ignored",
		"long":       strings.Repeat("x", 1000),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := ReadNotReadyReasons(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []NotReadyReason{
		{Unit: "a.service", Reason: "not ready"},
		{Unit: "b.service", Reason: "KVM is unavailable: /dev/kvm is absent"},
		{Unit: "long", Reason: strings.Repeat("x", maxReasonLength)},
	}
	if !slices.Equal(got, want) {
		t.Errorf("ReadNotReadyReasons = %v, want %v", got, want)
	}
}
