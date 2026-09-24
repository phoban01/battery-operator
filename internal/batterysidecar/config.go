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
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/phoban01/battery-operator/internal/battery"
)

// Where the Manifests put battery's files. The Operator's container and
// battery's mount the same ConfigMap volume at ConfigDir.
const (
	// DefaultConfigMap is the name of the ConfigMap, in the Operator's
	// namespace, that holds battery's configuration under ConfigKey:
	// config/manager's battery-config after kustomize's namePrefix. The
	// Operator's RBAC grants it access to this name only.
	DefaultConfigMap = "battery-operator-battery-config"
	// ConfigKey is the ConfigMap key, and the file name, of the
	// configuration.
	ConfigKey = "config.json"
	// ConfigDir is where the ConfigMap is mounted in both containers. TLSDir
	// is beside it, not inside it, since a read-only ConfigMap volume cannot
	// hold another volume's mount point.
	ConfigDir = "/etc/battery/config"
	// DefaultConfigFile is the configuration file's path.
	DefaultConfigFile = ConfigDir + "/" + ConfigKey

	// TLSDir holds battery's flintlockd client certificate and key, and the
	// serving CA's certificate (DP-005).
	TLSDir = "/etc/battery/tls"
	// ClientCertFile, ClientKeyFile and ServingCAFile are the files in
	// TLSDir.
	ClientCertFile = TLSDir + "/tls.crt"
	ClientKeyFile  = TLSDir + "/tls.key"
	ServingCAFile  = TLSDir + "/serving-ca.crt"

	// APIAddress is where battery's gRPC API listens: the Operator's
	// default battery address, on loopback (DP-001).
	APIAddress = battery.DefaultAddress
	// MetricsAddress is where battery's /metrics listens, on loopback too,
	// since nothing outside the pod needs it yet.
	MetricsAddress = "127.0.0.1:9090"

	// PlaceholderHostName and PlaceholderHostAddress name the one Host an
	// empty inventory renders, because poolmgrd v0.3.3 refuses to start
	// with none. The name is not a valid Node name, so it never collides
	// with a real Host, and the address, under the reserved .invalid
	// top-level domain, never resolves.
	PlaceholderHostName    = "_no-hosts"
	PlaceholderHostAddress = "no-hosts.invalid:9090"
)

// File is battery's configuration file: the schema of poolmgrd v0.3.3's
// internal/config package, whose JSON tags these repeat.
type File struct {
	Hosts         []HostConfig `json:"hosts"`
	APIServer     *APIServer   `json:"api_server,omitempty"`
	MetricsAddr   string       `json:"metrics_addr,omitempty"`
	SweepInterval string       `json:"sweep_interval,omitempty"`
	WarningWindow string       `json:"warning_window,omitempty"`
}

// HostConfig is one flintlockd battery may dial.
type HostConfig struct {
	Name    string  `json:"name"`
	Address string  `json:"address"`
	TLS     HostTLS `json:"tls"`
}

// HostTLS is how battery connects to a Host.
type HostTLS struct {
	Insecure bool   `json:"insecure,omitempty"`
	CertFile string `json:"cert_file,omitempty"`
	KeyFile  string `json:"key_file,omitempty"`
	CAFile   string `json:"ca_file,omitempty"`
}

// APIServer is battery's own gRPC API.
type APIServer struct {
	Addr           string    `json:"addr"`
	TLS            ServerTLS `json:"tls"`
	BasicAuthToken string    `json:"basic_auth_token,omitempty"`
}

// ServerTLS is how battery's API serves.
type ServerTLS struct {
	Insecure       bool   `json:"insecure,omitempty"`
	CertFile       string `json:"cert_file,omitempty"`
	KeyFile        string `json:"key_file,omitempty"`
	ValidateClient bool   `json:"validate_client,omitempty"`
	ClientCAFile   string `json:"client_ca_file,omitempty"`
}

// Host is one of battery's Hosts, as the Inventory Controller knows it: a
// Node's name and its flintlockd's address, host:port.
type Host struct {
	Name    string
	Address string
}

// Render returns battery's configuration file for hosts: every Host over
// mutual TLS with battery's client certificate, verified against the
// serving CA, sorted by name so that the same Hosts always render the same
// bytes; battery's API and metrics on loopback. With no Hosts it renders
// the placeholder Host.
func Render(hosts []Host) ([]byte, error) {
	f := File{
		//= docs/requirements/06-deployment.md#battery-sidecar
		//# The Manifests SHALL run battery as a container in the
		//# Operator's pod, listening on a loopback address only.
		APIServer:   &APIServer{Addr: APIAddress, TLS: ServerTLS{Insecure: true}},
		MetricsAddr: MetricsAddress,
	}
	if len(hosts) == 0 {
		hosts = []Host{{Name: PlaceholderHostName, Address: PlaceholderHostAddress}}
	}
	sorted := slices.SortedFunc(slices.Values(hosts), func(a, b Host) int { return strings.Compare(a.Name, b.Name) })
	for i, h := range sorted {
		if h.Name == "" || h.Address == "" {
			return nil, fmt.Errorf("battery configuration: host %d has no name or no address", i)
		}
		if i > 0 && sorted[i-1].Name == h.Name {
			return nil, fmt.Errorf("battery configuration: duplicate host %q", h.Name)
		}
		f.Hosts = append(f.Hosts, HostConfig{
			Name:    h.Name,
			Address: h.Address,
			//= docs/requirements/06-deployment.md#battery-sidecar
			//# The Manifests SHALL give battery, from a Secret, a client
			//# certificate and key for `flintlockd` and the certificate authority that
			//# verifies the Hosts' `flintlockd` serving certificates.
			TLS: HostTLS{CertFile: ClientCertFile, KeyFile: ClientKeyFile, CAFile: ServingCAFile},
		})
	}
	out, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("battery configuration: %w", err)
	}
	return append(out, '\n'), nil
}

// Parse reads a configuration file strictly: a field battery does not know
// is an error, as is anything after the object.
func Parse(data []byte) (*File, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f File
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("battery configuration: %w", err)
	}
	if dec.More() {
		return nil, errors.New("battery configuration: trailing data after the object")
	}
	return &f, nil
}
