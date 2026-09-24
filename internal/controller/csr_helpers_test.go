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
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"testing"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/phoban01/battery-operator/internal/hostcert"
)

const (
	// operatorNamespace is the Operator's namespace in the tests.
	operatorNamespace = "battery-operator-system"
	// execAgentUser is the Exec Agent's ServiceAccount.
	execAgentUser = "system:serviceaccount:" + operatorNamespace + ":exec-agent"
	// trustDomain is the SPIFFE trust domain the Operator is configured
	// with.
	trustDomain = "example.test"
	// maxDuration is the configured maximum certificate duration.
	maxDuration = 24 * time.Hour
)

// testCA is a throwaway CA in the layout cert-manager gives a CA's Secret.
type testCA struct {
	cert    *x509.Certificate
	certPEM []byte
	keyPEM  []byte
}

func newTestCA(t *testing.T, cn string, notAfter time.Time) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
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

// execAgentServingRequest is a correct request for the serving certificate
// of the Exec Agent on node, whose address is ip.
func execAgentServingRequest(node, ip string) request {
	return request{
		signer: hostcert.ExecAgentServingSigner,
		uris:   []string{hostcert.ExecAgentID(trustDomain, node)},
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

// signer is the CertificateSigningRequest signer and the CA bundle's
// reconciler over controller-runtime's fake client, which holds the Nodes,
// the CA Secrets and the requests. There is no API server: a test submits a
// request with the requester's identity already on it, as the API server
// records it from the requester's credentials, and runs a reconcile. What
// only an API server does (recording the requester, RBAC on the signer
// names) is tested in test/e2e/certificates_test.go.
type signer struct {
	c        client.Client
	csr      *CertificateSigningRequestReconciler
	bundle   *caBundleReconciler
	serving  *testCA
	clientCA *testCA
	n        int
}

// newSigner is a signer with the Nodes nodeA (dual-stack) and nodeB, a
// serving CA that expires before maxDuration has passed, so that the
// certificates it signs end with it, and a client CA that outlives
// maxDuration.
func newSigner(t *testing.T) *signer {
	t.Helper()
	//= docs/requirements/08-test-doubles.md#test-environments
	//# The unit tests SHALL test each subreconciler, and the rest of
	//# the logic of the controllers and the Exec Agent, against the fake battery,
	//# the fake `flintlockd` and a fake Kubernetes client, without a Kubernetes
	//# API server.
	s := &signer{
		serving:  newTestCA(t, "serving CA", time.Now().Add(2*time.Hour)),
		clientCA: newTestCA(t, "flintlockd client CA", time.Now().Add(10*365*24*time.Hour)),
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	s.c = fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			testNode(nodeA, nodeAIP, nodeAIPv6),
			testNode(nodeB, nodeBIP),
			s.serving.secret(DefaultServingCASecret),
			s.clientCA.secret(DefaultClientCASecret),
		).
		WithStatusSubresource(&certificatesv1.CertificateSigningRequest{}).
		Build()

	config := SignerConfig{
		TrustDomain:             trustDomain,
		MaxDuration:             maxDuration,
		Namespace:               operatorNamespace,
		ServingCASecret:         DefaultServingCASecret,
		ClientCASecret:          DefaultClientCASecret,
		CABundleConfigMap:       DefaultCABundleConfigMap,
		ExecAgentServiceAccount: "exec-agent",
	}
	if err := config.Complete(); err != nil {
		t.Fatal(err)
	}
	cas := map[string]client.Reader{DefaultServingCASecret: s.c, DefaultClientCASecret: s.c}
	s.csr = &CertificateSigningRequestReconciler{Client: s.c, Scheme: scheme, APIReader: s.c, Config: config, cas: cas}
	s.bundle = &caBundleReconciler{Client: s.c, Config: config, secrets: cas, configMap: s.c}
	return s
}

// testNode is a Node with the internal addresses ips, a host name and an
// external address.
func testNode(name string, ips ...string) *corev1.Node {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	node.Status.Addresses = []corev1.NodeAddress{
		{Type: corev1.NodeHostName, Address: name},
		{Type: corev1.NodeExternalIP, Address: "203.0.113.1"},
	}
	for _, ip := range ips {
		node.Status.Addresses = append(node.Status.Addresses, corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: ip})
	}
	return node
}

// submit stores a CertificateSigningRequest for r from user, with the
// requester's Node node, or none when node is empty, as the API server
// records them for a pod's bound ServiceAccount token. It returns the
// request's name.
func (s *signer) submit(t *testing.T, user, node string, r request) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// The subject asks for more than the Operator issues (CT-007).
	tmpl := &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: "admin", Organization: []string{"system:masters"}},
		DNSNames: r.dnsNames,
	}
	for _, u := range r.uris {
		parsed, err := url.Parse(u)
		if err != nil {
			t.Fatal(err)
		}
		tmpl.URIs = append(tmpl.URIs, parsed)
	}
	for _, ip := range r.ips {
		tmpl.IPAddresses = append(tmpl.IPAddresses, net.ParseIP(ip))
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		t.Fatal(err)
	}

	s.n++
	csr := &certificatesv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("test-%d", s.n)},
		Spec: certificatesv1.CertificateSigningRequestSpec{
			Request:           pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}),
			SignerName:        r.signer,
			Usages:            r.usages,
			ExpirationSeconds: r.expirationSeconds,
			Username:          user,
		},
	}
	if node != "" {
		csr.Spec.Extra = map[string]certificatesv1.ExtraValue{hostcert.NodeNameExtra: {node}}
	}
	if err := s.c.Create(context.Background(), csr); err != nil {
		t.Fatal(err)
	}
	return csr.Name
}

// reconcile runs the signer's reconcile of the request name, and returns
// the request as stored after it.
func (s *signer) reconcile(t *testing.T, name string) *certificatesv1.CertificateSigningRequest {
	t.Helper()
	if _, err := s.csr.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: name}}); err != nil {
		t.Fatalf("reconciling %s: %v", name, err)
	}
	return s.get(t, name)
}

// get reads the request name.
func (s *signer) get(t *testing.T, name string) *certificatesv1.CertificateSigningRequest {
	t.Helper()
	csr := &certificatesv1.CertificateSigningRequest{}
	if err := s.c.Get(context.Background(), client.ObjectKey{Name: name}, csr); err != nil {
		t.Fatal(err)
	}
	return csr
}

// approveAsAdmin approves the request name as someone other than the
// Operator.
func (s *signer) approveAsAdmin(t *testing.T, name string) {
	t.Helper()
	csr := s.get(t, name)
	csr.Status.Conditions = append(csr.Status.Conditions, certificatesv1.CertificateSigningRequestCondition{
		Type:    certificatesv1.CertificateApproved,
		Status:  corev1.ConditionTrue,
		Reason:  "AdminApproved",
		Message: "Approved by someone other than the Operator",
	})
	if err := s.c.SubResource("approval").Update(context.Background(), csr); err != nil {
		t.Fatal(err)
	}
}

// issue submits r as the Exec Agent on node, reconciles it, and returns its
// certificate, failing t unless it was approved and signed.
func (s *signer) issue(t *testing.T, node string, r request) *x509.Certificate {
	t.Helper()
	return certificate(t, s.reconcile(t, s.submit(t, execAgentUser, node, r)))
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

// certificate returns csr's certificate, failing t unless csr is approved,
// not denied, and signed with exactly one PEM certificate.
func certificate(t *testing.T, csr *certificatesv1.CertificateSigningRequest) *x509.Certificate {
	t.Helper()
	if d := condition(csr, certificatesv1.CertificateDenied); d != nil {
		t.Fatalf("%s was denied: %s: %s", csr.Name, d.Reason, d.Message)
	}
	if condition(csr, certificatesv1.CertificateApproved) == nil {
		t.Fatalf("%s was not approved: %+v", csr.Name, csr.Status.Conditions)
	}
	if len(csr.Status.Certificate) == 0 {
		t.Fatalf("%s was not signed: %+v", csr.Name, csr.Status.Conditions)
	}
	block, trailing := pem.Decode(csr.Status.Certificate)
	if block == nil || block.Type != "CERTIFICATE" || len(trailing) != 0 {
		t.Fatalf("%s: status.certificate is not one PEM certificate", csr.Name)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}
