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

package execagent_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/execagent"
	"github.com/phoban01/battery-operator/internal/fakeflintlock"
	"github.com/phoban01/battery-operator/internal/hostcert"
)

// The tests here run the Exec Agent against a fake API server whose
// signer approves and signs every CertificateSigningRequest at once, from
// throwaway CAs, recording the requester as the API server does. Whether
// the Operator signs a request is internal/controller's to test; the e2e
// suite has the Operator sign the agent's real requests (test/e2e,
// TestHostCertificates).

//= docs/requirements/05-exec-agent.md#certificates
//= type=test
//# The Exec Agent SHALL generate its Host's `flintlockd` serving
//# key and its own client key on the Host, and SHALL NOT send either key off
//# the Host.

//= docs/requirements/05-exec-agent.md#certificates
//= type=test
//# The Exec Agent SHALL obtain its Host's `flintlockd` serving
//# certificate with a `CertificateSigningRequest` for the signer
//# `battery.liquidmetal-x.dev/flintlockd-serving` that names the Host's
//# internal address and the SPIFFE ID
//# `spiffe://<trust domain>/flintlock/host/<node name>`, and nothing else.

//= docs/requirements/05-exec-agent.md#certificates
//= type=test
//# The Exec Agent SHALL obtain its own client certificate with a
//# `CertificateSigningRequest` for the signer
//# `battery.liquidmetal-x.dev/flintlockd-client` that names the SPIFFE ID
//# `spiffe://<trust domain>/flintlock/client/exec-agent/<node name>`, and
//# nothing else.

//= docs/requirements/05-exec-agent.md#certificates
//= type=test
//# The Exec Agent SHALL obtain the serving certificate of its exec
//# API with a `CertificateSigningRequest` for the signer
//# `battery.liquidmetal-x.dev/exec-agent-serving`, for a key generated on the
//# Host, that names the Host's internal address and
//# `spiffe://<trust domain>/flintlock/client/exec-agent/<node name>`, and
//# nothing else.

// TestAgentRequestsItsCertificates checks the three requests the agent
// makes when it starts: one per signer name, each naming exactly what its
// requirement says, each for a key the agent generated and holds, and none
// carrying anything but the request; and that the agent then serves the
// exec API with the certificate it obtained.
func TestAgentRequestsItsCertificates(t *testing.T) {
	t.Parallel()
	h := newHost(t, hostOptions{})
	leaves := h.Certificates().Current()
	flintlockdKey := readKey(t, filepath.Join(h.CertDir, execagent.FlintlockdKeyFile))

	requests := agentRequests(t, h)
	want := map[string]struct {
		uri    string
		ip     bool
		usages []certificatesv1.KeyUsage
		public crypto.PublicKey
	}{
		hostcert.ServingSigner: {
			uri: hostcert.HostID(trustDomain, h.Node), ip: true,
			usages: serverUsages, public: flintlockdKey.Public(),
		},
		hostcert.ClientSigner: {
			uri:    hostcert.ExecAgentID(trustDomain, h.Node),
			usages: clientUsages, public: leaves.FlintlockdClient.PublicKey,
		},
		hostcert.ExecAgentServingSigner: {
			uri: hostcert.ExecAgentID(trustDomain, h.Node), ip: true,
			usages: serverUsages, public: leaves.Serving.PublicKey,
		},
	}
	if len(requests) != len(want) {
		t.Fatalf("the agent made %d requests, want one for each of %d signers", len(requests), len(want))
	}
	for _, csr := range requests {
		w, ok := want[csr.Spec.SignerName]
		if !ok {
			t.Errorf("the agent requested a certificate from the signer %s", csr.Spec.SignerName)
			continue
		}
		delete(want, csr.Spec.SignerName)
		block, rest := pem.Decode(csr.Spec.Request)
		if block == nil || block.Type != "CERTIFICATE REQUEST" || len(bytes.TrimSpace(rest)) > 0 {
			t.Errorf("%s: spec.request holds more than one certificate request", csr.Spec.SignerName)
			continue
		}
		req, err := x509.ParseCertificateRequest(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		uris := make([]string, 0, len(req.URIs))
		for _, u := range req.URIs {
			uris = append(uris, u.String())
		}
		if !slices.Equal(uris, []string{w.uri}) || len(req.DNSNames) > 0 || len(req.EmailAddresses) > 0 {
			t.Errorf("%s: the request names the URIs %v, DNS names %v and email addresses %v, want only %s",
				csr.Spec.SignerName, uris, req.DNSNames, req.EmailAddresses, w.uri)
		}
		wantIPs := 0
		if w.ip {
			wantIPs = 1
		}
		if len(req.IPAddresses) != wantIPs || w.ip && req.IPAddresses[0].String() != hostAddress {
			t.Errorf("%s: the request names the addresses %v", csr.Spec.SignerName, req.IPAddresses)
		}
		if !slices.Equal(csr.Spec.Usages, w.usages) {
			t.Errorf("%s: the request's usages are %v, want %v", csr.Spec.SignerName, csr.Spec.Usages, w.usages)
		}
		if !publicKeyEqual(req.PublicKey, w.public) {
			t.Errorf("%s: the request is not for the key the agent holds", csr.Spec.SignerName)
		}
	}
	for signer := range want {
		t.Errorf("the agent made no request for the signer %s", signer)
	}

	// The exec API serves the certificate from exec-agent-serving, which a
	// consumer verifies against the published serving CA for the Host's
	// address.
	caPEM, err := os.ReadFile(h.ServingCAFile)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caPEM)
	conn, err := tls.Dial("tcp", h.Address, &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2"}})
	if err != nil {
		t.Fatalf("connecting to the exec API: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if peer := conn.ConnectionState().PeerCertificates[0]; !peer.Equal(leaves.Serving) {
		t.Errorf("the exec API serves %v, not the certificate the agent obtained", peer.Subject)
	}
}

//= docs/requirements/05-exec-agent.md#certificates
//= type=test
//# The Exec Agent SHALL write its Host's `flintlockd` serving
//# certificate, its key and the `flintlockd` client CA bundle to the
//# configured Host path, replacing each file atomically and leaving the key
//# readable only by its owner.

//= docs/requirements/05-exec-agent.md#certificates
//= type=test
//# The Exec Agent SHALL keep its own client key in memory only.

//= docs/requirements/05-exec-agent.md#certificates
//= type=test
//# The Exec Agent SHALL verify its Host's `flintlockd` serving
//# certificate against the serving CA certificate the Operator publishes.

// TestAgentDeliversFlintlockdCertificates checks what the agent leaves on
// the Host: flintlockd's certificate, its key, readable by its owner only,
// and the published client CA bundle, and nothing else, no temporary file
// and no other key, so that its client key is on no disk. A fake flintlockd
// started on those files, as the Host Image starts flintlockd, answers the
// agent over mutual TLS with its client certificate, and the agent verifies
// it against the published serving CA: the Host is reported ready. An agent
// restarted keeps flintlockd's certificate, so that flintlockd is not
// restarted with it, and obtains a new client certificate.
func TestAgentDeliversFlintlockdCertificates(t *testing.T) {
	t.Parallel()
	h := newHost(t, hostOptions{})
	awaitReady(t, h)

	entries, err := os.ReadDir(h.CertDir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if want := []string{execagent.FlintlockdClientCAFile, execagent.FlintlockdCertFile, execagent.FlintlockdKeyFile}; !slices.Equal(names, want) {
		t.Errorf("the agent left %v on the Host, want exactly %v", names, want)
	}
	info, err := os.Stat(filepath.Join(h.CertDir, execagent.FlintlockdKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("flintlockd's key has mode %v, want 0600", info.Mode().Perm())
	}
	pair, err := tls.LoadX509KeyPair(filepath.Join(h.CertDir, execagent.FlintlockdCertFile), filepath.Join(h.CertDir, execagent.FlintlockdKeyFile))
	if err != nil {
		t.Fatalf("flintlockd's certificate and key do not go together: %v", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if !leaf.Equal(h.Certificates().Current().FlintlockdServing) {
		t.Error("flintlockd's certificate on the Host is not the one the agent obtained")
	}
	cm, err := h.Kube.CoreV1().ConfigMaps(agentNamespace).Get(context.Background(), hostcert.CABundleConfigMap, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := os.ReadFile(filepath.Join(h.CertDir, execagent.FlintlockdClientCAFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(bundle) != cm.Data[hostcert.ClientCAKey] {
		t.Error("the client CA bundle on the Host is not the one the Operator publishes")
	}

	// The client key: no key the agent can write is it.
	client := h.Certificates().Current().FlintlockdClient
	for path, key := range keysUnder(t, filepath.Dir(h.CertDir)) {
		if publicKeyEqual(key.Public(), client.PublicKey) {
			t.Errorf("the agent's client key is on disk, in %s", path)
		}
	}

	// A restart keeps flintlockd's files as they are.
	before, err := os.ReadFile(filepath.Join(h.CertDir, execagent.FlintlockdCertFile))
	if err != nil {
		t.Fatal(err)
	}
	h.StopAgent()
	h.StartAgent()
	after, err := os.ReadFile(filepath.Join(h.CertDir, execagent.FlintlockdCertFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("a restarted agent replaced flintlockd's certificate, which restarts flintlockd")
	}
	if h.Certificates().Current().FlintlockdClient.Equal(client) {
		t.Error("a restarted agent has its old client certificate, whose key it cannot have kept")
	}
	awaitReady(t, h)
}

// TestAgentRefusesFlintlockdFromAnotherCA checks the other side of EA-066:
// a flintlockd whose serving certificate names the Host's address but was
// not signed by the published serving CA is not talked to, and the Host is
// reported not ready.
func TestAgentRefusesFlintlockdFromAnotherCA(t *testing.T) {
	t.Parallel()
	impostor, err := fakeflintlock.WriteTestCerts(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The impostor asks for no client certificate, so only the agent's own
	// verification can refuse it.
	h := newHost(t, hostOptions{FlintlockdTLS: impostor.ServerTLS(false)})
	eventually(t, "the agent refusing flintlockd's certificate", func() bool {
		a := h.ReadNode().Annotations
		return a[execagent.AnnotationReady] == "false" && a[execagent.AnnotationReason] == execagent.ReasonFlintlockdNotReady &&
			strings.Contains(a[execagent.AnnotationMessage], "certificate")
	})
}

//= docs/requirements/05-exec-agent.md#certificates
//= type=test
//# The Exec Agent SHALL request a replacement for each certificate
//# before two thirds of the current one's validity have passed.

// TestAgentRenewsBeforeTwoThirds drives the agent's renewal clock: halfway
// through the certificates' validity nothing is renewed; at two thirds of
// each, every one has been, flintlockd's is replaced on the Host, and the
// Host stays ready.
func TestAgentRenewsBeforeTwoThirds(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Now())
	h := newHost(t, hostOptions{CertificateClock: clk})
	awaitReady(t, h)
	first := h.Certificates().Current()
	firstFile, err := os.ReadFile(filepath.Join(h.CertDir, execagent.FlintlockdCertFile))
	if err != nil {
		t.Fatal(err)
	}
	all := []*x509.Certificate{first.FlintlockdServing, first.FlintlockdClient, first.Serving}
	at := func(frac float64, pick func(a, b time.Time) bool) time.Time {
		var t0 time.Time
		for _, c := range all {
			t1 := c.NotBefore.Add(time.Duration(float64(c.NotAfter.Sub(c.NotBefore)) * frac))
			if t0.IsZero() || pick(t1, t0) {
				t0 = t1
			}
		}
		return t0
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	requests := len(agentRequests(t, h))

	if err := clk.BlockUntil(waitCtx, 1); err != nil {
		t.Fatal(err)
	}
	clk.Set(at(0.5, time.Time.Before))
	time.Sleep(500 * time.Millisecond)
	if n := len(agentRequests(t, h)); n != requests {
		t.Errorf("halfway through their validity the agent made %d new requests, want none", n-requests)
	}

	clk.Set(at(2.0/3, time.Time.After))
	eventually(t, "every certificate renewed", func() bool {
		now := h.Certificates().Current()
		return !now.FlintlockdServing.Equal(first.FlintlockdServing) &&
			!now.FlintlockdClient.Equal(first.FlintlockdClient) && !now.Serving.Equal(first.Serving)
	})
	if n := len(agentRequests(t, h)); n != requests+3 {
		t.Errorf("the agent made %d new requests to renew three certificates", n-requests)
	}
	now, err := os.ReadFile(filepath.Join(h.CertDir, execagent.FlintlockdCertFile))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(now, firstFile) {
		t.Error("flintlockd's certificate on the Host was not replaced")
	}
	if _, err := tls.LoadX509KeyPair(filepath.Join(h.CertDir, execagent.FlintlockdCertFile), filepath.Join(h.CertDir, execagent.FlintlockdKeyFile)); err != nil {
		t.Errorf("flintlockd's renewed certificate and key do not go together: %v", err)
	}
	awaitReady(t, h)
}

var (
	serverUsages = []certificatesv1.KeyUsage{
		certificatesv1.UsageDigitalSignature, certificatesv1.UsageKeyEncipherment, certificatesv1.UsageServerAuth,
	}
	clientUsages = []certificatesv1.KeyUsage{
		certificatesv1.UsageDigitalSignature, certificatesv1.UsageKeyEncipherment, certificatesv1.UsageClientAuth,
	}
)

// agentRequests are the CertificateSigningRequests the Exec Agent of h
// has made.
func agentRequests(t *testing.T, h *testHost) []certificatesv1.CertificateSigningRequest {
	t.Helper()
	var out []certificatesv1.CertificateSigningRequest
	for _, csr := range h.Requests(t) {
		if csr.Spec.Username == agentUser && slices.Equal(csr.Spec.Extra[hostcert.NodeNameExtra], certificatesv1.ExtraValue{h.Node}) {
			out = append(out, csr)
		}
	}
	return out
}

// awaitReady waits until the Host's Node report says it is ready.
func awaitReady(t *testing.T, h *testHost) {
	t.Helper()
	eventually(t, "the Host reported ready", func() bool {
		return h.ReadNode().Annotations[execagent.AnnotationReason] == execagent.ReasonReady
	})
}

// readKey reads a PEM private key.
func readKey(t *testing.T, path string) crypto.Signer {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	key, ok := parseKey(data)
	if !ok {
		t.Fatalf("%s holds no private key", path)
	}
	return key
}

// keysUnder returns every private key in a PEM file under dir.
func keysUnder(t *testing.T, dir string) map[string]crypto.Signer {
	t.Helper()
	keys := map[string]crypto.Signer{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if key, ok := parseKey(data); ok {
			keys[path] = key
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

// parseKey parses the first private key block in data.
func parseKey(data []byte) (crypto.Signer, bool) {
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return nil, false
		}
		if !strings.Contains(block.Type, "PRIVATE KEY") {
			continue
		}
		if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
			if s, ok := k.(crypto.Signer); ok {
				return s, true
			}
		}
		if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
			return k, true
		}
	}
}

// publicKeyEqual reports whether a and b are the same public key.
func publicKeyEqual(a, b crypto.PublicKey) bool {
	eq, ok := a.(interface{ Equal(crypto.PublicKey) bool })
	return ok && eq.Equal(b)
}
