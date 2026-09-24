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
//# The Manifests SHALL supply battery's configuration, its Hosts
//# included, from an object the Operator may write.

// TestBatteryConfigFromConfigMap checks that battery reads its
// configuration from a ConfigMap the Operator may update, the one its
// --battery-config-map names, mounted in both containers from the same
// volume at the path the Operator's --battery-config-file names; and that
// what ships is what the Inventory Controller renders for no Hosts.
func TestBatteryConfigFromConfigMap(t *testing.T) {
	t.Parallel()
	objs := build(t, "config/default")
	d, manager, bat := operator(t, objs)
	cm, _ := batteryConfig(t, objs, d, bat)

	want, err := batterysidecar.Render(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := cm.Data[batterysidecar.ConfigKey]; got != string(want) {
		t.Errorf("the shipped configuration is not Render(nil):\n%s\nwant:\n%s", got, want)
	}

	if name, _ := flag(manager.Args, "battery-config-map"); name != cm.Name || name != batterysidecar.DefaultConfigMap {
		t.Errorf("the Operator writes ConfigMap %q; battery reads %s; the default is %s", name, cm.Name, batterysidecar.DefaultConfigMap)
	}
	file, _ := flag(manager.Args, "battery-config-file")
	batFile, _ := flag(bat.Args, "config")
	if file != batFile || file != batterysidecar.DefaultConfigFile {
		t.Errorf("the Operator reads %q, battery %q; want both %s", file, batFile, batterysidecar.DefaultConfigFile)
	}
	if mountAt(t, manager, path.Dir(file)).Name != mountAt(t, bat, path.Dir(batFile)).Name {
		t.Error("the Operator and battery mount battery's configuration from different volumes")
	}

	operatorSA := serviceAccount(operatorNamespace, d.Spec.Template.Spec.ServiceAccountName)
	for _, verb := range []string{"get", verbUpdate} {
		if !slices.ContainsFunc(grants(t, objs), func(g grant) bool {
			return g.subject == operatorSA && g.allows(operatorNamespace, "", "configmaps", verb, cm.Name)
		}) {
			t.Errorf("the Operator may not %s ConfigMap %s", verb, cm.Name)
		}
	}
}

//= docs/requirements/06-deployment.md#battery-sidecar
//= type=test
//# The Manifests SHALL give battery, from a Secret, a client
//# certificate and key for `flintlockd` and the certificate authority that
//# verifies the Hosts' `flintlockd` serving certificates.

// TestBatteryTLSFromSecrets checks that the files battery's configuration
// names for every Host are mounted from Secrets: its client certificate and
// key from the Secret cert-manager issues its certificate into, and the
// serving CA's certificate, without its key, from the serving CA's Secret.
func TestBatteryTLSFromSecrets(t *testing.T) {
	t.Parallel()
	objs := build(t, "config/default")
	d, _, bat := operator(t, objs)
	_, f := batteryConfig(t, objs, d, bat)
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
	if len(f.Hosts) == 0 {
		t.Fatal("battery's configuration has no Hosts")
	}
	for _, h := range f.Hosts {
		tls := h.TLS
		if tls.Insecure || tls.CertFile != batterysidecar.ClientCertFile || tls.KeyFile != batterysidecar.ClientKeyFile ||
			tls.CAFile != batterysidecar.ServingCAFile {
			t.Errorf("host %s is reached with %+v, not the mounted client certificate and serving CA", h.Name, tls)
		}
	}
}

//= docs/requirements/06-deployment.md#battery-sidecar
//= type=test
//# The Operator SHALL restart battery through a single mechanism
//# that the Inventory Controller invokes, and SHALL wait until battery answers
//# again before any controller calls it.

// TestRestartMechanism checks what the Restarter needs of the pod: a shared
// process namespace, the two containers as the same user, and battery run
// as the process the Restarter looks for, restarted by the kubelet when it
// exits.
func TestRestartMechanism(t *testing.T) {
	t.Parallel()
	d, manager, bat := operator(t, build(t, "config/default"))
	spec := d.Spec.Template.Spec
	if spec.ShareProcessNamespace == nil || !*spec.ShareProcessNamespace {
		t.Error("the Operator's pod does not share its process namespace")
	}
	if spec.SecurityContext == nil || spec.SecurityContext.RunAsUser == nil {
		t.Fatal("the pod does not name the user its containers run as")
	}
	for _, c := range []corev1.Container{manager, bat} {
		if c.SecurityContext != nil && (c.SecurityContext.RunAsUser != nil || c.SecurityContext.RunAsGroup != nil) {
			t.Errorf("the %s container runs as its own user", c.Name)
		}
		if c.SecurityContext == nil || c.SecurityContext.Capabilities == nil ||
			!slices.Contains(c.SecurityContext.Capabilities.Drop, "ALL") || len(c.SecurityContext.Capabilities.Add) != 0 {
			t.Errorf("the %s container keeps capabilities; the same user needs none to signal", c.Name)
		}
	}
	if path.Base(strings.TrimSpace(firstOr(bat.Command, "/"+batterysidecar.ProcessName))) != batterysidecar.ProcessName {
		t.Errorf("battery runs %v, not %s", bat.Command, batterysidecar.ProcessName)
	}
	if spec.RestartPolicy != "" && spec.RestartPolicy != corev1.RestartPolicyAlways {
		t.Errorf("the pod's restart policy is %s; the kubelet has to start battery again", spec.RestartPolicy)
	}
}

// firstOr returns list's first element, or def when it is empty: an image's
// entrypoint when a container overrides no command.
func firstOr(list []string, def string) string {
	if len(list) == 0 {
		return def
	}
	return list[0]
}

// ptrString renders an optional count.
func ptrString(n *int32) string {
	if n == nil {
		return "the default number of"
	}
	return strconv.Itoa(int(*n))
}
