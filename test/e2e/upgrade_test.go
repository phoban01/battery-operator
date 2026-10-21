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
	"encoding/json"
	"maps"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/batterysidecar"
	"github.com/phoban01/battery-operator/internal/controller/inventory"
)

// The battery upgrade the feature takes the Operator through.
const (
	// previousPoolmgrdImage is the last battery that read its Hosts from
	// its configuration file, and kept them in its database without TLS
	// settings (BA-086).
	previousPoolmgrdImage = poolmgrdRepository + ":0.4.0"
	// placeholderHost is the Host an Operator before v0.5 gave battery
	// v0.4.0 when no Node was a Host, since battery v0.4.0 refused to start
	// with none.
	placeholderHost = "_no-hosts"
	// rolloutTimeout bounds a restart of the Operator's pod: the old pod's
	// termination, the pull of poolmgrd and the new pod's readiness.
	rolloutTimeout = 5 * time.Minute
)

// upgradeKey is the context key of the poolmgrd image the suite deployed,
// which the feature puts back.
type upgradeKey struct{}

// TestBatteryUpgrade: battery v0.4.0 runs as the Operator's sidecar with
// its Hosts in its configuration file, as an Operator before v0.5 left it,
// fills a Pool and binds a claim; the configuration goes back to the one
// the Manifests ship, and the sidecar is upgraded to the suite's poolmgrd,
// which migrates battery's database and keeps the Hosts in it without TLS
// settings; the Inventory Controller gives them their TLS settings and
// removes the placeholder Host; the claim keeps its Lease and renews it,
// and a new claim binds through the migrated database.
func TestBatteryUpgrade(t *testing.T) {
	ns := envconf.RandomName("e2e-upgrade", 20)
	testenv.Test(t, upgradeFeature(ns))
}

func upgradeFeature(ns string) features.Feature {
	const oldClaim, newClaim = "before-upgrade", "after-upgrade"
	return features.New("an upgrade of battery with a claim Bound").
		Setup(createNamespace(ns)).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: holderName}}
			if err := mustClient(t, cfg).Create(ctx, sa); err != nil && !apierrors.IsAlreadyExists(err) {
				t.Fatalf("creating the Holder %s: %v", holderName, err)
			}
			return ctx
		}).
		Assess("the Operator is set back to battery v0.4.0, with its Hosts in its file", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c := mustClient(t, cfg)
			current := batteryImage(ctx, t, c)
			if current == previousPoolmgrdImage {
				t.Fatalf("the suite deployed %s, which is the version the upgrade starts from", current)
			}
			hosts := readyHosts(ctx, t, c)
			setBatteryConfig(ctx, t, c, v04Config(t, hosts))
			setBatteryImage(ctx, t, c, previousPoolmgrdImage)
			// battery v0.4.0 has no AddHost or UpdateHost, so the Inventory
			// Controller fails to bring its Hosts in step, but publishes the
			// ones battery places on, and cordons the placeholder.
			waitForBatteryHosts(ctx, t, cfg, hosts)
			return context.WithValue(ctx, upgradeKey{}, current)
		}).
		Assess("a Pool fills and a claim binds on battery v0.4.0", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c := mustClient(t, cfg)
			pool := placedPool(ns, placedPoolName, map[string]string{hostLabel: isTrue})
			if err := c.Create(ctx, pool); err != nil {
				t.Fatalf("creating the Pool %s: %v", pool.Name, err)
			}
			waitForPoolReady(ctx, t, c, pool, metav1.ConditionTrue, poolReasonAtSize, func(p *batteryv1alpha1.Pool) bool {
				return p.Status.Available == poolSize
			})
			claim := newClaimOn(ns, oldClaim)
			if err := c.Create(ctx, claim); err != nil {
				t.Fatalf("creating the claim %s: %v", claim.Name, err)
			}
			waitForClaimBound(ctx, t, c, claim)
			return ctx
		}).
		Assess("the Operator is upgraded to the suite's battery, which keeps its Hosts", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/10-battery.md#hosts
			//= type=test
			//# When battery starts on a database that battery v0.4.0 wrote,
			//# battery SHALL keep the Hosts in it, with no TLS settings, and SHALL read no
			//# Host from its configuration file.

			//= docs/requirements/04-inventory.md#upgrade
			//= type=test
			//# When battery holds a Host whose TLS settings are not those of
			//# IN-004, the Inventory Controller SHALL update the Host with `UpdateHost` to
			//# those settings, keeping its address.

			//= docs/requirements/06-deployment.md#battery-sidecar
			//= type=test
			//# The Manifests SHALL keep battery's database on a persistent
			//# volume.
			c := mustClient(t, cfg)
			image, _ := ctx.Value(upgradeKey{}).(string)
			if image == "" {
				t.Fatal("the image the suite deployed was not recorded")
			}
			shipped, err := batterysidecar.Render()
			if err != nil {
				t.Fatal(err)
			}
			setBatteryConfig(ctx, t, c, string(shipped))
			setBatteryImage(ctx, t, c, image)

			// The configuration names no Host, so every Host battery holds
			// came through the migrated database; each gets its TLS
			// settings, and the placeholder goes.
			want := readyHosts(ctx, t, c)
			var held map[string]bool
			err = wait.PollUntilContextTimeout(ctx, 2*time.Second, placementTimeout, true, func(ctx context.Context) (bool, error) {
				statuses, err := batteryHostStatuses(ctx, cfg)
				if err != nil {
					return false, nil
				}
				held = map[string]bool{}
				got := map[string]string{}
				for _, h := range statuses {
					held[h.Host.Name] = true
					if !h.Host.Cordoned && h.Host.TLS == inventory.TLS {
						got[h.Host.Name] = h.Host.Address
					}
				}
				return maps.Equal(got, want) && !held[placeholderHost], nil
			})
			if err != nil {
				t.Fatalf("after the upgrade battery holds %v, want %v with TLS and no placeholder", held, want)
			}
			return ctx
		}).
		Assess("the claim keeps its Lease across the upgrade, and renews it", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/02-claims.md#recovery
			//= type=test
			//# When the Claim Controller starts, and when its connection to
			//# battery is restored, the Claim Controller SHALL reconcile every Bound
			//# claim that is not being deleted against battery's Leases.
			c := mustClient(t, cfg)
			claim := newClaimOn(ns, oldClaim)
			before := waitForClaimBound(ctx, t, c, claim)

			// The recovery reads the Lease from the migrated database: the
			// claim stays Bound on the same Lease, Synced, with an expiry
			// still to come.
			waitForClaimStatus(ctx, t, c, claim, "Bound on its Lease after the upgrade", func(st batteryv1alpha1.MicroVMClaimStatus) bool {
				return st.Phase == batteryv1alpha1.MicroVMClaimBound &&
					st.LeaseID == before.Status.LeaseID &&
					meta.IsStatusConditionTrue(st.Conditions, batteryv1alpha1.ConditionSynced) &&
					st.LeaseExpiresAt != nil && st.LeaseExpiresAt.After(time.Now())
			})

			// A renewal is relayed as a Heartbeat for the migrated Lease.
			renew := metav1.NewMicroTime(time.Now().Truncate(time.Microsecond))
			patch := client.MergeFrom(before.DeepCopy())
			before.Spec.RenewTime = &renew
			if err := c.Patch(ctx, before, patch); err != nil {
				t.Fatalf("renewing the claim %s: %v", claim.Name, err)
			}
			waitForClaimStatus(ctx, t, c, claim, "renewed after the upgrade", func(st batteryv1alpha1.MicroVMClaimStatus) bool {
				return st.Phase == batteryv1alpha1.MicroVMClaimBound &&
					st.ObservedRenewTime != nil && st.ObservedRenewTime.Equal(&renew) &&
					st.LeaseExpiresAt != nil && st.LeaseExpiresAt.After(renew.Time)
			})
			return ctx
		}).
		Assess("a new claim binds through the migrated database", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/02-claims.md#claim-retry
			//= type=test
			//# The Claim Controller SHALL send with every `ClaimVM` for a
			//# claim a request id equal to the claim's `metadata.uid`.
			//
			// battery v0.4.0 stores the request id in the column its
			// migration added, so a bind here is a write through the
			// migrated schema.
			c := mustClient(t, cfg)
			claim := newClaimOn(ns, newClaim)
			if err := c.Create(ctx, claim); err != nil {
				t.Fatalf("creating the claim %s: %v", claim.Name, err)
			}
			got := waitForClaimBound(ctx, t, c, claim)
			old := &batteryv1alpha1.MicroVMClaim{}
			if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: oldClaim}, old); err != nil {
				t.Fatal(err)
			}
			if got.Status.LeaseID == old.Status.LeaseID || got.Status.MicroVM.UID == old.Status.MicroVM.UID {
				t.Fatalf("the new claim got the old claim's Lease or MicroVM: %+v", got.Status)
			}
			return ctx
		}).
		Teardown(deleteNamespace(ns)).
		Teardown(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			// Whatever happened above, leave the suite's battery running, with
			// the configuration the Manifests ship.
			image, _ := ctx.Value(upgradeKey{}).(string)
			if image == "" {
				return ctx
			}
			c := mustClient(t, cfg)
			if batteryImage(ctx, t, c) != image {
				shipped, err := batterysidecar.Render()
				if err != nil {
					t.Fatal(err)
				}
				setBatteryConfig(ctx, t, c, string(shipped))
				setBatteryImage(ctx, t, c, image)
			}
			return ctx
		}).
		Feature()
}

// newClaimOn is a claim of name on the feature's Pool.
func newClaimOn(ns, name string) *batteryv1alpha1.MicroVMClaim {
	return &batteryv1alpha1.MicroVMClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: batteryv1alpha1.MicroVMClaimSpec{
			PoolRef:            batteryv1alpha1.PoolReference{Name: placedPoolName},
			ServiceAccountName: holderName,
		},
	}
}

// batteryImage is the image of battery's container in the Operator's
// Deployment.
func batteryImage(ctx context.Context, t *testing.T, c client.Client) string {
	t.Helper()
	var d appsv1.Deployment
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: operatorDeployment}, &d); err != nil {
		t.Fatalf("reading the Deployment %s: %v", operatorDeployment, err)
	}
	for _, ct := range d.Spec.Template.Spec.Containers {
		if ct.Name == batteryContainer {
			return ct.Image
		}
	}
	t.Fatalf("the Deployment %s has no container %q", operatorDeployment, batteryContainer)
	return ""
}

// setBatteryImage sets battery's container to image and waits for the
// Operator's pod to be replaced and ready with it. The Deployment's
// strategy is Recreate (DP-002), so battery's database has one writer
// throughout.
func setBatteryImage(ctx context.Context, t *testing.T, c client.Client, image string) {
	t.Helper()
	var d appsv1.Deployment
	key := types.NamespacedName{Namespace: namespace, Name: operatorDeployment}
	if err := c.Get(ctx, key, &d); err != nil {
		t.Fatalf("reading the Deployment %s: %v", operatorDeployment, err)
	}
	patch := client.MergeFrom(d.DeepCopy())
	for i := range d.Spec.Template.Spec.Containers {
		if d.Spec.Template.Spec.Containers[i].Name == batteryContainer {
			d.Spec.Template.Spec.Containers[i].Image = image
		}
	}
	if err := c.Patch(ctx, &d, patch); err != nil {
		t.Fatalf("setting battery's image to %s: %v", image, err)
	}
	generation := d.Generation

	var pod *corev1.Pod
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, rolloutTimeout, true, func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, key, &d); err != nil {
			return false, nil
		}
		if d.Status.ObservedGeneration < generation || d.Status.UpdatedReplicas < 1 || d.Status.AvailableReplicas < 1 {
			return false, nil
		}
		pod = batteryPod(ctx, c, image)
		return pod != nil, nil
	})
	if err != nil {
		t.Fatalf("waiting for the Operator to run battery %s: %v; the Deployment's status is %s", image, err, describe(d.Status))
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == batteryContainer && cs.RestartCount > 0 {
			t.Fatalf("battery %s restarted %d times in %s; its last state is %s", image, cs.RestartCount, pod.Name, describe(cs.LastTerminationState))
		}
	}
}

// batteryPod is the Operator's ready pod whose battery container runs
// image, or nil.
func batteryPod(ctx context.Context, c client.Client, image string) *corev1.Pod {
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(namespace)); err != nil {
		return nil
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if !p.DeletionTimestamp.IsZero() || p.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Name == batteryContainer && cs.Image == image && cs.Ready {
				return p
			}
		}
	}
	return nil
}

// setBatteryConfig sets battery's configuration file in its ConfigMap. The
// battery container reads it when it next starts, which setBatteryImage
// brings.
func setBatteryConfig(ctx context.Context, t *testing.T, c client.Client, config string) {
	t.Helper()
	var cm corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: batteryConfigMap}, &cm); err != nil {
		t.Fatalf("reading battery's ConfigMap: %v", err)
	}
	patch := client.MergeFrom(cm.DeepCopy())
	cm.Data[batterysidecar.ConfigKey] = config
	if err := c.Patch(ctx, &cm, patch); err != nil {
		t.Fatalf("writing battery's configuration: %v", err)
	}
}

// v04Config is battery's configuration as an Operator before v0.5 wrote it
// for battery v0.4.0: hosts, each with the TLS settings of IN-004, and the
// placeholder Host.
func v04Config(t *testing.T, hosts map[string]string) string {
	t.Helper()
	type tlsConfig struct {
		CertFile string `json:"cert_file"`
		KeyFile  string `json:"key_file"`
		CAFile   string `json:"ca_file"`
	}
	type hostConfig struct {
		Name    string    `json:"name"`
		Address string    `json:"address"`
		TLS     tlsConfig `json:"tls"`
	}
	tls := tlsConfig{CertFile: inventory.TLS.CertFile, KeyFile: inventory.TLS.KeyFile, CAFile: inventory.TLS.CAFile}
	list := make([]hostConfig, 0, 1+len(hosts))
	list = append(list, hostConfig{Name: placeholderHost, Address: "no-hosts.invalid:9090", TLS: tls})
	for name, address := range hosts {
		list = append(list, hostConfig{Name: name, Address: address, TLS: tls})
	}
	out, err := json.MarshalIndent(map[string]any{
		"hosts":        list,
		"api_server":   map[string]any{"addr": batterysidecar.APIAddress, "tls": map[string]bool{"insecure": true}},
		"metrics_addr": batterysidecar.MetricsAddress,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(out) + "\n"
}

// waitForClaimStatus waits until want holds of claim's status.
func waitForClaimStatus(ctx context.Context, t *testing.T, c client.Client, claim *batteryv1alpha1.MicroVMClaim, what string, want func(batteryv1alpha1.MicroVMClaimStatus) bool) {
	t.Helper()
	got := &batteryv1alpha1.MicroVMClaim{}
	err := wait.PollUntilContextTimeout(ctx, time.Second, placementTimeout, true, func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, client.ObjectKeyFromObject(claim), got); err != nil {
			return false, nil
		}
		return want(got.Status), nil
	})
	if err != nil {
		t.Fatalf("waiting for the claim %s to be %s: %v; its status is %s", claim.Name, what, err, describe(got.Status))
	}
}
