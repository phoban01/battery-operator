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
	"fmt"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/phoban01/battery-operator/internal/hostcert"
	"github.com/phoban01/battery-operator/internal/reconcile"
)

// csrChain is the signer's chain, in order: leave alone what is not the
// Operator's to decide, review, decide (deny, mark failed or approve), and
// sign.
func csrChain() reconcile.Chain[*csrScope] {
	return reconcile.Steps(
		csrUndecided{},
		csrNotDenied{},
		csrReview{},
		csrDeny{},
		csrFail{},
		csrApprove{},
		csrSign{},
	)
}

// csrUndecided stops the chain for a request that is not for one of the
// Operator's signer names, or that is already signed or marked Failed.
type csrUndecided struct{}

// Reconcile implements reconcile.SubReconciler.
func (csrUndecided) Reconcile(_ context.Context, s *csrScope) (reconcile.Result, error) {
	csr := s.Object
	if !isOurSigner(csr.Spec.SignerName) {
		return reconcile.Result{Stop: true}, nil
	}
	if len(csr.Status.Certificate) > 0 || hasCondition(csr, certificatesv1.CertificateFailed) {
		return reconcile.Result{Stop: true}, nil
	}
	return reconcile.Result{}, nil
}

// csrNotDenied stops the chain for a denied request.
type csrNotDenied struct{}

// Reconcile implements reconcile.SubReconciler.
func (csrNotDenied) Reconcile(_ context.Context, s *csrScope) (reconcile.Result, error) {
	//= docs/requirements/09-certificates.md#signing
	//# The Operator SHALL sign a request only while it carries the
	//# condition `Approved` and not the condition `Denied`.
	if hasCondition(s.Object, certificatesv1.CertificateDenied) {
		return reconcile.Result{Stop: true}, nil
	}
	return reconcile.Result{}, nil
}

// csrDeny denies a request that is not approved and failed a check, and
// stops the chain.
type csrDeny struct{}

// Reconcile implements reconcile.SubReconciler.
func (csrDeny) Reconcile(_ context.Context, s *csrScope) (reconcile.Result, error) {
	if hasCondition(s.Object, certificatesv1.CertificateApproved) || s.denial == nil {
		return reconcile.Result{}, nil
	}
	//= docs/requirements/09-certificates.md#approval
	//# If a request for any of the three signer names fails any check
	//# and does not carry the condition `Approved`, then the Operator SHALL deny
	//# it with a reason that names the check.
	s.approval = s.condition(certificatesv1.CertificateDenied, s.denial.reason, s.denial.message)
	return reconcile.Result{Stop: true}, nil
}

// csrFail marks Failed a request that someone approved and that failed a
// check, and stops the chain.
type csrFail struct{}

// Reconcile implements reconcile.SubReconciler.
func (csrFail) Reconcile(_ context.Context, s *csrScope) (reconcile.Result, error) {
	if !hasCondition(s.Object, certificatesv1.CertificateApproved) || s.denial == nil {
		return reconcile.Result{}, nil
	}
	//= docs/requirements/09-certificates.md#signing
	//# If a request for any of the three signer names carries the
	//# condition `Approved` and fails any check of [Approval](#approval), then
	//# the Operator SHALL mark it `Failed`, with a reason that names the check,
	//# and never sign it.
	msg := "Approved, but failed a check, so it is not signed: " + s.denial.message
	s.failure = s.condition(certificatesv1.CertificateFailed, s.denial.reason, msg)
	return reconcile.Result{Stop: true}, nil
}

// csrApprove approves a request that passed every check and is not yet
// approved.
type csrApprove struct{}

// Reconcile implements reconcile.SubReconciler.
func (csrApprove) Reconcile(_ context.Context, s *csrScope) (reconcile.Result, error) {
	if s.denial != nil || hasCondition(s.Object, certificatesv1.CertificateApproved) {
		return reconcile.Result{}, nil
	}
	s.approval = s.condition(certificatesv1.CertificateApproved,
		"ExecAgentOwnNode", "The Exec Agent requested a certificate for its own Node")
	return reconcile.Result{}, nil
}

// csrSign issues the certificate of an approved, reviewed request with its
// signer's CA. The scope's write stores it once the request is stored
// approved.
type csrSign struct{}

// Reconcile implements reconcile.SubReconciler.
func (csrSign) Reconcile(ctx context.Context, s *csrScope) (reconcile.Result, error) {
	if s.denial != nil {
		return reconcile.Result{}, nil
	}
	csr := s.Object
	//= docs/requirements/09-certificates.md#signing
	//# The Operator SHALL sign an approved
	//# `battery.liquidmetal-x.dev/flintlockd-serving` or
	//# `battery.liquidmetal-x.dev/exec-agent-serving` request with the serving
	//# CA, and an approved `battery.liquidmetal-x.dev/flintlockd-client` request
	//# with the `flintlockd` client CA, each read from its Secret in the
	//# Operator's namespace.
	secretName := s.Config.ServingCASecret
	if csr.Spec.SignerName == hostcert.ClientSigner {
		secretName = s.Config.ClientCASecret
	}
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: s.Config.Namespace, Name: secretName}
	if err := s.cas[secretName].Get(ctx, key, secret); err != nil {
		return reconcile.Result{}, fmt.Errorf("reading CA Secret %s: %w", key, err)
	}
	ca, err := loadCA(secret)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("loading CA Secret %s: %w", key, err)
	}

	certPEM, err := ca.sign(s.req, s.node, csr.Spec.Usages, s.Clock.Now(), certificateDuration(csr, s.Config.MaxDuration))
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("signing CertificateSigningRequest %s with CA Secret %s: %w", csr.Name, key, err)
	}
	s.certificate = certPEM
	return reconcile.Result{}, nil
}
