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
	"net"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/batterysidecar"
	"github.com/phoban01/battery-operator/internal/controller/inventory"
)

// batteryImage is battery's published image at the version go.mod pins,
// without its leading v: what the Makefile's POOLMGRD_IMG is, so that the
// sidecar and the client's protos cannot drift apart.
func batteryImage(t *testing.T) string {
	t.Helper()
	mod, err := os.ReadFile(filepath.Join(moduleRoot(t), "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(mod)) {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "github.com/liquidmetal-dev/battery" {
			return "ghcr.io/liquidmetal-dev/poolmgrd:" + strings.TrimPrefix(f[1], "v")
		}
	}
	t.Fatal("go.mod does not require github.com/liquidmetal-dev/battery")
	return ""
}

// isLoopback reports whether address, host:port, is on a loopback address.
func isLoopback(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// batteryConfig returns the configuration battery is given: the ConfigMap
// behind its -config file, parsed strictly.
func batteryConfig(t *testing.T, objs objects, d appsv1.Deployment, c corev1.Container) (corev1.ConfigMap, *batterysidecar.File) {
	t.Helper()
	file, ok := flag(c.Args, "config")
	if !ok {
		t.Fatal("battery is given no -config")
	}
	m := mountAt(t, c, path.Dir(file))
	v := volume(t, d, m.Name)
	if v.ConfigMap == nil {
		t.Fatalf("battery's configuration is in volume %s, which is not a ConfigMap", v.Name)
	}
	cm := one[corev1.ConfigMap](t, objs, "ConfigMap", v.ConfigMap.Name)
	data, ok := cm.Data[path.Base(file)]
	if !ok {
		t.Fatalf("ConfigMap %s has no %s", cm.Name, path.Base(file))
	}
	f, err := batterysidecar.Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	return cm, f
}

//= docs/requirements/06-deployment.md#battery-sidecar
//= type=test
//# The Manifests SHALL run battery as a container in the
//# Operator's pod, listening on a loopback address only.

// TestBatteryListensOnLoopback checks that battery runs in the Operator's
// pod, from its published image, with its API where the Operator calls it
// and its metrics both on loopback, and that it declares no port.
func TestBatteryListensOnLoopback(t *testing.T) {
	t.Parallel()
	objs := build(t, "config/default")
	d, manager, bat := operator(t, objs)
	if want := batteryImage(t); bat.Image != want {
		t.Errorf("battery runs %s, want %s", bat.Image, want)
	}
	_, f := batteryConfig(t, objs, d, bat)
	if f.APIServer == nil {
		t.Fatal("battery's gRPC API is not configured")
	}
	if !isLoopback(f.APIServer.Addr) || !isLoopback(f.MetricsAddr) {
		t.Errorf("battery listens on %q and %q; want loopback addresses only", f.APIServer.Addr, f.MetricsAddr)
	}
	addr, ok := flag(manager.Args, "battery-address")
	if !ok {
		addr = battery.DefaultAddress
	}
	if addr != f.APIServer.Addr {
		t.Errorf("the Operator calls battery on %s, but battery listens on %s", addr, f.APIServer.Addr)
	}
	if len(bat.Ports) != 0 || d.Spec.Template.Spec.HostNetwork {
		t.Errorf("battery is reachable from outside the pod: ports %v, host network %v",
			bat.Ports, d.Spec.Template.Spec.HostNetwork)
	}
}

//= docs/requirements/06-deployment.md#battery-sidecar
//= type=test
//# The Manifests SHALL run the Operator's pod as a single replica
//# that is replaced with the `Recreate` strategy.

// TestOperatorSingleReplicaRecreate checks the Operator's Deployment: one
// replica, replaced by stopping the old pod before starting the new one.
func TestOperatorSingleReplicaRecreate(t *testing.T) {
	t.Parallel()
	d, _, _ := operator(t, build(t, "config/default"))
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 {
		t.Errorf("the Operator runs %s replicas, want 1", ptrString(d.Spec.Replicas))
	}
	if d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("the Operator's pod is replaced with %q, want Recreate", d.Spec.Strategy.Type)
	}
}

//= docs/requirements/06-deployment.md#battery-sidecar
//= type=test
//# The Manifests SHALL keep battery's database on a persistent
//# volume.

// TestBatteryDatabaseOnPersistentVolume checks that battery's -db file is
// on a writable PersistentVolumeClaim in the Operator's namespace.
func TestBatteryDatabaseOnPersistentVolume(t *testing.T) {
	t.Parallel()
	objs := build(t, "config/default")
	d, _, bat := operator(t, objs)
	db, ok := flag(bat.Args, "db")
	if !ok || !path.IsAbs(db) {
		t.Fatalf("battery's database is %q, want an absolute -db path", db)
	}
	i := slices.IndexFunc(bat.VolumeMounts, func(m corev1.VolumeMount) bool {
		return strings.HasPrefix(db, strings.TrimSuffix(m.MountPath, "/")+"/")
	})
	if i < 0 {
		t.Fatalf("battery's database %s is on no volume", db)
	}
	m := bat.VolumeMounts[i]
	v := volume(t, d, m.Name)
	if v.PersistentVolumeClaim == nil || m.ReadOnly {
		t.Fatalf("battery's database is on volume %s, which is not a writable PersistentVolumeClaim", v.Name)
	}
	pvc := one[corev1.PersistentVolumeClaim](t, objs, "PersistentVolumeClaim", v.PersistentVolumeClaim.ClaimName)
	if pvc.Namespace != operatorNamespace {
		t.Errorf("the claim is in %s, want the Operator's namespace", pvc.Namespace)
	}
	// Only one pod mounts it at a time, and the Recreate strategy frees it.
	if !slices.Equal(pvc.Spec.AccessModes, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}) {
		t.Errorf("the claim's access modes are %v, want ReadWriteOnce", pvc.Spec.AccessModes)
	}
	if d.Spec.Template.Spec.SecurityContext.FSGroup == nil {
		t.Error("the pod sets no fsGroup, so battery's user may not be able to write a new volume")
	}
}

//= docs/requirements/06-deployment.md#battery-sidecar
//= type=test
//# The Manifests SHALL supply battery's configuration from a
//# ConfigMap that names no Hosts.

// TestBatteryConfigFromConfigMap checks that battery reads its
// configuration from a ConfigMap, the one batterysidecar.DefaultConfigMap
// names, at batterysidecar.DefaultConfigFile; that what ships is Render's
// output, which names no Hosts, since battery v0.5.0 refuses a file that
// does (BA-087); and that the Operator neither mounts it nor may write it,
// since battery's Hosts reach it through its HostAdmin API.
func TestBatteryConfigFromConfigMap(t *testing.T) {
	t.Parallel()
	objs := build(t, "config/default")
	d, manager, bat := operator(t, objs)
	cm, _ := batteryConfig(t, objs, d, bat)

	want, err := batterysidecar.Render()
	if err != nil {
		t.Fatal(err)
	}
	if got := cm.Data[batterysidecar.ConfigKey]; got != string(want) {
		t.Errorf("the shipped configuration is not Render():\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(cm.Data[batterysidecar.ConfigKey], `"hosts"`) {
		t.Error("the shipped configuration names Hosts")
	}
	if cm.Name != batterysidecar.DefaultConfigMap {
		t.Errorf("battery reads ConfigMap %s, want %s", cm.Name, batterysidecar.DefaultConfigMap)
	}
	if file, _ := flag(bat.Args, "config"); file != batterysidecar.DefaultConfigFile {
		t.Errorf("battery reads %q, want %s", file, batterysidecar.DefaultConfigFile)
	}
	for _, m := range manager.VolumeMounts {
		if v := volume(t, d, m.Name); v.ConfigMap != nil && v.ConfigMap.Name == cm.Name {
			t.Errorf("the Operator mounts battery's configuration at %s", m.MountPath)
		}
	}
}

//= docs/requirements/06-deployment.md#battery-sidecar
//= type=test
//# The Manifests SHALL give battery, from a Secret, a client
//# certificate and key for `flintlockd` and the certificate authority that
//# verifies the Hosts' `flintlockd` serving certificates.

// TestBatteryTLSFromSecrets checks that the files the Inventory Controller
// names for every Host (inventory.TLS) are mounted from Secrets: battery's
// client certificate and key from the Secret cert-manager issues its
// certificate into, and the serving CA's certificate, without its key,
// from the serving CA's Secret.
func TestBatteryTLSFromSecrets(t *testing.T) {
	t.Parallel()
	objs := build(t, "config/default")
	d, _, bat := operator(t, objs)
	clientSecret := one[certificate](t, objs, "Certificate", "battery-operator-battery-flintlockd-client").Spec.SecretName
	servingCASecret := one[certificate](t, objs, "Certificate", "battery-operator-flintlockd-serving-ca").Spec.SecretName

	m := mountAt(t, bat, batterysidecar.TLSDir)
	v := volume(t, d, m.Name)
	if v.Projected == nil {
		t.Fatalf("battery's TLS files are in volume %s, which is not projected from Secrets", v.Name)
	}
	// Each file in the volume, as "secret/key".
	files := map[string]string{}
	for _, src := range v.Projected.Sources {
		if src.Secret == nil {
			t.Fatalf("battery's TLS volume has a source that is not a Secret: %+v", src)
		}
		for _, item := range src.Secret.Items {
			files[path.Join(m.MountPath, item.Path)] = src.Secret.Name + "/" + item.Key
		}
	}
	want := map[string]string{
		batterysidecar.ClientCertFile: clientSecret + "/tls.crt",
		batterysidecar.ClientKeyFile:  clientSecret + "/tls.key",
		batterysidecar.ServingCAFile:  servingCASecret + "/tls.crt",
	}
	for file, src := range want {
		if files[file] != src {
			t.Errorf("battery's %s comes from %q, want %s", file, files[file], src)
		}
	}
	if len(files) != len(want) {
		t.Errorf("battery's TLS volume holds %v, want exactly %v", files, want)
	}
	tls := inventory.TLS
	if tls.Insecure || tls.CertFile != batterysidecar.ClientCertFile || tls.KeyFile != batterysidecar.ClientKeyFile ||
		tls.CAFile != batterysidecar.ServingCAFile {
		t.Errorf("Hosts are given %+v, not the mounted client certificate and serving CA", tls)
	}
}

// TestNoSharedProcessNamespace checks that the Operator's pod does not
// share its process namespace: nothing restarts battery since it takes its
// Hosts through its HostAdmin API (ADR 0008), and a shared namespace would
// let each container read the other's environment and files.
func TestNoSharedProcessNamespace(t *testing.T) {
	t.Parallel()
	d, manager, bat := operator(t, build(t, "config/default"))
	spec := d.Spec.Template.Spec
	if spec.ShareProcessNamespace != nil && *spec.ShareProcessNamespace {
		t.Error("the Operator's pod shares its process namespace")
	}
	for _, c := range []corev1.Container{manager, bat} {
		if c.SecurityContext == nil || c.SecurityContext.Capabilities == nil ||
			!slices.Contains(c.SecurityContext.Capabilities.Drop, "ALL") || len(c.SecurityContext.Capabilities.Add) != 0 {
			t.Errorf("the %s container keeps capabilities", c.Name)
		}
	}
}

//= docs/requirements/06-deployment.md#battery-sidecar
//= type=test
//# once the Operator's own mount of that Secret holds the
//# certificate the Secret holds.

// TestOperatorWatchesBatteryClientCertificate checks what reloading a
// renewed client certificate needs of the Manifests: the Operator watches
// the Secret cert-manager issues battery's certificate into, the one its
// --battery-client-secret names, and may read no other Secret by that
// grant; and its container mounts the volume battery reads the
// certificate from, at the path its --battery-client-cert-file names.
func TestOperatorWatchesBatteryClientCertificate(t *testing.T) {
	t.Parallel()
	objs := build(t, "config/default")
	d, manager, bat := operator(t, objs)
	clientSecret := one[certificate](t, objs, "Certificate", "battery-operator-battery-flintlockd-client").Spec.SecretName

	if name, _ := flag(manager.Args, "battery-client-secret"); name != clientSecret || name != batterysidecar.DefaultClientSecret {
		t.Errorf("the Operator watches Secret %q; cert-manager issues battery's certificate into %s; the default is %s",
			name, clientSecret, batterysidecar.DefaultClientSecret)
	}
	file, _ := flag(manager.Args, "battery-client-cert-file")
	if file != batterysidecar.ClientCertFile {
		t.Errorf("the Operator reads battery's certificate at %q, want %s", file, batterysidecar.ClientCertFile)
	}
	if mountAt(t, manager, path.Dir(file)).Name != mountAt(t, bat, batterysidecar.TLSDir).Name {
		t.Error("the Operator and battery mount battery's client certificate from different volumes")
	}

	operatorSA := serviceAccount(operatorNamespace, d.Spec.Template.Spec.ServiceAccountName)
	gs := grants(t, objs)
	for _, verb := range []string{verbGet, "list", "watch"} {
		if !slices.ContainsFunc(gs, func(g grant) bool {
			return g.subject == operatorSA && g.allows(operatorNamespace, "", "secrets", verb, clientSecret)
		}) {
			t.Errorf("the Operator may not %s Secret %s", verb, clientSecret)
		}
	}
	for _, g := range gs {
		if g.subject == operatorSA && g.allows(operatorNamespace, "", "secrets", verbGet, "another-secret") {
			t.Errorf("the Operator may read any Secret in its namespace: %+v", g.rule)
		}
	}
}

// ptrString renders an optional count.
func ptrString(n *int32) string {
	if n == nil {
		return "the default number of"
	}
	return strconv.Itoa(int(*n))
}
