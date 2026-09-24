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
	"errors"
	"slices"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/phoban01/battery-operator/internal/hostcert"
	"github.com/phoban01/battery-operator/internal/reconcile"
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
	cas map[string]client.Reader
}

// +kubebuilder:rbac:groups=certificates.k8s.io,resources=certificatesigningrequests,verbs=get;list;watch
// +kubebuilder:rbac:groups=certificates.k8s.io,resources=certificatesigningrequests/approval,verbs=update
// +kubebuilder:rbac:groups=certificates.k8s.io,resources=certificatesigningrequests/status,verbs=update
// +kubebuilder:rbac:groups=certificates.k8s.io,resources=signers,verbs=approve;sign,resourceNames=battery.liquidmetal-x.dev/flintlockd-serving;battery.liquidmetal-x.dev/flintlockd-client;battery.liquidmetal-x.dev/exec-agent-serving
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get
// +kubebuilder:rbac:groups="",namespace=system,resources=secrets,verbs=get;list;watch,resourceNames=flintlockd-serving-ca;flintlockd-client-ca

// Reconcile reviews a request for one of the Operator's signer names,
// approves or denies it, and signs it once approved and reviewed. It builds
// a csrScope, runs csrChain and writes the request once; the logic, and its
// requirement citations, are in the subreconcilers.
//
// A request that someone else approved is reviewed too. If it fails a check,
// it is marked Failed, with the check's reason, and never signed: the
// Approved condition cannot be withdrawn, and Denied cannot be added beside
// it.
func (r *CertificateSigningRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	csr := &certificatesv1.CertificateSigningRequest{}
	if err := r.Get(ctx, req.NamespacedName, csr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	s := &csrScope{
		Scope:     reconcile.NewScope(csr, r.Client, logf.FromContext(ctx), nil),
		APIReader: r.APIReader,
		Config:    r.Config,
		cas:       r.cas,
	}
	res, err := csrChain().Run(ctx, s)
	// The write runs even when the chain failed, so that a request the
	// chain approved before signing failed is stored approved.
	if werr := s.write(ctx); werr != nil {
		err = errors.Join(err, werr)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	return res.Ctrl(), nil
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
	r.cas = map[string]client.Reader{}
	caches := map[string]cache.Cache{}
	for _, name := range []string{r.Config.ServingCASecret, r.Config.ClientCASecret} {
		c, err := singleObjectCache(mgr, r.Config.Namespace, name)
		if err != nil {
			return err
		}
		r.cas[name] = c
		caches[name] = c
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

	return (&caBundleReconciler{Client: r.Client, Config: r.Config, secrets: r.cas}).setupWithManager(mgr, caches)
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
