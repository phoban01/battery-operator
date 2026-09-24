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

// recordingRestarter is a Restarter that records each configuration.
type recordingRestarter struct{ configs [][]byte }

func (r *recordingRestarter) Restart(_ context.Context, config []byte) error {
	r.configs = append(r.configs, config)
	return nil
}

// TestInventoryReconcilerGivesBatteryAReadyHost drives the controller
// itself through a Node joining: it requeues for the settle time and then
// the window, and restarts battery once with the Node as a Host.
func TestInventoryReconcilerGivesBatteryAReadyHost(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	raw, err := batterysidecar.Render(nil)
	if err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: "battery-system", Name: batterysidecar.DefaultConfigMap}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name},
			Data:       map[string]string{batterysidecar.ConfigKey: string(raw)},
		},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeA, Annotations: map[string]string{
			inventory.AnnotationReady:             "true",
			inventory.AnnotationFlintlockdAddress: "10.0.0.1:9090",
		}}},
	).Build()
	clk := clock.NewFake(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC))
	restarter := &recordingRestarter{}
	hosts := inventory.NewHostSet()
	r := &InventoryReconciler{
		Client:    c,
		Store:     inventory.ConfigMapStore{Reader: c, Writer: c, Key: key},
		Restarter: restarter,
		Hosts:     hosts,
		Options:   inventory.Options{SettleTime: 30 * time.Second, RestartWindow: time.Minute},
		Clock:     clk,
	}

	ctx := context.Background()
	for _, want := range []time.Duration{30 * time.Second, time.Minute, 0} {
		res, err := r.Reconcile(ctx, ctrl.Request{})
		if err != nil {
			t.Fatal(err)
		}
		if res.RequeueAfter != want {
			t.Fatalf("RequeueAfter = %s, want %s", res.RequeueAfter, want)
		}
		clk.Advance(res.RequeueAfter)
	}
	if len(restarter.configs) != 1 {
		t.Fatalf("restarts = %d, want 1", len(restarter.configs))
	}
	got, err := hosts.Hosts()
	if err != nil || len(got) != 1 || got[0] != (batterysidecar.Host{Name: nodeA, Address: "10.0.0.1:9090"}) {
		t.Errorf("published %v, %v, want node-a", got, err)
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
