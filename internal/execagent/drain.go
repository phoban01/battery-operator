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

package execagent

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"

	"github.com/phoban01/battery-operator/internal/clock"
)

const (
	// GuardNamePrefix names the guard pod and its PodDisruptionBudget after
	// the Host's Node. The admission policy allows each Host's Exec Agent
	// its own guard by this name.
	GuardNamePrefix = "exec-agent-drain-guard-"
	// labelDrainGuard selects the guard pod of one Host's Node.
	labelDrainGuard = Prefix + "drain-guard"
)

// drainGuard holds an eviction-based drain of the Host's Node open while
// claims are Bound on the Host. The Host's MicroVMs are no pods, so a drain
// would otherwise evict everything it can see and complete under a
// running command.
//
// It remembers what it last did and talks to the API server only when
// that has to change; every resync period it forgets, so that a guard
// deleted by hand is put back.
type drainGuard struct {
	cfg      *Config
	log      logr.Logger
	clk      clock.Clock
	kube     kubernetes.Interface
	claims   ClaimLookup
	hostNode string
	resync   time.Duration

	synced     time.Time
	guardKnown bool
	guard      bool
	abandoned  map[string]bool
}

// GuardName is the name of the guard pod and budget of a Host's Node.
func GuardName(hostNode string) string { return GuardNamePrefix + hostNode }

//= docs/requirements/05-exec-agent.md#drain
//# While claims are Bound on its Host, the Exec Agent SHALL hold an
//# eviction-based drain of the Host's Node open, and SHALL let the drain
//# complete when none remain or when the configured drain timeout elapses.

// reconcile places the guard while claims are Bound on the Host and
// removes it when none are, or once the Host's Node has been unschedulable
// for the drain timeout, at which point the claims still Bound are logged
// as abandoned. When the Node was first seen unschedulable is kept on the
// Node itself, so that a restart of the agent does not restart the
// timeout.
func (d *drainGuard) reconcile(ctx context.Context, host *corev1.Node) error {
	if !d.synced.IsZero() && d.clk.Now().Sub(d.synced) >= d.resync {
		d.guardKnown = false
	}
	bound, err := d.claims.BoundOnHost(ctx, d.hostNode)
	if err != nil {
		// Without the claims nothing is known, so the guard is left as it
		// is: removing it could let a drain through under a running command.
		return fmt.Errorf("listing the claims bound on this host: %w", err)
	}

	started, drainTracked := drainStarted(host)
	if !host.Spec.Unschedulable {
		d.abandoned = nil
		if drainTracked {
			if err := patchNodeAnnotations(ctx, d.kube, d.hostNode, map[string]any{AnnotationDrainStarted: nil}); err != nil {
				return err
			}
		}
		return d.setGuard(ctx, host, len(bound) > 0)
	}

	if !drainTracked {
		started = d.clk.Now().UTC()
		if err := patchNodeAnnotations(ctx, d.kube, d.hostNode, map[string]any{
			AnnotationDrainStarted: started.Format(time.RFC3339Nano),
		}); err != nil {
			return err
		}
		d.log.Info("Recorded the start of a drain of the Host's Node; Bound claims hold it open for at most the drain timeout",
			"node", d.hostNode, "boundClaims", len(bound), "drainTimeout", d.cfg.DrainTimeout.String())
	}

	if waited := d.clk.Now().Sub(started); len(bound) > 0 && waited >= d.cfg.DrainTimeout {
		for _, c := range bound {
			if d.abandoned[c.String()] {
				continue
			}
			if d.abandoned == nil {
				d.abandoned = map[string]bool{}
			}
			d.abandoned[c.String()] = true
			d.log.Info("Abandoned a Bound claim: the drain timeout elapsed and the drain of the Host's Node was let through",
				"claim", c.String(), "microVM", c.VMUID, "node", d.hostNode, "waited", waited.String(), "drainTimeout", d.cfg.DrainTimeout.String())
		}
		return d.setGuard(ctx, host, false)
	}
	return d.setGuard(ctx, host, len(bound) > 0)
}

// drainStarted reads when the agent first saw the Node unschedulable.
func drainStarted(host *corev1.Node) (time.Time, bool) {
	value, ok := host.Annotations[AnnotationDrainStarted]
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// setGuard creates or removes the guard: a pod on the Host's Node under a
// PodDisruptionBudget that allows no disruption. An eviction-based drain
// evicts every pod of the node and cannot finish while one refuses, so the
// guard is what holds the drain open. It exists whenever a claim is Bound,
// not only once a cordon is seen, because a drain's evictions follow its
// cordon faster than the agent can be sure to notice.
//
// The budget is written as minAvailable 1 rather than maxUnavailable 0:
// the second needs the disruption controller to find the pod's scale
// through a controller it knows, and the guard has none. The budget is
// made before the pod and removed after it, so the pod is never
// unprotected. The Host's Node owns both, so that they go when it does.
func (d *drainGuard) setGuard(ctx context.Context, host *corev1.Node, want bool) error {
	if d.guardKnown && d.guard == want {
		return nil
	}
	var err error
	if want {
		err = d.placeGuard(ctx, host)
	} else {
		err = d.removeGuard(ctx)
	}
	if err != nil {
		d.guardKnown = false
		return err
	}
	d.guardKnown, d.guard, d.synced = true, want, d.clk.Now()
	return nil
}

// removeGuard deletes the guard pod and then its budget.
func (d *drainGuard) removeGuard(ctx context.Context) error {
	name, namespace := GuardName(d.hostNode), d.cfg.Guard.Namespace
	err := d.kube.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: new(int64)})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting the drain guard pod: %w", err)
	}
	if err == nil {
		d.log.Info("Removed the drain guard; a drain of the Host's Node can complete", "node", d.hostNode)
	}
	err = d.kube.PolicyV1().PodDisruptionBudgets(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting the drain guard's pod disruption budget: %w", err)
	}
	return nil
}

// placeGuard creates the budget and then the guard pod. A guard pod that is
// there but has ended, which a node reboot can leave behind, is deleted so
// that the next reconcile places a live one.
func (d *drainGuard) placeGuard(ctx context.Context, host *corev1.Node) error {
	name, namespace := GuardName(d.hostNode), d.cfg.Guard.Namespace
	pods := d.kube.CoreV1().Pods(namespace)
	selector := map[string]string{labelDrainGuard: d.hostNode}
	owner := func(controller bool) []metav1.OwnerReference {
		ref := metav1.OwnerReference{APIVersion: "v1", Kind: "Node", Name: host.Name, UID: host.UID}
		if controller {
			ref.Controller = &controller
		}
		return []metav1.OwnerReference{ref}
	}

	minAvailable := intstr.FromInt32(1)
	_, err := d.kube.PolicyV1().PodDisruptionBudgets(namespace).Create(ctx, &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: selector, OwnerReferences: owner(false)},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvailable,
			Selector:     &metav1.LabelSelector{MatchLabels: selector},
		},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating the drain guard's pod disruption budget: %w", err)
	}

	noToken := false
	_, err = pods.Create(ctx, &corev1.Pod{
		// The owner reference names a controller so that a drain treats the
		// guard as a managed pod and evicts it, which the budget refuses,
		// instead of stopping to ask for --force.
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: selector, OwnerReferences: owner(true)},
		Spec: corev1.PodSpec{
			// Bound by name: the Host's Node may already be cordoned, and
			// the scheduler would refuse to put anything there.
			NodeName:                     d.hostNode,
			AutomountServiceAccountToken: &noToken,
			Tolerations:                  []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers:                   []corev1.Container{{Name: "guard", Image: d.cfg.Guard.Image}},
		},
	}, metav1.CreateOptions{})
	if err == nil {
		d.log.Info("Placed the drain guard: Bound claims hold a drain of the Host's Node open", "node", d.hostNode)
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating the drain guard pod: %w", err)
	}
	existing, err := pods.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading the drain guard pod: %w", err)
	}
	if existing.Status.Phase == corev1.PodFailed || existing.Status.Phase == corev1.PodSucceeded || existing.DeletionTimestamp != nil {
		if err := pods.Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: new(int64)}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting the ended drain guard pod: %w", err)
		}
		return fmt.Errorf("the drain guard pod had ended (%s) and was deleted; the next reconcile places a new one", existing.Status.Phase)
	}
	return nil
}
