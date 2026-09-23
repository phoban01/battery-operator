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
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-logr/logr"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/hostcert"
)

// The files the agent writes flintlockd's certificates to, in the
// configured directory (EA-064). FlintlockdCertFile is written last, so
// that whatever restarts flintlockd when it changes finds the key and the
// client CA bundle that go with it already in place.
const (
	FlintlockdCertFile     = "tls.crt"
	FlintlockdKeyFile      = "tls.key"
	FlintlockdClientCAFile = "client-ca.crt"
)

// pemCertificate is the PEM block type of a certificate.
const pemCertificate = "CERTIFICATE"

const (
	// renewFrom and renewSpread place a certificate's renewal at a random
	// point from 55% to 65% of its validity, always before two thirds have
	// passed (EA-063), and spread so that the Hosts do not all renew at
	// once.
	renewFrom   = 0.55
	renewSpread = 0.10
	// minRenewInterval is the shortest time between obtaining a
	// certificate and renewing it.
	minRenewInterval = time.Minute
	// caResync is how often the published CA certificates are read again
	// between renewals.
	caResync = 5 * time.Minute
	// certRetry paces new attempts at a certificate after a failure: a CA
	// bundle not yet published, a request denied or an API server that
	// does not answer.
	certRetryBase = time.Second
	certRetryMax  = time.Minute
	// signedPoll paces the reads of a request while it waits to be signed.
	signedPollBase = 100 * time.Millisecond
	signedPollMax  = 2 * time.Second
)

// certKind is one of the three certificates the agent holds.
type certKind int

const (
	// flintlockdServing is its Host's flintlockd serving certificate
	// (EA-061), written to the Host for flintlockd.
	flintlockdServing certKind = iota
	// flintlockdClient is the agent's own client certificate for
	// flintlockd (EA-062), held in memory.
	flintlockdClient
	// agentServing is the serving certificate of the exec API (EA-068),
	// held in memory.
	agentServing
	numCertKinds
)

func (k certKind) String() string {
	switch k {
	case flintlockdServing:
		return "flintlockd-serving"
	case flintlockdClient:
		return "flintlockd-client"
	case agentServing:
		return "exec-agent-serving"
	}
	return "unknown"
}

// certSpec is what one certificate is requested for.
type certSpec struct {
	signer string
	uri    *url.URL
	// ip is the address a serving certificate names; nil for the client
	// certificate.
	ip     net.IP
	usages []certificatesv1.KeyUsage
	// extKeyUsage is what the certificate is verified for.
	extKeyUsage x509.ExtKeyUsage
}

// held is a certificate the agent holds, with its key, and when it is to be
// renewed.
type held struct {
	cert    *tls.Certificate
	renewAt time.Time
}

// Certificates obtains, renews and holds the Exec Agent's certificates
// through CertificateSigningRequests that the Operator approves and signs
// (ADR 0003, ADR 0004): its Host's flintlockd serving certificate, which it
// writes to the Host, and its own client certificate for flintlockd and
// serving certificate for the exec API, which it keeps in memory. It also
// holds the CA certificates the Operator publishes. Run starts it; until
// then it holds nothing, and every handshake that needs it fails.
type Certificates struct {
	kube kubernetes.Interface
	cfg  *Config
	clk  clock.Clock

	mu sync.RWMutex
	// specs are what each certificate is requested for, set by obtain.
	specs [numCertKinds]certSpec
	certs [numCertKinds]*held
	// servingCAs and clientCAs are the published CA certificates;
	// clientCAPEM is the client CA bundle as written for flintlockd.
	servingCAs  *x509.CertPool
	clientCAs   *x509.CertPool
	clientCAPEM []byte
	servingPEM  []byte
}

// NewCertificates returns the agent's Certificates, which request their
// certificates through kube with the agent's own identity. clk schedules
// renewals; nil is the real clock.
func NewCertificates(kube kubernetes.Interface, cfg *Config, clk clock.Clock) *Certificates {
	if clk == nil {
		clk = clock.Real{}
	}
	return &Certificates{kube: kube, cfg: cfg, clk: clk}
}

// Leaves are the certificates the agent holds, without their keys.
type Leaves struct {
	// FlintlockdServing is its Host's flintlockd serving certificate, as
	// written to the Host.
	FlintlockdServing *x509.Certificate
	// FlintlockdClient is the agent's client certificate for flintlockd.
	FlintlockdClient *x509.Certificate
	// Serving is the exec API's serving certificate.
	Serving *x509.Certificate
}

// Current returns the certificates the agent holds now; a field is nil
// until its certificate is obtained.
func (c *Certificates) Current() Leaves {
	c.mu.RLock()
	defer c.mu.RUnlock()
	leaf := func(k certKind) *x509.Certificate {
		if c.certs[k] == nil {
			return nil
		}
		return c.certs[k].cert.Leaf
	}
	return Leaves{FlintlockdServing: leaf(flintlockdServing), FlintlockdClient: leaf(flintlockdClient), Serving: leaf(agentServing)}
}

// obtain gets every certificate the agent needs, waiting as long as ctx
// allows for the Operator to publish its CA certificates and sign the
// requests: flintlockd's serving certificate, reusing the one already on
// the Host while it has no need of renewal, and the agent's client and
// serving certificates, always new. address is the address the exec API is
// served on.
func (c *Certificates) obtain(ctx context.Context, address string) error {
	if err := c.setSpecs(address); err != nil {
		return err
	}
	if err := c.retry(ctx, "read the published CA certificates", c.refreshCAs); err != nil {
		return err
	}
	if err := c.retry(ctx, "write the client CA bundle for flintlockd", func(context.Context) error { return c.writeClientCA() }); err != nil {
		return err
	}
	if h := c.reuseFlintlockdServing(ctx); h != nil {
		c.set(flintlockdServing, h)
	}
	for k := range numCertKinds {
		if c.get(k) != nil {
			continue
		}
		if err := c.retry(ctx, "obtain the "+k.String()+" certificate", func(ctx context.Context) error { return c.renew(ctx, k) }); err != nil {
			return err
		}
	}
	return nil
}

// setSpecs works out what each certificate is requested for.
func (c *Certificates) setSpecs(address string) error {
	agentIP := net.ParseIP(address)
	if agentIP == nil {
		return fmt.Errorf("execagent: the exec API's address %q is not an IP address", address)
	}
	host, _, err := net.SplitHostPort(c.cfg.Flintlockd)
	if err != nil {
		return fmt.Errorf("execagent: flintlockd endpoint %q: %w", c.cfg.Flintlockd, err)
	}
	flIP := net.ParseIP(host)
	if flIP == nil {
		return fmt.Errorf("execagent: flintlockd endpoint %q is not at an IP address", c.cfg.Flintlockd)
	}
	hostID, err := url.Parse(hostcert.HostID(c.cfg.TrustDomain, c.cfg.HostNode))
	if err != nil {
		return err
	}
	agentID, err := url.Parse(hostcert.ExecAgentID(c.cfg.TrustDomain, c.cfg.HostNode))
	if err != nil {
		return err
	}
	serverUsages := []certificatesv1.KeyUsage{
		certificatesv1.UsageDigitalSignature, certificatesv1.UsageKeyEncipherment, certificatesv1.UsageServerAuth,
	}
	clientUsages := []certificatesv1.KeyUsage{
		certificatesv1.UsageDigitalSignature, certificatesv1.UsageKeyEncipherment, certificatesv1.UsageClientAuth,
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	//= docs/requirements/05-exec-agent.md#certificates
	//# The Exec Agent SHALL obtain its Host's `flintlockd` serving
	//# certificate with a `CertificateSigningRequest` for the signer
	//# `battery.liquidmetal-x.dev/flintlockd-serving` that names the Host's
	//# internal address and the SPIFFE ID
	//# `spiffe://<trust domain>/flintlock/host/<node name>`, and nothing else.
	c.specs[flintlockdServing] = certSpec{
		signer: hostcert.ServingSigner, uri: hostID, ip: flIP, usages: serverUsages, extKeyUsage: x509.ExtKeyUsageServerAuth,
	}

	//= docs/requirements/05-exec-agent.md#certificates
	//# The Exec Agent SHALL obtain its own client certificate with a
	//# `CertificateSigningRequest` for the signer
	//# `battery.liquidmetal-x.dev/flintlockd-client` that names the SPIFFE ID
	//# `spiffe://<trust domain>/flintlock/client/exec-agent/<node name>`, and
	//# nothing else.
	c.specs[flintlockdClient] = certSpec{
		signer: hostcert.ClientSigner, uri: agentID, usages: clientUsages, extKeyUsage: x509.ExtKeyUsageClientAuth,
	}

	//= docs/requirements/05-exec-agent.md#certificates
	//# The Exec Agent SHALL obtain the serving certificate of its exec
	//# API with a `CertificateSigningRequest` for the signer
	//# `battery.liquidmetal-x.dev/exec-agent-serving`, for a key generated on the
	//# Host, that names the Host's internal address and
	//# `spiffe://<trust domain>/flintlock/client/exec-agent/<node name>`, and
	//# nothing else.
	c.specs[agentServing] = certSpec{
		signer: hostcert.ExecAgentServingSigner, uri: agentID, ip: agentIP, usages: serverUsages, extKeyUsage: x509.ExtKeyUsageServerAuth,
	}
	return nil
}

// keepRenewed renews each certificate when it is due, and reads the
// published CA certificates again every caResync, until ctx ends. A renewal
// that fails leaves the certificate in use and is tried again.
func (c *Certificates) keepRenewed(ctx context.Context) {
	log := logr.FromContextOrDiscard(ctx)
	attempts := [numCertKinds]int{}
	for {
		next := c.clk.Now().Add(caResync)
		for k := range numCertKinds {
			if h := c.get(k); h != nil && h.renewAt.Before(next) {
				next = h.renewAt
			}
		}
		timer := c.clk.NewTimer(next.Sub(c.clk.Now()))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C():
		}

		if err := c.refreshCAs(ctx); err != nil {
			log.Error(err, "Failed to read the published CA certificates; kept the ones in use")
		} else if err := c.writeClientCA(); err != nil {
			log.Error(err, "Failed to write the client CA bundle for flintlockd", "directory", c.cfg.FlintlockdCertDir)
		}
		now := c.clk.Now()
		for k := range numCertKinds {
			h := c.get(k)
			if h == nil || now.Before(h.renewAt) {
				continue
			}
			if err := c.renew(ctx, k); err != nil {
				if ctx.Err() != nil {
					return
				}
				wait := clock.Exponential{Base: certRetryBase, Max: certRetryMax}.Next(attempts[k])
				attempts[k]++
				log.Error(err, "Failed to renew a certificate; kept the one in use", "certificate", k.String(), "retryIn", wait)
				c.set(k, &held{cert: h.cert, renewAt: now.Add(wait)})
				continue
			}
			attempts[k] = 0
		}
	}
}

// retry runs what until it succeeds or ctx ends, logging each failure and
// waiting a little longer after each.
func (c *Certificates) retry(ctx context.Context, what string, fn func(context.Context) error) error {
	log := logr.FromContextOrDiscard(ctx)
	for attempt := 0; ; attempt++ {
		err := fn(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("execagent: failed to %s: %w", what, err)
		}
		wait := clock.Exponential{Base: certRetryBase, Max: certRetryMax}.Next(attempt)
		log.Error(err, "Failed to "+what+"; retrying", "retryIn", wait)
		select {
		case <-ctx.Done():
			return fmt.Errorf("execagent: failed to %s: %w", what, err)
		case <-c.clk.After(wait):
		}
	}
}

func (c *Certificates) get(k certKind) *held {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.certs[k]
}

func (c *Certificates) set(k certKind, h *held) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.certs[k] = h
}

func (c *Certificates) spec(k certKind) certSpec {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.specs[k]
}

// refreshCAs reads the CA certificates the Operator publishes.
func (c *Certificates) refreshCAs(ctx context.Context) error {
	ref := c.cfg.CABundle
	cm, err := c.kube.CoreV1().ConfigMaps(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading the CA bundle ConfigMap %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	servingPool, servingPEM, err := caPool(cm, hostcert.ServingCAKey)
	if err != nil {
		return err
	}
	clientPool, clientPEM, err := caPool(cm, hostcert.ClientCAKey)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !bytes.Equal(servingPEM, c.servingPEM) || !bytes.Equal(clientPEM, c.clientCAPEM) {
		logr.FromContextOrDiscard(ctx).Info("Read the published CA certificates", "configMap", ref.Namespace+"/"+ref.Name)
	}
	c.servingCAs, c.servingPEM = servingPool, servingPEM
	c.clientCAs, c.clientCAPEM = clientPool, clientPEM
	return nil
}

// caPool parses the certificates under key in the CA bundle ConfigMap, and
// encodes them again, so that nothing but certificates is carried over.
func caPool(cm *corev1.ConfigMap, key string) (*x509.CertPool, []byte, error) {
	data := []byte(cm.Data[key])
	pool := x509.NewCertPool()
	var out bytes.Buffer
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type != pemCertificate {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, nil, fmt.Errorf("parsing %s in the CA bundle ConfigMap %s/%s: %w", key, cm.Namespace, cm.Name, err)
		}
		pool.AddCert(cert)
		_ = pem.Encode(&out, &pem.Block{Type: pemCertificate, Bytes: cert.Raw})
	}
	if out.Len() == 0 {
		return nil, nil, fmt.Errorf("the CA bundle ConfigMap %s/%s has no certificates under %s", cm.Namespace, cm.Name, key)
	}
	return pool, out.Bytes(), nil
}

// renew requests a new certificate of kind k, waits for it to be signed,
// checks it, and puts it in use: flintlockd's is written to the Host.
func (c *Certificates) renew(ctx context.Context, k certKind) error {
	spec := c.spec(k)

	//= docs/requirements/05-exec-agent.md#certificates
	//# The Exec Agent SHALL generate its Host's `flintlockd` serving
	//# key and its own client key on the Host, and SHALL NOT send either key off
	//# the Host.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generating a key: %w", err)
	}
	tmpl := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: c.cfg.HostNode},
		URIs:    []*url.URL{spec.uri},
	}
	if spec.ip != nil {
		tmpl.IPAddresses = []net.IP{spec.ip}
	}
	// Only the request, which carries the public key, leaves the Host.
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		return fmt.Errorf("creating a certificate request: %w", err)
	}
	csr := &certificatesv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "exec-agent-" + c.cfg.HostNode + "-"},
		Spec: certificatesv1.CertificateSigningRequestSpec{
			Request:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}),
			SignerName: spec.signer,
			Usages:     spec.usages,
		},
	}
	if d := c.cfg.CertificateDuration; d > 0 {
		s := int32(d / time.Second)
		csr.Spec.ExpirationSeconds = &s
	}
	csrs := c.kube.CertificatesV1().CertificateSigningRequests()
	created, err := csrs.Create(ctx, csr, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("creating a CertificateSigningRequest for %s: %w", spec.signer, err)
	}
	log := logr.FromContextOrDiscard(ctx).WithValues("certificateSigningRequest", created.Name, "signer", spec.signer)
	log.Info("Requested a certificate")

	chain, err := c.awaitSigned(ctx, created.Name)
	if err != nil {
		return err
	}
	cert, err := c.check(k, chain, key)
	if err != nil {
		return fmt.Errorf("the certificate of CertificateSigningRequest %s: %w", created.Name, err)
	}
	h := &held{cert: cert, renewAt: c.renewAt(cert.Leaf)}
	// A certificate already due when it arrives, because the Operator
	// issued it for very little time, as it does when its CA is about to
	// expire (CT-004), is renewed after a pause, not at once and forever.
	if floor := c.clk.Now().Add(minRenewInterval); h.renewAt.Before(floor) {
		h.renewAt = floor
	}
	if k == flintlockdServing {
		if err := c.writeFlintlockdServing(cert, key); err != nil {
			return err
		}
	}
	c.set(k, h)
	log.Info("Obtained a certificate", "notAfter", cert.Leaf.NotAfter, "renewAt", h.renewAt)
	return nil
}

// awaitSigned polls the request name until the Operator signs it, and
// returns its certificate chain. A request that is denied, or fails, is an
// error: the agent makes a new one.
func (c *Certificates) awaitSigned(ctx context.Context, name string) ([]byte, error) {
	csrs := c.kube.CertificatesV1().CertificateSigningRequests()
	for attempt := 0; ; attempt++ {
		csr, err := csrs.Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			return nil, fmt.Errorf("the CertificateSigningRequest %s was deleted before it was signed", name)
		case err != nil && ctx.Err() != nil:
			return nil, err
		case err == nil:
			for _, cond := range csr.Status.Conditions {
				if (cond.Type == certificatesv1.CertificateDenied || cond.Type == certificatesv1.CertificateFailed) &&
					cond.Status != corev1.ConditionFalse {
					return nil, fmt.Errorf("the CertificateSigningRequest %s is %s: %s: %s", name, cond.Type, cond.Reason, cond.Message)
				}
			}
			if len(csr.Status.Certificate) > 0 {
				return csr.Status.Certificate, nil
			}
		}
		// Real time, not the renewal clock: this is a wait on the API
		// server, which a test's clock does not stand in for.
		wait := clock.Exponential{Base: signedPollBase, Max: signedPollMax}.Next(attempt)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for the CertificateSigningRequest %s to be signed: %w", name, ctx.Err())
		case <-time.After(wait):
		}
	}
}

// check parses a signed certificate chain and refuses it unless it is for
// key, was signed by the published CA of its kind, and names exactly what
// was asked for.
func (c *Certificates) check(k certKind, chainPEM []byte, key crypto.Signer) (*tls.Certificate, error) {
	spec := c.spec(k)
	cert := &tls.Certificate{PrivateKey: key}
	for rest := chainPEM; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type == pemCertificate {
			cert.Certificate = append(cert.Certificate, block.Bytes)
		}
	}
	if len(cert.Certificate) == 0 {
		return nil, errors.New("no certificate")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	cert.Leaf = leaf
	if pub, ok := leaf.PublicKey.(interface{ Equal(crypto.PublicKey) bool }); !ok || !pub.Equal(key.Public()) {
		return nil, errors.New("it is not for the key the agent generated")
	}
	c.mu.RLock()
	roots := c.servingCAs
	if k == flintlockdClient {
		roots = c.clientCAs
	}
	c.mu.RUnlock()
	intermediates := x509.NewCertPool()
	for _, der := range cert.Certificate[1:] {
		if ic, err := x509.ParseCertificate(der); err == nil {
			intermediates.AddCert(ic)
		}
	}
	opts := x509.VerifyOptions{
		Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{spec.extKeyUsage}, CurrentTime: c.clk.Now(),
	}
	if _, err := leaf.Verify(opts); err != nil {
		return nil, fmt.Errorf("it does not verify against the published CA: %w", err)
	}
	if len(leaf.URIs) != 1 || leaf.URIs[0].String() != spec.uri.String() {
		return nil, fmt.Errorf("it names the URIs %v, not %s", leaf.URIs, spec.uri)
	}
	if spec.ip != nil {
		//= docs/requirements/05-exec-agent.md#serving
		//# The Exec Agent SHALL serve its exec API over TLS on the Host's
		//# internal address, with a serving certificate that names that address.
		if err := leaf.VerifyHostname(spec.ip.String()); err != nil {
			return nil, fmt.Errorf("it does not name the address %s: %w", spec.ip, err)
		}
	}
	return cert, nil
}

// renewAt is when a certificate is to be renewed.
func (c *Certificates) renewAt(leaf *x509.Certificate) time.Time {
	//= docs/requirements/05-exec-agent.md#certificates
	//# The Exec Agent SHALL request a replacement for each certificate
	//# before two thirds of the current one's validity have passed.
	validity := leaf.NotAfter.Sub(leaf.NotBefore)
	jitter, err := rand.Int(rand.Reader, big.NewInt(1000))
	if err != nil {
		jitter = big.NewInt(0)
	}
	frac := renewFrom + renewSpread*float64(jitter.Int64())/1000
	return leaf.NotBefore.Add(time.Duration(float64(validity) * frac))
}

// reuseFlintlockdServing returns flintlockd's serving certificate from the
// Host when it is one the agent would keep: for the key beside it, signed by
// the published serving CA, naming what the agent asks for, and not yet due
// for renewal. Reusing it keeps flintlockd from being restarted every time
// the agent is.
func (c *Certificates) reuseFlintlockdServing(ctx context.Context) *held {
	log := logr.FromContextOrDiscard(ctx)
	dir := c.cfg.FlintlockdCertDir
	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, FlintlockdCertFile), filepath.Join(dir, FlintlockdKeyFile))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Info("Replacing flintlockd's serving certificate: the one on the Host cannot be read", "reason", err.Error())
		}
		return nil
	}
	key, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil
	}
	var chain bytes.Buffer
	for _, der := range pair.Certificate {
		_ = pem.Encode(&chain, &pem.Block{Type: pemCertificate, Bytes: der})
	}
	cert, err := c.check(flintlockdServing, chain.Bytes(), key)
	if err != nil {
		log.Info("Replacing flintlockd's serving certificate: the one on the Host is not one the agent would request", "reason", err.Error())
		return nil
	}
	h := &held{cert: cert, renewAt: c.renewAt(cert.Leaf)}
	if !c.clk.Now().Before(h.renewAt) {
		return nil
	}
	log.Info("Kept flintlockd's serving certificate on the Host", "notAfter", cert.Leaf.NotAfter, "renewAt", h.renewAt)
	return h
}

// writeFlintlockdServing writes flintlockd's serving certificate and key to
// the Host: the key first, so that the certificate, written last, always
// finds its key.
func (c *Certificates) writeFlintlockdServing(cert *tls.Certificate, key crypto.Signer) error {
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("encoding flintlockd's serving key: %w", err)
	}
	var chain bytes.Buffer
	for _, der := range cert.Certificate {
		_ = pem.Encode(&chain, &pem.Block{Type: pemCertificate, Bytes: der})
	}
	dir := c.cfg.FlintlockdCertDir
	if err := writeAtomic(dir, FlintlockdKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return writeAtomic(dir, FlintlockdCertFile, chain.Bytes(), 0o644)
}

// writeClientCA writes the published client CA bundle for flintlockd when
// it differs from the one on the Host.
func (c *Certificates) writeClientCA() error {
	c.mu.RLock()
	bundle := c.clientCAPEM
	c.mu.RUnlock()
	path := filepath.Join(c.cfg.FlintlockdCertDir, FlintlockdClientCAFile)
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, bundle) {
		return nil
	}
	return writeAtomic(c.cfg.FlintlockdCertDir, FlintlockdClientCAFile, bundle, 0o644)
}

// writeAtomic replaces dir/name with data: it writes a temporary file in
// dir with mode, flushes it and renames it over name, so that a reader sees
// the old file or the new one and never part of either.
func writeAtomic(dir, name string, data []byte, mode os.FileMode) (err error) {
	//= docs/requirements/05-exec-agent.md#certificates
	//# The Exec Agent SHALL write its Host's `flintlockd` serving
	//# certificate, its key and the `flintlockd` client CA bundle to the
	//# configured Host path, replacing each file atomically and leaving the key
	//# readable only by its owner.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+name+".*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	// The mode is set before anything is written, so the key is never
	// readable by anyone else, not even for a moment.
	if err = tmp.Chmod(mode); err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	if err = os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	return nil
}

// clientCertificate is the client certificate the agent presents to
// flintlockd, from memory.
func (c *Certificates) clientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	//= docs/requirements/05-exec-agent.md#certificates
	//# The Exec Agent SHALL keep its own client key in memory only.
	if h := c.get(flintlockdClient); h != nil {
		return h.cert, nil
	}
	return nil, errors.New("execagent: no client certificate for flintlockd yet")
}

// servingCertificate is the exec API's serving certificate, from memory.
func (c *Certificates) servingCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if h := c.get(agentServing); h != nil {
		return h.cert, nil
	}
	return nil, errors.New("execagent: no serving certificate yet")
}

// verifyFlintlockd verifies flintlockd's serving certificate against the
// serving CA the Operator publishes, for the address the agent dialled.
func (c *Certificates) verifyFlintlockd(cs tls.ConnectionState) error {
	//= docs/requirements/05-exec-agent.md#certificates
	//# The Exec Agent SHALL verify its Host's `flintlockd` serving
	//# certificate against the serving CA certificate the Operator publishes.
	c.mu.RLock()
	roots := c.servingCAs
	c.mu.RUnlock()
	if roots == nil {
		return errors.New("execagent: the serving CA is not known yet")
	}
	if len(cs.PeerCertificates) == 0 {
		return errors.New("execagent: flintlockd presented no certificate")
	}
	intermediates := x509.NewCertPool()
	for _, ic := range cs.PeerCertificates[1:] {
		intermediates.AddCert(ic)
	}
	_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
		Roots: roots, Intermediates: intermediates, DNSName: cs.ServerName,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		return fmt.Errorf("execagent: flintlockd's serving certificate: %w", err)
	}
	return nil
}

// flintlockdTLS is the client side of the mutual TLS to flintlockd. Each
// handshake takes the client certificate and the serving CA the agent holds
// at that moment, so a renewal needs no new connection. The server name is
// left to gRPC, which takes it from the endpoint, so the serving
// certificate has to name the Host's address.
func (c *Certificates) flintlockdTLS() *tls.Config {
	return &tls.Config{
		MinVersion:           tls.VersionTLS12,
		GetClientCertificate: c.clientCertificate,
		// The standard verification takes a fixed pool of roots; the
		// published serving CA is read at each handshake instead, and
		// verified in full by verifyFlintlockd.
		InsecureSkipVerify: true, //nolint:gosec // verifyFlintlockd verifies the chain and the address.
		VerifyConnection:   c.verifyFlintlockd,
	}
}

// servingTLS is the TLS configuration of the exec API. Clients are not
// asked for certificates: they authenticate with a bearer token.
func (c *Certificates) servingTLS() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: c.servingCertificate,
		NextProtos:     []string{"h2"},
	}
}
