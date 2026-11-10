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
	"context"
	"fmt"
	"maps"
	"net"
	"os/exec"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/batterysidecar"
	"github.com/phoban01/battery-operator/internal/controller/inventory"
)

// batteryContainer is battery's container in the Operator's pod.
const batteryContainer = "battery"

// quietPeriod is how long battery is watched for a restart after the
// Manifests are applied again: several times the settle time of the
// suite's overlay (config/manager-inventory-timing.yaml).
const quietPeriod = 30 * time.Second

// TestInventoryThroughHostAdmin: battery holds the kind workers with mutual
// TLS; a cordoned worker's Host is drained in battery, so that its warm
// MicroVM goes and the Host is kept, and it is uncordoned with the Node
// and filled again; and applying the Manifests again changes nothing
// battery runs with, and restarts nothing.
func TestInventoryThroughHostAdmin(t *testing.T) {
	ns := envconf.RandomName("e2e-inventory", 20)
	testenv.Test(t, features.New("battery's Hosts through its HostAdmin API").
		Setup(createNamespace(ns)).
		Assess("battery holds every Host, with mutual TLS", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/04-inventory.md#applying
			//= type=test
			//# While a Node is a Host, the Inventory Controller SHALL keep a
			//# Host of the Node's name in battery, uncordoned, at the Node's `flintlockd`
			//# address and with the TLS settings of IN-004, adding it with `AddHost`,
			//# and changing its address or TLS settings with `UpdateHost` and uncordoning
			//# it with `UncordonHost` wherever battery's Host differs.
			c := mustClient(t, cfg)
			want := readyHosts(ctx, t, c)
			held := waitForBatteryHosts(ctx, t, cfg, want)
			for _, h := range held {
				if h.Host.TLS != inventory.TLS {
					t.Errorf("battery reaches %s with %+v, want %+v", h.Host.Name, h.Host.TLS, inventory.TLS)
				}
			}
			return ctx
		}).
		Assess("a cordoned Node's Host is drained in battery, and back with the Node", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/04-inventory.md#admission
			//= type=test
			//# When a Node that is a Host is cordoned, or carries the
			//# `ToBeDeletedByClusterAutoscaler` taint, the Inventory Controller SHALL
			//# drain the Host in battery with `DrainHost`, and SHALL keep it in
			//# battery's Hosts.
			c := mustClient(t, cfg)
			want := readyHosts(ctx, t, c)
			nodes, err := hostNodes(ctx, c)
			if err != nil || len(nodes) == 0 {
				t.Fatalf("listing the Hosts: %v", err)
			}
			name := nodes[0].Name
			restarts := batteryRestarts(ctx, t, c)

			// A warm MicroVM on the Node's Host alone: a Pool of one placed
			// on it by its hostname label.
			pool := placedPool(ns, "on-"+name, map[string]string{corev1.LabelHostname: name})
			pool.Spec.Size = 1
			if err := c.Create(ctx, pool); err != nil {
				t.Fatalf("creating the Pool %s: %v", pool.Name, err)
			}
			t.Cleanup(func() { _ = c.Delete(context.Background(), pool) })
			waitForPoolReady(ctx, t, c, pool, metav1.ConditionTrue, poolReasonAtSize, nil)
			waitForBatteryHost(ctx, t, cfg, name, func(h *battery.HostStatus) bool { return h.VMCount == 1 })

			setUnschedulable(ctx, t, c, name, true)
			t.Cleanup(func() { setUnschedulable(context.Background(), t, c, name, false) })

			// The drain cordons the Host, so it leaves the Hosts battery
			// places on, and deletes its warm MicroVM; the Pool, which
			// names no other Host, stays short meanwhile.
			cordoned := maps.Clone(want)
			delete(cordoned, name)
			held := waitForBatteryHosts(ctx, t, cfg, cordoned)
			if _, ok := held[name]; !ok {
				t.Fatalf("battery no longer holds %s", name)
			}
			waitForBatteryHost(ctx, t, cfg, name, func(h *battery.HostStatus) bool {
				return h.Host.Draining && h.VMCount == 0
			})

			setUnschedulable(ctx, t, c, name, false)
			waitForBatteryHosts(ctx, t, cfg, want)
			waitForBatteryHost(ctx, t, cfg, name, func(h *battery.HostStatus) bool {
				return !h.Host.Draining && h.VMCount == 1
			})
			waitForPoolReady(ctx, t, c, pool, metav1.ConditionTrue, poolReasonAtSize, nil)
			if got := batteryRestarts(ctx, t, c); got != restarts {
				t.Errorf("battery restarted %d times, want none", got-restarts)
			}
			if err := c.Delete(ctx, pool); err != nil {
				t.Fatalf("deleting the Pool %s: %v", pool.Name, err)
			}
			waitForGone(ctx, t, c, pool)
			return ctx
		}).
		Assess("applying the Manifests again changes nothing battery runs with", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/06-deployment.md#battery-sidecar
			//= type=test
			//# The Manifests SHALL supply battery's configuration from a
			//# ConfigMap that names no Hosts.
			c := mustClient(t, cfg)
			want := readyHosts(ctx, t, c)
			waitForBatteryHosts(ctx, t, cfg, want)
			restarts := batteryRestarts(ctx, t, c)
			if err := applyManifests(ctx, cfg, settingsFromEnv()); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(quietPeriod)
			for time.Now().Before(deadline) {
				if got := batteryRestarts(ctx, t, c); got != restarts {
					t.Fatalf("battery restarted %d times after the Manifests were applied again, want none", got-restarts)
				}
				time.Sleep(2 * time.Second)
			}
			if got, err := batteryHosts(ctx, cfg); err != nil || !maps.Equal(got, want) {
				t.Fatalf("battery places on %v (%v), want %v", got, err, want)
			}
			return ctx
		}).
		Feature())
}

// setUnschedulable cordons or uncordons a Node, as kubectl cordon does.
func setUnschedulable(ctx context.Context, t *testing.T, c client.Client, name string, unschedulable bool) {
	t.Helper()
	n := &corev1.Node{}
	if err := c.Get(ctx, client.ObjectKey{Name: name}, n); err != nil {
		t.Fatal(err)
	}
	patch := client.MergeFrom(n.DeepCopy())
	n.Spec.Unschedulable = unschedulable
	if err := c.Patch(ctx, n, patch); err != nil {
		t.Fatalf("setting %s unschedulable=%v: %v", name, unschedulable, err)
	}
}

// batteryHostStatuses reads battery's Hosts through its HostAdmin API.
// battery listens on loopback in the Operator's pod (DP-001), so the call
// goes over a port-forward to the pod, which reaches that loopback, and
// the battery client's loopback check passes on this side.
func batteryHostStatuses(ctx context.Context, cfg *envconf.Config) ([]*battery.HostStatus, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	_, remote, err := net.SplitHostPort(batterysidecar.APIAddress)
	if err != nil {
		return nil, err
	}

	pfCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	pf := exec.CommandContext(pfCtx, settingsFromEnv().kubectl, "--kubeconfig", cfg.KubeconfigFile(),
		"--namespace", namespace, "port-forward", "deployment/"+operatorDeployment,
		strconv.Itoa(port)+":"+remote)
	if err := pf.Start(); err != nil {
		return nil, fmt.Errorf("port-forward: %w", err)
	}
	defer func() { cancel(); _ = pf.Wait() }()

	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 15*time.Second, true, func(context.Context) (bool, error) {
		conn, err := net.DialTimeout("tcp", address, time.Second)
		if err != nil {
			return false, nil
		}
		_ = conn.Close()
		return true, nil
	}); err != nil {
		return nil, fmt.Errorf("port-forward to battery did not open: %w", err)
	}
	conn, err := battery.Dial(battery.Config{Address: address})
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	return conn.ListHosts(ctx)
}

// batteryHosts are the Hosts battery places on, held and uncordoned, by
// name, with their addresses.
func batteryHosts(ctx context.Context, cfg *envconf.Config) (map[string]string, error) {
	held, err := batteryHostStatuses(ctx, cfg)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, h := range held {
		if !h.Host.Cordoned {
			out[h.Host.Name] = h.Host.Address
		}
	}
	return out, nil
}

// waitForBatteryHosts waits until battery places on want, and returns every
// Host battery holds, by name.
// waitForBatteryHost waits until battery holds the Host called name and
// ok holds of it.
func waitForBatteryHost(ctx context.Context, t *testing.T, cfg *envconf.Config, name string, ok func(*battery.HostStatus) bool) {
	t.Helper()
	var got *battery.HostStatus
	var lastErr error
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, placementTimeout, true, func(ctx context.Context) (bool, error) {
		statuses, err := batteryHostStatuses(ctx, cfg)
		if err != nil {
			lastErr = err
			return false, nil
		}
		got = nil
		for _, h := range statuses {
			if h.Host.Name == name {
				got = h
			}
		}
		return got != nil && ok(got), nil
	})
	if err != nil {
		t.Fatalf("waiting for battery's Host %s: %v (last error %v); it is %+v", name, err, lastErr, got)
	}
}

func waitForBatteryHosts(ctx context.Context, t *testing.T, cfg *envconf.Config, want map[string]string) map[string]*battery.HostStatus {
	t.Helper()
	var got map[string]string
	var held map[string]*battery.HostStatus
	var lastErr error
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, placementTimeout, true, func(ctx context.Context) (bool, error) {
		statuses, err := batteryHostStatuses(ctx, cfg)
		if err != nil {
			lastErr = err
			return false, nil
		}
		got, held = map[string]string{}, map[string]*battery.HostStatus{}
		for _, h := range statuses {
			held[h.Host.Name] = h
			if !h.Host.Cordoned {
				got[h.Host.Name] = h.Host.Address
			}
		}
		return maps.Equal(got, want), nil
	})
	if err != nil {
		t.Fatalf("battery places on %v, want %v (last error %v)", got, want, lastErr)
	}
	return held
}

// batteryRestarts is how many times battery's container has restarted in
// the Operator's pod.
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
