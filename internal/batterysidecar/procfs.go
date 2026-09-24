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

package batterysidecar

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Process identifies one process: its PID and its start time, in clock
// ticks since boot, so that a PID the kernel has reused is not mistaken
// for the process it once named.
type Process struct {
	PID   int
	Start uint64
}

// Processes finds and signals processes. ProcFS is the production
// implementation; the Restarter's tests use a fake.
type Processes interface {
	// Find returns the running processes whose executable, the base name of
	// their argv[0], is name.
	Find(name string) ([]Process, error)
	// Terminate sends p SIGTERM, if p is still running.
	Terminate(p Process) error
	// Running reports whether p is still running: not exited, and not a
	// zombie waiting to be reaped.
	Running(p Process) (bool, error)
}

// ProcFS implements Processes over a proc filesystem: in the Operator's
// pod, with shareProcessNamespace, /proc shows battery's processes too.
type ProcFS struct {
	// Root is the proc filesystem's mount point, /proc when empty.
	Root string
	// kill sends a signal; syscall.Kill when nil.
	kill func(pid int, sig syscall.Signal) error
}

var _ Processes = ProcFS{}

func (p ProcFS) root() string {
	if p.Root == "" {
		return "/proc"
	}
	return p.Root
}

// Find implements Processes.
func (p ProcFS) Find(name string) ([]Process, error) {
	entries, err := os.ReadDir(p.root())
	if err != nil {
		return nil, fmt.Errorf("listing processes: %w", err)
	}
	var out []Process
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || !e.IsDir() {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join(p.root(), e.Name(), "cmdline"))
		if err != nil {
			continue // it exited, or is not ours to read
		}
		argv0, _, _ := bytes.Cut(cmdline, []byte{0})
		if len(argv0) == 0 || filepath.Base(string(argv0)) != name {
			continue
		}
		st, err := p.stat(pid)
		if err != nil || st.zombie {
			continue
		}
		out = append(out, Process{PID: pid, Start: st.start})
	}
	return out, nil
}

// Running implements Processes.
func (p ProcFS) Running(proc Process) (bool, error) {
	st, err := p.stat(proc.PID)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return st.start == proc.Start && !st.zombie, nil
}

// Terminate implements Processes. It checks the process's start time first,
// so a PID that now names another process is left alone.
func (p ProcFS) Terminate(proc Process) error {
	running, err := p.Running(proc)
	if err != nil || !running {
		return err
	}
	kill := p.kill
	if kill == nil {
		kill = syscall.Kill
	}
	if err := kill(proc.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signalling process %d: %w", proc.PID, err)
	}
	return nil
}

type procStat struct {
	start  uint64
	zombie bool
}

// stat reads a process's state and start time from /proc/<pid>/stat,
// whose second field, the command, is in parentheses and may itself hold
// spaces and parentheses; the fields after its last ')' are plain.
func (p ProcFS) stat(pid int) (procStat, error) {
	data, err := os.ReadFile(filepath.Join(p.root(), strconv.Itoa(pid), "stat"))
	if err != nil {
		return procStat{}, err
	}
	i := bytes.LastIndexByte(data, ')')
	if i < 0 {
		return procStat{}, fmt.Errorf("process %d: malformed stat", pid)
	}
	// After the command: state (field 3) ... starttime (field 22).
	fields := strings.Fields(string(data[i+1:]))
	const stateIdx, startIdx = 0, 22 - 3
	if len(fields) <= startIdx {
		return procStat{}, fmt.Errorf("process %d: stat has %d fields after the command", pid, len(fields))
	}
	start, err := strconv.ParseUint(fields[startIdx], 10, 64)
	if err != nil {
		return procStat{}, fmt.Errorf("process %d: start time: %w", pid, err)
	}
	state := fields[stateIdx]
	return procStat{start: start, zombie: state == "Z" || state == "X"}, nil
}
