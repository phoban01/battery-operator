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

package controller

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	certutil "k8s.io/client-go/util/cert"
	"k8s.io/client-go/util/keyutil"
)

// backdate is how far before the moment of signing a certificate becomes
// valid, to allow for clocks that run behind the Operator's.
const backdate = 5 * time.Minute

// signingCA is a CA certificate and its key, from a CA Secret.
type signingCA struct {
	cert *x509.Certificate
	key  crypto.Signer
}

// loadCA reads a CA from a Secret in the kubernetes.io/tls layout
// cert-manager writes: the CA certificate first in tls.crt, its key in
// tls.key.
func loadCA(secret *corev1.Secret) (*signingCA, error) {
	certs, err := certutil.ParseCertsPEM(secret.Data[corev1.TLSCertKey])
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", corev1.TLSCertKey, err)
	}
	if !certs[0].IsCA {
		return nil, fmt.Errorf("the first certificate in %s is not a CA", corev1.TLSCertKey)
	}
	key, err := keyutil.ParsePrivateKeyPEM(secret.Data[corev1.TLSPrivateKeyKey])
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", corev1.TLSPrivateKeyKey, err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("the key in %s cannot sign", corev1.TLSPrivateKeyKey)
	}
	return &signingCA{cert: certs[0], key: signer}, nil
}

// certificateDuration is how long csr's certificate is valid for, before the
// CA's own expiry is taken into account.
func certificateDuration(csr *certificatesv1.CertificateSigningRequest, maxDuration time.Duration) time.Duration {
	d := maxDuration
	if s := csr.Spec.ExpirationSeconds; s != nil {
		d = min(d, time.Duration(*s)*time.Second)
	}
	return d
}

// sign issues a certificate for req, valid from shortly before now for d or
// until the CA expires, whichever is sooner, with usages, and returns it PEM
// encoded. The certificate carries req's subject alternative names, which
// the review has checked, the subject CN=node, and nothing else of req.
func (ca *signingCA) sign(req *x509.CertificateRequest, node string, usages []certificatesv1.KeyUsage, now time.Time, d time.Duration) ([]byte, error) {
	if !now.Before(ca.cert.NotAfter) {
		return nil, fmt.Errorf("the CA expired at %s", ca.cert.NotAfter.Format(time.RFC3339))
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	keyUsage, extKeyUsage, err := x509Usages(usages)
	if err != nil {
		return nil, err
	}

	//= docs/requirements/09-certificates.md#signing
	//# The Operator SHALL issue each certificate for the shorter of the
	//# request's `expirationSeconds` and the configured maximum duration, and
	//# never beyond the signing CA's own expiry.
	notAfter := now.Add(d)
	if notAfter.After(ca.cert.NotAfter) {
		notAfter = ca.cert.NotAfter
	}
	notBefore := now.Add(-backdate)
	if notBefore.Before(ca.cert.NotBefore) {
		notBefore = ca.cert.NotBefore
	}

	//= docs/requirements/09-certificates.md#signing
	//# The Operator SHALL set the subject of every certificate it
	//# issues to a common name of the requester's Node name, ignoring the
	//# subject the request asks for.
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: node},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              keyUsage,
		ExtKeyUsage:           extKeyUsage,
		BasicConstraintsValid: true,
		IsCA:                  false,
		DNSNames:              req.DNSNames,
		IPAddresses:           req.IPAddresses,
		URIs:                  req.URIs,
		EmailAddresses:        req.EmailAddresses,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, req.PublicKey, ca.key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: certutil.CertificateBlockType, Bytes: der}), nil
}

// x509Usages maps the key usages the signers issue with to x509's.
func x509Usages(usages []certificatesv1.KeyUsage) (x509.KeyUsage, []x509.ExtKeyUsage, error) {
	var ku x509.KeyUsage
	var eku []x509.ExtKeyUsage
	for _, u := range usages {
		switch u {
		case certificatesv1.UsageDigitalSignature:
			ku |= x509.KeyUsageDigitalSignature
		case certificatesv1.UsageKeyEncipherment:
			ku |= x509.KeyUsageKeyEncipherment
		case certificatesv1.UsageServerAuth:
			eku = append(eku, x509.ExtKeyUsageServerAuth)
		case certificatesv1.UsageClientAuth:
			eku = append(eku, x509.ExtKeyUsageClientAuth)
		default:
			return 0, nil, fmt.Errorf("the key usage %q is not issued", u)
		}
	}
	return ku, eku, nil
}

// caCertificatesPEM returns the certificates in a CA Secret's tls.crt, PEM
// encoded again so that nothing else in the file, a key least of all, is
// carried over.
func caCertificatesPEM(secret *corev1.Secret) (string, error) {
	certs, err := certutil.ParseCertsPEM(secret.Data[corev1.TLSCertKey])
	if err != nil {
		return "", fmt.Errorf("parsing %s: %w", corev1.TLSCertKey, err)
	}
	if len(certs) == 0 {
		return "", errors.New("no certificates")
	}
	var buf bytes.Buffer
	for _, c := range certs {
		if err := pem.Encode(&buf, &pem.Block{Type: certutil.CertificateBlockType, Bytes: c.Raw}); err != nil {
			return "", err
		}
	}
	return buf.String(), nil
}
