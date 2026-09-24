//go:build e2e

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

package e2e

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"slices"
	"testing"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/phoban01/battery-operator/internal/execagent"
	"github.com/phoban01/battery-operator/internal/hostcert"
)

// trustDomain is the Manifests' SPIFFE trust domain.
const trustDomain = "battery.liquidmetal-x.dev"

// TestHostCertificates: on every Host, the Exec Agent obtains its three
// certificates through CertificateSigningRequests that the Operator
// approves and signs, writes flintlockd's to the Host, and the fake
// flintlockd serves with them, which the agent verifies against the serving
// CA the Operator publishes before it reports the Host ready.
func TestHostCertificates(t *testing.T) {
	//= docs/requirements/08-test-doubles.md#test-environments
	//= type=test
	//# The e2e suite SHALL test the behaviour that depends on the
	//# Kubernetes API server, including CRD validation, admission policies,
	//# TokenReview, TokenRequest and `CertificateSigningRequest`s, in the kind
	//# cluster.

	f := features.New("the Hosts' certificates").
		Assess("the Operator signs every Host's certificates", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c := mustClient(t, cfg)
			hosts, err := hostNodes(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			for _, h := range hosts {
				t.Run(h.Name, func(t *testing.T) {
					ip, err := internalIP(h)
					if err != nil {
						t.Fatal(err)
					}
					//= docs/requirements/05-exec-agent.md#certificates
					//= type=test
					//# The Exec Agent SHALL obtain its Host's `flintlockd` serving
					//# certificate with a `CertificateSigningRequest` for the signer
					//# `battery.liquidmetal-x.dev/flintlockd-serving` that names the Host's
					//# internal address and the SPIFFE ID
					//# `spiffe://<trust domain>/flintlock/host/<node name>`, and nothing else.
					cert := waitForIssued(ctx, t, c, hostcert.ServingSigner, h.Name)
					wantNames(t, cert, hostcert.HostID(trustDomain, h.Name), ip)

					//= docs/requirements/05-exec-agent.md#certificates
					//= type=test
					//# The Exec Agent SHALL obtain its own client certificate with a
					//# `CertificateSigningRequest` for the signer
					//# `battery.liquidmetal-x.dev/flintlockd-client` that names the SPIFFE ID
					//# `spiffe://<trust domain>/flintlock/client/exec-agent/<node name>`, and
					//# nothing else.
					cert = waitForIssued(ctx, t, c, hostcert.ClientSigner, h.Name)
					wantNames(t, cert, hostcert.ExecAgentID(trustDomain, h.Name), "")

					cert = waitForIssued(ctx, t, c, hostcert.ExecAgentServingSigner, h.Name)
					wantNames(t, cert, hostcert.ExecAgentID(trustDomain, h.Name), ip)
				})
			}
			return ctx
		}).
		Assess("the fake flintlockd serves with them, and the Exec Agent reports its Host ready", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c := mustClient(t, cfg)
			hosts, err := hostNodes(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			for _, h := range hosts {
				t.Run(h.Name, func(t *testing.T) {
					var n corev1.Node
					err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
						if err := c.Get(ctx, client.ObjectKey{Name: h.Name}, &n); err != nil {
							return false, nil
						}
						return n.Annotations[execagent.AnnotationReady] == isTrue, nil
					})
					if err != nil {
						t.Fatalf("the Exec Agent does not report %s ready: %s=%q, %s=%q",
							h.Name, execagent.AnnotationReason, n.Annotations[execagent.AnnotationReason],
							execagent.AnnotationMessage, n.Annotations[execagent.AnnotationMessage])
					}
				})
			}
			return ctx
		}).
		Feature()

	testenv.Test(t, f)
}

// waitForIssued waits for the Exec Agent of node to have a request for
// signer that the Operator has approved and signed, and returns its
// certificate.
func waitForIssued(ctx context.Context, t *testing.T, c client.Client, signer, node string) *x509.Certificate {
	t.Helper()
	agent := fmt.Sprintf("system:serviceaccount:%s:%s", namespace, execAgentServiceAccount)
	var found string
	var cert *x509.Certificate
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		var csrs certificatesv1.CertificateSigningRequestList
		if err := c.List(ctx, &csrs); err != nil {
			return false, nil
		}
		for _, csr := range csrs.Items {
			if csr.Spec.SignerName != signer || csr.Spec.Username != agent ||
				!slices.Contains(csr.Spec.Extra[hostcert.NodeNameExtra], node) {
				continue
			}
			found = csr.Name
			if !approved(csr) || len(csr.Status.Certificate) == 0 {
				continue
			}
			block, _ := pem.Decode(csr.Status.Certificate)
			if block == nil {
				return false, fmt.Errorf("the request %s holds no PEM certificate", csr.Name)
			}
			parsed, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return false, fmt.Errorf("the request %s: %w", csr.Name, err)
			}
			cert = parsed
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("no signed request of %s's Exec Agent for %s (last seen: %q): %v", node, signer, found, err)
	}
	return cert
}

func approved(csr certificatesv1.CertificateSigningRequest) bool {
	for _, cond := range csr.Status.Conditions {
		if cond.Type == certificatesv1.CertificateDenied || cond.Type == certificatesv1.CertificateFailed {
			return false
		}
	}
	for _, cond := range csr.Status.Conditions {
		if cond.Type == certificatesv1.CertificateApproved && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// wantNames fails t unless cert names the SPIFFE ID id and, when ip is not
// empty, the address ip, and nothing else.
func wantNames(t *testing.T, cert *x509.Certificate, id, ip string) {
	t.Helper()
	uris := make([]string, 0, len(cert.URIs))
	for _, u := range cert.URIs {
		uris = append(uris, u.String())
	}
	if !slices.Equal(uris, []string{id}) || len(cert.DNSNames) > 0 || len(cert.EmailAddresses) > 0 {
		t.Errorf("the certificate names %v, DNS names %v and email addresses %v, want only %s",
			uris, cert.DNSNames, cert.EmailAddresses, id)
	}
	var want []net.IP
	if ip != "" {
		want = []net.IP{net.ParseIP(ip)}
	}
	if !slices.EqualFunc(cert.IPAddresses, want, func(a, b net.IP) bool { return a.Equal(b) }) {
		t.Errorf("the certificate names the addresses %v, want %v", cert.IPAddresses, want)
	}
}
