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
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

// servingCert serves the exec API's certificate and follows its files: a
// certificate renewed on disk is picked up at the next handshake, as long
// as it still names the agent's address. A renewal that does not, or that
// cannot be read, is logged and the certificate in use is kept.
type servingCert struct {
	files   ServerTLS
	address net.IP
	log     logr.Logger

	mu       sync.Mutex
	cert     *tls.Certificate
	modified time.Time
}

//= docs/requirements/05-exec-agent.md#serving
//# The Exec Agent SHALL serve its exec API over TLS on the Host's
//# internal address, with a serving certificate that names that address.

// newServingCert loads the serving certificate and refuses one that does
// not name address as an IP address: a consumer dials the Exec Agent by
// the Host's internal address and verifies the certificate against it
// (EA-002), so a certificate for any other name would fail every request,
// and failing at start says why.
func newServingCert(files ServerTLS, address string, log logr.Logger) (*servingCert, error) {
	ip := net.ParseIP(address)
	if ip == nil {
		return nil, fmt.Errorf("execagent: the exec API's address %q is not an IP address", address)
	}
	s := &servingCert{files: files, address: ip, log: log}
	cert, modified, err := s.load()
	if err != nil {
		return nil, err
	}
	s.cert, s.modified = cert, modified
	return s, nil
}

// load reads the certificate and key and checks the certificate names the
// address.
func (s *servingCert) load() (*tls.Certificate, time.Time, error) {
	info, err := os.Stat(s.files.CertFile)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("execagent: reading the serving certificate: %w", err)
	}
	cert, err := tls.LoadX509KeyPair(s.files.CertFile, s.files.KeyFile)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("execagent: loading the serving certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("execagent: parsing the serving certificate: %w", err)
	}
	if err := leaf.VerifyHostname(s.address.String()); err != nil {
		return nil, time.Time{}, fmt.Errorf("execagent: the serving certificate does not name the exec API's address %s: %w", s.address, err)
	}
	cert.Leaf = leaf
	return &cert, info.ModTime(), nil
}

// get is tls.Config.GetCertificate.
func (s *servingCert) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if info, err := os.Stat(s.files.CertFile); err == nil && !info.ModTime().Equal(s.modified) {
		cert, modified, err := s.load()
		if err != nil {
			s.log.Error(err, "Kept the serving certificate in use: the renewed one cannot be used")
			s.modified = info.ModTime()
		} else {
			s.cert, s.modified = cert, modified
		}
	}
	if s.cert == nil {
		return nil, errors.New("execagent: no serving certificate")
	}
	return s.cert, nil
}

// tlsConfig is the TLS configuration of the exec API. Clients are not
// asked for certificates: they authenticate with a bearer token.
func (s *servingCert) tlsConfig() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: s.get,
		NextProtos:     []string{"h2"},
	}
}
