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

	corev1 "k8s.io/api/core/v1"

	"github.com/phoban01/battery-operator/internal/controller"
)

// writeVerbs are the verbs that change an object.
const verbUpdate = "update"

// verbGet reads an object.
const verbGet = "get"

var writeVerbs = []string{"create", verbUpdate, "patch", "delete", "deletecollection"}

// operatorIdentity is the Operator's ServiceAccount, as a grant's subject.
func operatorIdentity(t *testing.T, objs objects) string {
	t.Helper()
	d, _, _ := operator(t, objs)
	return serviceAccount(d.Namespace, d.Spec.Template.Spec.ServiceAccountName)
}

//= docs/requirements/06-deployment.md#access
//= type=test
//# The Manifests SHALL grant write access to the status
//# subresources of `Pool` and `MicroVMClaim` to the Operator's identity and
//# to no other identity they create.

// TestStatusWriters checks, over every binding config/default and
// config/exec-agent render, that the Operator may update and patch the
// status of Pools and MicroVMClaims, and nobody else may write it.
func TestStatusWriters(t *testing.T) {
	t.Parallel()
	objs := build(t, "config/default", "config/exec-agent")
	op := operatorIdentity(t, objs)
	gs := grants(t, objs)
	for _, resource := range []string{"pools/status", "microvmclaims/status"} {
		for _, verb := range []string{verbUpdate, "patch"} {
			if !slices.ContainsFunc(gs, func(g grant) bool {
				return g.subject == op && g.namespace == "" && g.allows("any", "battery.liquidmetal-x.dev", resource, verb, "")
			}) {
				t.Errorf("the Operator may not %s %s in every namespace", verb, resource)
			}
		}
		for _, g := range gs {
			if g.subject == op {
				continue
			}
			for _, verb := range writeVerbs {
				if g.allows(g.namespace, "battery.liquidmetal-x.dev", resource, verb, "") {
					t.Errorf("%s may %s %s", g.subject, verb, resource)
				}
			}
		}
	}
}

//= docs/requirements/06-deployment.md#access
//= type=test
//# The Manifests SHALL grant the Operator's identity no access to
//# Secrets outside its own namespace.

// TestOperatorSecretsOnlyInItsNamespace checks, over every binding
// config/default and config/exec-agent render, that nothing grants the
// Operator access to Secrets cluster-wide or in another namespace.
func TestOperatorSecretsOnlyInItsNamespace(t *testing.T) {
	t.Parallel()
	objs := build(t, "config/default", "config/exec-agent")
	op := operatorIdentity(t, objs)
	for _, g := range grants(t, objs) {
		if g.subject != op || g.namespace == operatorNamespace {
			continue
		}
		if matches(g.rule.APIGroups, "") && matches(g.rule.Resources, "secrets") {
			where := "in every namespace"
			if g.namespace != "" {
				where = "in " + g.namespace
			}
			t.Errorf("the Operator may %v Secrets %s", g.rule.Verbs, where)
		}
	}
}

//= docs/requirements/09-certificates.md#spiffe-identity
//= type=test
//# The Manifests SHALL grant read access to the Secrets of the
//# serving CA and the `flintlockd` client CA to the Operator's identity only.

// TestCASecretReaders checks that the Operator may read the CA Secrets and
// that no other identity the Manifests bind may, and that in the
// Operator's pod only the manager holds the Operator's token: battery gets
// the serving CA's certificate from the kubelet, never its key.
func TestCASecretReaders(t *testing.T) {
	t.Parallel()
	objs := build(t, "config/default", "config/exec-agent")
	op := operatorIdentity(t, objs)
	gs := grants(t, objs)
	for _, secret := range []string{controller.DefaultServingCASecret, controller.DefaultClientCASecret} {
		if !slices.ContainsFunc(gs, func(g grant) bool {
			return g.subject == op && g.allows(operatorNamespace, "", "secrets", verbGet, secret)
		}) {
			t.Errorf("the Operator may not read Secret %s", secret)
		}
		for _, g := range gs {
			if g.subject == op {
				continue
			}
			for _, verb := range []string{verbGet, "list", "watch"} {
				if g.allows(operatorNamespace, "", "secrets", verb, secret) {
					t.Errorf("%s may %s Secret %s", g.subject, verb, secret)
				}
			}
		}
	}

	d, manager, bat := operator(t, objs)
	spec := d.Spec.Template.Spec
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Error("the kubelet mounts the Operator's token into every container of its pod")
	}
	tokenVolumes := map[string]bool{}
	for _, v := range spec.Volumes {
		if v.Projected != nil && slices.ContainsFunc(v.Projected.Sources, func(s corev1.VolumeProjection) bool {
			return s.ServiceAccountToken != nil
		}) {
			tokenVolumes[v.Name] = true
		}
		if v.Projected != nil {
			for _, s := range v.Projected.Sources {
				if s.Secret == nil || (s.Secret.Name != controller.DefaultServingCASecret && s.Secret.Name != controller.DefaultClientCASecret) {
					continue
				}
				if s.Secret.Name == controller.DefaultClientCASecret || len(s.Secret.Items) == 0 ||
					slices.ContainsFunc(s.Secret.Items, func(k corev1.KeyToPath) bool { return k.Key != "tls.crt" }) {
					t.Errorf("volume %s mounts more of CA Secret %s than the serving CA's certificate", v.Name, s.Secret.Name)
				}
			}
		}
		if v.Secret != nil && (v.Secret.SecretName == controller.DefaultServingCASecret || v.Secret.SecretName == controller.DefaultClientCASecret) {
			t.Errorf("volume %s mounts all of CA Secret %s", v.Name, v.Secret.SecretName)
		}
	}
	if !slices.ContainsFunc(manager.VolumeMounts, func(m corev1.VolumeMount) bool { return tokenVolumes[m.Name] }) {
		t.Error("the manager has no ServiceAccount token")
	}
	if slices.ContainsFunc(bat.VolumeMounts, func(m corev1.VolumeMount) bool { return tokenVolumes[m.Name] }) {
		t.Error("battery holds the Operator's ServiceAccount token")
	}
}

//= docs/requirements/09-certificates.md#approval
//= type=test
//# The Manifests SHALL grant write access to the ConfigMap
//# `host-address-pins` to the Operator's identity and to no other identity
//# they create.

// TestAddressPinWriters checks, over every binding config/default and
// config/exec-agent render, that the Operator may create and update the
// address pins ConfigMap and nobody else may write it. The kubelet's access,
// which the Node authorizer grants and no binding, is checked in the e2e
// suite.
func TestAddressPinWriters(t *testing.T) {
	t.Parallel()
	objs := build(t, "config/default", "config/exec-agent")
	op := operatorIdentity(t, objs)
	gs := grants(t, objs)
	for _, verb := range []string{"create", verbUpdate, verbGet} {
		if !slices.ContainsFunc(gs, func(g grant) bool {
			return g.subject == op && g.allows(operatorNamespace, "", "configmaps", verb, controller.AddressPinsConfigMap)
		}) {
			t.Errorf("the Operator may not %s ConfigMap %s", verb, controller.AddressPinsConfigMap)
		}
	}
	for _, g := range gs {
		if g.subject == op {
			continue
		}
		for _, verb := range writeVerbs {
			if g.allows(operatorNamespace, "", "configmaps", verb, controller.AddressPinsConfigMap) {
				t.Errorf("%s may %s ConfigMap %s", g.subject, verb, controller.AddressPinsConfigMap)
			}
		}
	}
}
