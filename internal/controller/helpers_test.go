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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"time"

	. "github.com/onsi/gomega"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/phoban01/battery-operator/internal/hostcert"
)

// testCA is a throwaway CA in the layout cert-manager gives a CA's Secret.
type testCA struct {
	cert    *x509.Certificate
	certPEM []byte
	keyPEM  []byte
}

func newTestCA(cn string, notAfter time.Time) *testCA {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	Expect(err).NotTo(HaveOccurred())
	cert, err := x509.ParseCertificate(der)
	Expect(err).NotTo(HaveOccurred())
	keyDER, err := x509.MarshalECPrivateKey(key)
	Expect(err).NotTo(HaveOccurred())
	return &testCA{
		cert:    cert,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}
}

// secret is the CA's Secret, named name in the Operator's namespace.
func (ca *testCA) secret(name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: operatorNamespace, Name: name},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       ca.certPEM,
			corev1.TLSPrivateKeyKey: ca.keyPEM,
			"ca.crt":                ca.certPEM,
		},
	}
}

// verify checks that cert chains to ca for usage.
func (ca *testCA) verify(cert *x509.Certificate, usage x509.ExtKeyUsage) error {
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	_, err := cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{usage}})
	return err
}

var (
	servingUsages = []certificatesv1.KeyUsage{
		certificatesv1.UsageDigitalSignature, certificatesv1.UsageKeyEncipherment, certificatesv1.UsageServerAuth,
	}
	clientUsages = []certificatesv1.KeyUsage{
		certificatesv1.UsageDigitalSignature, certificatesv1.UsageKeyEncipherment, certificatesv1.UsageClientAuth,
	}
)

// request is what a test CertificateSigningRequest asks for.
type request struct {
	signer            string
	uris              []string
	ips               []string
	dnsNames          []string
	usages            []certificatesv1.KeyUsage
	expirationSeconds *int32
}

// servingRequest is a correct serving request for the Node node with the
// address ip.
func servingRequest(node, ip string) request {
	return request{
		signer: hostcert.ServingSigner,
		uris:   []string{hostcert.HostID(trustDomain, node)},
		ips:    []string{ip},
		usages: servingUsages,
	}
}

// clientRequest is a correct client request for the Exec Agent on node.
func clientRequest(node string) request {
	return request{
		signer: hostcert.ClientSigner,
		uris:   []string{hostcert.ExecAgentID(trustDomain, node)},
		usages: clientUsages,
	}
}

// createCSR creates a CertificateSigningRequest for r as user, with the
// requester's Node node, or none when node is empty. The API server records
// both on the request, as it does for a pod's bound ServiceAccount token.
func createCSR(user, node string, r request) string {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: "test"}, DNSNames: r.dnsNames}
	for _, u := range r.uris {
		parsed, err := url.Parse(u)
		Expect(err).NotTo(HaveOccurred())
		tmpl.URIs = append(tmpl.URIs, parsed)
	}
	for _, ip := range r.ips {
		tmpl.IPAddresses = append(tmpl.IPAddresses, net.ParseIP(ip))
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	Expect(err).NotTo(HaveOccurred())

	impersonate := rest.ImpersonationConfig{UserName: user}
	if node != "" {
		impersonate.Extra = map[string][]string{hostcert.NodeNameExtra: {node}}
	}
	requesterCfg := rest.CopyConfig(cfg)
	requesterCfg.Impersonate = impersonate
	requester, err := client.New(requesterCfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())

	csr := &certificatesv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "test-"},
		Spec: certificatesv1.CertificateSigningRequestSpec{
			Request:           pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}),
			SignerName:        r.signer,
			Usages:            r.usages,
			ExpirationSeconds: r.expirationSeconds,
		},
	}
	Expect(requester.Create(ctx, csr)).To(Succeed())
	Expect(csr.Spec.Username).To(Equal(user))
	return csr.Name
}

// getCSR reads the CertificateSigningRequest name.
func getCSR(g Gomega, name string) *certificatesv1.CertificateSigningRequest {
	csr := &certificatesv1.CertificateSigningRequest{}
	g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, csr)).To(Succeed())
	return csr
}

// condition returns csr's condition of type t, or nil.
func condition(csr *certificatesv1.CertificateSigningRequest, t certificatesv1.RequestConditionType) *certificatesv1.CertificateSigningRequestCondition {
	for i := range csr.Status.Conditions {
		if csr.Status.Conditions[i].Type == t {
			return &csr.Status.Conditions[i]
		}
	}
	return nil
}

// waitForCertificate waits for the CertificateSigningRequest name to be
// approved and signed, and returns its certificate.
func waitForCertificate(name string) *x509.Certificate {
	var csr *certificatesv1.CertificateSigningRequest
	Eventually(func(g Gomega) {
		csr = getCSR(g, name)
		g.Expect(condition(csr, certificatesv1.CertificateDenied)).To(BeNil())
		g.Expect(condition(csr, certificatesv1.CertificateApproved)).NotTo(BeNil())
		g.Expect(csr.Status.Certificate).NotTo(BeEmpty())
	}).Should(Succeed())
	block, trailing := pem.Decode(csr.Status.Certificate)
	Expect(block).NotTo(BeNil())
	Expect(block.Type).To(Equal("CERTIFICATE"))
	Expect(trailing).To(BeEmpty())
	cert, err := x509.ParseCertificate(block.Bytes)
	Expect(err).NotTo(HaveOccurred())
	return cert
}

// createNode creates a Node with the internal address ip.
func createNode(name, ip string) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	Expect(k8sClient.Create(ctx, node)).To(Succeed())
	node.Status.Addresses = []corev1.NodeAddress{
		{Type: corev1.NodeHostName, Address: name},
		{Type: corev1.NodeExternalIP, Address: "203.0.113.1"},
		{Type: corev1.NodeInternalIP, Address: ip},
	}
	Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())
}
