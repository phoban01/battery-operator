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
	"slices"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/phoban01/battery-operator/internal/hostcert"
)

// CertificateSigningRequestReconciler approves, denies and signs the
// CertificateSigningRequests for the two signer names of ADR 0003.
type CertificateSigningRequestReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIReader reads Nodes without a cache, so the Operator needs only get
	// on them.
	APIReader client.Reader
	Config    SignerConfig

	// cas reads the CA Secrets by name, each through a cache of that one
	// Secret.
	cas map[string]cache.Cache
}

// +kubebuilder:rbac:groups=certificates.k8s.io,resources=certificatesigningrequests,verbs=get;list;watch
// +kubebuilder:rbac:groups=certificates.k8s.io,resources=certificatesigningrequests/approval,verbs=update
// +kubebuilder:rbac:groups=certificates.k8s.io,resources=certificatesigningrequests/status,verbs=update
// +kubebuilder:rbac:groups=certificates.k8s.io,resources=signers,verbs=approve;sign,resourceNames=battery.liquidmetal-x.dev/flintlockd-serving;battery.liquidmetal-x.dev/flintlockd-client;battery.liquidmetal-x.dev/exec-agent-serving
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get
// +kubebuilder:rbac:groups="",namespace=system,resources=secrets,verbs=get;list;watch,resourceNames=flintlockd-serving-ca;flintlockd-client-ca

// Reconcile reviews a request for one of the Operator's signer names,
// approves or denies it, and signs it once approved and reviewed.
//
// A request that someone else approved is reviewed too. If it fails a check,
// it is marked Failed, with the check's reason, and never signed: the
// Approved condition cannot be withdrawn, and Denied cannot be added beside
// it.
func (r *CertificateSigningRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	csr := &certificatesv1.CertificateSigningRequest{}
	if err := r.Get(ctx, req.NamespacedName, csr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !isOurSigner(csr.Spec.SignerName) {
		return ctrl.Result{}, nil
	}
	if len(csr.Status.Certificate) > 0 || hasCondition(csr, certificatesv1.CertificateFailed) {
		return ctrl.Result{}, nil
	}

	//= docs/requirements/09-certificates.md#signing
	//# The Operator SHALL sign a request only while it carries the
	//# condition `Approved` and not the condition `Denied`.
	if hasCondition(csr, certificatesv1.CertificateDenied) {
		return ctrl.Result{}, nil
	}

	//= docs/requirements/09-certificates.md#signing
	//# The Operator SHALL sign a request only when it passes every
	//# check of [Approval](#approval) for its signer name, whoever approved it.
	ok, d, err := r.review(ctx, csr)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !hasCondition(csr, certificatesv1.CertificateApproved) {
		if d != nil {
			if err := r.setCondition(ctx, csr, certificatesv1.CertificateDenied, d.reason, d.message); err != nil {
				return ctrl.Result{}, err
			}
			log.Info("Denied CertificateSigningRequest", "reason", d.reason, "message", d.message)
			return ctrl.Result{}, nil
		}
		if err := r.setCondition(ctx, csr, certificatesv1.CertificateApproved,
			"ExecAgentOwnNode", "The Exec Agent requested a certificate for its own Node"); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("Approved CertificateSigningRequest")
		// The update returns the request as stored, so this checks what
		// the API server now holds.
		if !hasCondition(csr, certificatesv1.CertificateApproved) || hasCondition(csr, certificatesv1.CertificateDenied) {
			return ctrl.Result{}, nil
		}
	} else if d != nil {
		msg := "Approved, but failed a check, so it is not signed: " + d.message
		if err := r.setCondition(ctx, csr, certificatesv1.CertificateFailed, d.reason, msg); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("Refused to sign an approved CertificateSigningRequest", "reason", d.reason, "message", d.message)
		return ctrl.Result{}, nil
	}

	if err := r.sign(ctx, csr, ok); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("Signed CertificateSigningRequest", "signer", csr.Spec.SignerName, "node", ok.node)
	return ctrl.Result{}, nil
}

// setCondition adds a condition to csr: Approved or Denied through the
// approval subresource, Failed through status.
func (r *CertificateSigningRequestReconciler) setCondition(ctx context.Context, csr *certificatesv1.CertificateSigningRequest,
	t certificatesv1.RequestConditionType, reason, message string) error {
	csr.Status.Conditions = append(csr.Status.Conditions, certificatesv1.CertificateSigningRequestCondition{
		Type:           t,
		Status:         corev1.ConditionTrue,
		Reason:         reason,
		Message:        message,
		LastUpdateTime: metav1.Now(),
	})
	var err error
	if t == certificatesv1.CertificateFailed {
		err = r.Status().Update(ctx, csr)
	} else {
		err = r.SubResource("approval").Update(ctx, csr)
	}
	if err != nil {
		return fmt.Errorf("setting condition %s on CertificateSigningRequest %s: %w", t, csr.Name, err)
	}
	return nil
}

// sign issues the certificate of an approved, reviewed request with its
// signer's CA.
func (r *CertificateSigningRequestReconciler) sign(ctx context.Context, csr *certificatesv1.CertificateSigningRequest, ok *reviewed) error {
	//= docs/requirements/09-certificates.md#signing
	//# The Operator SHALL sign an approved
	//# `battery.liquidmetal-x.dev/flintlockd-serving` or
	//# `battery.liquidmetal-x.dev/exec-agent-serving` request with the serving
	//# CA, and an approved `battery.liquidmetal-x.dev/flintlockd-client` request
	//# with the `flintlockd` client CA, each read from its Secret in the
	//# Operator's namespace.
	secretName := r.Config.ServingCASecret
	if csr.Spec.SignerName == hostcert.ClientSigner {
		secretName = r.Config.ClientCASecret
	}
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: r.Config.Namespace, Name: secretName}
	if err := r.cas[secretName].Get(ctx, key, secret); err != nil {
		return fmt.Errorf("reading CA Secret %s: %w", key, err)
	}
	ca, err := loadCA(secret)
	if err != nil {
		return fmt.Errorf("loading CA Secret %s: %w", key, err)
	}

	certPEM, err := ca.sign(ok.req, ok.node, csr.Spec.Usages, time.Now(), certificateDuration(csr, r.Config.MaxDuration))
	if err != nil {
		return fmt.Errorf("signing CertificateSigningRequest %s with CA Secret %s: %w", csr.Name, key, err)
	}
	csr.Status.Certificate = certPEM
	if err := r.Status().Update(ctx, csr); err != nil {
		return fmt.Errorf("writing the certificate of CertificateSigningRequest %s: %w", csr.Name, err)
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager, together with
// the controller that publishes the CA certificates.
func (r *CertificateSigningRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := r.Config.Validate(); err != nil {
		return err
	}
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	r.cas = map[string]cache.Cache{}
	for _, name := range []string{r.Config.ServingCASecret, r.Config.ClientCASecret} {
		c, err := singleObjectCache(mgr, r.Config.Namespace, name)
		if err != nil {
			return err
		}
		r.cas[name] = c
	}

	//= docs/requirements/09-certificates.md#signing
	//# The Operator SHALL approve and sign `CertificateSigningRequest`s
	//# for the signer names `battery.liquidmetal-x.dev/flintlockd-serving`,
	//# `battery.liquidmetal-x.dev/flintlockd-client` and
	//# `battery.liquidmetal-x.dev/exec-agent-serving`, and for no other signer
	//# name.
	ours := predicate.NewPredicateFuncs(func(o client.Object) bool {
		csr, ok := o.(*certificatesv1.CertificateSigningRequest)
		return ok && isOurSigner(csr.Spec.SignerName)
	})
	if err := ctrl.NewControllerManagedBy(mgr).
		For(&certificatesv1.CertificateSigningRequest{}, builder.WithPredicates(ours)).
		Named("certificatesigningrequest").
		Complete(r); err != nil {
		return err
	}

	return (&caBundleReconciler{Client: r.Client, Config: r.Config, secrets: r.cas}).setupWithManager(mgr)
}

// isOurSigner reports whether name is one of the three signer names the
// Operator approves and signs for.
func isOurSigner(name string) bool {
	return name == hostcert.ServingSigner || name == hostcert.ClientSigner || name == hostcert.ExecAgentServingSigner
}

// hasCondition reports whether csr carries a condition of type t that is not
// False.
func hasCondition(csr *certificatesv1.CertificateSigningRequest, t certificatesv1.RequestConditionType) bool {
	return slices.ContainsFunc(csr.Status.Conditions, func(c certificatesv1.CertificateSigningRequestCondition) bool {
		return c.Type == t && c.Status != corev1.ConditionFalse
	})
}
