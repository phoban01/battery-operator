//go:build e2e

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

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/yaml"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/batterysidecar"
)

// What the Manifests deploy, by name.
const (
	// namespace is the Operator's and the Exec Agent's namespace.
	namespace = "battery-operator-system"
	// operatorDeployment is the Operator's, with battery as its sidecar.
	operatorDeployment = "battery-operator-controller-manager"
	// execAgentDaemonSet runs the Exec Agent on every Host.
	execAgentDaemonSet = "battery-operator-exec-agent"
	// execAgentServiceAccount is the Exec Agent's identity.
	execAgentServiceAccount = "battery-operator-exec-agent"
	// batteryConfigMap holds battery's configuration file.
	batteryConfigMap = batterysidecar.DefaultConfigMap
	// fakeHostNamespace and fakeFlintlockdDaemonSet are the fake Host's,
	// from config/fake-host.yaml.
	fakeHostNamespace       = "battery-e2e-fake-host"
	fakeFlintlockdDaemonSet = "fake-flintlockd"
	// hostLabel marks the Nodes that are Hosts; kind-config.yaml sets it on
	// both workers.
	hostLabel = "battery.liquidmetal-x.dev/host"
	// isTrue is the value of hostLabel on a Host, and of the Exec Agent's
	// readiness annotation on a ready one.
	isTrue = "true"
	// flintlockdPort is where every Host's flintlockd serves, the fake one
	// included.
	flintlockdPort = 9090
)

// The images' repositories, as config/ names them.
const (
	operatorRepository       = "ghcr.io/phoban01/battery-operator"
	execAgentRepository      = "ghcr.io/phoban01/battery-operator/exec-agent"
	fakeFlintlockdRepository = "ghcr.io/phoban01/battery-operator/fake-flintlockd"
	poolmgrdRepository       = "ghcr.io/liquidmetal-dev/poolmgrd"
)

// certManagerVersion is the cert-manager the suite installs; the Manifests
// take their certificate authorities from it (config/certificates).
const certManagerVersion = "v1.20.2"

// settings are the suite's inputs, from the environment; `make test-e2e`
// sets them.
type settings struct {
	// cluster is the kind cluster's name. A cluster of that name that
	// already exists is used as it is.
	cluster string
	// kindConfig is the kind configuration the cluster is created from.
	kindConfig string
	// The images. The first three are loaded from the local Docker into
	// kind; poolmgrd's is pulled by the nodes, and empty keeps the one the
	// Manifests pin.
	operatorImage, execAgentImage, fakeFlintlockdImage, poolmgrdImage string
	// kustomize and kubectl are the commands the suite runs.
	kustomize, kubectl string
	// keepCluster leaves the cluster in place when the suite ends.
	keepCluster bool
	// logsDir, when set, receives the cluster's logs when the suite ends.
	logsDir string
}

func settingsFromEnv() settings {
	get := func(key, def string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return def
	}
	keep, _ := strconv.ParseBool(os.Getenv("E2E_KEEP_CLUSTER"))
	return settings{
		cluster:             get("KIND_CLUSTER", "battery-operator-e2e"),
		kindConfig:          get("KIND_CONFIG", "kind-config.yaml"),
		operatorImage:       get("IMG", operatorRepository+":e2e"),
		execAgentImage:      get("EXEC_AGENT_IMG", execAgentRepository+":e2e"),
		fakeFlintlockdImage: get("FAKE_FLINTLOCKD_IMG", fakeFlintlockdRepository+":e2e"),
		poolmgrdImage:       os.Getenv("POOLMGRD_IMG"),
		kustomize:           get("KUSTOMIZE", "kustomize"),
		kubectl:             get("KUBECTL", "kubectl"),
		keepCluster:         keep,
		logsDir:             os.Getenv("E2E_LOGS_DIR"),
	}
}

// newClient is a client of the cluster that knows this project's types.
func newClient(cfg *envconf.Config) (client.Client, error) {
	//= docs/requirements/08-test-doubles.md#test-environments
	//# The e2e suite SHALL test the behaviour that depends on the
	//# Kubernetes API server, including CRD validation, admission policies,
	//# TokenReview, TokenRequest and `CertificateSigningRequest`s, in the kind
	//# cluster.
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := batteryv1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return client.New(cfg.Client().RESTConfig(), client.Options{Scheme: scheme})
}

// installCertManager installs cert-manager, at certManagerVersion, and
// waits for its webhook.
func installCertManager(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
	url := fmt.Sprintf("https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml", certManagerVersion)
	s := settingsFromEnv()
	if _, err := run(ctx, nil, s.kubectl, "--kubeconfig", cfg.KubeconfigFile(),
		"apply", "--server-side", "--force-conflicts", "-f", url); err != nil {
		return ctx, fmt.Errorf("installing cert-manager: %w", err)
	}
	c, err := newClient(cfg)
	if err != nil {
		return ctx, err
	}
	for _, name := range []string{"cert-manager", "cert-manager-cainjector", "cert-manager-webhook"} {
		if err := waitForDeploymentAvailable(ctx, c, "cert-manager", name, 5*time.Minute); err != nil {
			return ctx, err
		}
	}
	return ctx, nil
}

// deploy applies the overlay in config/, through a kustomization it writes
// to .run/ that sets the images and battery's Hosts.
//
// battery's Hosts are the kind nodes labelled as Hosts, at their internal
// addresses, rendered with batterysidecar.Render as the Inventory
// Controller renders them. The addresses are kind's, so the suite renders
// them when the cluster exists rather than config/ holding them. The
// Inventory Controller rewrites the file from the Exec Agents' reports once
// it admits the Hosts; #99 drops this in favour of it.
func deploy(s settings) env.Func {
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		//= docs/requirements/08-test-doubles.md#test-environments
		//# The e2e suite SHALL run the Operator with battery's `poolmgrd`
		//# as its sidecar, the Exec Agent, and the fake `flintlockd` as each Host's
		//# `flintlockd`, in a kind cluster, from the Manifests.
		c, err := newClient(cfg)
		if err != nil {
			return ctx, err
		}
		hosts, err := hostNodes(ctx, c)
		if err != nil {
			return ctx, err
		}
		var batteryHosts []batterysidecar.Host
		for _, n := range hosts {
			ip, err := internalIP(n)
			if err != nil {
				return ctx, err
			}
			batteryHosts = append(batteryHosts, batterysidecar.Host{
				Name:    n.Name,
				Address: net.JoinHostPort(ip, strconv.Itoa(flintlockdPort)),
			})
		}
		batteryConfig, err := batterysidecar.Render(batteryHosts)
		if err != nil {
			return ctx, err
		}

		dir, err := writeRunKustomization(s, batteryConfig)
		if err != nil {
			return ctx, err
		}
		manifests, err := run(ctx, nil, s.kustomize, "build", dir)
		if err != nil {
			return ctx, fmt.Errorf("building the Manifests: %w", err)
		}
		// cert-manager's webhook may refuse the Certificates for a while
		// after it reports available, until its CA is injected.
		var applyErr error
		err = wait.PollUntilContextTimeout(ctx, 5*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
			_, applyErr = run(ctx, manifests, s.kubectl, "--kubeconfig", cfg.KubeconfigFile(),
				"apply", "--server-side", "--force-conflicts", "-f", "-")
			return applyErr == nil, nil
		})
		if err != nil {
			return ctx, fmt.Errorf("applying the Manifests: %w: %w", err, applyErr)
		}
		return ctx, nil
	}
}

// writeRunKustomization writes .run/kustomization.yaml, which is config/
// with the suite's images and battery's configuration, and returns its
// directory. kustomize needs it below the repository, since it refuses an
// absolute path to config/.
func writeRunKustomization(s settings, batteryConfig []byte) (string, error) {
	images := []map[string]string{}
	for _, img := range []struct{ repository, ref string }{
		{operatorRepository, s.operatorImage},
		{execAgentRepository, s.execAgentImage},
		{fakeFlintlockdRepository, s.fakeFlintlockdImage},
		{poolmgrdRepository, s.poolmgrdImage},
	} {
		if img.ref == "" {
			continue
		}
		images = append(images, kustomizeImage(img.repository, img.ref))
	}
	patch, err := json.Marshal([]map[string]string{{
		"op": "replace", "path": "/data/" + batterysidecar.ConfigKey, "value": string(batteryConfig),
	}})
	if err != nil {
		return "", err
	}
	k := map[string]any{
		"apiVersion": "kustomize.config.k8s.io/v1beta1",
		"kind":       "Kustomization",
		"resources":  []string{"../config"},
		"images":     images,
		"patches": []map[string]any{{
			"target": map[string]string{"kind": "ConfigMap", "name": batteryConfigMap},
			"patch":  string(patch),
		}},
	}
	out, err := yaml.Marshal(k)
	if err != nil {
		return "", err
	}
	dir := ".run"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	header := []byte("# Written by the e2e suite (setup_test.go) for each run; not committed.\n")
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), append(header, out...), 0o644); err != nil {
		return "", err
	}
	return dir, nil
}

// kustomizeImage is a kustomize images entry that replaces repository with
// ref, a reference with a tag or a digest.
func kustomizeImage(repository, ref string) map[string]string {
	img := map[string]string{"name": repository}
	name, digest, hasDigest := strings.Cut(ref, "@")
	if hasDigest {
		img["digest"] = digest
	}
	// A tag follows the last colon after the last slash; a colon before
	// that is a registry's port.
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name, img["newTag"] = name[:i], name[i+1:]
	}
	img["newName"] = name
	return img
}

// waitForDeployment waits until the Operator is available, and the Exec
// Agent and the fake flintlockd run on every Host.
func waitForDeployment(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
	c, err := newClient(cfg)
	if err != nil {
		return ctx, err
	}
	if err := waitForDeploymentAvailable(ctx, c, namespace, operatorDeployment, 5*time.Minute); err != nil {
		return ctx, err
	}
	hosts, err := hostNodes(ctx, c)
	if err != nil {
		return ctx, err
	}
	for _, ds := range []types.NamespacedName{
		{Namespace: fakeHostNamespace, Name: fakeFlintlockdDaemonSet},
		{Namespace: namespace, Name: execAgentDaemonSet},
	} {
		if err := waitForDaemonSetReady(ctx, c, ds, len(hosts), 5*time.Minute); err != nil {
			return ctx, err
		}
	}
	return ctx, nil
}

func waitForDeploymentAvailable(ctx context.Context, c client.Client, ns, name string, timeout time.Duration) error {
	var d appsv1.Deployment
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &d); err != nil {
			return false, nil
		}
		for _, cond := range d.Status.Conditions {
			if cond.Type == appsv1.DeploymentAvailable && cond.Status == corev1.ConditionTrue {
				return d.Status.ObservedGeneration >= d.Generation, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("waiting for the Deployment %s/%s to be available: %w (status %+v)", ns, name, err, d.Status)
	}
	return nil
}

func waitForDaemonSetReady(ctx context.Context, c client.Client, key types.NamespacedName, want int, timeout time.Duration) error {
	var ds appsv1.DaemonSet
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, key, &ds); err != nil {
			return false, nil
		}
		st := ds.Status
		return st.ObservedGeneration >= ds.Generation && int(st.DesiredNumberScheduled) == want &&
			st.NumberReady == st.DesiredNumberScheduled && st.UpdatedNumberScheduled == st.DesiredNumberScheduled, nil
	})
	if err != nil {
		return fmt.Errorf("waiting for the DaemonSet %s to run on %d Hosts: %w (status %+v)", key, want, err, ds.Status)
	}
	return nil
}

// hostNodes are the Nodes labelled as Hosts, by name.
func hostNodes(ctx context.Context, c client.Client) ([]corev1.Node, error) {
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes, client.MatchingLabels{hostLabel: isTrue}); err != nil {
		return nil, fmt.Errorf("listing the Hosts: %w", err)
	}
	if len(nodes.Items) == 0 {
		return nil, fmt.Errorf("no Node is labelled %s=true; is the cluster from kind-config.yaml?", hostLabel)
	}
	return nodes.Items, nil
}

// nonHostNode is a Node that is not a Host: kind's control plane.
func nonHostNode(ctx context.Context, c client.Client) (*corev1.Node, error) {
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes); err != nil {
		return nil, err
	}
	for i := range nodes.Items {
		if nodes.Items[i].Labels[hostLabel] != isTrue {
			return &nodes.Items[i], nil
		}
	}
	return nil, fmt.Errorf("every Node is a Host")
}

func internalIP(n corev1.Node) (string, error) {
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			return a.Address, nil
		}
	}
	return "", fmt.Errorf("the Node %s has no internal address", n.Name)
}

// run runs a command with stdin, and returns its standard output, or an
// error with its standard error.
func run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, stderr.String())
	}
	return stdout.Bytes(), nil
}
