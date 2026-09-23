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
	"crypto/x509"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/phoban01/battery-operator/internal/hostcert"
)

const (
	nodeA   = "node-a"
	nodeAIP = "10.0.0.1"
	nodeB   = "node-b"
	nodeBIP = "10.0.0.2"
	// nodeGone has no Node object.
	nodeGone   = "node-gone"
	nodeGoneIP = "10.0.0.3"
	otherUser  = "system:serviceaccount:" + operatorNamespace + ":someone-else"
)

var _ = Describe("CertificateSigningRequest Controller", Ordered, func() {
	BeforeAll(func() {
		createNode(nodeA, nodeAIP)
		createNode(nodeB, nodeBIP)
	})

	Context("When the Exec Agent requests its own Node's certificates", func() {
		//= docs/requirements/09-certificates.md#approval
		//= type=test
		//# The Operator SHALL approve a
		//# `battery.liquidmetal-x.dev/flintlockd-serving` request only when the
		//# requester is the Exec Agent's ServiceAccount, the requester's
		//# `authentication.kubernetes.io/node-name` names an existing Node, and the
		//# request's subject alternative names are exactly that Node's internal
		//# address and `spiffe://<trust domain>/flintlock/host/<node name>`.
		//
		//= docs/requirements/09-certificates.md#signing
		//= type=test
		//# The Operator SHALL approve and sign `CertificateSigningRequest`s
		//# for the signer names `battery.liquidmetal-x.dev/flintlockd-serving` and
		//# `battery.liquidmetal-x.dev/flintlockd-client`, and for no other signer
		//# name.
		//
		//= docs/requirements/09-certificates.md#signing
		//= type=test
		//# The Operator SHALL sign an approved
		//# `battery.liquidmetal-x.dev/flintlockd-serving` request with the serving CA
		//# and an approved `battery.liquidmetal-x.dev/flintlockd-client` request with
		//# the `flintlockd` client CA, each read from its Secret in the Operator's
		//# namespace.
		//
		//= docs/requirements/09-certificates.md#signing
		//= type=test
		//# The Operator SHALL sign a request only while it carries the
		//# condition `Approved` and not the condition `Denied`.
		It("should approve and sign a serving certificate with the serving CA", func() {
			name := createCSR(execAgentUser, nodeA, servingRequest(nodeA, nodeAIP))
			cert := waitForCertificate(name)

			Expect(servingCA.verify(cert, x509.ExtKeyUsageServerAuth)).To(Succeed())
			Expect(clientCA.verify(cert, x509.ExtKeyUsageServerAuth)).NotTo(Succeed())
			Expect(cert.URIs).To(HaveLen(1))
			Expect(cert.URIs[0].String()).To(Equal("spiffe://example.test/flintlock/host/node-a"))
			Expect(cert.IPAddresses).To(HaveLen(1))
			Expect(cert.IPAddresses[0].String()).To(Equal(nodeAIP))
			Expect(cert.DNSNames).To(BeEmpty())
			Expect(cert.IsCA).To(BeFalse())
			Expect(cert.KeyUsage).To(Equal(x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment))
			Expect(cert.ExtKeyUsage).To(Equal([]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}))
		})

		//= docs/requirements/09-certificates.md#approval
		//= type=test
		//# The Operator SHALL approve a
		//# `battery.liquidmetal-x.dev/flintlockd-client` request only when the
		//# requester is the Exec Agent's ServiceAccount and the request's only
		//# subject alternative name is
		//# `spiffe://<trust domain>/flintlock/client/exec-agent/<node name>` for the
		//# Node in the requester's `authentication.kubernetes.io/node-name`.
		//
		//= docs/requirements/09-certificates.md#approval
		//= type=test
		//# The Operator SHALL approve a request only when its key usages
		//# are digital signature and key encipherment with server auth for
		//# `flintlockd-serving`, or with client auth for `flintlockd-client`.
		It("should approve and sign a client certificate with the client CA", func() {
			name := createCSR(execAgentUser, nodeB, clientRequest(nodeB))
			cert := waitForCertificate(name)

			Expect(clientCA.verify(cert, x509.ExtKeyUsageClientAuth)).To(Succeed())
			Expect(servingCA.verify(cert, x509.ExtKeyUsageClientAuth)).NotTo(Succeed())
			Expect(cert.URIs).To(HaveLen(1))
			Expect(cert.URIs[0].String()).To(Equal("spiffe://example.test/flintlock/client/exec-agent/node-b"))
			Expect(cert.IPAddresses).To(BeEmpty())
			Expect(cert.DNSNames).To(BeEmpty())
			Expect(cert.KeyUsage).To(Equal(x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment))
			Expect(cert.ExtKeyUsage).To(Equal([]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}))
		})
	})

	//= docs/requirements/09-certificates.md#signing
	//= type=test
	//# The Operator SHALL issue each certificate for the shorter of the
	//# request's `expirationSeconds` and the configured maximum duration, and
	//# never beyond the signing CA's own expiry.
	Context("When it chooses a certificate's validity", func() {
		DescribeTable("should issue a client certificate for the shorter of the request's and the configured duration",
			func(expirationSeconds *int32, want time.Duration) {
				r := clientRequest(nodeA)
				r.expirationSeconds = expirationSeconds
				start := time.Now()
				cert := waitForCertificate(createCSR(execAgentUser, nodeA, r))
				Expect(cert.NotAfter).To(BeTemporally(">=", start.Add(want).Add(-time.Second).Truncate(time.Second)))
				Expect(cert.NotAfter).To(BeTemporally("<=", time.Now().Add(want)))
				Expect(cert.NotBefore).To(BeTemporally("<=", start))
			},
			Entry("when the request asks for less", ptr.To[int32](3600), time.Hour),
			Entry("when the request asks for more", ptr.To[int32](48*3600), maxDuration),
			Entry("when the request does not say", nil, maxDuration),
		)

		It("should not issue a certificate beyond the signing CA's expiry", func() {
			cert := waitForCertificate(createCSR(execAgentUser, nodeA, servingRequest(nodeA, nodeAIP)))
			Expect(servingCA.cert.NotAfter).To(BeTemporally("<", time.Now().Add(maxDuration)))
			Expect(cert.NotAfter).To(BeTemporally("==", servingCA.cert.NotAfter))
		})
	})

	//= docs/requirements/09-certificates.md#approval
	//= type=test
	//# If a request for either signer name fails any check, then the
	//# Operator SHALL deny it with a reason that names the check.
	//
	//= docs/requirements/09-certificates.md#signing
	//= type=test
	//# The Operator SHALL sign a request only while it carries the
	//# condition `Approved` and not the condition `Denied`.
	Context("When a request fails a check", func() {
		with := func(r request, change func(*request)) request {
			change(&r)
			return r
		}

		DescribeTable("should deny it with the check's reason, and not sign it",
			func(user, node string, r request, reason string) {
				name := createCSR(user, node, r)
				Eventually(func(g Gomega) {
					csr := getCSR(g, name)
					denied := condition(csr, certificatesv1.CertificateDenied)
					g.Expect(denied).NotTo(BeNil())
					g.Expect(denied.Status).To(Equal(corev1.ConditionTrue))
					g.Expect(denied.Reason).To(Equal(reason))
					g.Expect(denied.Message).NotTo(BeEmpty())
					g.Expect(condition(csr, certificatesv1.CertificateApproved)).To(BeNil())
				}).Should(Succeed())
				Consistently(func(g Gomega) {
					g.Expect(getCSR(g, name).Status.Certificate).To(BeEmpty())
				}, 500*time.Millisecond).Should(Succeed())
			},

			// The requester.
			Entry("a serving request from another ServiceAccount",
				otherUser, nodeA, servingRequest(nodeA, nodeAIP), ReasonRequester),
			Entry("a client request from another ServiceAccount",
				otherUser, nodeA, clientRequest(nodeA), ReasonRequester),

			// The requester's Node.
			Entry("a serving request whose requester has no Node",
				execAgentUser, "", servingRequest(nodeA, nodeAIP), ReasonNodeName),
			Entry("a client request whose requester has no Node",
				execAgentUser, "", clientRequest(nodeA), ReasonNodeName),
			Entry("a serving request whose requester's Node does not exist",
				execAgentUser, nodeGone, servingRequest(nodeGone, nodeGoneIP), ReasonNodeNotFound),

			// Another Node's names.
			Entry("a serving request for another Node",
				execAgentUser, nodeA, servingRequest(nodeB, nodeBIP), ReasonSubjectAltNames),
			Entry("a serving request with another Node's SPIFFE ID",
				execAgentUser, nodeA, with(servingRequest(nodeA, nodeAIP), func(r *request) {
					r.uris = []string{hostcert.HostID(trustDomain, nodeB)}
				}), ReasonSubjectAltNames),
			Entry("a client request for another Node's Exec Agent",
				execAgentUser, nodeA, clientRequest(nodeB), ReasonSubjectAltNames),

			// Extra subject alternative names.
			Entry("a serving request with a DNS name",
				execAgentUser, nodeA, with(servingRequest(nodeA, nodeAIP), func(r *request) {
					r.dnsNames = []string{"node-a.example.test"}
				}), ReasonSubjectAltNames),
			Entry("a serving request with a second URI",
				execAgentUser, nodeA, with(servingRequest(nodeA, nodeAIP), func(r *request) {
					r.uris = append(r.uris, hostcert.ExecAgentID(trustDomain, nodeA))
				}), ReasonSubjectAltNames),
			Entry("a serving request with a second IP address",
				execAgentUser, nodeA, with(servingRequest(nodeA, nodeAIP), func(r *request) {
					r.ips = append(r.ips, nodeBIP)
				}), ReasonSubjectAltNames),
			Entry("a client request with an IP address",
				execAgentUser, nodeA, with(clientRequest(nodeA), func(r *request) {
					r.ips = []string{nodeAIP}
				}), ReasonSubjectAltNames),
			Entry("a client request with a DNS name",
				execAgentUser, nodeA, with(clientRequest(nodeA), func(r *request) {
					r.dnsNames = []string{"node-a.example.test"}
				}), ReasonSubjectAltNames),

			// Missing subject alternative names.
			Entry("a serving request without the Node's address",
				execAgentUser, nodeA, with(servingRequest(nodeA, nodeAIP), func(r *request) {
					r.ips = nil
				}), ReasonSubjectAltNames),
			Entry("a serving request without the SPIFFE ID",
				execAgentUser, nodeA, with(servingRequest(nodeA, nodeAIP), func(r *request) {
					r.uris = nil
				}), ReasonSubjectAltNames),
			Entry("a client request without the SPIFFE ID",
				execAgentUser, nodeA, with(clientRequest(nodeA), func(r *request) {
					r.uris = nil
				}), ReasonSubjectAltNames),

			// Wrong names.
			Entry("a serving request with an address that is not the Node's",
				execAgentUser, nodeA, servingRequest(nodeA, "10.0.0.9"), ReasonSubjectAltNames),
			Entry("a serving request with the Node's external address",
				execAgentUser, nodeA, servingRequest(nodeA, "203.0.113.1"), ReasonSubjectAltNames),
			Entry("a serving request under another trust domain",
				execAgentUser, nodeA, with(servingRequest(nodeA, nodeAIP), func(r *request) {
					r.uris = []string{hostcert.HostID("other.test", nodeA)}
				}), ReasonSubjectAltNames),
			Entry("a client request for battery's SPIFFE ID",
				execAgentUser, nodeA, with(clientRequest(nodeA), func(r *request) {
					r.uris = []string{"spiffe://example.test/flintlock/client/battery"}
				}), ReasonSubjectAltNames),

			// Key usages.
			Entry("a serving request for client auth",
				execAgentUser, nodeA, with(servingRequest(nodeA, nodeAIP), func(r *request) {
					r.usages = clientUsages
				}), ReasonKeyUsages),
			Entry("a client request for server auth",
				execAgentUser, nodeA, with(clientRequest(nodeA), func(r *request) {
					r.usages = servingUsages
				}), ReasonKeyUsages),
			Entry("a serving request with an extra key usage",
				execAgentUser, nodeA, with(servingRequest(nodeA, nodeAIP), func(r *request) {
					r.usages = append(servingUsages[:3:3], certificatesv1.UsageCertSign)
				}), ReasonKeyUsages),
			Entry("a client request without key encipherment",
				execAgentUser, nodeA, with(clientRequest(nodeA), func(r *request) {
					r.usages = []certificatesv1.KeyUsage{certificatesv1.UsageDigitalSignature, certificatesv1.UsageClientAuth}
				}), ReasonKeyUsages),
		)
	})

	//= docs/requirements/09-certificates.md#signing
	//= type=test
	//# The Operator SHALL approve and sign `CertificateSigningRequest`s
	//# for the signer names `battery.liquidmetal-x.dev/flintlockd-serving` and
	//# `battery.liquidmetal-x.dev/flintlockd-client`, and for no other signer
	//# name.
	It("should leave requests for other signer names alone", func() {
		r := servingRequest(nodeA, nodeAIP)
		r.signer = "example.test/other"
		name := createCSR(execAgentUser, nodeA, r)
		Consistently(func(g Gomega) {
			csr := getCSR(g, name)
			g.Expect(csr.Status.Conditions).To(BeEmpty())
			g.Expect(csr.Status.Certificate).To(BeEmpty())
		}, 2*time.Second).Should(Succeed())
	})

	//= docs/requirements/09-certificates.md#signing
	//= type=test
	//# The Operator SHALL publish the certificates of the serving CA and the
	//# `flintlockd` client CA, without their keys, in a ConfigMap in its
	//# namespace.
	Context("When it publishes the CA certificates", func() {
		bundleKey := client.ObjectKey{Namespace: operatorNamespace, Name: DefaultCABundleConfigMap}
		expectBundle := func(serving, clientCert []byte) {
			Eventually(func(g Gomega) {
				cm := &corev1.ConfigMap{}
				g.Expect(k8sClient.Get(ctx, bundleKey, cm)).To(Succeed())
				g.Expect(cm.Data).To(HaveLen(2))
				g.Expect(cm.Data).To(HaveKeyWithValue(ServingCAKey, string(serving)))
				g.Expect(cm.Data).To(HaveKeyWithValue(ClientCAKey, string(clientCert)))
				g.Expect(cm.BinaryData).To(BeEmpty())
				for _, v := range cm.Data {
					g.Expect(strings.Contains(v, "PRIVATE KEY")).To(BeFalse())
				}
			}).Should(Succeed())
		}

		It("should publish both CA certificates, without their keys", func() {
			expectBundle(servingCA.certPEM, clientCA.certPEM)
		})

		It("should publish a CA's new certificate when its Secret changes", func() {
			rotated := newTestCA("rotated client CA", time.Now().Add(365*24*time.Hour))
			Expect(k8sClient.Update(ctx, rotated.secret(DefaultClientCASecret))).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Update(ctx, clientCA.secret(DefaultClientCASecret))).To(Succeed())
				expectBundle(servingCA.certPEM, clientCA.certPEM)
			})
			expectBundle(servingCA.certPEM, rotated.certPEM)
		})

		It("should publish the CA certificates again when the ConfigMap is deleted", func() {
			expectBundle(servingCA.certPEM, clientCA.certPEM)
			Expect(k8sClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: bundleKey.Namespace, Name: bundleKey.Name}})).To(Succeed())
			expectBundle(servingCA.certPEM, clientCA.certPEM)
		})
	})
})
