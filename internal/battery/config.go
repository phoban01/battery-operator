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

package battery

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"time"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// DefaultAddress is where the Operator looks for battery when no address is
// configured: loopback, because battery is a sidecar in the Operator's pod
// (DP-001). Port 8443, battery's own example, is the Operator's metrics
// port, so battery listens on 50051 instead.
const DefaultAddress = "127.0.0.1:50051"

// Defaults for the settings a Config leaves zero. The keepalive values suit
// a long-lived control-plane connection: often enough to notice a dead
// peer, rare enough not to trip a server's minimum ping interval. battery
// sets no keepalive enforcement policy, so it runs grpc-go's default
// MinTime of 5 minutes, and a client that pings more often eventually gets
// GOAWAY ENHANCE_YOUR_CALM ("too_many_pings") and has to reconnect.
const (
	DefaultCallTimeout      = 10 * time.Second
	DefaultKeepaliveTime    = 6 * time.Minute
	DefaultKeepaliveTimeout = 10 * time.Second
	DefaultReconnectBase    = time.Second
	DefaultReconnectMax     = 30 * time.Second
	// minConnectTimeout is the least time one connection attempt gets for
	// its TCP, TLS and HTTP/2 handshake: grpc-go's own default. With
	// WithConnectParams an attempt's deadline is MinConnectTimeout or the
	// current backoff, whichever is longer, so a small ReconnectMax would
	// otherwise cut a handshake short on a busy machine and leave the client
	// retrying a connection it had nearly made.
	minConnectTimeout = 20 * time.Second
)

// Config is what Dial needs to reach battery.
type Config struct {
	// Address is battery's gRPC address, host:port, on loopback.
	Address string
	// TLS is the client's TLS material. The connection is plaintext when it
	// is empty, which is what a sidecar on loopback needs.
	TLS TLSConfig
	// CallTimeout is the deadline of every unary call. The Subscribe stream
	// is not a unary call and lives as long as its caller's context.
	CallTimeout time.Duration
	// KeepaliveTime and KeepaliveTimeout are the connection's HTTP/2
	// keepalive settings.
	KeepaliveTime    time.Duration
	KeepaliveTimeout time.Duration
	// ReconnectBase and ReconnectMax bound the exponential backoff with
	// which a lost connection is re-established.
	ReconnectBase time.Duration
	ReconnectMax  time.Duration
}

// TLSConfig is the client's TLS material, as file paths. The server's
// certificate is always verified: against CAFile when it is set, against
// the system roots otherwise.
type TLSConfig struct {
	CAFile   string
	CertFile string
	KeyFile  string
}

// enabled reports whether any TLS material is configured.
func (t TLSConfig) enabled() bool {
	return t.CAFile != "" || t.CertFile != "" || t.KeyFile != ""
}

// BindFlags binds c to the Operator's command line flags.
func (c *Config) BindFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.Address, "battery-address", DefaultAddress,
		"battery's gRPC address, host:port. It has to be a loopback address: battery runs as a sidecar.")
	fs.DurationVar(&c.CallTimeout, "battery-call-timeout", DefaultCallTimeout,
		"The deadline of every unary call to battery.")
	fs.StringVar(&c.TLS.CAFile, "battery-ca-file", "",
		"The CA that verifies battery's serving certificate. Setting any TLS file turns TLS on.")
	fs.StringVar(&c.TLS.CertFile, "battery-cert-file", "",
		"The client certificate the Operator presents to battery, with --battery-key-file.")
	fs.StringVar(&c.TLS.KeyFile, "battery-key-file", "",
		"The key of --battery-cert-file.")
	fs.DurationVar(&c.KeepaliveTime, "battery-keepalive-time", DefaultKeepaliveTime,
		"How long the battery connection may be idle before the Operator pings it.")
	fs.DurationVar(&c.KeepaliveTimeout, "battery-keepalive-timeout", DefaultKeepaliveTimeout,
		"How long the Operator waits for a keepalive ping's answer before it closes the battery connection.")
	fs.DurationVar(&c.ReconnectBase, "battery-reconnect-base", DefaultReconnectBase,
		"The first delay before reconnecting to battery; it grows exponentially.")
	fs.DurationVar(&c.ReconnectMax, "battery-reconnect-max", DefaultReconnectMax,
		"The longest delay between attempts to reconnect to battery.")
}

// withDefaults returns c with its zero fields filled in.
func (c Config) withDefaults() Config {
	if c.CallTimeout <= 0 {
		c.CallTimeout = DefaultCallTimeout
	}
	if c.KeepaliveTime <= 0 {
		c.KeepaliveTime = DefaultKeepaliveTime
	}
	if c.KeepaliveTimeout <= 0 {
		c.KeepaliveTimeout = DefaultKeepaliveTimeout
	}
	if c.ReconnectBase <= 0 {
		c.ReconnectBase = DefaultReconnectBase
	}
	if c.ReconnectMax <= 0 {
		c.ReconnectMax = DefaultReconnectMax
	}
	return c
}

// Validate reports the first thing wrong with c.
func (c Config) Validate() error {
	if err := validateLoopback(c.Address); err != nil {
		return err
	}
	if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
		return errors.New("battery: the client certificate and key have to be set together")
	}
	return nil
}

//= docs/requirements/06-deployment.md#battery-connection
//# The Operator SHALL call battery over gRPC on the loopback
//# address of DP-001, with a deadline on every unary call.

// validateLoopback accepts host:port where host is localhost or a loopback
// IP, and nothing else: battery's API has to stay private to the pod.
func validateLoopback(address string) error {
	if address == "" {
		return errors.New("battery: an address is required")
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("battery: address %q: %w", address, err)
	}
	if port == "" {
		return fmt.Errorf("battery: address %q has no port", address)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("battery: address %q is not a loopback address; battery runs as a sidecar and listens on loopback only", address)
}

// transportCredentials builds the connection's credentials: plaintext when
// no TLS material is configured, otherwise TLS 1.2 or better with the
// configured CA, or the system roots when none is given, and a client
// certificate when one is configured. Server certificates are always
// verified.
func transportCredentials(t TLSConfig) (credentials.TransportCredentials, error) {
	if !t.enabled() {
		return insecure.NewCredentials(), nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if t.CAFile != "" {
		pem, err := os.ReadFile(t.CAFile)
		if err != nil {
			return nil, fmt.Errorf("battery: reading the CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("battery: CA file %s holds no certificate", t.CAFile)
		}
		cfg.RootCAs = pool
	}
	if t.CertFile != "" {
		pair, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("battery: loading the client certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return credentials.NewTLS(cfg), nil
}
