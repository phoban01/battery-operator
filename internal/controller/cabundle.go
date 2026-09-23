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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// The keys of the CA bundle ConfigMap.
const (
	ServingCAKey = "serving-ca.crt"
	ClientCAKey  = "client-ca.crt"
)

// caBundleReconciler publishes the certificates of the serving CA and the
// flintlockd client CA in the CA bundle ConfigMap.
type caBundleReconciler struct {
	client.Client
	Config SignerConfig

	// secrets reads the CA Secrets by name, configMap the CA bundle
	// ConfigMap, each through a cache of that one object.
	secrets   map[string]cache.Cache
	configMap client.Reader
}

// +kubebuilder:rbac:groups="",namespace=system,resources=configmaps,verbs=get;list;watch;update,resourceNames=flintlockd-ca
// +kubebuilder:rbac:groups="",namespace=system,resources=configmaps,verbs=create

// Reconcile writes the CA certificates the CA Secrets hold into the CA
// bundle ConfigMap. A CA whose Secret does not exist yet is left out.
func (r *caBundleReconciler) Reconcile(ctx context.Context, _ reconcile.Request) (ctrl.Result, error) {
	//= docs/requirements/09-certificates.md#signing
	//# The Operator SHALL publish the certificates of the serving CA and the
	//# `flintlockd` client CA, without their keys, in a ConfigMap in its
	//# namespace.
	log := logf.FromContext(ctx)

	data := map[string]string{}
	for key, name := range map[string]string{ServingCAKey: r.Config.ServingCASecret, ClientCAKey: r.Config.ClientCASecret} {
		secret := &corev1.Secret{}
		if err := r.secrets[name].Get(ctx, client.ObjectKey{Namespace: r.Config.Namespace, Name: name}, secret); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return ctrl.Result{}, fmt.Errorf("reading CA Secret %s: %w", name, err)
		}
		certs, err := caCertificatesPEM(secret)
		if err != nil {
			// Waiting for the Secret to change: the next change triggers
			// another reconcile.
			log.Error(err, "Failed to read CA certificates from Secret", "secret", name)
			continue
		}
		data[key] = certs
	}

	cm := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: r.Config.Namespace, Name: r.Config.CABundleConfigMap}
	if err := r.configMap.Get(ctx, key, cm); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("reading ConfigMap %s: %w", key, err)
		}
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: key.Namespace,
				Name:      key.Name,
				Labels:    map[string]string{"app.kubernetes.io/managed-by": "battery-operator"},
			},
			Data: data,
		}
		if err := r.Create(ctx, cm); err != nil {
			return ctrl.Result{}, fmt.Errorf("creating ConfigMap %s: %w", key, err)
		}
		log.Info("Created CA bundle ConfigMap", "configMap", key)
		return ctrl.Result{}, nil
	}
	if maps.Equal(cm.Data, data) && len(cm.BinaryData) == 0 {
		return ctrl.Result{}, nil
	}
	cm.Data = data
	cm.BinaryData = nil
	if err := r.Update(ctx, cm); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating ConfigMap %s: %w", key, err)
	}
	log.Info("Updated CA bundle ConfigMap", "configMap", key)
	return ctrl.Result{}, nil
}

// setupWithManager watches the CA Secrets and the CA bundle ConfigMap, each
// through a cache of that one object, so that the Operator needs access to
// those names only.
func (r *caBundleReconciler) setupWithManager(mgr ctrl.Manager) error {
	cmCache, err := singleObjectCache(mgr, r.Config.Namespace, r.Config.CABundleConfigMap)
	if err != nil {
		return err
	}
	r.configMap = cmCache

	// Every change leads to the same reconcile: of the ConfigMap.
	bundle := []reconcile.Request{{NamespacedName: client.ObjectKey{
		Namespace: r.Config.Namespace, Name: r.Config.CABundleConfigMap,
	}}}
	b := ctrl.NewControllerManagedBy(mgr).
		Named("cabundle").
		WatchesRawSource(source.Kind(cmCache, &corev1.ConfigMap{},
			handler.TypedEnqueueRequestsFromMapFunc(func(context.Context, *corev1.ConfigMap) []reconcile.Request {
				return bundle
			})))
	for _, c := range r.secrets {
		b = b.WatchesRawSource(source.Kind(c, &corev1.Secret{},
			handler.TypedEnqueueRequestsFromMapFunc(func(context.Context, *corev1.Secret) []reconcile.Request {
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
