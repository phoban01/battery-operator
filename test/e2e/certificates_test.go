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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	certificatesv1 "k8s.io/api/certificates/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/phoban01/battery-operator/internal/controller"
	"github.com/phoban01/battery-operator/internal/execagent"
	"github.com/phoban01/battery-operator/internal/hostcert"
)

// trustDomain is the Manifests' SPIFFE trust domain.
const trustDomain = "battery.liquidmetal-x.dev"

// TestHostCertificates: on every Host, the Exec Agent obtains its three
// certificates through CertificateSigningRequests that the Operator
// approves and signs, writes flintlockd's to the Host, and the fake
// flintlockd serves with them, which the agent verifies against the serving
// CA the Operator publishes before it reports the Host ready. The agent's
// RBAC lets it create and read its requests, and no more.
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
		Assess("the Operator pinned every Host's address, where no kubelet or Exec Agent may change it", assessAddressPins).
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
		Assess("the Exec Agent may create and read its requests, and nothing else of them", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/05-exec-agent.md#certificates
			//= type=test
			//# The Manifests SHALL grant the Exec Agent's identity permission
			//# to create and read `CertificateSigningRequest`s, and no other permission
			//# on them.
			c := mustClient(t, cfg)
			agent := fmt.Sprintf("system:serviceaccount:%s:%s", namespace, execAgentServiceAccount)
			allowed := func(attrs authorizationv1.ResourceAttributes) bool {
				t.Helper()
				sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
					User:               agent,
					Groups:             []string{"system:serviceaccounts", "system:serviceaccounts:" + namespace, "system:authenticated"},
					ResourceAttributes: &attrs,
				}}
				if err := c.Create(ctx, sar); err != nil {
					t.Fatal(err)
				}
				return sar.Status.Allowed
			}
			csrs := func(verb, sub string) authorizationv1.ResourceAttributes {
				return authorizationv1.ResourceAttributes{Group: certificatesv1.GroupName, Resource: "certificatesigningrequests", Subresource: sub, Verb: verb}
			}
			for _, verb := range []string{"create", "get"} {
				if !allowed(csrs(verb, "")) {
					t.Errorf("the agent may not %s CertificateSigningRequests", verb)
				}
			}
			for _, verb := range []string{"list", "watch", "update", "patch", "delete", "deletecollection"} {
				if allowed(csrs(verb, "")) {
					t.Errorf("the agent may %s CertificateSigningRequests", verb)
				}
			}
			for _, sub := range []string{"approval", "status"} {
				if allowed(csrs("update", sub)) {
					t.Errorf("the agent may update CertificateSigningRequests/%s", sub)
				}
			}
			for _, signer := range []string{hostcert.ServingSigner, hostcert.ClientSigner, hostcert.ExecAgentServingSigner} {
				for _, verb := range []string{"approve", "sign"} {
					if allowed(authorizationv1.ResourceAttributes{Group: certificatesv1.GroupName, Resource: "signers", Name: signer, Verb: verb}) {
						t.Errorf("the agent may %s for %s", verb, signer)
					}
				}
			}
			// Its one other certificate grant: reading the Operator's CA
			// bundle ConfigMap, and no other ConfigMap.
			configMap := func(verb, name string) authorizationv1.ResourceAttributes {
				return authorizationv1.ResourceAttributes{Namespace: namespace, Resource: "configmaps", Name: name, Verb: verb}
			}
			if !allowed(configMap("get", controller.DefaultCABundleConfigMap)) {
				t.Error("the agent may not read the Operator's CA bundle ConfigMap")
			}
			for _, attrs := range []authorizationv1.ResourceAttributes{
				configMap("get", "other"), configMap("list", ""), configMap("update", controller.DefaultCABundleConfigMap),
			} {
				if allowed(attrs) {
					t.Errorf("the agent may %s ConfigMap %q", attrs.Verb, attrs.Name)
				}
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

// operatorUser is the Operator's ServiceAccount, as the Manifests name it.
const operatorUser = "system:serviceaccount:" + namespace + ":battery-operator-controller-manager"

// operatorLease is the Operator's leader-election Lease (cmd/main.go).
const operatorLease = "6838d5b8.liquidmetal-x.dev"

// TestSigner: the parts of the CertificateSigningRequest signer that only
// an API server shows, with the Manifests' RBAC. Its review and signing
// logic is unit-tested in internal/controller.
func TestSigner(t *testing.T) {
	//= docs/requirements/08-test-doubles.md#test-environments
	//= type=test
	//# The e2e suite SHALL test the behaviour that depends on the
	//# Kubernetes API server, including CRD validation, admission policies,
	//# TokenReview, TokenRequest and `CertificateSigningRequest`s, in the kind
	//# cluster.

	f := features.New("the CertificateSigningRequest signer").
		Assess("the Operator runs under leader election with the leader-election Role", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c := mustClient(t, cfg)
			var lease coordinationv1.Lease
			err := wait.PollUntilContextTimeout(ctx, 2*time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
				if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: operatorLease}, &lease); err != nil {
					return false, nil
				}
				return lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != "", nil
			})
			if err != nil {
				t.Fatalf("no holder of the Lease %s/%s: %v", namespace, operatorLease, err)
			}
			return ctx
		}).
		Assess("the Operator may approve and sign for its three signer names, and no other", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/09-certificates.md#signing
			//= type=test
			//# The Operator SHALL approve and sign `CertificateSigningRequest`s
			//# for the signer names `battery.liquidmetal-x.dev/flintlockd-serving`,
			//# `battery.liquidmetal-x.dev/flintlockd-client` and
			//# `battery.liquidmetal-x.dev/exec-agent-serving`, and for no other signer
			//# name.
			c := mustClient(t, cfg)
			for _, signer := range []string{hostcert.ServingSigner, hostcert.ClientSigner, hostcert.ExecAgentServingSigner, "example.test/other"} {
				want := signer != "example.test/other"
				for _, verb := range []string{"approve", "sign"} {
					sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
						User: operatorUser,
						ResourceAttributes: &authorizationv1.ResourceAttributes{
							Group: certificatesv1.GroupName, Resource: "signers", Verb: verb, Name: signer,
						},
					}}
					if err := c.Create(ctx, sar); err != nil {
						t.Fatal(err)
					}
					if sar.Status.Allowed != want {
						t.Errorf("the Operator may %s for %s: %t, want %t", verb, signer, sar.Status.Allowed, want)
					}
				}
			}
			return ctx
		}).
		Assess("a request from anyone but the Exec Agent is denied, and one for another signer left alone", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/09-certificates.md#approval
			//= type=test
			//# If a request for any of the three signer names fails any check
			//# and does not carry the condition `Approved`, then the Operator SHALL deny
			//# it with a reason that names the check.
			c := mustClient(t, cfg)
			// The suite's own identity, which the API server records on the
			// request, is the cluster's admin, not the Exec Agent.
			denied := adminCSR(ctx, t, c, hostcert.ServingSigner)
			other := adminCSR(ctx, t, c, "example.test/other")

			var got certificatesv1.CertificateSigningRequest
			err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
				if err := c.Get(ctx, client.ObjectKey{Name: denied}, &got); err != nil {
					return false, nil
				}
				return len(got.Status.Conditions) > 0, nil
			})
			if err != nil {
				t.Fatalf("the Operator did not decide on %s: %v", denied, err)
			}
			if cond := got.Status.Conditions[0]; cond.Type != certificatesv1.CertificateDenied || cond.Reason != controller.ReasonRequester {
				t.Errorf("conditions = %+v, want Denied for %s", got.Status.Conditions, controller.ReasonRequester)
			}
			if len(got.Status.Certificate) != 0 {
				t.Error("a denied request was signed")
			}

			// By now the Operator has seen the other request too.
			if err := c.Get(ctx, client.ObjectKey{Name: other}, &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Status.Conditions) != 0 || len(got.Status.Certificate) != 0 {
				t.Errorf("the request for another signer has status %+v, want it untouched", got.Status)
			}
			return ctx
		}).
		Feature()

	testenv.Test(t, f)
}

// adminCSR creates a request for signer as the suite's identity, deletes
// it when t ends, and returns its name.
func adminCSR(ctx context.Context, t *testing.T, c client.Client, signer string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "e2e"}}, key)
	if err != nil {
		t.Fatal(err)
	}
	csr := &certificatesv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-signer-"},
		Spec: certificatesv1.CertificateSigningRequestSpec{
			Request:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}),
			SignerName: signer,
			Usages: []certificatesv1.KeyUsage{
				certificatesv1.UsageDigitalSignature, certificatesv1.UsageKeyEncipherment, certificatesv1.UsageServerAuth,
			},
		},
	}
	if err := c.Create(ctx, csr); err != nil {
		t.Fatalf("creating a request for %s: %v", signer, err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), csr) })
	return csr.Name
}

// assessAddressPins checks that the Operator pinned each Host's internal
// address to its Node, and that neither the Exec Agent nor any Host's
// kubelet may write the pins.
func assessAddressPins(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	const authenticated = "system:authenticated"
	//= docs/requirements/09-certificates.md#approval
	//= type=test
	//# When the Operator signs a
	//# `battery.liquidmetal-x.dev/flintlockd-serving` or
	//# `battery.liquidmetal-x.dev/exec-agent-serving` request for a Node that has
	//# no pinned address, the Operator SHALL pin the request's IP address to that
	//# Node, and SHALL store the certificate only once the pin is stored.
	c := mustClient(t, cfg)
	hosts, err := hostNodes(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	var pins corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: controller.AddressPinsConfigMap}, &pins); err != nil {
		t.Fatalf("reading the address pins: %v", err)
	}
	for _, h := range hosts {
		ip, err := internalIP(h)
		if err != nil {
			t.Fatal(err)
		}
		if got := net.ParseIP(pins.Data[h.Name]); got == nil || !got.Equal(net.ParseIP(ip)) {
			t.Errorf("%s is pinned to %q, want %s", h.Name, pins.Data[h.Name], ip)
		}
	}

	//= docs/requirements/09-certificates.md#approval
	//= type=test
	//# The Manifests SHALL grant write access to the ConfigMap
	//# `host-address-pins` to the Operator's identity and to no other identity
	//# they create.
	allowed := func(user string, groups []string, verb string) bool {
		t.Helper()
		sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   user,
			Groups: groups,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: namespace, Resource: "configmaps", Name: controller.AddressPinsConfigMap, Verb: verb,
			},
		}}
		if err := c.Create(ctx, sar); err != nil {
			t.Fatal(err)
		}
		return sar.Status.Allowed
	}
	if !allowed(operatorUser, nil, "update") {
		t.Error("the Operator may not update the address pins")
	}
	agent := fmt.Sprintf("system:serviceaccount:%s:%s", namespace, execAgentServiceAccount)
	others := map[string][]string{
		agent: {"system:serviceaccounts", "system:serviceaccounts:" + namespace, authenticated},
	}
	// Each Host's kubelet, as the Node authorizer sees it.
	for _, h := range hosts {
		others["system:node:"+h.Name] = []string{"system:nodes", authenticated}
	}
	for user, groups := range others {
		for verb := range strings.FieldsSeq("create update patch delete deletecollection") {
			if allowed(user, groups, verb) {
				t.Errorf("%s may %s the address pins", user, verb)
			}
		}
	}
	return ctx
}
