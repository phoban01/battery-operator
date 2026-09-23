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

package execagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phoban01/battery-operator/internal/fakeflintlock"
	"github.com/phoban01/battery-operator/internal/hostcheck"
)

// fakePrerequisites points cfg at stand-ins for KVM and the thin pool, as
// execagenttest.FakeHostPrerequisites does for the envtest Hosts: /dev/null
// as the KVM device node, since a test cannot make a character device, a
// sysfs directory listing the KVM device, and a sysfs block directory whose
// one device-mapper device is the thin pool under its default name.
func fakePrerequisites(t *testing.T, cfg *Config) {
	t.Helper()
	dir := t.TempDir()
	cfg.KVMDevice = "/dev/null"
	cfg.KVMSysfsDir = filepath.Join(dir, "kvm")
	if err := os.MkdirAll(cfg.KVMSysfsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.KVMSysfsDir, "dev"), []byte("10:232\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	cfg.SysBlockDir = filepath.Join(dir, "block")
	dm := filepath.Join(cfg.SysBlockDir, "dm-0", "dm")
	if err := os.MkdirAll(dm, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dm, "name"), []byte(hostcheck.DefaultThinPool+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

//= docs/requirements/05-exec-agent.md#host-checks
//= type=test
//# The Exec Agent SHALL report its Host not ready while the local
//# `flintlockd` does not answer `ServerInfo` with the exec service enabled.

//= docs/requirements/05-exec-agent.md#host-checks
//= type=test
//# The Exec Agent SHALL report its Host not ready while the Host
//# has no KVM device.

//= docs/requirements/05-exec-agent.md#host-checks
//= type=test
//# The Exec Agent SHALL report its Host not ready while
//# containerd's thin pool, under the name the Exec Agent's configuration
//# gives, is not present.

// TestHostPrerequisites checks readiness with every Host prerequisite
// present, which is ready, and with each one missing in turn, which is not
// ready with that prerequisite's own reason: the KVM device not listed in
// sysfs, its node absent or not a character device, the thin pool absent
// or configured under another name, flintlockd not answering ServerInfo,
// and flintlockd answering with exec disabled.
func TestHostPrerequisites(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		exec   bool // whether the fake flintlockd has exec enabled
		spoil  func(*testing.T, *Config, *fakeflintlock.Server)
		reason string
		says   string // what the message has to say
	}{
		{name: "all present", exec: true, reason: ReasonReady, says: "the Host has a KVM device"},
		{
			name: "KVM not in sysfs", exec: true, reason: ReasonKVMUnavailable, says: "lists no KVM device",
			spoil: func(t *testing.T, cfg *Config, _ *fakeflintlock.Server) {
				if err := os.Remove(filepath.Join(cfg.KVMSysfsDir, "dev")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "KVM device node absent", exec: true, reason: ReasonKVMUnavailable, says: "KVM is unavailable",
			spoil: func(t *testing.T, cfg *Config, _ *fakeflintlock.Server) {
				cfg.KVMDevice = filepath.Join(t.TempDir(), "kvm")
			},
		},
		{
			name: "KVM device node not a character device", exec: true, reason: ReasonKVMUnavailable, says: "not a character device",
			spoil: func(t *testing.T, cfg *Config, _ *fakeflintlock.Server) {
				cfg.KVMDevice = t.TempDir()
			},
		},
		{
			name: "thin pool absent", exec: true, reason: ReasonThinPoolMissing, says: "flintlock-thinpool is not present",
			spoil: func(t *testing.T, cfg *Config, _ *fakeflintlock.Server) {
				if err := os.RemoveAll(filepath.Join(cfg.SysBlockDir, "dm-0")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "thin pool under another name", exec: true, reason: ReasonThinPoolMissing, says: "other-thinpool is not present",
			spoil: func(_ *testing.T, cfg *Config, _ *fakeflintlock.Server) {
				cfg.ThinPool = "other-thinpool"
			},
		},
		{
			name: "flintlockd does not answer", exec: true, reason: ReasonFlintlockdNotReady, says: "does not answer ServerInfo",
			spoil: func(_ *testing.T, _ *Config, fake *fakeflintlock.Server) {
				fake.SetFaults(fakeflintlock.Faults{Unresponsive: true})
			},
		},
		{name: "exec disabled", exec: false, reason: ReasonExecDisabled, says: "exec service disabled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake, certs, _ := serveFake(t, fakeflintlock.Config{Name: "h", ExecEnabled: tc.exec, Version: testVersion})
			defer fake.SetFaults(fakeflintlock.Faults{})
			fl, err := DialFlintlockd(fake.Addr(), clientTLS(certs), loopback)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = fl.Close() }()
			cfg := validConfig()
			cfg.NotReadyDir = filepath.Join(t.TempDir(), "absent")
			cfg.CallTimeout = time.Second
			fakePrerequisites(t, cfg)
			if tc.spoil != nil {
				tc.spoil(t, cfg, fake)
			}
			r := checkReadiness(context.Background(), cfg, fl)
			if r.ready != (tc.reason == ReasonReady) || r.reason != tc.reason || !strings.Contains(r.message, tc.says) {
				t.Errorf("readiness = %+v, want reason %s and a message saying %q", r, tc.reason, tc.says)
			}
		})
	}
}

// TestPrerequisiteDefaults checks where the agent looks for the Host
// prerequisites when its configuration does not say.
func TestPrerequisiteDefaults(t *testing.T) {
	t.Parallel()
	cfg := validConfig()
	if cfg.KVMDevice != "/dev/kvm" || cfg.KVMSysfsDir != "/sys/class/misc/kvm" || cfg.ThinPool != "flintlock-thinpool" || cfg.SysBlockDir != "/sys/block" {
		t.Errorf("defaults: KVM device %q, KVM sysfs %q, thin pool %q, sysfs block directory %q; want /dev/kvm, /sys/class/misc/kvm, flintlock-thinpool, /sys/block",
			cfg.KVMDevice, cfg.KVMSysfsDir, cfg.ThinPool, cfg.SysBlockDir)
	}
}

//= docs/requirements/05-exec-agent.md#host-checks
//= type=test
//# The Exec Agent SHALL publish in its Node report the address
//# at which battery reaches the Host's `flintlockd`, as the annotation
//# `battery.liquidmetal-x.dev/flintlockd-address`.

// TestNodeReportCarriesFlintlockdAddress checks the Node report's
// flintlockd address by its literal key. node_envtest_test.go's
// TestNodeReport checks it on a Node, through Run.
func TestNodeReportCarriesFlintlockdAddress(t *testing.T) {
	t.Parallel()
	report := nodeReport(readiness{ready: true, reason: ReasonReady}, "10.0.0.7:10270", "10.0.0.7:9090")
	if got := report["battery.liquidmetal-x.dev/flintlockd-address"]; got != "10.0.0.7:9090" {
		t.Errorf("flintlockd address in the Node report = %v, want 10.0.0.7:9090", got)
	}
}
