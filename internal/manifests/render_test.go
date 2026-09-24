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
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// moduleRoot is the repository's root, found from this file's path.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test's source file")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// object is one rendered document.
type object struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	raw               []byte
}

// objects is a rendering, of one or more kustomizations.
type objects []object

// build runs `kustomize build` on each of dirs, relative to the module root,
// and returns every document they render.
func build(t *testing.T, dirs ...string) objects {
	t.Helper()
	root := moduleRoot(t)
	kustomize := os.Getenv("KUSTOMIZE")
	if kustomize == "" {
		kustomize = filepath.Join(root, "bin", "kustomize")
	}
	if _, err := os.Stat(kustomize); err != nil {
		t.Fatalf("kustomize is not installed at %s (run `make kustomize`, or set $KUSTOMIZE): %v", kustomize, err)
	}
	// go test caches a result against the files the test process opens, and
	// kustomize reads config/ in a process of its own; reading every file
	// here makes a change to any of them run the tests again.
	if err := filepath.WalkDir(filepath.Join(root, "config"), func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		_, err = os.ReadFile(p)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var out objects
	for _, dir := range dirs {
		cmd := exec.Command(kustomize, "build", filepath.Join(root, dir))
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		rendered, err := cmd.Output()
		if err != nil {
			t.Fatalf("kustomize build %s: %v\n%s", dir, err, stderr.String())
		}
		for doc := range strings.SplitSeq(string(rendered), "\n---\n") {
			if strings.TrimSpace(doc) == "" {
				continue
			}
			var o object
			if err := yaml.Unmarshal([]byte(doc), &o); err != nil {
				t.Fatalf("%s: %v", dir, err)
			}
			o.raw = []byte(doc)
			out = append(out, o)
		}
	}
	return out
}

// all returns the objects of kind, decoded strictly into T.
func all[T any](t *testing.T, objs objects, kind string) []T {
	t.Helper()
	var out []T
	for _, o := range objs {
		if o.Kind != kind {
			continue
		}
		var v T
		if err := yaml.UnmarshalStrict(o.raw, &v); err != nil {
			t.Fatalf("%s %s/%s: %v", kind, o.Namespace, o.Name, err)
		}
		out = append(out, v)
	}
	return out
}

// one returns the object of kind named name, decoded strictly into T.
func one[T any](t *testing.T, objs objects, kind, name string) T {
	t.Helper()
	for _, o := range objs {
		if o.Kind == kind && o.Name == name {
			var v T
			if err := yaml.UnmarshalStrict(o.raw, &v); err != nil {
				t.Fatalf("%s %s: %v", kind, name, err)
			}
			return v
		}
	}
	t.Fatalf("no %s %s is rendered", kind, name)
	var zero T
	return zero
}

// operatorNamespace and operatorDeployment name what config/default
// renders, after its namespace and namePrefix.
const (
	operatorNamespace  = "battery-operator-system"
	operatorDeployment = "battery-operator-controller-manager"
)

// operator returns the Operator's Deployment, its manager container and
// its battery container.
func operator(t *testing.T, objs objects) (appsv1.Deployment, corev1.Container, corev1.Container) {
	t.Helper()
	d := one[appsv1.Deployment](t, objs, "Deployment", operatorDeployment)
	containers := d.Spec.Template.Spec.Containers
	if len(containers) != 2 || containers[0].Name != "manager" || containers[1].Name != "battery" {
		t.Fatalf("the Operator's pod runs %d containers; want the manager, first, and battery", len(containers))
	}
	return d, containers[0], containers[1]
}

// flag returns the value of --name (or -name) in args, and whether it is
// there.
func flag(args []string, name string) (string, bool) {
	for _, a := range args {
		for _, prefix := range []string{"--" + name + "=", "-" + name + "="} {
			if v, ok := strings.CutPrefix(a, prefix); ok {
				return v, true
			}
		}
	}
	return "", false
}

// volume returns the pod's volume named name.
func volume(t *testing.T, d appsv1.Deployment, name string) corev1.Volume {
	t.Helper()
	i := slices.IndexFunc(d.Spec.Template.Spec.Volumes, func(v corev1.Volume) bool { return v.Name == name })
	if i < 0 {
		t.Fatalf("the Operator's pod has no volume %s", name)
	}
	return d.Spec.Template.Spec.Volumes[i]
}

// mountAt returns the container's mount at path.
func mountAt(t *testing.T, c corev1.Container, path string) corev1.VolumeMount {
	t.Helper()
	i := slices.IndexFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool { return m.MountPath == path })
	if i < 0 {
		t.Fatalf("the %s container mounts nothing at %s", c.Name, path)
	}
	return c.VolumeMounts[i]
}

// A grant is one rule a binding gives one subject, in a namespace, or
// cluster-wide when namespace is empty.
type grant struct {
	subject   string
	namespace string
	rule      rbacv1.PolicyRule
}

// grants resolves every RoleBinding and ClusterRoleBinding in objs.
func grants(t *testing.T, objs objects) []grant {
	t.Helper()
	roles := map[string][]rbacv1.PolicyRule{}
	for _, r := range all[rbacv1.Role](t, objs, "Role") {
		roles["Role/"+r.Namespace+"/"+r.Name] = r.Rules
	}
	for _, r := range all[rbacv1.ClusterRole](t, objs, "ClusterRole") {
		roles["ClusterRole//"+r.Name] = r.Rules
	}
	var out []grant
	add := func(ref rbacv1.RoleRef, bindingNS, scope string, subjects []rbacv1.Subject) {
		key := ref.Kind + "/" + bindingNS + "/" + ref.Name
		if ref.Kind == "ClusterRole" {
			key = "ClusterRole//" + ref.Name
		}
		rules, ok := roles[key]
		if !ok {
			t.Fatalf("a binding refers to %s, which is not rendered", key)
		}
		for _, s := range subjects {
			id := s.Kind + ":" + s.Namespace + "/" + s.Name
			for _, r := range rules {
				out = append(out, grant{subject: id, namespace: scope, rule: r})
			}
		}
	}
	for _, b := range all[rbacv1.RoleBinding](t, objs, "RoleBinding") {
		add(b.RoleRef, b.Namespace, b.Namespace, b.Subjects)
	}
	for _, b := range all[rbacv1.ClusterRoleBinding](t, objs, "ClusterRoleBinding") {
		add(b.RoleRef, "", "", b.Subjects)
	}
	return out
}

// serviceAccount is a grant's subject for a ServiceAccount.
func serviceAccount(namespace, name string) string {
	return "ServiceAccount:" + namespace + "/" + name
}

func matches(list []string, v string) bool {
	return slices.Contains(list, "*") || slices.Contains(list, v)
}

// allows reports whether the grant allows verb on group's resource, for the
// object named name, or for some object when name is empty, in namespace.
func (g grant) allows(namespace, group, resource, verb, name string) bool {
	if g.namespace != "" && g.namespace != namespace {
		return false
	}
	if !matches(g.rule.APIGroups, group) || !matches(g.rule.Verbs, verb) {
		return false
	}
	base, sub, _ := strings.Cut(resource, "/")
	if !matches(g.rule.Resources, resource) && (sub == "" || !slices.Contains(g.rule.Resources, base+"/*")) {
		return false
	}
	return name == "" || len(g.rule.ResourceNames) == 0 || slices.Contains(g.rule.ResourceNames, name)
}
