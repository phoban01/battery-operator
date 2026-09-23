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
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/phoban01/battery-operator/internal/hostcheck"
)

// Defaults of the Exec Agent's configuration.
const (
	// DefaultPort is the port of the exec API.
	DefaultPort = 10270
	// DefaultExecOpenTimeout bounds how long flintlockd has to open the
	// exec stream of a request (EA-021).
	DefaultExecOpenTimeout = 10 * time.Second
	// DefaultCallTimeout bounds every unary call the agent relays or makes
	// to flintlockd, and every TokenReview.
	DefaultCallTimeout = 10 * time.Second
	// DefaultDrainTimeout bounds how long Bound claims hold a drain of the
	// Host's Node open (EA-040).
	DefaultDrainTimeout = time.Hour
	// DefaultSyncInterval is how often readiness, the Node report and the
	// drain guard are reconciled.
	DefaultSyncInterval = 2 * time.Second
	// DefaultGuardImage is the image of the drain guard pod, which only has
	// to stay running.
	DefaultGuardImage = "registry.k8s.io/pause:3.10"
)

// Config is the configuration of the Exec Agent. cmd/exec-agent fills it
// from its flags.
type Config struct {
	// HostNode is the name of the Host's own Node, from the downward API.
	HostNode string
	// Flintlockd is the endpoint of this Host's flintlockd: an IP address
	// of this Host and a port (EA-001).
	Flintlockd string
	// FlintlockdTLS is the client certificate the agent presents to
	// flintlockd and the CA it verifies flintlockd's serving certificate
	// against (EA-001). Until #31, they are files on the Host.
	FlintlockdTLS ClientTLS
	// Address is the address the exec API is served on. Empty means the
	// Host's internal address, read from its Node (EA-002).
	Address string
	// Port is the port of the exec API.
	Port int
	// TLS is the serving certificate of the exec API (EA-002). It is
	// required: there is no mode that serves in the clear.
	TLS ServerTLS
	// TokenAudiences are the audiences the agent's TokenReviews ask for,
	// and a caller's token has to carry every one (EA-010). Empty is
	// DefaultTokenAudience.
	TokenAudiences []string
	// ExecOpenTimeout bounds how long flintlockd has to open the exec
	// stream of a request (EA-021).
	ExecOpenTimeout time.Duration
	// CallTimeout bounds every unary call to flintlockd and every
	// TokenReview (EA-014).
	CallTimeout time.Duration
	// NotReadyDir is the directory the Host Image's units write not ready
	// reasons to (EA-033).
	NotReadyDir string
	// KVMDevice is the path of the Host's KVM device node, which has to be a
	// character device (EA-031).
	KVMDevice string
	// KVMSysfsDir is where sysfs lists the KVM device, which has to be there
	// (EA-031).
	KVMSysfsDir string
	// ThinPool is the device-mapper name of containerd's thin pool, the
	// pool_name of containerd's devmapper snapshotter (EA-032).
	ThinPool string
	// SysBlockDir is sysfs's directory of block devices, where the agent
	// looks for the thin pool (EA-032).
	SysBlockDir string
	// DrainTimeout bounds how long Bound claims hold a drain open (EA-040).
	DrainTimeout time.Duration
	// Guard configures the drain guard pod (EA-040).
	Guard Guard
	// SyncInterval is the reconcile period.
	SyncInterval time.Duration
}

// ServerTLS is the serving certificate of the exec API.
type ServerTLS struct {
	CertFile string
	KeyFile  string
}

// ClientTLS is the agent's client certificate for flintlockd, and the CA
// that signed flintlockd's serving certificate.
type ClientTLS struct {
	CertFile string
	KeyFile  string
	CAFile   string
}

// Guard is where and from what the drain guard pod is made.
type Guard struct {
	// Namespace is the namespace of the guard pod and its
	// PodDisruptionBudget, normally the Exec Agent's own.
	Namespace string
	Image     string
}

// ApplyDefaults fills every unset field that has a default.
func (c *Config) ApplyDefaults() {
	setDefault(&c.NotReadyDir, hostcheck.DefaultNotReadyDir)
	setDefault(&c.KVMDevice, hostcheck.DefaultKVMDevice)
	setDefault(&c.KVMSysfsDir, hostcheck.DefaultKVMSysfsDir)
	setDefault(&c.ThinPool, hostcheck.DefaultThinPool)
	setDefault(&c.SysBlockDir, hostcheck.DefaultSysBlockDir)
	setDefault(&c.Guard.Image, DefaultGuardImage)
	if len(c.TokenAudiences) == 0 {
		c.TokenAudiences = []string{DefaultTokenAudience}
	}
	if c.Port == 0 {
		c.Port = DefaultPort
	}
	if c.ExecOpenTimeout == 0 {
		c.ExecOpenTimeout = DefaultExecOpenTimeout
	}
	if c.CallTimeout == 0 {
		c.CallTimeout = DefaultCallTimeout
	}
	if c.DrainTimeout == 0 {
		c.DrainTimeout = DefaultDrainTimeout
	}
	if c.SyncInterval == 0 {
		c.SyncInterval = DefaultSyncInterval
	}
}

func setDefault(field *string, value string) {
	if *field == "" {
		*field = value
	}
}

// Validate reports every invalid field, sorted. hostAddrs are this Host's
// addresses, which the flintlockd endpoint has to be one of (EA-001).
func (c *Config) Validate(hostAddrs []net.IP) error {
	var errs []error
	fail := func(field, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", field, fmt.Sprintf(format, args...)))
	}

	if c.HostNode == "" {
		fail("host-node", "is required")
	}
	if err := hostcheck.ValidateHostEndpoint(c.Flintlockd, hostAddrs); err != nil {
		fail("flintlockd", "%v", err)
	}
	if c.FlintlockdTLS.CertFile == "" || c.FlintlockdTLS.KeyFile == "" {
		fail("flintlockd-cert-file", "a client certificate and key are required: flintlockd is reached over mutual TLS")
	}
	if c.FlintlockdTLS.CAFile == "" {
		fail("flintlockd-ca-file", "is required: flintlockd's serving certificate is always verified")
	}
	if c.Address != "" && net.ParseIP(c.Address) == nil {
		fail("address", "%q is not an IP address", c.Address)
	}
	if c.Port <= 0 || c.Port > 65535 {
		fail("port", "%d is not a port", c.Port)
	}
	if c.TLS.CertFile == "" {
		fail("tls-cert-file", "is required: the exec API is never served in the clear")
	}
	if c.TLS.KeyFile == "" {
		fail("tls-key-file", "is required")
	}
	for field, d := range map[string]time.Duration{
		"exec-open-timeout": c.ExecOpenTimeout,
		"call-timeout":      c.CallTimeout,
		"drain-timeout":     c.DrainTimeout,
		"sync-interval":     c.SyncInterval,
	} {
		if d <= 0 {
			fail(field, "has to be positive")
		}
	}
	if len(c.TokenAudiences) == 0 || slices.Contains(c.TokenAudiences, "") {
		fail("token-audiences", "at least one audience is required, and none may be empty")
	}
	if c.Guard.Namespace == "" {
		fail("guard-namespace", "is required")
	}
	if len(errs) == 0 {
		return nil
	}
	slices.SortStableFunc(errs, func(a, b error) int { return strings.Compare(a.Error(), b.Error()) })
	return fmt.Errorf("execagent: invalid configuration: %w", errors.Join(errs...))
}
