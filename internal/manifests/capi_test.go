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

// The tests of the Host Pool Templates, config/capi
// (docs/requirements/12-host-pool.md). Each renders config/capi, the
// example pool a user copies, and testdata/capi, two pools filled in as a
// user fills them in. `make capi-check` validates the same renders against
// the Cluster API and CAPA CRDs.

import (
	"bufio"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/phoban01/battery-operator/internal/execagent"
)

const (
	capiTemplates = "config/capi"
	capiTestdata  = "internal/manifests/testdata/capi"
	hostConfPath  = "/etc/battery/host.conf"
	hostLabel     = "battery.liquidmetal-x.dev/host"
	hostPoolLabel = "battery.liquidmetal-x.dev/host-pool"
	clusterLabel  = "cluster.x-k8s.io/cluster-name"
)

// capiRenders are the renders every test checks.
var capiRenders = []string{capiTemplates, capiTestdata}

// capiRef is a MachineDeployment's reference to a template.
type capiRef struct {
	APIGroup string `json:"apiGroup"`
	Kind     string `json:"kind"`
	Name     string `json:"name"`
}

// machineDeployment is the part of a MachineDeployment the tests read.
type machineDeployment struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		ClusterName string `json:"clusterName"`
		Replicas    *int   `json:"replicas"`
		Selector    struct {
			MatchLabels map[string]string `json:"matchLabels"`
		} `json:"selector"`
		Template struct {
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				ClusterName string `json:"clusterName"`
				Version     string `json:"version"`
				Bootstrap   struct {
					ConfigRef capiRef `json:"configRef"`
				} `json:"bootstrap"`
				InfrastructureRef capiRef `json:"infrastructureRef"`
				Deletion          struct {
					NodeDrainTimeoutSeconds *int `json:"nodeDrainTimeoutSeconds"`
				} `json:"deletion"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

// The names of the two pools in testdata/capi.
const (
	testPoolA = "hosts-a"
	testPoolB = "hosts-b"
)

// awsMachineTemplate is the part of an AWSMachineTemplate the tests read,
// and its spec.template.spec as a map, to look for fields by name.
type awsMachineTemplate struct {
	Spec struct {
		Template struct {
			Spec struct {
				InstanceType string `json:"instanceType"`
				AMI          struct {
					ID string `json:"id"`
				} `json:"ami"`
				Subnet struct {
					ID      string `json:"id"`
					Filters []any  `json:"filters"`
				} `json:"subnet"`
				AdditionalSecurityGroups []struct {
					ID string `json:"id"`
				} `json:"additionalSecurityGroups"`
				RootVolume struct {
					Size int `json:"size"`
				} `json:"rootVolume"`
				CloudInit struct {
					InsecureSkipSecretsManager bool `json:"insecureSkipSecretsManager"`
				} `json:"cloudInit"`
				InstanceMetadataOptions struct {
					HTTPEndpoint            string `json:"httpEndpoint"`
					HTTPTokens              string `json:"httpTokens"`
					HTTPPutResponseHopLimit int    `json:"httpPutResponseHopLimit"`
				} `json:"instanceMetadataOptions"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	raw map[string]any
}

// kubeadmConfigTemplate is the part of a KubeadmConfigTemplate the tests
// read.
type kubeadmConfigTemplate struct {
	Spec struct {
		Template struct {
			Spec struct {
				Format string `json:"format"`
				Files  []struct {
					Path        string `json:"path"`
					Owner       string `json:"owner"`
					Permissions string `json:"permissions"`
					Content     string `json:"content"`
				} `json:"files"`
				JoinConfiguration struct {
					NodeRegistration struct {
						Taints           []corev1.Taint `json:"taints"`
						KubeletExtraArgs []struct {
							Name  string `json:"name"`
							Value string `json:"value"`
						} `json:"kubeletExtraArgs"`
					} `json:"nodeRegistration"`
				} `json:"joinConfiguration"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

// machineHealthCheck is the part of a MachineHealthCheck the tests read.
type machineHealthCheck struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		ClusterName string `json:"clusterName"`
		Selector    struct {
			MatchLabels map[string]string `json:"matchLabels"`
		} `json:"selector"`
		Checks struct {
			UnhealthyNodeConditions []unhealthyCondition `json:"unhealthyNodeConditions"`
		} `json:"checks"`
	} `json:"spec"`
}

// unhealthyCondition is a Node condition a MachineHealthCheck acts on.
type unhealthyCondition struct {
	Type           string `json:"type"`
	Status         string `json:"status"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
}

// hostPool is one pool of a render: its MachineDeployment and the objects
// it names.
type hostPool struct {
	md      machineDeployment
	machine awsMachineTemplate
	kubeadm kubeadmConfigTemplate
}

// buildCAPI renders dir, which is under config/ or testdata, and reads
// every file under testdata first so that go test sees a change to one.
func buildCAPI(t *testing.T, dir string) objects {
	t.Helper()
	if err := filepath.WalkDir(filepath.Join(moduleRoot(t), capiTestdata), func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		_, err = os.ReadFile(p)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	objs := build(t, dir)
	if len(objs) == 0 {
		t.Fatalf("%s renders nothing", dir)
	}
	return objs
}

// decode decodes o, without strictness: the tests read only some fields.
func decode[T any](t *testing.T, o object) T {
	t.Helper()
	var v T
	if err := yaml.Unmarshal(o.raw, &v); err != nil {
		t.Fatalf("%s %s: %v", o.Kind, o.Name, err)
	}
	return v
}

// named returns the object of kind named name in objs.
func named(t *testing.T, objs objects, kind, name string) object {
	t.Helper()
	i := slices.IndexFunc(objs, func(o object) bool { return o.Kind == kind && o.Name == name })
	if i < 0 {
		t.Fatalf("no %s %s is rendered", kind, name)
	}
	return objs[i]
}

// hostPools returns every pool of objs, by its MachineDeployment.
func hostPools(t *testing.T, objs objects) []hostPool {
	t.Helper()
	var pools []hostPool
	for _, o := range objs {
		if o.Kind != "MachineDeployment" {
			continue
		}
		md := decode[machineDeployment](t, o)
		spec := md.Spec.Template.Spec
		machineObj := named(t, objs, spec.InfrastructureRef.Kind, spec.InfrastructureRef.Name)
		machine := decode[awsMachineTemplate](t, machineObj)
		raw := decode[map[string]any](t, machineObj)
		machine.raw, _ = dig(raw, "spec", "template", "spec").(map[string]any)
		pools = append(pools, hostPool{
			md:      md,
			machine: machine,
			kubeadm: decode[kubeadmConfigTemplate](t, named(t, objs, spec.Bootstrap.ConfigRef.Kind, spec.Bootstrap.ConfigRef.Name)),
		})
	}
	if len(pools) == 0 {
		t.Fatal("no MachineDeployment is rendered")
	}
	return pools
}

// dig returns the value at path in m, or nil.
func dig(m map[string]any, path ...string) any {
	var v any = m
	for _, p := range path {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = mm[p]
	}
	return v
}

// hostConf returns the Host configuration file a pool writes, as its lines
// of KEY=value, and fails if the pool writes none.
func hostConf(t *testing.T, p hostPool) map[string]string {
	t.Helper()
	for _, f := range p.kubeadm.Spec.Template.Spec.Files {
		if f.Path == hostConfPath {
			return keyValues(t, f.Content)
		}
	}
	t.Fatalf("%s: the bootstrap files write no %s", p.md.Metadata.Name, hostConfPath)
	return nil
}

// keyValues parses KEY=value lines, skipping blank lines and comments.
func keyValues(t *testing.T, content string) map[string]string {
	t.Helper()
	kv := map[string]string{}
	s := bufio.NewScanner(strings.NewReader(content))
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("%q is not KEY=value", line)
		}
		kv[k] = v
	}
	return kv
}

// hostImageFile reads a KEY=value file of the Host Image.
func hostImageFile(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(moduleRoot(t), path))
	if err != nil {
		t.Fatal(err)
	}
	return keyValues(t, string(b))
}

//= docs/requirements/12-host-pool.md#host-pool
//= type=test
//# The Host Pool Templates SHALL define each Host pool as one
//# Cluster API MachineDeployment with an AWSMachineTemplate of its own,
//# which names a single instance type, an AMI by explicit id and a subnet by
//# explicit id.

// TestHostPoolIsOneMachineDeployment checks every pool's MachineDeployment
// and the AWSMachineTemplate it alone uses.
func TestHostPoolIsOneMachineDeployment(t *testing.T) {
	t.Parallel()
	instanceType := regexp.MustCompile(`^[a-z0-9]+\.[a-z0-9-]+$`)
	for _, dir := range capiRenders {
		objs := buildCAPI(t, dir)
		pools := hostPools(t, objs)
		for _, p := range pools {
			name := p.md.Metadata.Name
			ref := p.md.Spec.Template.Spec.InfrastructureRef
			if ref.APIGroup != "infrastructure.cluster.x-k8s.io" || ref.Kind != "AWSMachineTemplate" {
				t.Errorf("%s/%s: the infrastructure is %s %s, not an AWSMachineTemplate", dir, name, ref.APIGroup, ref.Kind)
			}
			users := 0
			for _, q := range pools {
				if q.md.Spec.Template.Spec.InfrastructureRef.Name == ref.Name {
					users++
				}
			}
			if users != 1 {
				t.Errorf("%s/%s: its AWSMachineTemplate %s is used by %d MachineDeployments", dir, name, ref.Name, users)
			}
			spec := p.machine.Spec.Template.Spec
			if !instanceType.MatchString(spec.InstanceType) {
				t.Errorf("%s/%s: instance type %q is not one instance type", dir, name, spec.InstanceType)
			}
			if !strings.HasPrefix(spec.AMI.ID, "ami-") {
				t.Errorf("%s/%s: the AMI %q is not an explicit id", dir, name, spec.AMI.ID)
			}
			for _, lookup := range []string{"imageLookupOrg", "imageLookupFormat", "imageLookupBaseOS"} {
				if _, ok := p.machine.raw[lookup]; ok {
					t.Errorf("%s/%s: the AMI is looked up, with %s", dir, name, lookup)
				}
			}
			if ami, _ := p.machine.raw["ami"].(map[string]any); len(ami) != 1 {
				t.Errorf("%s/%s: the AMI is %v; want an id alone", dir, name, ami)
			}
			if !strings.HasPrefix(spec.Subnet.ID, "subnet-") || len(spec.Subnet.Filters) != 0 {
				t.Errorf("%s/%s: the subnet is %q with %d filters; want an explicit id alone", dir, name, spec.Subnet.ID, len(spec.Subnet.Filters))
			}
		}
	}
}

//= docs/requirements/12-host-pool.md#host-pool
//= type=test
//# The Host Pool Templates SHALL take a Host pool's name, cluster,
//# size, Kubernetes version, instance type, AMI, subnet, health check
//# interval, drain timeout and Host configuration file from one settings
//# object per pool, which is never applied to a cluster.

// TestHostPoolSettingsLand renders two pools filled in with values of
// their own, and finds each value where it belongs, and no placeholder and
// no settings object in the output.
func TestHostPoolSettingsLand(t *testing.T) {
	t.Parallel()
	type want struct {
		cluster, version, instanceType, ami, subnet string
		replicas, drain, unhealthy                  int
		conf                                        map[string]string
	}
	wants := map[string]want{
		testPoolA: {"workload-a", "v1.35.8", "m6id.metal", "ami-0123456789abcdef0", "subnet-0aaaaaaaaaaaaaaaa", 3, 4500, 301,
			map[string]string{"GUEST_SUBNET": "10.220.0.0/16", "FLINTLOCKD_CLIENT_CIDRS": "192.168.0.0/16"}},
		testPoolB: {"workload-a", "v1.35.7", "c6id.metal", "ami-0fedcba9876543210", "subnet-0bbbbbbbbbbbbbbbb", 5, 7200, 600,
			map[string]string{"GUEST_SUBNET": "10.230.0.0/24", "THIN_POOL_DEVICE": "/dev/nvme2n1"}},
	}
	objs := buildCAPI(t, capiTestdata)
	pools := hostPools(t, objs)
	if len(pools) != len(wants) {
		t.Fatalf("%d pools are rendered; want %d", len(pools), len(wants))
	}
	for _, p := range pools {
		name := p.md.Metadata.Name
		w, ok := wants[name]
		if !ok {
			t.Errorf("an unexpected pool %s is rendered", name)
			continue
		}
		md := p.md.Spec
		spec := md.Template.Spec
		for _, got := range []struct{ what, got, want string }{
			{"clusterName", md.ClusterName, w.cluster},
			{"template clusterName", spec.ClusterName, w.cluster},
			{"cluster label", md.Template.Metadata.Labels[clusterLabel], w.cluster},
			{"pool label", md.Template.Metadata.Labels[hostPoolLabel], name},
			{"selector's pool label", md.Selector.MatchLabels[hostPoolLabel], name},
			{"version", spec.Version, w.version},
			{"bootstrap config", spec.Bootstrap.ConfigRef.Name, name},
			{"infrastructure", spec.InfrastructureRef.Name, name},
			{"instance type", p.machine.Spec.Template.Spec.InstanceType, w.instanceType},
			{"AMI", p.machine.Spec.Template.Spec.AMI.ID, w.ami},
			{"subnet", p.machine.Spec.Template.Spec.Subnet.ID, w.subnet},
		} {
			if got.got != got.want {
				t.Errorf("%s: %s is %q; want %q", name, got.what, got.got, got.want)
			}
		}
		if md.Replicas == nil || *md.Replicas != w.replicas {
			t.Errorf("%s: replicas is %v; want %d", name, md.Replicas, w.replicas)
		}
		if d := spec.Deletion.NodeDrainTimeoutSeconds; d == nil || *d != w.drain {
			t.Errorf("%s: nodeDrainTimeoutSeconds is %v; want %d", name, d, w.drain)
		}
		mhc := decode[machineHealthCheck](t, named(t, objs, "MachineHealthCheck", name))
		if mhc.Spec.ClusterName != w.cluster || mhc.Spec.Selector.MatchLabels[hostPoolLabel] != name {
			t.Errorf("%s: its MachineHealthCheck is for %s, pool %s", name, mhc.Spec.ClusterName, mhc.Spec.Selector.MatchLabels[hostPoolLabel])
		}
		for _, c := range mhc.Spec.Checks.UnhealthyNodeConditions {
			if c.TimeoutSeconds != w.unhealthy {
				t.Errorf("%s: the %s=%s timeout is %d; want %d", name, c.Type, c.Status, c.TimeoutSeconds, w.unhealthy)
			}
		}
		conf := hostConf(t, p)
		for k, v := range w.conf {
			if conf[k] != v {
				t.Errorf("%s: host.conf has %s=%q; want %q", name, k, conf[k], v)
			}
		}
	}
	placeholders := regexp.MustCompile(`\b(POOL|CLUSTER|KUBERNETES_VERSION|INSTANCE_TYPE|AMI_ID|SUBNET_ID|SECURITY_GROUP_ID|HOST_CONF)\b`)
	for _, dir := range capiRenders {
		for _, o := range buildCAPI(t, dir) {
			if o.Kind == "ConfigMap" {
				t.Errorf("%s: the settings object %s is rendered; it is local-config", dir, o.Name)
			}
			if m := placeholders.Find(o.raw); m != nil {
				t.Errorf("%s: %s %s still has the placeholder %s", dir, o.Kind, o.Name, m)
			}
		}
	}
}

//= docs/requirements/12-host-pool.md#host-pool
//= type=test
//# The Host Pool Templates SHALL attach to every Host of a pool the
//# additional security groups that the pool's settings list by id.

// TestHostPoolSecurityGroups finds each filled-in pool's security groups,
// in order, on its AWSMachineTemplate, and an id on every pool's.
func TestHostPoolSecurityGroups(t *testing.T) {
	t.Parallel()
	wants := map[string][]string{
		testPoolA: {"sg-0aaaaaaaaaaaaaaa1", "sg-0aaaaaaaaaaaaaaa2"},
		testPoolB: {"sg-0bbbbbbbbbbbbbbb1"},
	}
	for _, dir := range capiRenders {
		for _, p := range hostPools(t, buildCAPI(t, dir)) {
			name := p.md.Metadata.Name
			var got []string
			for _, g := range p.machine.Spec.Template.Spec.AdditionalSecurityGroups {
				got = append(got, g.ID)
			}
			if len(got) == 0 {
				t.Errorf("%s/%s: no additional security group", dir, name)
			}
			for _, id := range got {
				if !strings.HasPrefix(id, "sg-") {
					t.Errorf("%s/%s: security group %q is not an id", dir, name, id)
				}
			}
			if dir == capiTestdata && !slices.Equal(got, wants[name]) {
				t.Errorf("%s/%s: the security groups are %v; want %v", dir, name, got, wants[name])
			}
		}
	}
}

//= docs/requirements/12-host-pool.md#host-pool
//= type=test
//# The Host Pool Templates SHALL set the size of every Host's
//# root volume, in GiB, from the pool's settings.

// TestHostPoolRootVolume finds each filled-in pool's root volume size on
// its AWSMachineTemplate, and a size on every pool's.
func TestHostPoolRootVolume(t *testing.T) {
	t.Parallel()
	wants := map[string]int{testPoolA: 150, testPoolB: 400}
	for _, dir := range capiRenders {
		for _, p := range hostPools(t, buildCAPI(t, dir)) {
			name := p.md.Metadata.Name
			got := p.machine.Spec.Template.Spec.RootVolume.Size
			if got <= 0 {
				t.Errorf("%s/%s: the root volume has no size", dir, name)
			}
			if dir == capiTestdata && got != wants[name] {
				t.Errorf("%s/%s: the root volume is %d GiB; want %d", dir, name, got, wants[name])
			}
		}
	}
}

//= docs/requirements/12-host-pool.md#host-pool
//= type=test
//# The Host Pool Templates SHALL default a Host pool's Kubernetes
//# version to the version that the Host Image pins.

// TestHostPoolKubernetesVersion compares the example pool's Kubernetes
// version with hostimage/versions.env.
func TestHostPoolKubernetesVersion(t *testing.T) {
	t.Parallel()
	pinned := hostImageFile(t, "hostimage/versions.env")["KUBERNETES_VERSION"]
	if pinned == "" {
		t.Fatal("hostimage/versions.env pins no KUBERNETES_VERSION")
	}
	for _, p := range hostPools(t, buildCAPI(t, capiTemplates)) {
		if v := p.md.Spec.Template.Spec.Version; v != pinned {
			t.Errorf("%s: the Kubernetes version is %s; the Host Image pins %s", p.md.Metadata.Name, v, pinned)
		}
	}
}

//= docs/requirements/12-host-pool.md#host-pool
//= type=test
//# The Host Pool Templates SHALL set no node drain timeout on a
//# Host pool's Machines that is shorter than the Exec Agent's default
//# drain timeout.

// TestHostPoolDrainTimeout compares every pool's node drain timeout with
// the Exec Agent's default --drain-timeout.
func TestHostPoolDrainTimeout(t *testing.T) {
	t.Parallel()
	least := int(execagent.DefaultDrainTimeout.Seconds())
	for _, dir := range capiRenders {
		for _, p := range hostPools(t, buildCAPI(t, dir)) {
			d := p.md.Spec.Template.Spec.Deletion.NodeDrainTimeoutSeconds
			switch {
			case d == nil:
				t.Errorf("%s/%s: no nodeDrainTimeoutSeconds", dir, p.md.Metadata.Name)
			case *d != 0 && *d < least:
				// 0 is no timeout, which is never shorter.
				t.Errorf("%s/%s: nodeDrainTimeoutSeconds %d is shorter than the Exec Agent's %ds", dir, p.md.Metadata.Name, *d, least)
			}
		}
	}
}

//= docs/requirements/12-host-pool.md#host-pool
//= type=test
//# The Host Pool Templates SHALL define a MachineHealthCheck for each
//# Host pool that replaces a Machine whose Node stays not ready beyond the
//# interval its host-pool.yaml sets.

// TestHostPoolHealthCheck finds, for every pool, a MachineHealthCheck of
// its cluster that selects its Machines and acts on Ready=False and
// Ready=Unknown.
func TestHostPoolHealthCheck(t *testing.T) {
	t.Parallel()
	for _, dir := range capiRenders {
		objs := buildCAPI(t, dir)
		var checks []machineHealthCheck
		for _, o := range objs {
			if o.Kind == "MachineHealthCheck" {
				checks = append(checks, decode[machineHealthCheck](t, o))
			}
		}
		for _, p := range hostPools(t, objs) {
			labels := p.md.Spec.Template.Metadata.Labels
			found := false
			for _, c := range checks {
				if c.Spec.ClusterName != p.md.Spec.ClusterName || len(c.Spec.Selector.MatchLabels) == 0 {
					continue
				}
				selects := true
				for k, v := range c.Spec.Selector.MatchLabels {
					selects = selects && labels[k] == v
				}
				if !selects {
					continue
				}
				found = true
				for _, status := range []string{"False", "Unknown"} {
					if !slices.ContainsFunc(c.Spec.Checks.UnhealthyNodeConditions, func(u unhealthyCondition) bool {
						return u.Type == "Ready" && u.Status == status && u.TimeoutSeconds > 0
					}) {
						t.Errorf("%s/%s: no Ready=%s condition with a timeout", dir, c.Metadata.Name, status)
					}
				}
			}
			if !found {
				t.Errorf("%s/%s: no MachineHealthCheck selects its Machines", dir, p.md.Metadata.Name)
			}
		}
	}
}

//= docs/requirements/12-host-pool.md#host-configuration
//= type=test
//# The Host Pool Templates SHALL write the Host configuration file
//# `/etc/battery/host.conf` through the files of each Host pool's
//# KubeadmConfigTemplate, with only keys that the Host Image's defaults
//# file names.

// TestHostPoolWritesHostConf checks every pool's bootstrap files against
// the Host Image's defaults file.
func TestHostPoolWritesHostConf(t *testing.T) {
	t.Parallel()
	defaults := hostImageFile(t, "hostimage/rootfs/usr/share/battery/host.conf.defaults")
	for _, dir := range capiRenders {
		for _, p := range hostPools(t, buildCAPI(t, dir)) {
			name := p.md.Metadata.Name
			if f := p.kubeadm.Spec.Template.Spec.Format; f != "cloud-config" {
				t.Errorf("%s/%s: the bootstrap format is %q; the Host Image runs cloud-init", dir, name, f)
			}
			for _, f := range p.kubeadm.Spec.Template.Spec.Files {
				if f.Path == hostConfPath && (f.Owner != "root:root" || f.Permissions != "0644") {
					t.Errorf("%s/%s: %s is %s %s; want root:root 0644", dir, name, hostConfPath, f.Owner, f.Permissions)
				}
			}
			for k := range hostConf(t, p) {
				if _, ok := defaults[k]; !ok {
					t.Errorf("%s/%s: host.conf sets %s, which the Host Image does not read", dir, name, k)
				}
			}
		}
	}
}

//= docs/requirements/12-host-pool.md#host-configuration
//= type=test
//# The Host Pool Templates SHALL set `FLINTLOCKD_CLIENT_CIDRS` and
//# `PROTECTED_CIDRS` in every Host configuration file they write.

// TestHostPoolSetsClusterRanges checks that every pool sets both, and that
// the filled-in pools set lists of IPv4 CIDRs, as the Host Image requires.
func TestHostPoolSetsClusterRanges(t *testing.T) {
	t.Parallel()
	for _, dir := range capiRenders {
		for _, p := range hostPools(t, buildCAPI(t, dir)) {
			conf := hostConf(t, p)
			for _, k := range []string{"FLINTLOCKD_CLIENT_CIDRS", "PROTECTED_CIDRS"} {
				v := conf[k]
				if v == "" {
					t.Errorf("%s/%s: host.conf does not set %s", dir, p.md.Metadata.Name, k)
					continue
				}
				if dir != capiTestdata {
					// The example's values are REPLACE placeholders.
					continue
				}
				for cidr := range strings.SplitSeq(v, ",") {
					if ip, _, err := net.ParseCIDR(cidr); err != nil || ip.To4() == nil {
						t.Errorf("%s/%s: %s has %q, which is not an IPv4 CIDR", dir, p.md.Metadata.Name, k, cidr)
					}
				}
			}
		}
	}
}

//= docs/requirements/12-host-pool.md#host-node
//= type=test
//# The Host Pool Templates SHALL register every Host's kubelet
//# with the label `battery.liquidmetal-x.dev/host` set to `true`,
//# through the node labels of the kubeadm join configuration.

// TestHostPoolLabelsHosts checks every pool's node labels, and that the
// Exec Agent's DaemonSet selects the label they set.
func TestHostPoolLabelsHosts(t *testing.T) {
	t.Parallel()
	ds := one[appsv1.DaemonSet](t, build(t, "config/exec-agent"), "DaemonSet", "battery-operator-exec-agent")
	if got := ds.Spec.Template.Spec.NodeSelector[hostLabel]; got != "true" {
		t.Fatalf("the Exec Agent selects %s=%q; the Host label is %s=true", hostLabel, got, hostLabel)
	}
	for _, dir := range capiRenders {
		for _, p := range hostPools(t, buildCAPI(t, dir)) {
			var labels []string
			for _, a := range p.kubeadm.Spec.Template.Spec.JoinConfiguration.NodeRegistration.KubeletExtraArgs {
				if a.Name == "node-labels" {
					labels = append(labels, strings.Split(a.Value, ",")...)
				}
			}
			if !slices.Contains(labels, hostLabel+"=true") {
				t.Errorf("%s/%s: the node labels %v do not include %s=true", dir, p.md.Metadata.Name, labels, hostLabel)
			}
		}
	}
}

//= docs/requirements/12-host-pool.md#host-node
//= type=test
//# The Host Pool Templates SHALL join every Host with the taint
//# `battery.liquidmetal-x.dev/host=true:NoSchedule`, so that only pods
//# that tolerate it run on a Host's own Node.

// TestHostPoolTaintsHosts checks every pool's join taints, and that the
// Exec Agent's DaemonSet tolerates the taint.
func TestHostPoolTaintsHosts(t *testing.T) {
	t.Parallel()
	taint := corev1.Taint{Key: hostLabel, Value: "true", Effect: corev1.TaintEffectNoSchedule}
	ds := one[appsv1.DaemonSet](t, build(t, "config/exec-agent"), "DaemonSet", "battery-operator-exec-agent")
	tolerates := func(tol corev1.Toleration) bool {
		if tol.Effect != "" && tol.Effect != taint.Effect {
			return false
		}
		if tol.Operator == corev1.TolerationOpExists {
			return tol.Key == "" || tol.Key == taint.Key
		}
		return tol.Key == taint.Key && tol.Value == taint.Value
	}
	if !slices.ContainsFunc(ds.Spec.Template.Spec.Tolerations, tolerates) {
		t.Errorf("the Exec Agent does not tolerate the taint %s", taint.ToString())
	}
	for _, dir := range capiRenders {
		for _, p := range hostPools(t, buildCAPI(t, dir)) {
			taints := p.kubeadm.Spec.Template.Spec.JoinConfiguration.NodeRegistration.Taints
			if !slices.ContainsFunc(taints, func(tt corev1.Taint) bool { return tt.MatchTaint(&taint) && tt.Value == taint.Value }) {
				t.Errorf("%s/%s: the join taints %v do not include %s", dir, p.md.Metadata.Name, taints, taint.ToString())
			}
		}
	}
}

//= docs/requirements/12-host-pool.md#host-access
//= type=test
//# The Host Pool Templates SHALL NOT give a Host an AWS instance
//# profile, and SHALL NOT fetch a Host's bootstrap data from AWS Secrets
//# Manager.

// TestHostPoolGivesHostsNoAWSIdentity checks every AWSMachineTemplate, and
// looks for AWS identities anywhere in the renders.
func TestHostPoolGivesHostsNoAWSIdentity(t *testing.T) {
	t.Parallel()
	identity := regexp.MustCompile(`(?i)iamInstanceProfile|arn:aws|AWS_ACCESS_KEY_ID|AWS_SECRET_ACCESS_KEY|AWS_SESSION_TOKEN|AWS_ROLE_ARN|AWS_PROFILE`)
	for _, dir := range capiRenders {
		objs := buildCAPI(t, dir)
		for _, o := range objs {
			if m := identity.Find(o.raw); m != nil {
				t.Errorf("%s: %s %s names %s", dir, o.Kind, o.Name, m)
			}
		}
		for _, p := range hostPools(t, objs) {
			if _, ok := p.machine.raw["iamInstanceProfile"]; ok {
				t.Errorf("%s/%s: the Hosts have an instance profile", dir, p.md.Metadata.Name)
			}
			if !p.machine.Spec.Template.Spec.CloudInit.InsecureSkipSecretsManager {
				t.Errorf("%s/%s: the Hosts fetch their bootstrap data from Secrets Manager", dir, p.md.Metadata.Name)
			}
		}
	}
}

//= docs/requirements/12-host-pool.md#host-access
//= type=test
//# The Host Pool Templates SHALL require session tokens for the
//# instance metadata service on every Host, with a response hop limit
//# of 1.

// TestHostPoolRequiresIMDSv2 checks every AWSMachineTemplate's metadata
// options.
func TestHostPoolRequiresIMDSv2(t *testing.T) {
	t.Parallel()
	for _, dir := range capiRenders {
		for _, p := range hostPools(t, buildCAPI(t, dir)) {
			o := p.machine.Spec.Template.Spec.InstanceMetadataOptions
			if o.HTTPTokens != "required" || o.HTTPPutResponseHopLimit != 1 {
				t.Errorf("%s/%s: the metadata options are tokens %q, hop limit %d; want required, 1", dir, p.md.Metadata.Name, o.HTTPTokens, o.HTTPPutResponseHopLimit)
			}
		}
	}
}
