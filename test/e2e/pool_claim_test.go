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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/batterysidecar"
	"github.com/phoban01/battery-operator/internal/execagent"
)

// What the placement features create.
const (
	// holderName is the ServiceAccount the claims name as their Holder.
	holderName = "runner"
	// placedPoolName is the Pool on the Hosts, which the claims are made
	// from.
	placedPoolName = "placed"
	// poolSize is the size of the Pools the features make.
	poolSize = 2
	// placementTimeout bounds each wait on the Operator and battery: the
	// Inventory Controller's settle time and window (config/
	// manager-inventory-timing.yaml), a restart of battery, which waits for
	// the kubelet to update battery's ConfigMap volume, and poolmgrd filling
	// the Pool.
	placementTimeout = 5 * time.Minute
)

// Reasons of the Pool's Ready condition, as the Pool Controller sets them
// (internal/controller/pool_status.go, pool_deletion.go).
const (
	poolReasonAtSize          = "AtSize"
	poolReasonNoEligibleHost  = "NoEligibleHost"
	poolReasonDeletionBlocked = "DeletionBlocked"
)

// TestPoolPlacementAndClaim: through battery's real poolmgrd, the Inventory
// Controller gives battery the kind workers once their Exec Agents report
// them ready, a Pool whose selector matches them becomes Ready at its size
// with MicroVMs on the fake flintlockd, and a claim against it binds to one
// of them and is released.
func TestPoolPlacementAndClaim(t *testing.T) {
	//= docs/requirements/08-test-doubles.md#test-environments
	//= type=test
	//# The e2e suite SHALL run the Operator with battery's `poolmgrd`
	//# as its sidecar, the Exec Agent, and the fake `flintlockd` as each Host's
	//# `flintlockd`, in a kind cluster, from the Manifests.
	ns := envconf.RandomName("e2e-placement", 20)
	testenv.Test(t, placementFeature(ns))
}

func placementFeature(ns string) features.Feature {
	claimName := "bound"
	return features.New("Pool placement and a claim").
		Setup(createNamespace(ns)).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: holderName}}
			if err := mustClient(t, cfg).Create(ctx, sa); err != nil && !apierrors.IsAlreadyExists(err) {
				t.Fatalf("creating the Holder %s: %v", holderName, err)
			}
			return ctx
		}).
		Assess("the Inventory Controller gives battery every Host its Exec Agent reports ready", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c := mustClient(t, cfg)
			//= docs/requirements/04-inventory.md#admission
			//= type=test
			//# The Inventory Controller SHALL give battery each Host under the
			//# Node's name, at the `flintlockd` address the Host's Node report gives.
			want := readyHosts(ctx, t, c)
			var got map[string]string
			err := wait.PollUntilContextTimeout(ctx, 2*time.Second, placementTimeout, true, func(ctx context.Context) (bool, error) {
				var err error
				if got, err = batteryHosts(ctx, c); err != nil {
					return false, nil
				}
				return maps.Equal(got, want), nil
			})
			if err != nil {
				t.Fatalf("battery's Hosts are %v, want %v, the Hosts' Node reports", got, want)
			}
			return ctx
		}).
		Assess("a Pool whose selector matches no Host says so", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/03-pools.md#placement
			//= type=test
			//# While a Pool's selector matches no Host, the Pool Controller
			//# SHALL set the Pool's condition `Ready` false with the reason
			//# `NoEligibleHost`.
			c := mustClient(t, cfg)
			pool := placedPool(ns, "nowhere", map[string]string{hostLabel: "false"})
			if err := c.Create(ctx, pool); err != nil {
				t.Fatalf("creating the Pool %s: %v", pool.Name, err)
			}
			waitForPoolReady(ctx, t, c, pool, metav1.ConditionFalse, poolReasonNoEligibleHost, nil)
			return ctx
		}).
		Assess("a Pool on the Hosts becomes Ready at its size", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/03-pools.md#placement
			//= type=test
			//# The Pool Controller SHALL set a Pool's `flintlock_hosts` in
			//# battery to the names of the Hosts whose Nodes match the Pool's
			//# `spec.placement.nodeSelector`.
			//
			// poolmgrd fills a Pool only on the Hosts it names, so the Pool
			// reaching its size shows it names the kind workers.
			c := mustClient(t, cfg)
			pool := placedPool(ns, placedPoolName, map[string]string{hostLabel: isTrue})
			// Nothing renews the claim below, which has to outlast the
			// Pool's deletion waiting for it.
			pool.Spec.Lease.ExpiryThreshold = &metav1.Duration{Duration: placementTimeout}
			if err := c.Create(ctx, pool); err != nil {
				t.Fatalf("creating the Pool %s: %v", pool.Name, err)
			}

			//= docs/requirements/03-pools.md#pool-status
			//= type=test
			//# The Pool Controller SHALL set a Pool's condition `Ready` true
			//# while battery holds the Pool, its selector matches a Host, and the sum of
			//# its available, leased and provisioning MicroVMs is at least its size.

			//= docs/requirements/03-pools.md#pool-status
			//= type=test
			//# The Pool Controller SHALL take a Pool's counts of available,
			//# leased, provisioning and quarantined MicroVMs from battery's `PoolStatus`
			//# for the Pool and from nothing else.

			//= docs/requirements/03-pools.md#pool-status
			//= type=test
			//# The Pool Controller SHALL set a Pool's condition `Exhausted`
			//# true while battery reports no available MicroVM in a Pool whose size is
			//# greater than zero, and false otherwise.
			waitForPoolReady(ctx, t, c, pool, metav1.ConditionTrue, poolReasonAtSize, func(p *batteryv1alpha1.Pool) bool {
				return p.Status.Available == poolSize &&
					meta.IsStatusConditionFalse(p.Status.Conditions, batteryv1alpha1.PoolConditionExhausted)
			})
			return ctx
		}).
		Assess("a claim on the Pool binds to one of its MicroVMs", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c := mustClient(t, cfg)
			claim := &batteryv1alpha1.MicroVMClaim{
				ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: claimName},
				Spec: batteryv1alpha1.MicroVMClaimSpec{
					PoolRef:            batteryv1alpha1.PoolReference{Name: placedPoolName},
					ServiceAccountName: holderName,
				},
			}
			if err := c.Create(ctx, claim); err != nil {
				t.Fatalf("creating the claim %s: %v", claimName, err)
			}

			//= docs/requirements/02-claims.md#binding
			//= type=test
			//# When battery's `ClaimVM` succeeds for a claim, the Claim
			//# Controller SHALL write the lease id, the MicroVM's uid, the Host's name as
			//# the node name and the time of binding to the claim's status, set the phase
			//# to `Bound` and set the condition `Bound` true, before it makes any other
			//# call to battery for that claim.

			//= docs/requirements/02-claims.md#binding
			//= type=test
			//# When a claim is Bound, the Claim Controller SHALL set the claim's
			//# Exec Agent address from the Node report of the claim's Host.
			got := waitForClaimBound(ctx, t, c, claim)
			st := got.Status
			if st.LeaseID == "" || st.MicroVM == nil || st.MicroVM.UID == "" || st.BoundTime == nil {
				t.Fatalf("the Bound claim's status names no lease id, MicroVM uid or binding time: %+v", st)
			}
			hosts := readyHosts(ctx, t, c)
			if _, ok := hosts[st.Host.NodeName]; !ok {
				t.Fatalf("the claim is bound on %q, which is none of the Hosts %v", st.Host.NodeName, hosts)
			}
			var node corev1.Node
			if err := c.Get(ctx, client.ObjectKey{Name: st.Host.NodeName}, &node); err != nil {
				t.Fatal(err)
			}
			if want := node.Annotations[execagent.AnnotationAddress]; want == "" || st.Host.AgentAddress != want {
				t.Fatalf("the claim's agent address is %q, want %q from the Node report of %s",
					st.Host.AgentAddress, want, node.Name)
			}
			return ctx
		}).
		Assess("a deleted Pool whose MicroVM a claim holds waits for the claim", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/03-pools.md#deletion
			//= type=test
			//# While battery refuses `DeletePool` for a deleted Pool that has
			//# leased or quarantined MicroVMs, the Pool Controller SHALL set the Pool's
			//# condition `Ready` false with the reason `DeletionBlocked` and a message
			//# that gives those counts.

			//= docs/requirements/03-pools.md#deletion
			//= type=test
			//# When battery refuses `DeletePool` for a deleted Pool whose
			//# spec in battery is its drained spec, the Pool Controller SHALL claim each
			//# available MicroVM of the Pool with `ClaimVM`, release it at once with
			//# `ReleaseVM`, and then call `DeletePool` again.
			//
			// poolmgrd refuses DeletePool while the Pool has any MicroVM
			// (BA-070): the Pool Controller drains the available ones and
			// leaves the claim's.
			c := mustClient(t, cfg)
			pool := &batteryv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: placedPoolName}}
			if err := c.Delete(ctx, pool); err != nil {
				t.Fatalf("deleting the Pool %s: %v", placedPoolName, err)
			}
			waitForPoolReady(ctx, t, c, pool, metav1.ConditionFalse, poolReasonDeletionBlocked, func(p *batteryv1alpha1.Pool) bool {
				return p.Status.Available == 0 && p.Status.Provisioning == 0 && p.Status.Leased == 1
			})
			claim := &batteryv1alpha1.MicroVMClaim{}
			if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: claimName}, claim); err != nil {
				t.Fatal(err)
			}
			if claim.Status.Phase != batteryv1alpha1.MicroVMClaimBound {
				t.Fatalf("the claim is %s while its Pool waits for it, want Bound", claim.Status.Phase)
			}
			return ctx
		}).
		Assess("a deleted claim releases its Lease and goes", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/02-claims.md#release
			//= type=test
			//# When a claim that has a lease id is deleted, the Claim
			//# Controller SHALL call battery's `ReleaseVM` for the Lease, and SHALL remove
			//# the finalizer only once battery has released the Lease or reported it
			//# unknown.
			c := mustClient(t, cfg)
			claim := &batteryv1alpha1.MicroVMClaim{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: claimName}}
			if err := c.Delete(ctx, claim); err != nil {
				t.Fatalf("deleting the claim %s: %v", claimName, err)
			}
			waitForGone(ctx, t, c, claim)
			return ctx
		}).
		Assess("the deleted Pool goes once the claim has", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/03-pools.md#declaration
			//= type=test
			//# The Pool Controller SHALL add a finalizer to each Pool, and when
			//# the Pool is deleted SHALL call `DeletePool`, drain the Pool while battery
			//# refuses it (PO-030, PO-031), and remove the finalizer only once battery
			//# has deleted the Pool or reported it unknown.
			c := mustClient(t, cfg)
			waitForGone(ctx, t, c, &batteryv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: placedPoolName}})
			return ctx
		}).
		Teardown(deleteNamespace(ns)).
		Feature()
}

// placedPool is a Pool of poolSize MicroVMs on the Hosts selector matches,
// renewed every two seconds.
func placedPool(ns, name string, selector map[string]string) *batteryv1alpha1.Pool {
	pool := validPool(ns, name)
	pool.Spec.Size = poolSize
	pool.Spec.Placement.NodeSelector = selector
	pool.Spec.Lease.HeartbeatInterval = &metav1.Duration{Duration: 2 * time.Second}
	return pool
}

// readyHosts waits for the Exec Agent of every Host to report it ready,
// with a flintlockd address, and returns the Hosts by Node name, with the
// flintlockd address their Node reports give.
func readyHosts(ctx context.Context, t *testing.T, c client.Client) map[string]string {
	t.Helper()
	var hosts map[string]string
	var notReady *corev1.Node
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, placementTimeout, true, func(ctx context.Context) (bool, error) {
		nodes, err := hostNodes(ctx, c)
		if err != nil {
			return false, nil
		}
		hosts, notReady = map[string]string{}, nil
		for i, n := range nodes {
			addr := n.Annotations[execagent.AnnotationFlintlockdAddress]
			if n.Annotations[execagent.AnnotationReady] != isTrue || addr == "" {
				notReady = &nodes[i]
				return false, nil
			}
			hosts[n.Name] = addr
		}
		return true, nil
	})
	if err != nil {
		if notReady == nil {
			t.Fatalf("listing the Hosts: %v", err)
		}
		t.Fatalf("the Exec Agent does not report %s ready: %v", notReady.Name, notReady.Annotations)
	}
	return hosts
}

// batteryHosts are the Hosts in battery's configuration, by name, with
// their addresses.
func batteryHosts(ctx context.Context, c client.Client) (map[string]string, error) {
	var cm corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: batteryConfigMap}, &cm); err != nil {
		return nil, err
	}
	f, err := batterysidecar.Parse([]byte(cm.Data[batterysidecar.ConfigKey]))
	if err != nil {
		return nil, err
	}
	hosts := map[string]string{}
	for _, h := range f.Hosts {
		hosts[h.Name] = h.Address
	}
	return hosts, nil
}

// waitForPoolReady waits for pool's Ready condition to have status and
// reason, and for more to hold of it when more is not nil.
func waitForPoolReady(
	ctx context.Context, t *testing.T, c client.Client, pool *batteryv1alpha1.Pool,
	status metav1.ConditionStatus, reason string, more func(*batteryv1alpha1.Pool) bool,
) {
	t.Helper()
	got := &batteryv1alpha1.Pool{}
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, placementTimeout, true, func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, client.ObjectKeyFromObject(pool), got); err != nil {
			return false, nil
		}
		cond := meta.FindStatusCondition(got.Status.Conditions, batteryv1alpha1.PoolConditionReady)
		return cond != nil && cond.Status == status && cond.Reason == reason &&
			cond.ObservedGeneration == got.Generation && (more == nil || more(got)), nil
	})
	if err != nil {
		t.Fatalf("waiting for the Pool %s to be Ready=%s (%s): %v; its status is %s",
			pool.Name, status, reason, err, describe(got.Status))
	}
}

// waitForClaimBound waits for claim to be Bound, with the Host's node name
// and the Exec Agent's address, and returns it.
func waitForClaimBound(ctx context.Context, t *testing.T, c client.Client, claim *batteryv1alpha1.MicroVMClaim) *batteryv1alpha1.MicroVMClaim {
	t.Helper()
	got := &batteryv1alpha1.MicroVMClaim{}
	err := wait.PollUntilContextTimeout(ctx, time.Second, placementTimeout, true, func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, client.ObjectKeyFromObject(claim), got); err != nil {
			return false, nil
		}
		st := got.Status
		return st.Phase == batteryv1alpha1.MicroVMClaimBound &&
			meta.IsStatusConditionTrue(st.Conditions, batteryv1alpha1.ConditionBound) &&
			st.Host != nil && st.Host.NodeName != "" && st.Host.AgentAddress != "", nil
	})
	if err != nil {
		t.Fatalf("waiting for the claim %s to be Bound: %v; its status is %s", claim.Name, err, describe(got.Status))
	}
	return got
}

// waitForGone waits until obj no longer exists.
func waitForGone(ctx context.Context, t *testing.T, c client.Client, obj client.Object) {
	t.Helper()
	got := obj.DeepCopyObject().(client.Object)
	err := wait.PollUntilContextTimeout(ctx, time.Second, placementTimeout, true, func(ctx context.Context) (bool, error) {
		err := c.Get(ctx, client.ObjectKeyFromObject(obj), got)
		return apierrors.IsNotFound(err), nil
	})
	if err != nil {
		t.Fatalf("waiting for %s to be gone: %v; it has finalizers %v", obj.GetName(), err, got.GetFinalizers())
	}
}

// describe formats a status for a failure message.
func describe(status any) string { return fmt.Sprintf("%+v", status) }
