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
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/batterysidecar"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/controller/inventory"
	"github.com/phoban01/battery-operator/internal/fakebattery"
	"github.com/phoban01/battery-operator/internal/fakeflintlock"
)

// testNamespace is the Operator's namespace in the Inventory Controller's
// tests; addrNodeA is node-a's flintlockd; reportReady is a ready Node
// report.
const (
	testNamespace = "battery-system"
	addrNodeA     = "10.0.0.1:9090"
	reportReady   = "true"
)

// batteryClientSecret is battery's client certificate's Secret in these
// tests.
var batteryClientSecret = client.ObjectKey{Namespace: testNamespace, Name: batterysidecar.DefaultClientSecret}

// mountedCertificate is the Operator's mount of battery's client
// certificate in these tests: what the test sets.
type mountedCertificate struct {
	mu   sync.Mutex
	cert []byte
}

func (m *mountedCertificate) ClientCertificate() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cert, nil
}

func (m *mountedCertificate) set(cert string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cert = []byte(cert)
}

// startHostAdminFakeBattery starts the fake battery holding no Hosts, which
// reaches every address through one fake flintlockd. It returns the
// connection to it and the fake's Hosts.
func startHostAdminFakeBattery(t *testing.T) (*battery.Connection, *fakebattery.Hosts) {
	t.Helper()
	fl := fakeflintlock.New(fakeflintlock.Config{Name: "flintlockd"})
	t.Cleanup(func() { _ = fl.Close() })
	flConn, err := fl.Conn()
	if err != nil {
		t.Fatalf("fake flintlockd: %v", err)
	}
	t.Cleanup(func() { _ = flConn.Close() })
	hosts := fakebattery.NewHosts()
	bc, _ := startPoolFakeBatteryWith(t, fakebattery.Config{
		Hosts: hosts,
		Dial:  func(string) (grpc.ClientConnInterface, error) { return flConn, nil },
	})
	conn, ok := bc.(*battery.Connection)
	if !ok {
		t.Fatalf("the battery client is a %T", bc)
	}
	return conn, hosts
}

// newInventoryReconciler is the Inventory Controller against the fake
// battery and the fake client holding battery's client certificate's
// Secret with cert, and objs. The mount holds cert too.
func newInventoryReconciler(t *testing.T, cert string, objs ...client.Object) (
	*InventoryReconciler, client.Client, *clock.Fake, *fakebattery.Hosts, *mountedCertificate,
) {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	objs = append(objs, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: batteryClientSecret.Namespace, Name: batteryClientSecret.Name},
		Data:       map[string][]byte{batterysidecar.ClientCertKey: []byte(cert), "tls.key": []byte("key")},
	})
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	clk := clock.NewFake(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	admin, hosts := startHostAdminFakeBattery(t)
	mount := &mountedCertificate{cert: []byte(cert)}
	r := &InventoryReconciler{
		Client:       c,
		Admin:        admin,
		Hosts:        inventory.NewHostSet(),
		Mount:        mount,
		Options:      inventory.Options{SettleTime: 30 * time.Second, RemoveRetry: 10 * time.Second},
		Clock:        clk,
		ClientSecret: batteryClientSecret,
		Secrets:      c,
	}
	return r, c, clk, hosts, mount
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
// itself through a Node joining, against the fake battery: it requeues
// for the settle time, then adds the Host to battery with mutual TLS and
// publishes it.
func TestInventoryReconcilerGivesBatteryAReadyHost(t *testing.T) {
	r, _, clk, hosts, _ := newInventoryReconciler(t, "certificate",
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeA, Annotations: map[string]string{
			inventory.AnnotationReady:             reportReady,
			inventory.AnnotationFlintlockdAddress: addrNodeA,
		}}},
	)
	reconcileExpecting(t, r, clk, 30*time.Second, 0)
	st, err := r.Admin.GetHost(context.Background(), nodeA)
	if err != nil {
		t.Fatalf("battery does not hold node-a: %v", err)
	}
	if st.Host.Address != addrNodeA || st.Host.Cordoned || st.Host.TLS != inventory.TLS {
		t.Errorf("battery holds %+v", st.Host)
	}
	if hosts.Dials(nodeA) != 1 {
		t.Errorf("battery connected to node-a %d times, want once", hosts.Dials(nodeA))
	}
	got, err := r.Hosts.Hosts()
	if err != nil || len(got) != 1 || got[0] != (inventory.Host{Name: nodeA, Address: addrNodeA}) {
		t.Errorf("published %v, %v, want node-a", got, err)
	}
}

// TestInventoryReconcilerReloadsARenewedCertificate drives the controller
// through cert-manager renewing battery's client certificate: it reads the
// Secret, waits for its mount to hold the renewal, and then makes battery
// connect to the Host again, without restarting anything.
func TestInventoryReconcilerReloadsARenewedCertificate(t *testing.T) {
	r, c, clk, hosts, mount := newInventoryReconciler(t, "certificate 1",
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeA, Annotations: map[string]string{
			inventory.AnnotationReady:             reportReady,
			inventory.AnnotationFlintlockdAddress: addrNodeA,
		}}},
	)
	ctx := context.Background()
	reconcileExpecting(t, r, clk, 30*time.Second, 0)
	dials := hosts.Dials(nodeA)

	secret := &corev1.Secret{}
	if err := c.Get(ctx, batteryClientSecret, secret); err != nil {
		t.Fatal(err)
	}
	secret.Data[batterysidecar.ClientCertKey] = []byte("certificate 2")
	if err := c.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	reconcileExpecting(t, r, clk, inventory.DefaultMountPoll)
	if hosts.Dials(nodeA) != dials {
		t.Fatal("battery reconnected before the mount held the renewal")
	}
	mount.set("certificate 2")
	reconcileExpecting(t, r, clk, 0, 0)
	if hosts.Dials(nodeA) != dials+1 {
		t.Errorf("battery connected to node-a %d more times, want once", hosts.Dials(nodeA)-dials)
	}
}

// TestInventoryReconcilerWithoutTheSecret checks that a missing Secret is
// no certificate rather than an error.
func TestInventoryReconcilerWithoutTheSecret(t *testing.T) {
	r, c, clk, _, _ := newInventoryReconciler(t, "certificate")
	if err := c.Delete(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: batteryClientSecret.Namespace, Name: batteryClientSecret.Name,
	}}); err != nil {
		t.Fatal(err)
	}
	reconcileExpecting(t, r, clk, 0)
}

// TestNodeReportChanged checks which Node updates reach the controller.
func TestNodeReportChanged(t *testing.T) {
	base := func() *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name:        nodeA,
			Labels:      map[string]string{"pool": "fast"},
			Annotations: map[string]string{inventory.AnnotationReady: reportReady},
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
