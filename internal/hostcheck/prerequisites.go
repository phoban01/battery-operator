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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Defaults of the Host prerequisite checks (docs/host-prerequisites.md).
const (
	// DefaultKVMDevice is the KVM device on the Host.
	DefaultKVMDevice = "/dev/kvm"
	// DefaultKVMSysfsDir is where the kernel lists the KVM device in sysfs
	// once the kvm module has registered it.
	DefaultKVMSysfsDir = "/sys/class/misc/kvm"
	// DefaultThinPool is the device-mapper name of containerd's thin pool,
	// the pool_name of containerd's devmapper snapshotter. It is
	// flintlock's default, and the name flintlock-runner's Host Image gives
	// the pool: the logical volume thinpool in the volume group flintlock.
	DefaultThinPool = "flintlock-thinpool"
	// DefaultSysBlockDir is where the kernel lists block devices, the
	// device-mapper devices among them, in sysfs.
	DefaultSysBlockDir = "/sys/block"
)

// CheckKVM reports whether the Host has a KVM device: the kernel lists one
// in sysfs (`<sysfsDir>/dev`, present once the kvm module has registered
// the device), and the Host's device node is a character device. It does
// not open the device. flintlockd, on the Host, is what opens it, and an
// unprivileged container could not, however the node is mounted into it.
func CheckKVM(device, sysfsDir string) error {
	if _, err := os.Stat(filepath.Join(sysfsDir, "dev")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("KVM is unavailable: the kernel lists no KVM device in %s; is the kvm module loaded and virtualization enabled?", sysfsDir)
		}
		return fmt.Errorf("KVM is unavailable: %w", err)
	}
	info, err := os.Stat(device)
	if err != nil {
		return fmt.Errorf("KVM is unavailable: %w", err)
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("KVM is unavailable: %s is not a character device", device)
	}
	return nil
}

// CheckThinPool reports whether a device-mapper device named pool exists,
// by reading the name of every device-mapper device under sysBlockDir
// (`<sysBlockDir>/dm-*/dm/name`).
//
// sysfs is read rather than /dev/mapper because it needs nothing mounted
// into a container: sysfs lists every block device of the Host, whereas a
// container's /dev holds none of them, and /dev/mapper/<name> is often a
// symbolic link to ../dm-N, which a /dev/mapper mounted on its own cannot
// resolve.
func CheckThinPool(sysBlockDir, pool string) error {
	if pool == "" {
		return errors.New("no thin pool is configured")
	}
	names, err := filepath.Glob(filepath.Join(sysBlockDir, "dm-*", "dm", "name"))
	if err != nil {
		return fmt.Errorf("listing the device-mapper devices in %s: %w", sysBlockDir, err)
	}
	for _, file := range names {
		data, err := os.ReadFile(file)
		if errors.Is(err, os.ErrNotExist) {
			continue // removed between the listing and the read
		}
		if err != nil {
			return fmt.Errorf("reading the device-mapper device name %s: %w", file, err)
		}
		if strings.TrimSpace(string(data)) == pool {
			return nil
		}
	}
	return fmt.Errorf("containerd's thin pool %s is not present: no device-mapper device of that name in %s", pool, sysBlockDir)
}
