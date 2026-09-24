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
	"crypto/x509"
	"slices"
	"strings"
	"testing"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/phoban01/battery-operator/internal/hostcert"
)

const (
	nodeA   = "node-a"
	nodeAIP = "10.0.0.1"
	// nodeAIPv6 is node-a's second internal address: it is dual-stack.
	nodeAIPv6 = "fd00::1"
	nodeB     = "node-b"
	nodeBIP   = "10.0.0.2"
	// nodeGone has no Node object.
	nodeGone   = "node-gone"
	nodeGoneIP = "10.0.0.3"
	otherUser  = "system:serviceaccount:" + operatorNamespace + ":someone-else"
	// nodeADNSName is a DNS name no request may carry.
	nodeADNSName = "node-a.example.test"
)

// wantIssuedTo checks the subject of cert: CN=node and nothing else,
// whatever the request asked for.
func wantIssuedTo(t *testing.T, cert *x509.Certificate, node string) {
	t.Helper()
	if cert.Subject.CommonName != node || len(cert.Subject.Organization) != 0 || len(cert.Subject.Names) != 1 {
		t.Errorf("subject = %v, want CN=%s and nothing else", cert.Subject, node)
	}
}

// wantNames checks cert's subject alternative names: the one URI uri, the
// addresses ips, and no DNS names.
func wantNames(t *testing.T, cert *x509.Certificate, uri string, ips ...string) {
	t.Helper()
	if len(cert.URIs) != 1 || cert.URIs[0].String() != uri {
		t.Errorf("URIs = %v, want [%s]", cert.URIs, uri)
	}
	got := make([]string, 0, len(cert.IPAddresses))
	for _, ip := range cert.IPAddresses {
		got = append(got, ip.String())
	}
	if !slices.Equal(got, ips) {
		t.Errorf("IP addresses = %v, want %v", got, ips)
	}
	if len(cert.DNSNames) != 0 {
		t.Errorf("DNS names = %v, want none", cert.DNSNames)
	}
}

// wantUsages checks cert's key usages: digital signature, key
// encipherment and ext.
func wantUsages(t *testing.T, cert *x509.Certificate, ext x509.ExtKeyUsage) {
	t.Helper()
	if cert.KeyUsage != x509.KeyUsageDigitalSignature|x509.KeyUsageKeyEncipherment {
		t.Errorf("key usage = %v, want digital signature and key encipherment", cert.KeyUsage)
	}
	if !slices.Equal(cert.ExtKeyUsage, []x509.ExtKeyUsage{ext}) {
		t.Errorf("extended key usage = %v, want [%v]", cert.ExtKeyUsage, ext)
	}
	if cert.IsCA {
		t.Error("the certificate is a CA")
	}
}

func TestSignerIssuesFlintlockdServingCertificates(t *testing.T) {
	//= docs/requirements/09-certificates.md#approval
	//= type=test
	//# The Operator SHALL approve a
	//# `battery.liquidmetal-x.dev/flintlockd-serving` request only when the
	//# requester is the Exec Agent's ServiceAccount, the requester's
	//# `authentication.kubernetes.io/node-name` names an existing Node, and the
	//# request's subject alternative names are exactly one of that Node's
	//# internal addresses and `spiffe://<trust domain>/flintlock/host/<node name>`.

	//= docs/requirements/09-certificates.md#signing
	//= type=test
	//# The Operator SHALL approve and sign `CertificateSigningRequest`s
	//# for the signer names `battery.liquidmetal-x.dev/flintlockd-serving`,
	//# `battery.liquidmetal-x.dev/flintlockd-client` and
	//# `battery.liquidmetal-x.dev/exec-agent-serving`, and for no other signer
	//# name.

	//= docs/requirements/09-certificates.md#signing
	//= type=test
	//# The Operator SHALL sign an approved
	//# `battery.liquidmetal-x.dev/flintlockd-serving` or
	//# `battery.liquidmetal-x.dev/exec-agent-serving` request with the serving
	//# CA, and an approved `battery.liquidmetal-x.dev/flintlockd-client` request
	//# with the `flintlockd` client CA, each read from its Secret in the
	//# Operator's namespace.

	//= docs/requirements/09-certificates.md#signing
	//= type=test
	//# The Operator SHALL sign a request only while it carries the
	//# condition `Approved` and not the condition `Denied`.

	//= docs/requirements/09-certificates.md#signing
	//= type=test
	//# The Operator SHALL set the subject of every certificate it
	//# issues to a common name of the requester's Node name, ignoring the
	//# subject the request asks for.

	for name, ip := range map[string]string{"the Node's IPv4 address": nodeAIP, "the dual-stack Node's IPv6 address": nodeAIPv6} {
		t.Run(name, func(t *testing.T) {
			s := newSigner(t)
			cert := s.issue(t, nodeA, servingRequest(nodeA, ip))
			if err := s.serving.verify(cert, x509.ExtKeyUsageServerAuth); err != nil {
				t.Errorf("not signed by the serving CA: %v", err)
			}
			if s.clientCA.verify(cert, x509.ExtKeyUsageServerAuth) == nil {
				t.Error("signed by the client CA")
			}
			wantIssuedTo(t, cert, nodeA)
			wantNames(t, cert, "spiffe://example.test/flintlock/host/node-a", ip)
			wantUsages(t, cert, x509.ExtKeyUsageServerAuth)
		})
	}
}

func TestSignerIssuesExecAgentServingCertificates(t *testing.T) {
	//= docs/requirements/09-certificates.md#approval
	//= type=test
	//# The Operator SHALL approve a
	//# `battery.liquidmetal-x.dev/exec-agent-serving` request only when the
	//# requester is the Exec Agent's ServiceAccount, the requester's
	//# `authentication.kubernetes.io/node-name` names an existing Node, and the
	//# request's subject alternative names are exactly one of that Node's
	//# internal addresses and
	//# `spiffe://<trust domain>/flintlock/client/exec-agent/<node name>`.

	s := newSigner(t)
	cert := s.issue(t, nodeB, execAgentServingRequest(nodeB, nodeBIP))
	if err := s.serving.verify(cert, x509.ExtKeyUsageServerAuth); err != nil {
		t.Errorf("not signed by the serving CA: %v", err)
	}
	if s.clientCA.verify(cert, x509.ExtKeyUsageServerAuth) == nil {
		t.Error("signed by the client CA")
	}
	wantIssuedTo(t, cert, nodeB)
	wantNames(t, cert, "spiffe://example.test/flintlock/client/exec-agent/node-b", nodeBIP)
	wantUsages(t, cert, x509.ExtKeyUsageServerAuth)
}

func TestSignerIssuesClientCertificates(t *testing.T) {
	//= docs/requirements/09-certificates.md#approval
	//= type=test
	//# The Operator SHALL approve a
	//# `battery.liquidmetal-x.dev/flintlockd-client` request only when the
	//# requester is the Exec Agent's ServiceAccount and the request's only
	//# subject alternative name is
	//# `spiffe://<trust domain>/flintlock/client/exec-agent/<node name>` for the
	//# Node in the requester's `authentication.kubernetes.io/node-name`.

	//= docs/requirements/09-certificates.md#approval
	//= type=test
	//# The Operator SHALL approve a request only when its key usages
	//# are digital signature and key encipherment, with server auth for
	//# `flintlockd-serving` and `exec-agent-serving`, or with client auth for
	//# `flintlockd-client`.

	s := newSigner(t)
	cert := s.issue(t, nodeB, clientRequest(nodeB))
	if err := s.clientCA.verify(cert, x509.ExtKeyUsageClientAuth); err != nil {
		t.Errorf("not signed by the client CA: %v", err)
	}
	if s.serving.verify(cert, x509.ExtKeyUsageClientAuth) == nil {
		t.Error("signed by the serving CA")
	}
	wantIssuedTo(t, cert, nodeB)
	wantNames(t, cert, "spiffe://example.test/flintlock/client/exec-agent/node-b")
	wantUsages(t, cert, x509.ExtKeyUsageClientAuth)
}

func TestSignerChoosesTheValidity(t *testing.T) {
	//= docs/requirements/09-certificates.md#signing
	//= type=test
	//# The Operator SHALL issue each certificate for the shorter of the
	//# request's `expirationSeconds` and the configured maximum duration, and
	//# never beyond the signing CA's own expiry.

	for _, tc := range []struct {
		name              string
		expirationSeconds *int32
		want              time.Duration
	}{
		{"the request asks for less", ptr.To[int32](3600), time.Hour},
		{"the request asks for more", ptr.To[int32](48 * 3600), maxDuration},
		{"the request does not say", nil, maxDuration},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSigner(t)
			r := clientRequest(nodeA)
			r.expirationSeconds = tc.expirationSeconds
			start := time.Now()
			cert := s.issue(t, nodeA, r)
			if earliest := start.Add(tc.want).Add(-time.Second).Truncate(time.Second); cert.NotAfter.Before(earliest) {
				t.Errorf("NotAfter = %v, want at least %v", cert.NotAfter, earliest)
			}
			if latest := time.Now().Add(tc.want); cert.NotAfter.After(latest) {
				t.Errorf("NotAfter = %v, want at most %v", cert.NotAfter, latest)
			}
			if cert.NotBefore.After(start) {
				t.Errorf("NotBefore = %v, want no later than %v", cert.NotBefore, start)
			}
		})
	}

	t.Run("never beyond the signing CA's expiry", func(t *testing.T) {
		s := newSigner(t)
		if !s.serving.cert.NotAfter.Before(time.Now().Add(maxDuration)) {
			t.Fatal("the serving CA outlives maxDuration")
		}
		cert := s.issue(t, nodeA, servingRequest(nodeA, nodeAIP))
		if !cert.NotAfter.Equal(s.serving.cert.NotAfter) {
			t.Errorf("NotAfter = %v, want the serving CA's %v", cert.NotAfter, s.serving.cert.NotAfter)
		}
	})
}

func TestSignerDeniesARequestThatFailsACheck(t *testing.T) {
	//= docs/requirements/09-certificates.md#approval
	//= type=test
	//# If a request for any of the three signer names fails any check
	//# and does not carry the condition `Approved`, then the Operator SHALL deny
	//# it with a reason that names the check.

	//= docs/requirements/09-certificates.md#signing
	//= type=test
	//# The Operator SHALL sign a request only while it carries the
	//# condition `Approved` and not the condition `Denied`.

	with := func(r request, change func(*request)) request {
		change(&r)
		return r
	}
	for _, tc := range []struct {
		name   string
		user   string
		node   string
		r      request
		reason string
	}{
		// The requester.
		{"a serving request from another ServiceAccount",
			otherUser, nodeA, servingRequest(nodeA, nodeAIP), ReasonRequester},
		{"a client request from another ServiceAccount",
			otherUser, nodeA, clientRequest(nodeA), ReasonRequester},
		{"an Exec Agent serving request from another ServiceAccount",
			otherUser, nodeA, execAgentServingRequest(nodeA, nodeAIP), ReasonRequester},

		// The requester's Node.
		{"a serving request whose requester has no Node",
			execAgentUser, "", servingRequest(nodeA, nodeAIP), ReasonNodeName},
		{"a client request whose requester has no Node",
			execAgentUser, "", clientRequest(nodeA), ReasonNodeName},
		{"an Exec Agent serving request whose requester has no Node",
			execAgentUser, "", execAgentServingRequest(nodeA, nodeAIP), ReasonNodeName},
		{"a serving request whose requester's Node does not exist",
			execAgentUser, nodeGone, servingRequest(nodeGone, nodeGoneIP), ReasonNodeNotFound},
		{"an Exec Agent serving request whose requester's Node does not exist",
			execAgentUser, nodeGone, execAgentServingRequest(nodeGone, nodeGoneIP), ReasonNodeNotFound},

		// Another Node's names.
		{"a serving request for another Node",
			execAgentUser, nodeA, servingRequest(nodeB, nodeBIP), ReasonSubjectAltNames},
		{"a serving request with another Node's SPIFFE ID",
			execAgentUser, nodeA, with(servingRequest(nodeA, nodeAIP), func(r *request) {
				r.uris = []string{hostcert.HostID(trustDomain, nodeB)}
			}), ReasonSubjectAltNames},
		{"a client request for another Node's Exec Agent",
			execAgentUser, nodeA, clientRequest(nodeB), ReasonSubjectAltNames},
		{"an Exec Agent serving request for another Node",
			execAgentUser, nodeA, execAgentServingRequest(nodeB, nodeBIP), ReasonSubjectAltNames},
		{"an Exec Agent serving request with another Node's address",
			execAgentUser, nodeA, execAgentServingRequest(nodeA, nodeBIP), ReasonSubjectAltNames},

		// Extra subject alternative names.
		{"a serving request with a DNS name",
			execAgentUser, nodeA, with(servingRequest(nodeA, nodeAIP), func(r *request) {
				r.dnsNames = []string{nodeADNSName}
			}), ReasonSubjectAltNames},
		{"a serving request with a second URI",
			execAgentUser, nodeA, with(servingRequest(nodeA, nodeAIP), func(r *request) {
				r.uris = append(r.uris, hostcert.ExecAgentID(trustDomain, nodeA))
			}), ReasonSubjectAltNames},
		{"a serving request with both of a dual-stack Node's addresses",
			execAgentUser, nodeA, with(servingRequest(nodeA, nodeAIP), func(r *request) {
				r.ips = append(r.ips, nodeAIPv6)
			}), ReasonSubjectAltNames},
		{"a client request with an IP address",
			execAgentUser, nodeA, with(clientRequest(nodeA), func(r *request) {
				r.ips = []string{nodeAIP}
			}), ReasonSubjectAltNames},
		{"a client request with a DNS name",
			execAgentUser, nodeA, with(clientRequest(nodeA), func(r *request) {
				r.dnsNames = []string{nodeADNSName}
			}), ReasonSubjectAltNames},
		{"an Exec Agent serving request with a DNS name",
			execAgentUser, nodeA, with(execAgentServingRequest(nodeA, nodeAIP), func(r *request) {
				r.dnsNames = []string{nodeADNSName}
			}), ReasonSubjectAltNames},

		// Missing subject alternative names.
		{"a serving request without the Node's address",
			execAgentUser, nodeA, with(servingRequest(nodeA, nodeAIP), func(r *request) {
				r.ips = nil
			}), ReasonSubjectAltNames},
		{"a serving request without the SPIFFE ID",
			execAgentUser, nodeA, with(servingRequest(nodeA, nodeAIP), func(r *request) {
				r.uris = nil
			}), ReasonSubjectAltNames},
		{"a client request without the SPIFFE ID",
			execAgentUser, nodeA, with(clientRequest(nodeA), func(r *request) {
				r.uris = nil
			}), ReasonSubjectAltNames},
		{"an Exec Agent serving request without the Node's address",
			execAgentUser, nodeA, with(execAgentServingRequest(nodeA, nodeAIP), func(r *request) {
				r.ips = nil
			}), ReasonSubjectAltNames},

		// Wrong names.
		{"a serving request with an address that is not the Node's",
			execAgentUser, nodeA, servingRequest(nodeA, "10.0.0.9"), ReasonSubjectAltNames},
		{"a serving request with the Node's external address",
			execAgentUser, nodeA, servingRequest(nodeA, "203.0.113.1"), ReasonSubjectAltNames},
		{"a serving request under another trust domain",
			execAgentUser, nodeA, with(servingRequest(nodeA, nodeAIP), func(r *request) {
				r.uris = []string{hostcert.HostID("other.test", nodeA)}
			}), ReasonSubjectAltNames},
		{"a client request for battery's SPIFFE ID",
			execAgentUser, nodeA, with(clientRequest(nodeA), func(r *request) {
				r.uris = []string{"spiffe://example.test/flintlock/client/battery"}
			}), ReasonSubjectAltNames},
		{"an Exec Agent serving request with flintlockd's SPIFFE ID",
			execAgentUser, nodeA, with(execAgentServingRequest(nodeA, nodeAIP), func(r *request) {
				r.uris = []string{hostcert.HostID(trustDomain, nodeA)}
			}), ReasonSubjectAltNames},
		{"a flintlockd serving request with the Exec Agent's SPIFFE ID",
			execAgentUser, nodeA, with(servingRequest(nodeA, nodeAIP), func(r *request) {
				r.uris = []string{hostcert.ExecAgentID(trustDomain, nodeA)}
			}), ReasonSubjectAltNames},

		// Key usages.
		{"a serving request for client auth",
			execAgentUser, nodeA, with(servingRequest(nodeA, nodeAIP), func(r *request) {
				r.usages = clientUsages
			}), ReasonKeyUsages},
		{"a client request for server auth",
			execAgentUser, nodeA, with(clientRequest(nodeA), func(r *request) {
				r.usages = servingUsages
			}), ReasonKeyUsages},
		{"an Exec Agent serving request for client auth",
			execAgentUser, nodeA, with(execAgentServingRequest(nodeA, nodeAIP), func(r *request) {
				r.usages = clientUsages
			}), ReasonKeyUsages},
		{"a serving request with an extra key usage",
			execAgentUser, nodeA, with(servingRequest(nodeA, nodeAIP), func(r *request) {
				r.usages = append(servingUsages[:3:3], certificatesv1.UsageCertSign)
			}), ReasonKeyUsages},
		{"a client request without key encipherment",
			execAgentUser, nodeA, with(clientRequest(nodeA), func(r *request) {
				r.usages = []certificatesv1.KeyUsage{certificatesv1.UsageDigitalSignature, certificatesv1.UsageClientAuth}
			}), ReasonKeyUsages},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSigner(t)
			name := s.submit(t, tc.user, tc.node, tc.r)
			csr := s.reconcile(t, name)
			denied := condition(csr, certificatesv1.CertificateDenied)
			if denied == nil || denied.Status != corev1.ConditionTrue {
				t.Fatalf("conditions = %+v, want Denied", csr.Status.Conditions)
			}
			if denied.Reason != tc.reason {
				t.Errorf("Denied reason = %q, want %q", denied.Reason, tc.reason)
			}
			if denied.Message == "" {
				t.Error("Denied has no message")
			}
			if condition(csr, certificatesv1.CertificateApproved) != nil {
				t.Error("the request is Approved as well as Denied")
			}
			// Once denied, it stays unsigned.
			if csr := s.reconcile(t, name); len(csr.Status.Certificate) != 0 {
				t.Error("a denied request was signed")
			}
		})
	}
}

func TestSignerSignsOnlyWhatPassesEveryCheckWhoeverApprovedIt(t *testing.T) {
	//= docs/requirements/09-certificates.md#signing
	//= type=test
	//# The Operator SHALL sign a request only when it passes every
	//# check of [Approval](#approval) for its signer name, whoever approved it.

	t.Run("a request that fails a check is marked Failed, and never signed", func(t *testing.T) {
		s := newSigner(t)
		name := s.submit(t, execAgentUser, nodeA, servingRequest(nodeB, nodeBIP))
		s.approveAsAdmin(t, name)
		csr := s.reconcile(t, name)
		failed := condition(csr, certificatesv1.CertificateFailed)
		if failed == nil {
			t.Fatalf("conditions = %+v, want Failed", csr.Status.Conditions)
		}
		if failed.Reason != ReasonSubjectAltNames {
			t.Errorf("Failed reason = %q, want %q", failed.Reason, ReasonSubjectAltNames)
		}
		if !strings.Contains(failed.Message, "Approved, but failed a check") {
			t.Errorf("Failed message = %q", failed.Message)
		}
		csr = s.reconcile(t, name)
		if len(csr.Status.Certificate) != 0 {
			t.Error("a request that failed a check was signed")
		}
		if condition(csr, certificatesv1.CertificateDenied) != nil {
			t.Error("an approved request was denied as well")
		}
	})

	t.Run("a request that passes every check is signed", func(t *testing.T) {
		s := newSigner(t)
		name := s.submit(t, execAgentUser, nodeA, clientRequest(nodeA))
		s.approveAsAdmin(t, name)
		csr := s.reconcile(t, name)
		cert := certificate(t, csr)
		if err := s.clientCA.verify(cert, x509.ExtKeyUsageClientAuth); err != nil {
			t.Errorf("not signed by the client CA: %v", err)
		}
		wantIssuedTo(t, cert, nodeA)
		if got := condition(csr, certificatesv1.CertificateApproved).Reason; got != "AdminApproved" {
			t.Errorf("Approved reason = %q, want the admin's", got)
		}
	})
}

func TestSignerMarksAnApprovedRequestThatFailsACheckFailed(t *testing.T) {
	//= docs/requirements/09-certificates.md#signing
	//= type=test
	//# If a request for any of the three signer names carries the
	//# condition `Approved` and fails any check of [Approval](#approval), then
	//# the Operator SHALL mark it `Failed`, with a reason that names the check,
	//# and never sign it.

	//= docs/requirements/09-certificates.md#approval
	//= type=test
	//# If a request for any of the three signer names fails any check
	//# and does not carry the condition `Approved`, then the Operator SHALL deny
	//# it with a reason that names the check.

	for _, tc := range []struct {
		name   string
		user   string
		node   string
		r      request
		reason string
	}{
		{"a serving request from another ServiceAccount",
			otherUser, nodeA, servingRequest(nodeA, nodeAIP), ReasonRequester},
		{"a client request whose requester has no Node",
			execAgentUser, "", clientRequest(nodeA), ReasonNodeName},
		{"an Exec Agent serving request whose requester's Node does not exist",
			execAgentUser, nodeGone, execAgentServingRequest(nodeGone, nodeGoneIP), ReasonNodeNotFound},
		{"a serving request for another Node",
			execAgentUser, nodeA, servingRequest(nodeB, nodeBIP), ReasonSubjectAltNames},
		{"a client request for another Node's Exec Agent",
			execAgentUser, nodeA, clientRequest(nodeB), ReasonSubjectAltNames},
		{"an Exec Agent serving request for another Node",
			execAgentUser, nodeA, execAgentServingRequest(nodeB, nodeBIP), ReasonSubjectAltNames},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSigner(t)
			name := s.submit(t, tc.user, tc.node, tc.r)
			s.approveAsAdmin(t, name)
			csr := s.reconcile(t, name)
			failed := condition(csr, certificatesv1.CertificateFailed)
			if failed == nil || failed.Status != corev1.ConditionTrue {
				t.Fatalf("conditions = %+v, want Failed", csr.Status.Conditions)
			}
			if failed.Reason != tc.reason {
				t.Errorf("Failed reason = %q, want %q", failed.Reason, tc.reason)
			}
			if !strings.Contains(failed.Message, "Approved, but failed a check") {
				t.Errorf("Failed message = %q", failed.Message)
			}
			// An approved request is not denied: Denied cannot be added
			// beside Approved.
			if condition(csr, certificatesv1.CertificateDenied) != nil {
				t.Error("an approved request was denied as well")
			}
			// Once Failed, it stays unsigned.
			if csr := s.reconcile(t, name); len(csr.Status.Certificate) != 0 {
				t.Error("an approved request that failed a check was signed")
			}
		})
	}
}

func TestSignerLeavesOtherSignerNamesAlone(t *testing.T) {
	//= docs/requirements/09-certificates.md#signing
	//= type=test
	//# The Operator SHALL approve and sign `CertificateSigningRequest`s
	//# for the signer names `battery.liquidmetal-x.dev/flintlockd-serving`,
	//# `battery.liquidmetal-x.dev/flintlockd-client` and
	//# `battery.liquidmetal-x.dev/exec-agent-serving`, and for no other signer
	//# name.

	s := newSigner(t)
	r := servingRequest(nodeA, nodeAIP)
	r.signer = "example.test/other"
	csr := s.reconcile(t, s.submit(t, execAgentUser, nodeA, r))
	if len(csr.Status.Conditions) != 0 || len(csr.Status.Certificate) != 0 {
		t.Errorf("status = %+v, want it untouched", csr.Status)
	}

	for _, name := range []string{hostcert.ServingSigner, hostcert.ClientSigner, hostcert.ExecAgentServingSigner} {
		if !isOurSigner(name) {
			t.Errorf("isOurSigner(%s) = false", name)
		}
	}
	for _, name := range []string{"example.test/other", "kubernetes.io/kube-apiserver-client", "battery.liquidmetal-x.dev/other"} {
		if isOurSigner(name) {
			t.Errorf("isOurSigner(%s) = true", name)
		}
	}
}

func TestCABundlePublishesTheCACertificates(t *testing.T) {
	//= docs/requirements/09-certificates.md#signing
	//= type=test
	//# The Operator SHALL publish the certificates of the serving CA and the
	//# `flintlockd` client CA, without their keys, in a ConfigMap in its
	//# namespace.

	ctx := context.Background()
	bundleKey := client.ObjectKey{Namespace: operatorNamespace, Name: DefaultCABundleConfigMap}
	reconcileBundle := func(t *testing.T, s *signer) *corev1.ConfigMap {
		t.Helper()
		if _, err := s.bundle.Reconcile(ctx, ctrl.Request{NamespacedName: bundleKey}); err != nil {
			t.Fatal(err)
		}
		cm := &corev1.ConfigMap{}
		if err := s.c.Get(ctx, bundleKey, cm); err != nil {
			t.Fatal(err)
		}
		return cm
	}
	wantBundle := func(t *testing.T, cm *corev1.ConfigMap, want map[string]string) {
		t.Helper()
		if len(cm.Data) != len(want) {
			t.Errorf("data has keys %v, want %d", keys(cm.Data), len(want))
		}
		for k, v := range want {
			if cm.Data[k] != v {
				t.Errorf("data[%s] is not the CA certificate", k)
			}
		}
		if len(cm.BinaryData) != 0 {
			t.Errorf("binaryData = %v, want none", keys(cm.BinaryData))
		}
		for k, v := range cm.Data {
			if strings.Contains(v, "PRIVATE KEY") {
				t.Errorf("data[%s] holds a private key", k)
			}
		}
	}

	t.Run("both CA certificates, without their keys", func(t *testing.T) {
		s := newSigner(t)
		wantBundle(t, reconcileBundle(t, s), map[string]string{
			ServingCAKey: string(s.serving.certPEM), ClientCAKey: string(s.clientCA.certPEM),
		})
	})

	t.Run("a CA's new certificate when its Secret changes", func(t *testing.T) {
		s := newSigner(t)
		reconcileBundle(t, s)
		rotated := newTestCA(t, "rotated client CA", time.Now().Add(365*24*time.Hour))
		if err := s.c.Update(ctx, rotated.secret(DefaultClientCASecret)); err != nil {
			t.Fatal(err)
		}
		wantBundle(t, reconcileBundle(t, s), map[string]string{
			ServingCAKey: string(s.serving.certPEM), ClientCAKey: string(rotated.certPEM),
		})
	})

	t.Run("again when the ConfigMap is deleted", func(t *testing.T) {
		s := newSigner(t)
		reconcileBundle(t, s)
		if err := s.c.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: bundleKey.Namespace, Name: bundleKey.Name}}); err != nil {
			t.Fatal(err)
		}
		wantBundle(t, reconcileBundle(t, s), map[string]string{
			ServingCAKey: string(s.serving.certPEM), ClientCAKey: string(s.clientCA.certPEM),
		})
	})

	t.Run("without a CA whose Secret does not exist yet", func(t *testing.T) {
		s := newSigner(t)
		if err := s.c.Delete(ctx, s.clientCA.secret(DefaultClientCASecret)); err != nil {
			t.Fatal(err)
		}
		wantBundle(t, reconcileBundle(t, s), map[string]string{ServingCAKey: string(s.serving.certPEM)})
	})
}

// keys returns m's keys, for messages.
func keys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}
