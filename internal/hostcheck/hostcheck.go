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

// Package hostcheck holds the checks the Exec Agent makes of the Host it
// runs on: that a flintlockd endpoint is an address of this Host
// (docs/requirements/05-exec-agent.md#serving), and what the Host Image
// reports through the not ready reason contract
// (docs/requirements/05-exec-agent.md#host-checks).
package hostcheck

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// DefaultNotReadyDir is the directory of the not ready reason contract; see
// ReadNotReadyReasons. It is the path flintlock-runner's Host Image writes
// to, the one Host Image that exists.
const DefaultNotReadyDir = "/run/flr/not-ready.d"

// maxReasonLength caps one not ready reason.
const maxReasonLength = 256

// HostAddresses lists the IP addresses of this Host's network interfaces.
// The Exec Agent runs in the Host's network namespace, so these are the
// Host's own.
func HostAddresses() ([]net.IP, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, fmt.Errorf("listing the host's addresses: %w", err)
	}
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			out = append(out, n.IP)
		}
	}
	return out, nil
}

// ValidateHostEndpoint accepts an endpoint only when it is a literal IP
// address of this Host, one of hostAddrs, and a port. A host name is refused
// even when it resolves to one of them, because what it resolves to is not
// this function's to know; so is every scheme, the unspecified address, and
// every address that is not one of this Host's.
func ValidateHostEndpoint(endpoint string, hostAddrs []net.IP) error {
	if endpoint == "" {
		return errors.New("is required")
	}
	if strings.Contains(endpoint, "://") || strings.HasPrefix(endpoint, "unix:") || strings.HasPrefix(endpoint, "dns:") {
		return fmt.Errorf("%q: only an address:port of this Host is accepted", endpoint)
	}
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return fmt.Errorf("%q: not an address:port: %w", endpoint, err)
	}
	if port == "" {
		return fmt.Errorf("%q: the port is missing", endpoint)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("%q: the host has to be a literal IP address of this Host, not a name", endpoint)
	}
	if ip.IsUnspecified() {
		return fmt.Errorf("%q: %s is not an address of any host", endpoint, host)
	}
	if !slices.ContainsFunc(hostAddrs, ip.Equal) {
		return fmt.Errorf("%q: %s is not an address of this Host; flintlockd is reached only on the Host itself", endpoint, host)
	}
	return nil
}

// NotReadyReason is one reason a unit of the Host Image reported.
type NotReadyReason struct {
	// Unit is the name of the file, by convention the reporting unit.
	Unit string
	// Reason is the first line of the file.
	Reason string
}

// ReadNotReadyReasons reads the not ready reason contract between the Host
// Image and the Exec Agent:
//
//   - The directory is by default DefaultNotReadyDir. It is on a tmpfs, so
//     a reboot clears it, and the Exec Agent mounts it read-only.
//   - Every regular file in it is one reason for the Host not being ready.
//     The file's name says who reports it, by convention the systemd unit,
//     for example `flintlock-kvm-check.service`; names starting with a dot
//     are ignored so that a writer can create a temporary file and rename it
//     into place.
//   - The first line of the file is the reason in words, for example `KVM is
//     unavailable: /dev/kvm is absent`. An empty file reports `not ready`.
//   - A unit removes its file when the condition clears. An absent or empty
//     directory means no unit has anything to report.
//
// A directory that exists and cannot be read is an error, which the caller
// reports as not ready: nothing guesses that a Host is fine.
func ReadNotReadyReasons(dir string) ([]NotReadyReason, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the not ready reasons: %w", err)
	}
	var out []NotReadyReason
	for _, entry := range entries {
		if !entry.Type().IsRegular() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue // removed between the listing and the read
		}
		if err != nil {
			return nil, fmt.Errorf("reading the not ready reason of %s: %w", entry.Name(), err)
		}
		line, _, _ := strings.Cut(strings.TrimSpace(string(data)), "\n")
		line = strings.TrimSpace(line)
		if line == "" {
			line = "not ready"
		}
		if len(line) > maxReasonLength {
			line = line[:maxReasonLength]
		}
		out = append(out, NotReadyReason{Unit: entry.Name(), Reason: line})
	}
	slices.SortFunc(out, func(a, b NotReadyReason) int { return strings.Compare(a.Unit, b.Unit) })
	return out, nil
}
