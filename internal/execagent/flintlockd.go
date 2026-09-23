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
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"time"

	mvmv1 "github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1"
	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	"github.com/phoban01/battery-operator/internal/hostcheck"
)

// Keepalive of the connection to flintlockd. It is a connection within the
// Host, so the pings are cheap, and they are what notices a flintlockd that
// died with streams open.
const (
	flintlockdKeepaliveTime    = 30 * time.Second
	flintlockdKeepaliveTimeout = 10 * time.Second
)

// Flintlockd is the Exec Agent's connection to the flintlockd on its own
// Host. The agent relays requests to it message for message, so it holds
// the generated clients.
type Flintlockd struct {
	conn *grpc.ClientConn
	vms  mvmv1.MicroVMClient
	exec execv1.MicroVMExecClient
}

//= docs/requirements/05-exec-agent.md#serving
//# The Exec Agent SHALL reach only its own Host's `flintlockd`,
//# over TLS with its client certificate for `flintlockd`, and SHALL refuse to
//# start with a `flintlockd` endpoint that is not an address of its own Host.

// DialFlintlockd connects to flintlockd at endpoint over mutual TLS, and
// refuses an endpoint that is not an IP address of this Host, one of
// hostAddrs, and a port. flintlockd serves on the Host's internal address
// and admits any certificate its client CA signed (ADR 0002), so the check
// is what keeps an Exec Agent to its own Host's flintlockd; it is made here
// as well as in Config.Validate.
//
// The agent presents the client certificate in files, and verifies
// flintlockd's serving certificate against the CA in files, for the
// endpoint's address. Until #31 the files are provisioned on the Host; #31
// replaces them with certificates the agent requests itself.
func DialFlintlockd(endpoint string, files ClientTLS, hostAddrs []net.IP) (*Flintlockd, error) {
	if err := hostcheck.ValidateHostEndpoint(endpoint, hostAddrs); err != nil {
		return nil, fmt.Errorf("execagent: flintlockd endpoint %w", err)
	}
	tlsCfg, err := flintlockdTLS(files)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(endpoint,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                flintlockdKeepaliveTime,
			Timeout:             flintlockdKeepaliveTimeout,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("execagent: dialling flintlockd at %s: %w", endpoint, err)
	}
	return &Flintlockd{conn: conn, vms: mvmv1.NewMicroVMClient(conn), exec: execv1.NewMicroVMExecClient(conn)}, nil
}

// flintlockdTLS is the client side of the mutual TLS to flintlockd. The
// server name is left to gRPC, which takes it from the endpoint, so the
// serving certificate has to name the Host's address.
func flintlockdTLS(files ClientTLS) (*tls.Config, error) {
	if files.CertFile == "" || files.KeyFile == "" || files.CAFile == "" {
		return nil, fmt.Errorf("execagent: flintlockd is reached over mutual TLS: a client certificate, its key and a CA are required")
	}
	cert, err := tls.LoadX509KeyPair(files.CertFile, files.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("execagent: loading the client certificate for flintlockd: %w", err)
	}
	pem, err := os.ReadFile(files.CAFile)
	if err != nil {
		return nil, fmt.Errorf("execagent: reading flintlockd's serving CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("execagent: no certificates in flintlockd's serving CA file %s", files.CAFile)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
	}, nil
}

// Close closes the connection; streams still open on it fail.
func (f *Flintlockd) Close() error { return f.conn.Close() }
