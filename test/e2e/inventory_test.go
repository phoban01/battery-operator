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
	"fmt"
	"maps"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"sigs.k8s.io/yaml"

	"github.com/phoban01/battery-operator/internal/controller/inventory"
)

// batteryContainer is battery's container in the Operator's pod.
const batteryContainer = "battery"

// quietPeriod is how long battery is watched for a restart after its Hosts
// are back: several times the settle time and restart window of the
// suite's overlay (config/manager-inventory-timing.yaml), so that a restart
// the Inventory Controller decided on would have come.
const quietPeriod = 30 * time.Second

// TestReapplyingTheManifestsKeepsBatteryHosts: once battery runs with the
// kind workers, applying the Manifests again, server-side by force as an
// upgrade does and then client-side, resets battery's ConfigMap, and the
// Inventory Controller writes battery's Hosts back without restarting
// battery.
func TestReapplyingTheManifestsKeepsBatteryHosts(t *testing.T) {
	testenv.Test(t, features.New("Re-applying the Manifests").
		Assess("battery's Hosts are written back, and battery is not restarted", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/04-inventory.md#keeping
			//= type=test
			//# If battery's configuration names Hosts other than those the
			//# Inventory Controller last wrote to it, then the Inventory Controller SHALL
			//# write those Hosts back to battery's configuration without restarting
			//# battery.
			c := mustClient(t, cfg)
			s := settingsFromEnv()
			want := readyHosts(ctx, t, c)
			waitForBatteryHosts(ctx, t, c, want, "")
			restarts := batteryRestarts(ctx, t, c)

			before := batteryConfigMapVersion(ctx, t, c)
			manifests, err := applyManifests(ctx, cfg, s)
			if err != nil {
				t.Fatal(err)
			}
			waitForBatteryHosts(ctx, t, c, want, before)

			before = batteryConfigMapVersion(ctx, t, c)
			cm, err := shippedBatteryConfigMap(manifests)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := run(ctx, cm, s.kubectl, "--kubeconfig", cfg.KubeconfigFile(), "apply", "-f", "-"); err != nil {
				t.Fatalf("applying battery's ConfigMap client-side: %v", err)
			}
			waitForBatteryHosts(ctx, t, c, want, before)

			// Nothing restarts battery for it, then or once a settle time
			// and a restart window have passed.
			deadline := time.Now().Add(quietPeriod)
			for time.Now().Before(deadline) {
				if got := batteryRestarts(ctx, t, c); got != restarts {
					t.Fatalf("battery restarted %d times after the Manifests were applied again, want none", got-restarts)
				}
				time.Sleep(2 * time.Second)
			}
			if got, err := batteryHosts(ctx, c); err != nil || !maps.Equal(got, want) {
				t.Fatalf("battery's Hosts are %v (%v), want %v", got, err, want)
			}
			return ctx
		}).
		Feature())
}

// waitForBatteryHosts waits until battery's ConfigMap names want and no
// restart of battery is pending, and, unless changedFrom is empty, until
// its resourceVersion is no longer changedFrom: the ConfigMap has been
// written since.
func waitForBatteryHosts(ctx context.Context, t *testing.T, c client.Client, want map[string]string, changedFrom string) {
	t.Helper()
	var got map[string]string
	var cm corev1.ConfigMap
	err := wait.PollUntilContextTimeout(ctx, time.Second, placementTimeout, true, func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: batteryConfigMap}, &cm); err != nil {
			return false, nil
		}
		var err error
		if got, err = batteryHosts(ctx, c); err != nil {
			return false, nil
		}
		return maps.Equal(got, want) && cm.Annotations[inventory.RestartPendingAnnotation] == "" &&
			(changedFrom == "" || cm.ResourceVersion != changedFrom), nil
	})
	if err != nil {
		t.Fatalf("battery's Hosts are %v, want %v; annotations %v", got, want, cm.Annotations)
	}
}

// batteryConfigMapVersion is battery's ConfigMap's resourceVersion.
func batteryConfigMapVersion(ctx context.Context, t *testing.T, c client.Client) string {
	t.Helper()
	var cm corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: batteryConfigMap}, &cm); err != nil {
		t.Fatal(err)
	}
	return cm.ResourceVersion
}

// batteryRestarts is how many times battery's container has restarted in
// the Operator's pod; the Inventory Controller's restarts of battery are
// among them.
func batteryRestarts(ctx context.Context, t *testing.T, c client.Client) int32 {
	t.Helper()
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(namespace),
		client.MatchingLabels{"control-plane": "controller-manager"}); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("the Operator runs %d pods, want 1", len(pods.Items))
	}
	for _, st := range pods.Items[0].Status.ContainerStatuses {
		if st.Name == batteryContainer {
			return st.RestartCount
		}
	}
	t.Fatalf("the Operator's pod has no container %s", batteryContainer)
	return 0
}

// shippedBatteryConfigMap is battery's ConfigMap from the Manifests, alone.
func shippedBatteryConfigMap(manifests []byte) ([]byte, error) {
	for doc := range bytes.SplitSeq(manifests, []byte("\n---\n")) {
		var obj struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal(doc, &obj); err != nil {
			return nil, err
		}
		if obj.Kind == "ConfigMap" && obj.Metadata.Name == batteryConfigMap && obj.Metadata.Namespace == namespace {
			return doc, nil
		}
	}
	return nil, fmt.Errorf("the Manifests have no ConfigMap %s/%s", namespace, batteryConfigMap)
}
