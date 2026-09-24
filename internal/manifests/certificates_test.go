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

package manifests_test

import (
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/phoban01/battery-operator/internal/controller"
	"github.com/phoban01/battery-operator/internal/hostcert"
)

// issuer is the part of a cert-manager Issuer the Manifests use; parsing
// strictly into it rejects anything else.
type issuer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              struct {
		SelfSigned *struct{} `json:"selfSigned,omitempty"`
		CA         *struct {
			SecretName string `json:"secretName"`
		} `json:"ca,omitempty"`
	} `json:"spec"`
}

// certificate is the part of a cert-manager Certificate the Manifests use.
type certificate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              struct {
		IsCA           bool     `json:"isCA,omitempty"`
		CommonName     string   `json:"commonName,omitempty"`
		SecretName     string   `json:"secretName"`
		Duration       string   `json:"duration,omitempty"`
		RenewBefore    string   `json:"renewBefore,omitempty"`
		DNSNames       []string `json:"dnsNames,omitempty"`
		IPAddresses    []string `json:"ipAddresses,omitempty"`
		EmailAddresses []string `json:"emailAddresses,omitempty"`
		URIs           []string `json:"uris,omitempty"`
		PrivateKey     struct {
			Algorithm      string `json:"algorithm"`
			Size           int    `json:"size"`
			RotationPolicy string `json:"rotationPolicy"`
		} `json:"privateKey"`
		Usages    []string `json:"usages"`
		IssuerRef struct {
			Group string `json:"group"`
			Kind  string `json:"kind"`
			Name  string `json:"name"`
		} `json:"issuerRef"`
	} `json:"spec"`
}

// issuerOf returns the Issuer c names, which has to be rendered with it, in
// its namespace.
func issuerOf(t *testing.T, objs objects, c certificate) issuer {
	t.Helper()
	if c.Spec.IssuerRef.Kind != "Issuer" || c.Spec.IssuerRef.Group != "cert-manager.io" {
		t.Fatalf("Certificate %s is issued by a %s of %s, want an Issuer of cert-manager.io",
			c.Name, c.Spec.IssuerRef.Kind, c.Spec.IssuerRef.Group)
	}
	iss := one[issuer](t, objs, "Issuer", c.Spec.IssuerRef.Name)
	if iss.Namespace != c.Namespace {
		t.Fatalf("Issuer %s is in %s, Certificate %s in %s", iss.Name, iss.Namespace, c.Name, c.Namespace)
	}
	return iss
}

//= docs/requirements/09-certificates.md#spiffe-identity
//= type=test
//# The Manifests SHALL create the serving CA and the `flintlockd`
//# client CA as cert-manager CA certificates in the Operator's namespace.

// TestCACertificates checks that each CA is a cert-manager CA Certificate
// in the Operator's namespace, self-signed, whose Secret has the name the
// Operator reads it by, and that keeps its key when it is renewed.
func TestCACertificates(t *testing.T) {
	t.Parallel()
	objs := build(t, "config/default")
	secrets := map[string]bool{}
	for _, name := range []string{"battery-operator-flintlockd-serving-ca", "battery-operator-flintlockd-client-ca"} {
		c := one[certificate](t, objs, "Certificate", name)
		if !c.Spec.IsCA || c.Namespace != operatorNamespace {
			t.Errorf("Certificate %s: isCA %v in %s, want a CA in %s", name, c.Spec.IsCA, c.Namespace, operatorNamespace)
		}
		if !slices.Contains(c.Spec.Usages, "cert sign") {
			t.Errorf("Certificate %s may not sign certificates: usages %v", name, c.Spec.Usages)
		}
		if c.Spec.PrivateKey.RotationPolicy != "Never" {
			t.Errorf("Certificate %s replaces its key on renewal, invalidating what it signed", name)
		}
		if iss := issuerOf(t, objs, c); iss.Spec.SelfSigned == nil {
			t.Errorf("Certificate %s is not self-signed", name)
		}
		secrets[c.Spec.SecretName] = true
	}
	for _, want := range []string{controller.DefaultServingCASecret, controller.DefaultClientCASecret} {
		if !secrets[want] {
			t.Errorf("no CA Certificate writes Secret %s, which the Operator reads; the CAs write %v", want, secrets)
		}
	}
}

//= docs/requirements/09-certificates.md#spiffe-identity
//= type=test
//# The Manifests SHALL issue battery's client certificate from the
//# `flintlockd` client CA with cert-manager, naming
//# `spiffe://<trust domain>/flintlock/client/battery` as its only subject
//# alternative name.

// TestBatteryClientCertificate checks battery's client certificate: issued
// by the Issuer backed by the flintlockd client CA's Secret, for client
// authentication, with battery's SPIFFE ID under the Operator's trust
// domain as its only subject alternative name. The Operator's trust domain
// is a real one, and the Exec Agent's is the same.
func TestBatteryClientCertificate(t *testing.T) {
	t.Parallel()
	objs := build(t, "config/default", "config/exec-agent")
	_, manager, _ := operator(t, objs)
	td, ok := flag(manager.Args, "trust-domain")
	if !ok || hostcert.ValidateTrustDomain(td) != nil || td == "cluster.local" {
		t.Fatalf("the Operator's trust domain is %q; want a real one", td)
	}
	agent := one[appsv1.DaemonSet](t, objs, "DaemonSet", "battery-operator-exec-agent")
	if agentTD, _ := flag(agent.Spec.Template.Spec.Containers[0].Args, "trust-domain"); agentTD != td {
		t.Errorf("the Exec Agent's trust domain is %q, the Operator's %q", agentTD, td)
	}

	c := one[certificate](t, objs, "Certificate", "battery-operator-battery-flintlockd-client")
	iss := issuerOf(t, objs, c)
	if iss.Spec.CA == nil || iss.Spec.CA.SecretName != controller.DefaultClientCASecret {
		t.Errorf("battery's certificate is issued by %s, which is not backed by %s", iss.Name, controller.DefaultClientCASecret)
	}
	if c.Namespace != operatorNamespace {
		t.Errorf("battery's certificate is in %s, want %s", c.Namespace, operatorNamespace)
	}
	if want := []string{hostcert.BatteryID(td)}; !slices.Equal(c.Spec.URIs, want) {
		t.Errorf("battery's certificate names URIs %v, want %v", c.Spec.URIs, want)
	}
	if len(c.Spec.DNSNames)+len(c.Spec.IPAddresses)+len(c.Spec.EmailAddresses) != 0 || c.Spec.CommonName != "" {
		t.Errorf("battery's certificate names more than its SPIFFE ID: %+v", c.Spec)
	}
	if c.Spec.IsCA || !slices.Contains(c.Spec.Usages, "client auth") || slices.Contains(c.Spec.Usages, "server auth") {
		t.Errorf("battery's certificate is not a client certificate: isCA %v, usages %v", c.Spec.IsCA, c.Spec.Usages)
	}
}
