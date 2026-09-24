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
	"maps"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/phoban01/battery-operator/internal/hostcert"
	"github.com/phoban01/battery-operator/internal/reconcile"
)

// The keys of the CA bundle ConfigMap.
const (
	ServingCAKey = hostcert.ServingCAKey
	ClientCAKey  = hostcert.ClientCAKey
)

// caBundleReconciler publishes the certificates of the serving CA and the
// flintlockd client CA in the CA bundle ConfigMap.
type caBundleReconciler struct {
	client.Client
	Config SignerConfig

	// secrets reads the CA Secrets by name, configMap the CA bundle
	// ConfigMap, each through a cache of that one object.
	secrets   map[string]client.Reader
	configMap client.Reader
}

// +kubebuilder:rbac:groups="",namespace=system,resources=configmaps,verbs=get;list;watch;update,resourceNames=flintlockd-ca
// +kubebuilder:rbac:groups="",namespace=system,resources=configmaps,verbs=create

// Reconcile writes the CA certificates the CA Secrets hold into the CA
// bundle ConfigMap. A CA whose Secret does not exist yet is left out. It
// builds a caBundleScope, runs caBundleChain and writes the ConfigMap once;
// the logic, and its requirement citations, are in the subreconcilers.
func (r *caBundleReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	cm := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: r.Config.Namespace, Name: r.Config.CABundleConfigMap}
	exists := true
	if err := r.configMap.Get(ctx, key, cm); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("reading ConfigMap %s: %w", key, err)
		}
		exists = false
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: key.Namespace,
				Name:      key.Name,
				Labels:    map[string]string{"app.kubernetes.io/managed-by": "battery-operator"},
			},
		}
	}
	s := &caBundleScope{
		Scope:   reconcile.NewScope(cm, r.Client, logf.FromContext(ctx), nil),
		Config:  r.Config,
		secrets: r.secrets,
		exists:  exists,
	}
	res, err := caBundleChain().Run(ctx, s)
	if err != nil {
		return ctrl.Result{}, err
	}
	return res.Ctrl(), s.write(ctx)
}

// caBundleScope is one reconcile of the CA bundle ConfigMap. Object is the
// ConfigMap as it is to be: the one fetched, or a new one while there is
// none.
type caBundleScope struct {
	*reconcile.Scope[*corev1.ConfigMap]

	// Config is the signer's configuration.
	Config SignerConfig
	// secrets reads the CA Secrets by name.
	secrets map[string]client.Reader
	// exists is whether the ConfigMap was there when fetched.
	exists bool
}

// write creates the ConfigMap, or updates it when the chain changed its
// data.
func (s *caBundleScope) write(ctx context.Context) error {
	cm, key := s.Object, client.ObjectKeyFromObject(s.Object)
	if !s.exists {
		if err := s.Client.Create(ctx, cm); err != nil {
			return fmt.Errorf("creating ConfigMap %s: %w", key, err)
		}
		s.Log.Info("Created CA bundle ConfigMap", "configMap", key)
		return nil
	}
	if maps.Equal(s.Original.Data, cm.Data) && len(s.Original.BinaryData) == 0 && len(cm.BinaryData) == 0 {
		return nil
	}
	if err := s.Client.Update(ctx, cm); err != nil {
		return fmt.Errorf("updating ConfigMap %s: %w", key, err)
	}
	s.Log.Info("Updated CA bundle ConfigMap", "configMap", key)
	return nil
}

// caBundleChain is the CA bundle reconciler's chain.
func caBundleChain() reconcile.Chain[*caBundleScope] {
	return reconcile.Steps[*caBundleScope](caBundleCertificates{})
}

// caBundleCertificates sets the ConfigMap's data to the CA certificates the
// CA Secrets hold, and nothing else.
type caBundleCertificates struct{}

// Reconcile implements reconcile.SubReconciler.
func (caBundleCertificates) Reconcile(ctx context.Context, s *caBundleScope) (reconcile.Result, error) {
	//= docs/requirements/09-certificates.md#signing
	//# The Operator SHALL publish the certificates of the serving CA and the
	//# `flintlockd` client CA, without their keys, in a ConfigMap in its
	//# namespace.
	data := map[string]string{}
	for key, name := range map[string]string{ServingCAKey: s.Config.ServingCASecret, ClientCAKey: s.Config.ClientCASecret} {
		secret := &corev1.Secret{}
		if err := s.secrets[name].Get(ctx, client.ObjectKey{Namespace: s.Config.Namespace, Name: name}, secret); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return reconcile.Result{}, fmt.Errorf("reading CA Secret %s: %w", name, err)
		}
		certs, err := caCertificatesPEM(secret)
		if err != nil {
			// Waiting for the Secret to change: the next change triggers
			// another reconcile.
			s.Log.Error(err, "Failed to read CA certificates from Secret", "secret", name)
			continue
		}
		data[key] = certs
	}
	s.Object.Data = data
	s.Object.BinaryData = nil
	return reconcile.Result{}, nil
}

// setupWithManager watches the CA Secrets, through secretCaches, and the CA
// bundle ConfigMap, each
// through a cache of that one object, so that the Operator needs access to
// those names only.
func (r *caBundleReconciler) setupWithManager(mgr ctrl.Manager, secretCaches map[string]cache.Cache) error {
	cmCache, err := singleObjectCache(mgr, r.Config.Namespace, r.Config.CABundleConfigMap)
	if err != nil {
		return err
	}
	r.configMap = cmCache

	// Every change leads to the same reconcile: of the ConfigMap.
	bundle := []ctrl.Request{{NamespacedName: client.ObjectKey{
		Namespace: r.Config.Namespace, Name: r.Config.CABundleConfigMap,
	}}}
	b := ctrl.NewControllerManagedBy(mgr).
		Named("cabundle").
		WatchesRawSource(source.Kind(cmCache, &corev1.ConfigMap{},
			handler.TypedEnqueueRequestsFromMapFunc(func(context.Context, *corev1.ConfigMap) []ctrl.Request {
				return bundle
			})))
	for _, c := range secretCaches {
		b = b.WatchesRawSource(source.Kind(c, &corev1.Secret{},
			handler.TypedEnqueueRequestsFromMapFunc(func(context.Context, *corev1.Secret) []ctrl.Request {
				return bundle
			})))
	}
	return b.Complete(r)
}

// singleObjectCache returns a cache, started with the manager, of the objects
// named name in namespace. Its list and watch requests select that name, so
// RBAC can limit them to it with resourceNames.
func singleObjectCache(mgr ctrl.Manager, namespace, name string) (cache.Cache, error) {
	c, err := cache.New(mgr.GetConfig(), cache.Options{
		HTTPClient:           mgr.GetHTTPClient(),
		Scheme:               mgr.GetScheme(),
		Mapper:               mgr.GetRESTMapper(),
		DefaultNamespaces:    map[string]cache.Config{namespace: {}},
		DefaultFieldSelector: fields.OneTermEqualSelector("metadata.name", name),
	})
	if err != nil {
		return nil, fmt.Errorf("creating the cache of %s/%s: %w", namespace, name, err)
	}
	if err := mgr.Add(c); err != nil {
		return nil, err
	}
	return c, nil
}
