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
	"flag"
	"fmt"

	"github.com/phoban01/battery-operator/internal/battery"
)

// Where the Manifests put battery's files.
const (
	// DefaultConfigMap is the name of the ConfigMap, in the Operator's
	// namespace, that holds battery's configuration under ConfigKey:
	// config/manager's battery-config after kustomize's namePrefix. Only
	// battery's container mounts it; the Operator neither reads nor writes
	// it.
	DefaultConfigMap = "battery-operator-battery-config"
	// ConfigKey is the ConfigMap key, and the file name, of the
	// configuration.
	ConfigKey = "config.json"
	// ConfigDir is where the ConfigMap is mounted in battery's container.
	// TLSDir is beside it, not inside it, since a read-only ConfigMap volume
	// cannot hold another volume's mount point.
	ConfigDir = "/etc/battery/config"
	// DefaultConfigFile is the configuration file's path.
	DefaultConfigFile = ConfigDir + "/" + ConfigKey

	// DefaultClientSecret is the name of the Secret, in the Operator's
	// namespace, that cert-manager issues battery's flintlockd client
	// certificate into (config/certificates/battery-client.yaml), and that
	// the Manifests mount at TLSDir. The Operator's RBAC grants it access to
	// this name only.
	DefaultClientSecret = "battery-flintlockd-client"
	// ClientCertKey is that Secret's key of the client certificate.
	ClientCertKey = "tls.crt"

	// TLSDir holds battery's flintlockd client certificate and key, and the
	// serving CA's certificate (DP-005). The Operator's container mounts the
	// same volume there, to see when a renewed certificate has reached it
	// (DP-035).
	TLSDir = "/etc/battery/tls"
	// ClientCertFile, ClientKeyFile and ServingCAFile are the files in
	// TLSDir: the TLS settings the Inventory Controller gives battery for
	// every Host (IN-004).
	ClientCertFile = TLSDir + "/tls.crt"
	ClientKeyFile  = TLSDir + "/tls.key"
	ServingCAFile  = TLSDir + "/serving-ca.crt"

	// APIAddress is where battery's gRPC API listens: the Operator's
	// default battery address, on loopback (DP-001).
	APIAddress = battery.DefaultAddress
	// MetricsAddress is where battery's /metrics listens, on loopback too,
	// since nothing outside the pod needs it yet.
	MetricsAddress = "127.0.0.1:9090"
)

// Config is where the Operator finds battery's client certificate, from
// its flags.
type Config struct {
	// ClientSecret names the Secret, in the Operator's namespace, that holds
	// battery's flintlockd client certificate: what cert-manager renews.
	ClientSecret string
	// ClientCertFile is the Operator's mount of that Secret's certificate.
	ClientCertFile string
}

// BindFlags binds c to the Operator's command line flags.
func (c *Config) BindFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.ClientSecret, "battery-client-secret", DefaultClientSecret,
		"The Secret, in the Operator's namespace, holding battery's flintlockd client certificate; battery reads it "+
			"again when it changes. The Operator's RBAC grants access to the default name only.")
	fs.StringVar(&c.ClientCertFile, "battery-client-cert-file", ClientCertFile,
		"Where the Operator's container mounts battery's flintlockd client certificate, the volume battery's "+
			"container mounts too.")
}

// File is battery's configuration file: the schema of poolmgrd's
// internal/config package, whose JSON tags these repeat. It has no Hosts:
// battery keeps them in its database, and refuses a file with a "hosts"
// key (BA-087).
type File struct {
	APIServer     *APIServer `json:"api_server,omitempty"`
	MetricsAddr   string     `json:"metrics_addr,omitempty"`
	SweepInterval string     `json:"sweep_interval,omitempty"`
	WarningWindow string     `json:"warning_window,omitempty"`
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

// Render returns battery's configuration file as the Manifests ship it:
// battery's API and metrics on loopback, and no Hosts.
func Render() ([]byte, error) {
	//= docs/requirements/06-deployment.md#battery-sidecar
	//# The Manifests SHALL supply battery's configuration from a
	//# ConfigMap that names no Hosts.
	f := File{
		//= docs/requirements/06-deployment.md#battery-sidecar
		//# The Manifests SHALL run battery as a container in the
		//# Operator's pod, listening on a loopback address only.
		APIServer:   &APIServer{Addr: APIAddress, TLS: ServerTLS{Insecure: true}},
		MetricsAddr: MetricsAddress,
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
