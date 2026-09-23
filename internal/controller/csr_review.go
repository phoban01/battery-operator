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
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"slices"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/phoban01/battery-operator/internal/hostcert"
)

// The reasons a request is denied for, one for each check.
const (
	// ReasonRequester: the requester is not the Exec Agent's ServiceAccount.
	ReasonRequester = "RequesterNotExecAgent"
	// ReasonNodeName: the requester's user info names no Node.
	ReasonNodeName = "RequesterNodeMissing"
	// ReasonNodeNotFound: the requester's Node does not exist.
	ReasonNodeNotFound = "NodeNotFound"
	// ReasonInvalidRequest: spec.request is not a well-formed, self-signed
	// PKCS#10 request.
	ReasonInvalidRequest = "InvalidRequest"
	// ReasonKeyUsages: spec.usages are not the signer's key usages.
	ReasonKeyUsages = "KeyUsagesMismatch"
	// ReasonSubjectAltNames: the request's subject alternative names are not
	// exactly the ones its signer and Node call for.
	ReasonSubjectAltNames = "SubjectAltNamesMismatch"
)

// denial is why a request is denied.
type denial struct {
	reason  string
	message string
}

func denyf(reason, format string, args ...any) *denial {
	return &denial{reason: reason, message: fmt.Sprintf(format, args...)}
}

// signerUsages are the key usages each signer name issues with.
var signerUsages = map[string][]certificatesv1.KeyUsage{
	hostcert.ServingSigner: {certificatesv1.UsageDigitalSignature, certificatesv1.UsageKeyEncipherment, certificatesv1.UsageServerAuth},
	hostcert.ClientSigner:  {certificatesv1.UsageDigitalSignature, certificatesv1.UsageKeyEncipherment, certificatesv1.UsageClientAuth},
}

// review decides whether csr is approved. It returns nil when every check
// passes, and otherwise why the request is denied. An error means the
// review could not be made and has to be retried.
func (r *CertificateSigningRequestReconciler) review(ctx context.Context, csr *certificatesv1.CertificateSigningRequest) (*denial, error) {
	//= docs/requirements/09-certificates.md#approval
	//# If a request for either signer name fails any check, then the
	//# Operator SHALL deny it with a reason that names the check.
	//
	//= docs/requirements/09-certificates.md#approval
	//# The Operator SHALL approve a
	//# `battery.liquidmetal-x.dev/flintlockd-serving` request only when the
	//# requester is the Exec Agent's ServiceAccount, the requester's
	//# `authentication.kubernetes.io/node-name` names an existing Node, and the
	//# request's subject alternative names are exactly that Node's internal
	//# address and `spiffe://<trust domain>/flintlock/host/<node name>`.
	//
	//= docs/requirements/09-certificates.md#approval
	//# The Operator SHALL approve a
	//# `battery.liquidmetal-x.dev/flintlockd-client` request only when the
	//# requester is the Exec Agent's ServiceAccount and the request's only
	//# subject alternative name is
	//# `spiffe://<trust domain>/flintlock/client/exec-agent/<node name>` for the
	//# Node in the requester's `authentication.kubernetes.io/node-name`.
	if want := r.Config.execAgentUsername(); csr.Spec.Username != want {
		return denyf(ReasonRequester, "The requester %q is not the Exec Agent's ServiceAccount %q", csr.Spec.Username, want), nil
	}
	nodes := csr.Spec.Extra[hostcert.NodeNameExtra]
	if len(nodes) != 1 || nodes[0] == "" {
		return denyf(ReasonNodeName, "The requester's %s names no single Node", hostcert.NodeNameExtra), nil
	}
	nodeName := nodes[0]

	req, err := parseRequest(csr.Spec.Request)
	if err != nil {
		return denyf(ReasonInvalidRequest, "%v", err), nil
	}

	//= docs/requirements/09-certificates.md#approval
	//# The Operator SHALL approve a request only when its key usages
	//# are digital signature and key encipherment with server auth for
	//# `flintlockd-serving`, or with client auth for `flintlockd-client`.
	if want := signerUsages[csr.Spec.SignerName]; !sameUsages(csr.Spec.Usages, want) {
		return denyf(ReasonKeyUsages, "The key usages %v are not %v", csr.Spec.Usages, want), nil
	}

	if csr.Spec.SignerName == hostcert.ClientSigner {
		return checkSANs(req, hostcert.ExecAgentID(r.Config.TrustDomain, nodeName), nil), nil
	}

	node := &corev1.Node{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
		if apierrors.IsNotFound(err) {
			return denyf(ReasonNodeNotFound, "The requester's Node %q does not exist", nodeName), nil
		}
		return nil, fmt.Errorf("reading Node %s: %w", nodeName, err)
	}
	var addrs []net.IP
	for _, a := range node.Status.Addresses {
		if ip := net.ParseIP(a.Address); a.Type == corev1.NodeInternalIP && ip != nil {
			addrs = append(addrs, ip)
		}
	}
	if len(addrs) == 0 {
		return denyf(ReasonSubjectAltNames, "The Node %q has no internal address", nodeName), nil
	}
	return checkSANs(req, hostcert.HostID(r.Config.TrustDomain, nodeName), addrs), nil
}

// checkSANs denies req unless its subject alternative names are exactly the
// URI uri and, when ips is not empty, one IP address from ips. ips holds a
// Node's internal addresses, of which a dual-stack Node has two.
func checkSANs(req *x509.CertificateRequest, uri string, ips []net.IP) *denial {
	if len(req.DNSNames) > 0 || len(req.EmailAddresses) > 0 {
		return denyf(ReasonSubjectAltNames, "The request names DNS names %v or email addresses %v, which are not allowed",
			req.DNSNames, req.EmailAddresses)
	}
	if len(req.URIs) != 1 || req.URIs[0].String() != uri {
		uris := make([]string, 0, len(req.URIs))
		for _, u := range req.URIs {
			uris = append(uris, u.String())
		}
		return denyf(ReasonSubjectAltNames, "The request names the URIs %v, not exactly %s", uris, uri)
	}
	if len(ips) == 0 {
		if len(req.IPAddresses) > 0 {
			return denyf(ReasonSubjectAltNames, "The request names the IP addresses %v, which are not allowed", req.IPAddresses)
		}
		return nil
	}
	if len(req.IPAddresses) != 1 || !slices.ContainsFunc(ips, req.IPAddresses[0].Equal) {
		return denyf(ReasonSubjectAltNames, "The request names the IP addresses %v, not exactly one of the Node's internal addresses %v",
			req.IPAddresses, ips)
	}
	return nil
}

// sameUsages reports whether got and want hold the same key usages, in any
// order.
func sameUsages(got, want []certificatesv1.KeyUsage) bool {
	g := slices.Clone(got)
	slices.Sort(g)
	g = slices.Compact(g)
	w := slices.Sorted(slices.Values(want))
	return len(g) == len(got) && slices.Equal(g, w)
}

// parseRequest decodes a PEM PKCS#10 request and checks its signature.
func parseRequest(data []byte) (*x509.CertificateRequest, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("the request is not a PEM CERTIFICATE REQUEST")
	}
	if len(rest) > 0 {
		if b, _ := pem.Decode(rest); b != nil {
			return nil, errors.New("the request holds more than one PEM block")
		}
	}
	req, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("the request does not parse: %w", err)
	}
	if err := req.CheckSignature(); err != nil {
		return nil, fmt.Errorf("the request's signature does not verify: %w", err)
	}
	return req, nil
}
