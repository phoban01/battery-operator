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
	"fmt"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/phoban01/battery-operator/internal/reconcile"
)

// csrScope is one reconcile of one CertificateSigningRequest. The review's
// checks record what they find in it, and the decision subreconcilers what
// is to be written; none of them writes to the API server. write does,
// once, after the chain.
type csrScope struct {
	*reconcile.Scope[*certificatesv1.CertificateSigningRequest]

	// APIReader reads Nodes without a cache.
	APIReader client.Reader
	// Config is the signer's configuration.
	Config SignerConfig
	// cas reads the CA Secrets by name.
	cas map[string]client.Reader

	// node is the requester's Node name, once csrRequester has found it.
	node string
	// req is the parsed PKCS#10 request, once csrRequest has parsed it.
	req *x509.CertificateRequest
	// denial is the first check the request failed, nil while it has
	// failed none.
	denial *denial

	// approval is the condition, Approved or Denied, that write adds
	// through the approval subresource; nil for none.
	approval *certificatesv1.CertificateSigningRequestCondition
	// failure is the Failed condition that write adds through status; nil
	// for none.
	failure *certificatesv1.CertificateSigningRequestCondition
	// certificate is the certificate that write sets in status once the
	// request is approved; nil for none.
	certificate []byte
}

// condition is a condition of type t, true, stamped with the scope's
// clock.
func (s *csrScope) condition(t certificatesv1.RequestConditionType, reason, message string) *certificatesv1.CertificateSigningRequestCondition {
	return &certificatesv1.CertificateSigningRequestCondition{
		Type:           t,
		Status:         corev1.ConditionTrue,
		Reason:         reason,
		Message:        message,
		LastUpdateTime: metav1.NewTime(s.Clock.Now()),
	}
}

// write stores what the chain decided, in the order the API server needs
// it: an Approved or Denied condition through the approval subresource,
// then a Failed condition or the certificate through status. The API
// server keeps status.certificate for approved requests only, so a request
// approved in this reconcile is written approved first, and its
// certificate goes only on a request that, as stored after that write, is
// still approved and not denied.
func (s *csrScope) write(ctx context.Context) error {
	csr := s.Object
	if s.approval != nil {
		t := s.approval.Type
		csr.Status.Conditions = append(csr.Status.Conditions, *s.approval)
		if err := s.Client.SubResource("approval").Update(ctx, csr); err != nil {
			return fmt.Errorf("setting condition %s on CertificateSigningRequest %s: %w", t, csr.Name, err)
		}
		if t == certificatesv1.CertificateDenied {
			s.Log.Info("Denied CertificateSigningRequest", "reason", s.denial.reason, "message", s.denial.message)
			return nil
		}
		s.Log.Info("Approved CertificateSigningRequest")
		// The re-check: the update returns the request as stored, so this
		// checks what the API server now holds.
		if !hasCondition(csr, certificatesv1.CertificateApproved) || hasCondition(csr, certificatesv1.CertificateDenied) {
			return nil
		}
	}

	switch {
	case s.failure != nil:
		csr.Status.Conditions = append(csr.Status.Conditions, *s.failure)
		if err := s.Client.Status().Update(ctx, csr); err != nil {
			return fmt.Errorf("setting condition %s on CertificateSigningRequest %s: %w", s.failure.Type, csr.Name, err)
		}
		s.Log.Info("Refused to sign an approved CertificateSigningRequest", "reason", s.denial.reason, "message", s.denial.message)
	case s.certificate != nil:
		csr.Status.Certificate = s.certificate
		if err := s.Client.Status().Update(ctx, csr); err != nil {
			return fmt.Errorf("writing the certificate of CertificateSigningRequest %s: %w", csr.Name, err)
		}
		s.Log.Info("Signed CertificateSigningRequest", "signer", csr.Spec.SignerName, "node", s.node)
	}
	return nil
}
