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

package inventory

import (
	"context"
	"errors"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/phoban01/battery-operator/internal/batterysidecar"
)

func TestHostSetBeforePublishing(t *testing.T) {
	h := NewHostSet()
	if h.Synced() {
		t.Error("a new HostSet is synced")
	}
	if _, err := h.Hosts(); !errors.Is(err, ErrNotSynced) {
		t.Errorf("Hosts: %v, want ErrNotSynced", err)
	}
	if _, err := h.Matching(context.Background(), nil, nil); !errors.Is(err, ErrNotSynced) {
		t.Errorf("Matching: %v, want ErrNotSynced", err)
	}
}

func TestHostSetPublish(t *testing.T) {
	h := NewHostSet()
	changed := h.Changed()
	h.publish(Hosts{nodeB: addrB, nodeA: addrA})
	select {
	case <-changed:
	default:
		t.Error("Changed was not closed by the first publish")
	}
	got, err := h.Hosts()
	if err != nil {
		t.Fatal(err)
	}
	want := []batterysidecar.Host{{Name: nodeA, Address: addrA}, {Name: nodeB, Address: addrB}}
	if !slices.Equal(got, want) {
		t.Errorf("Hosts = %v, want %v", got, want)
	}
	if !h.Has(nodeA) || h.Has("node-c") {
		t.Error("Has disagrees with the set")
	}

	changed = h.Changed()
	h.publish(Hosts{nodeA: addrA, nodeB: addrB})
	select {
	case <-changed:
		t.Error("Changed was closed by publishing the same set")
	default:
	}
	h.publish(Hosts{nodeA: addrA})
	select {
	case <-changed:
	default:
		t.Error("Changed was not closed by a change")
	}
}

// TestHostSetMatching checks that a Pool's selector matches the Nodes with
// its labels that battery runs with, and only those.
func TestHostSetMatching(t *testing.T) {
	const pool, fast = "pool", "fast"
	labelled := func(name string, labels map[string]string) client.Object {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(
		labelled(nodeA, map[string]string{pool: fast}),
		labelled(nodeB, map[string]string{pool: fast}),
		labelled("node-c", map[string]string{pool: fast}), // not a Host
		labelled("node-d", map[string]string{pool: "slow"}),
	).Build()
	h := NewHostSet()
	h.publish(Hosts{nodeA: addrA, nodeB: addrB, "node-d": "10.0.0.4:9090"})

	ctx := context.Background()
	got, err := h.Matching(ctx, c, map[string]string{pool: fast})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{nodeA, nodeB}; !slices.Equal(got, want) {
		t.Errorf("Matching(pool=fast) = %v, want %v", got, want)
	}
	if got, err = h.Matching(ctx, c, nil); err != nil || len(got) != 3 {
		t.Errorf("Matching(everything) = %v, %v, want the three Hosts", got, err)
	}
	if got, err = h.Matching(ctx, c, map[string]string{pool: "none"}); err != nil || len(got) != 0 {
		t.Errorf("Matching(pool=none) = %v, %v, want none", got, err)
	}
}

// TestConfigMapStore checks that the store reads battery's configuration
// without its placeholder Host, and writes it with the pending mark.
func TestConfigMapStore(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	c := h.stored()
	if len(c.Hosts) != 0 || c.Pending {
		t.Errorf("shipped configuration: %+v, want no Hosts and nothing pending", c)
	}

	raw, err := batterysidecar.Render(Hosts{nodeA: addrA}.List())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.Save(ctx, Config{Raw: raw, Pending: true, ClientCertificate: Digest([]byte("certificate"))}); err != nil {
		t.Fatal(err)
	}
	c = h.stored()
	if !c.Hosts.Equal(Hosts{nodeA: addrA}) || !c.Pending || string(c.Raw) != string(raw) ||
		c.ClientCertificate != Digest([]byte("certificate")) {
		t.Errorf("after a pending Save: %+v", c)
	}
	if err := h.store.Save(ctx, Config{Raw: raw}); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{}
	if err := h.c.Get(ctx, configMapKey, cm); err != nil {
		t.Fatal(err)
	}
	if _, ok := cm.Annotations[RestartPendingAnnotation]; ok {
		t.Error("the pending mark was left after the restart")
	}
	if _, ok := cm.Annotations[ClientCertificateAnnotation]; ok {
		t.Error("a client certificate is recorded after a Save without one")
	}
}

func TestConfigMapStoreRejectsAnUnknownField(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	cm := &corev1.ConfigMap{}
	if err := h.c.Get(ctx, configMapKey, cm); err != nil {
		t.Fatal(err)
	}
	cm.Data[batterysidecar.ConfigKey] = `{"hosts": [], "extra": true}`
	if err := h.c.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.Load(ctx); err == nil {
		t.Error("Load accepted a configuration battery would not")
	}
}
