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

package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/phoban01/battery-operator/internal/batterysidecar"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/controller/inventory"
)

// recordingRestarter is a Restarter that records each restart.
type recordingRestarter struct{ restarts []batterysidecar.Mounts }

func (r *recordingRestarter) Restart(_ context.Context, want batterysidecar.Mounts) error {
	r.restarts = append(r.restarts, want)
	return nil
}

// batteryClientSecret is battery's client certificate's Secret in these
// tests.
// testNamespace is the Operator's namespace in the Inventory Controller's tests.
const testNamespace = "battery-system"

var batteryClientSecret = client.ObjectKey{Namespace: testNamespace, Name: batterysidecar.DefaultClientSecret}

// newInventoryReconciler is the Inventory Controller against the fake
// client holding battery's shipped ConfigMap, its client certificate's
// Secret holding cert, and objs.
func newInventoryReconciler(t *testing.T, cert string, objs ...client.Object) (
	*InventoryReconciler, client.Client, *clock.Fake, *recordingRestarter,
) {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	raw, err := batterysidecar.Render(nil)
	if err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: testNamespace, Name: batterysidecar.DefaultConfigMap}
	objs = append(objs,
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name},
			Data:       map[string]string{batterysidecar.ConfigKey: string(raw)},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: batteryClientSecret.Namespace, Name: batteryClientSecret.Name},
			Data:       map[string][]byte{batterysidecar.ClientCertKey: []byte(cert), "tls.key": []byte("key")},
		},
	)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	clk := clock.NewFake(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC))
	restarter := &recordingRestarter{}
	r := &InventoryReconciler{
		Client:       c,
		Store:        inventory.ConfigMapStore{Reader: c, Writer: c, Key: key},
		Restarter:    restarter,
		Hosts:        inventory.NewHostSet(),
		Options:      inventory.Options{SettleTime: 30 * time.Second, RestartWindow: time.Minute},
		Clock:        clk,
		ClientSecret: batteryClientSecret,
		Secrets:      c,
	}
	return r, c, clk, restarter
}

// reconcileExpecting runs the controller once for each requeue it expects,
// advancing the clock by each.
func reconcileExpecting(t *testing.T, r *InventoryReconciler, clk *clock.Fake, requeues ...time.Duration) {
	t.Helper()
	for _, want := range requeues {
		res, err := r.Reconcile(context.Background(), ctrl.Request{})
		if err != nil {
			t.Fatal(err)
		}
		if res.RequeueAfter != want {
			t.Fatalf("RequeueAfter = %s, want %s", res.RequeueAfter, want)
		}
		clk.Advance(res.RequeueAfter)
	}
}

// TestInventoryReconcilerGivesBatteryAReadyHost drives the controller
// itself through a Node joining: it requeues for the settle time and then
// the window, and restarts battery once with the Node as a Host.
func TestInventoryReconcilerGivesBatteryAReadyHost(t *testing.T) {
	r, _, clk, restarter := newInventoryReconciler(t, "certificate",
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeA, Annotations: map[string]string{
			inventory.AnnotationReady:             "true",
			inventory.AnnotationFlintlockdAddress: "10.0.0.1:9090",
		}}},
	)
	reconcileExpecting(t, r, clk, 30*time.Second, time.Minute, 0)
	if len(restarter.restarts) != 1 {
		t.Fatalf("restarts = %d, want 1", len(restarter.restarts))
	}
	got, err := r.Hosts.Hosts()
	if err != nil || len(got) != 1 || got[0] != (batterysidecar.Host{Name: nodeA, Address: "10.0.0.1:9090"}) {
		t.Errorf("published %v, %v, want node-a", got, err)
	}
}

//= docs/requirements/06-deployment.md#battery-sidecar
//= type=test
//# When the client certificate in the Secret of DP-005 changes,
//# the Operator SHALL restart battery through the mechanism of DP-006

// TestInventoryReconcilerRestartsBatteryForARenewedCertificate drives the
// controller through cert-manager renewing battery's client certificate:
// it reads the Secret, waits one window, and restarts battery once, waiting
// for the renewed certificate.
func TestInventoryReconcilerRestartsBatteryForARenewedCertificate(t *testing.T) {
	r, c, clk, restarter := newInventoryReconciler(t, "certificate 1")
	ctx := context.Background()
	reconcileExpecting(t, r, clk, 0)

	secret := &corev1.Secret{}
	if err := c.Get(ctx, batteryClientSecret, secret); err != nil {
		t.Fatal(err)
	}
	secret.Data[batterysidecar.ClientCertKey] = []byte("certificate 2")
	if err := c.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	reconcileExpecting(t, r, clk, time.Minute, 0)
	if len(restarter.restarts) != 1 || string(restarter.restarts[0].ClientCertificate) != "certificate 2" {
		t.Fatalf("restarts %+v, want one, waiting for the renewed certificate", restarter.restarts)
	}
}

// TestInventoryReconcilerWithoutTheSecret checks that a missing Secret is
// no certificate rather than an error.
func TestInventoryReconcilerWithoutTheSecret(t *testing.T) {
	r, c, clk, restarter := newInventoryReconciler(t, "certificate")
	if err := c.Delete(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: batteryClientSecret.Namespace, Name: batteryClientSecret.Name,
	}}); err != nil {
		t.Fatal(err)
	}
	reconcileExpecting(t, r, clk, 0)
	if len(restarter.restarts) != 0 {
		t.Errorf("restarts = %d, want none", len(restarter.restarts))
	}
}

// TestNodeReportChanged checks which Node updates reach the controller.
func TestNodeReportChanged(t *testing.T) {
	base := func() *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name:        nodeA,
			Labels:      map[string]string{"pool": "fast"},
			Annotations: map[string]string{inventory.AnnotationReady: "true"},
		}}
	}
	for name, tc := range map[string]struct {
		change func(*corev1.Node)
		want   bool
	}{
		"cordoned":         {func(n *corev1.Node) { n.Spec.Unschedulable = true }, true},
		"not ready":        {func(n *corev1.Node) { n.Annotations[inventory.AnnotationReady] = "false" }, true},
		"address moved":    {func(n *corev1.Node) { n.Annotations[inventory.AnnotationFlintlockdAddress] = "x:1" }, true},
		"deleting":         {func(n *corev1.Node) { n.DeletionTimestamp = &metav1.Time{Time: time.Now()} }, true},
		"status only":      {func(n *corev1.Node) { n.Status.Phase = corev1.NodeRunning }, false},
		"other annotation": {func(n *corev1.Node) { n.Annotations["example.com/x"] = "y" }, false},
	} {
		n := base()
		tc.change(n)
		if got := nodeReportChanged.Update(event.UpdateEvent{ObjectOld: base(), ObjectNew: n}); got != tc.want {
			t.Errorf("%s: passed %v, want %v", name, got, tc.want)
		}
	}
}
