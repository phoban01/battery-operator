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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCheckKVM(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	device := filepath.Join(dir, "kvm")
	if err := CheckKVM(device); err == nil || !strings.Contains(err.Error(), "KVM is unavailable") {
		t.Errorf("CheckKVM of an absent device = %v, want KVM unavailable", err)
	}
	if err := os.WriteFile(device, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckKVM(device); err != nil {
		t.Errorf("CheckKVM of a device that opens = %v, want nil", err)
	}
	if os.Geteuid() != 0 {
		if err := os.Chmod(device, 0o400); err != nil {
			t.Fatal(err)
		}
		if err := CheckKVM(device); err == nil {
			t.Error("CheckKVM of a device that opens only for reading = nil, want an error")
		}
	}
}

// writeDeviceMapper writes a fake sysfs block directory under dir holding a
// device-mapper device for each name.
func writeDeviceMapper(t *testing.T, dir string, names ...string) {
	t.Helper()
	for i, name := range names {
		dm := filepath.Join(dir, "dm-"+strconv.Itoa(i), "dm")
		if err := os.MkdirAll(dm, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dm, "name"), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCheckThinPool(t *testing.T) {
	t.Parallel()
	sys := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sys, "nvme0n1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CheckThinPool(sys, DefaultThinPool); err == nil {
		t.Error("CheckThinPool with no device-mapper devices = nil, want an error")
	}
	writeDeviceMapper(t, sys, "flintlock-thinpool_tmeta", "other-thinpool")
	if err := CheckThinPool(sys, DefaultThinPool); err == nil || !strings.Contains(err.Error(), "flintlock-thinpool is not present") {
		t.Errorf("CheckThinPool with only other devices = %v, want not present", err)
	}
	writeDeviceMapper(t, sys, "flintlock-thinpool_tmeta", "other-thinpool", DefaultThinPool)
	if err := CheckThinPool(sys, DefaultThinPool); err != nil {
		t.Errorf("CheckThinPool with the pool present = %v, want nil", err)
	}
	if err := CheckThinPool(sys, ""); err == nil {
		t.Error("CheckThinPool with no pool configured = nil, want an error")
	}
}
