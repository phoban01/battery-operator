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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/phoban01/battery-operator/internal/batterysidecar"
)

// fakeProc writes one process into a fake proc filesystem.
func fakeProc(t *testing.T, root string, pid int, state string, start uint64, argv ...string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmdline := strings.Join(argv, "\x00") + "\x00"
	// Fields 3 to 21, the start time as field 22, and two more. The command
	// holds a space and a parenthesis, as a process may name itself.
	fields := append([]string{state}, slices.Repeat([]string{"0"}, 18)...)
	fields = append(fields, strconv.FormatUint(start, 10), "0", "0")
	stat := fmt.Sprintf("%d (odd) name) %s\n", pid, strings.Join(fields, " "))
	for name, data := range map[string]string{"cmdline": cmdline, "stat": stat} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// fakeProcRoot is a pod's shared process namespace: the pause process, the
// manager, battery, and an old battery not yet reaped.
func fakeProcRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	fakeProc(t, root, 1, "S", 10, "/pause")
	fakeProc(t, root, 7, "S", 20, "/manager", "--leader-elect")
	fakeProc(t, root, 12, "S", 30, "/poolmgrd", "-config=/etc/battery/config/config.json")
	fakeProc(t, root, 9, "Z", 25, "/poolmgrd")
	if err := os.Symlink("12", filepath.Join(root, "self")); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestProcFSFind(t *testing.T) {
	t.Parallel()
	got, err := batterysidecar.ProcFS{Root: fakeProcRoot(t)}.Find(batterysidecar.ProcessName)
	if err != nil {
		t.Fatal(err)
	}
	if want := []batterysidecar.Process{{PID: 12, Start: 30}}; !slices.Equal(got, want) {
		t.Errorf("Find: %v, want %v", got, want)
	}
}

func TestProcFSRunning(t *testing.T) {
	t.Parallel()
	p := batterysidecar.ProcFS{Root: fakeProcRoot(t)}
	for _, tc := range []struct {
		proc batterysidecar.Process
		want bool
	}{
		{batterysidecar.Process{PID: 12, Start: 30}, true},
		{batterysidecar.Process{PID: 12, Start: 29}, false}, // the PID was reused
		{batterysidecar.Process{PID: 9, Start: 25}, false},  // a zombie
		{batterysidecar.Process{PID: 40, Start: 1}, false},  // gone
	} {
		got, err := p.Running(tc.proc)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("Running(%v) = %v, want %v", tc.proc, got, tc.want)
		}
	}
}

// TestProcFSTerminate checks that SIGTERM goes to the process found, and
// not to another process that has since taken its PID.
func TestProcFSTerminate(t *testing.T) {
	t.Parallel()
	var sent []string
	p := batterysidecar.NewProcFSWithKill(fakeProcRoot(t), func(pid int, sig syscall.Signal) error {
		sent = append(sent, fmt.Sprintf("%d %v", pid, sig))
		return nil
	})
	if err := p.Terminate(batterysidecar.Process{PID: 12, Start: 29}); err != nil {
		t.Fatal(err)
	}
	if err := p.Terminate(batterysidecar.Process{PID: 12, Start: 30}); err != nil {
		t.Fatal(err)
	}
	if want := []string{fmt.Sprintf("12 %v", syscall.SIGTERM)}; !slices.Equal(sent, want) {
		t.Errorf("signals %v, want %v", sent, want)
	}

	gone := batterysidecar.NewProcFSWithKill(fakeProcRoot(t), func(int, syscall.Signal) error { return syscall.ESRCH })
	if err := gone.Terminate(batterysidecar.Process{PID: 12, Start: 30}); err != nil {
		t.Errorf("a process that exited before the signal: %v", err)
	}
}

// waitExec waits until the command line of the child pid names name.
// cmd.Start returns once the child's exec has replaced its memory, but the
// kernel sets the new command line a moment later, and until then
// /proc/<pid>/cmdline reads empty.
func waitExec(t *testing.T, pid int, name string) {
	t.Helper()
	path := filepath.Join("/proc", strconv.Itoa(pid), "cmdline")
	deadline := time.Now().Add(10 * time.Second)
	for {
		cmdline, err := os.ReadFile(path)
		argv0, _, _ := strings.Cut(string(cmdline), "\x00")
		if err == nil && filepath.Base(argv0) == name {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("child %d: command line %q (%v), want %s", pid, cmdline, err, name)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestProcFSRealProcess finds, signals and outlives a real child process
// through the real /proc.
func TestProcFSRealProcess(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no proc filesystem:", err)
	}
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skip("cannot run sleep:", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	waitExec(t, cmd.Process.Pid, "sleep")

	p := batterysidecar.ProcFS{}
	found, err := p.Find("sleep")
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(found, func(proc batterysidecar.Process) bool { return proc.PID == cmd.Process.Pid })
	if i < 0 {
		t.Fatalf("Find(sleep) = %v, without the child %d", found, cmd.Process.Pid)
	}
	child := found[i]
	if err := p.Terminate(child); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
		t.Fatalf("the child ended with %v, want SIGTERM", err)
	}
	if running, err := p.Running(child); err != nil || running {
		t.Errorf("Running after it was reaped: %v, %v", running, err)
	}
}
