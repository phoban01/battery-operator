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

package execagent_test

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/phoban01/battery-operator/internal/execagent"
	"github.com/phoban01/battery-operator/internal/execagent/execagenttest"
)

//= docs/requirements/05-exec-agent.md#serving
//= type=test
//# The Manifests SHALL run the Exec Agent on every Node labelled
//# `battery.liquidmetal-x.dev/host=true`, and on no other Node.

// TestDaemonSet reads config/exec-agent/daemonset.yaml strictly and checks
// what the rest of the agent relies on. It is a DaemonSet selecting exactly
// the Nodes labelled battery.liquidmetal-x.dev/host=true, tolerating every
// taint so that no labelled Node is left out, with no affinity or node name
// that would narrow or widen that. It runs in the Host's network namespace
// as the ServiceAccount the RBAC and the admission policy name, knows its
// Node, reaches flintlockd on the Host's address, knows the trust domain
// its certificates are requested under, and writes flintlockd's
// certificates to the Host.
func TestDaemonSet(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join(execagenttest.ModuleRoot(), "config", "exec-agent", "daemonset.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var ds appsv1.DaemonSet
	if err := yaml.UnmarshalStrict(data, &ds); err != nil {
		t.Fatal(err)
	}
	spec := ds.Spec.Template.Spec
	if ds.Namespace != execagenttest.AgentNamespace || spec.ServiceAccountName != execagenttest.AgentServiceAccount {
		t.Errorf("the DaemonSet runs as %s/%s, want %s/%s", ds.Namespace, spec.ServiceAccountName,
			execagenttest.AgentNamespace, execagenttest.AgentServiceAccount)
	}
	if ds.Kind != "DaemonSet" {
		t.Errorf("the Exec Agent runs as a %s, want a DaemonSet", ds.Kind)
	}
	if want := map[string]string{"battery.liquidmetal-x.dev/host": "true"}; !maps.Equal(spec.NodeSelector, want) {
		t.Errorf("the DaemonSet selects Nodes by %v, want exactly %v", spec.NodeSelector, want)
	}
	if spec.Affinity != nil || spec.NodeName != "" {
		t.Errorf("the DaemonSet narrows its Nodes further: affinity %v, node name %q", spec.Affinity, spec.NodeName)
	}
	if !slices.ContainsFunc(spec.Tolerations, func(tol corev1.Toleration) bool {
		return tol.Key == "" && tol.Operator == corev1.TolerationOpExists && tol.Effect == ""
	}) {
		t.Errorf("the DaemonSet tolerates %v, want every taint, so that a tainted Host still runs it", spec.Tolerations)
	}
	if !spec.HostNetwork {
		t.Error("the DaemonSet is not in the Host's network namespace")
	}
	if len(spec.Containers) != 1 {
		t.Fatalf("the DaemonSet has %d containers, want the exec agent alone", len(spec.Containers))
	}
	c := spec.Containers[0]
	env := map[string]string{}
	for _, e := range c.Env {
		if e.ValueFrom != nil && e.ValueFrom.FieldRef != nil {
			env[e.Name] = e.ValueFrom.FieldRef.FieldPath
		}
	}
	if env["NODE_NAME"] != "spec.nodeName" || env["HOST_IP"] != "status.hostIP" || env["POD_NAMESPACE"] != "metadata.namespace" {
		t.Errorf("the container's downward API is %v", env)
	}
	certDir := "--flintlockd-cert-dir=" + execagent.DefaultFlintlockdCertDir
	for _, flag := range []string{"--flintlockd=$(HOST_IP):", "--trust-domain=", certDir, "--kvm-device=/host/dev/kvm", "--thin-pool="} {
		if !slices.ContainsFunc(c.Args, func(a string) bool { return strings.HasPrefix(a, flag) }) {
			t.Errorf("the container's arguments %v have no %s", c.Args, flag)
		}
	}
	// flintlockd's files are written to the Host (EA-064).
	hostPaths := map[string]string{}
	for _, v := range spec.Volumes {
		if v.HostPath != nil {
			hostPaths[v.Name] = v.HostPath.Path
		}
	}
	if !slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool {
		return m.MountPath == execagent.DefaultFlintlockdCertDir && !m.ReadOnly && hostPaths[m.Name] == execagent.DefaultFlintlockdCertDir
	}) {
		t.Errorf("the container does not mount the Host's %s writable", execagent.DefaultFlintlockdCertDir)
	}
}
