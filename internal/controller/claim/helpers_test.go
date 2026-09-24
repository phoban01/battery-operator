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

package claim

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/controller/claimscope"
)

// start is the fake clock's start, on a whole second because metav1.Time
// keeps whole seconds.
var start = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

// stubBattery is a battery.Client whose ClaimVM answers with claimVM and
// records its calls. Every other method panics through the nil embedded
// Client: binding must not call them.
type stubBattery struct {
	battery.Client

	mu      sync.Mutex
	claimVM func(battery.PoolRef) (*battery.Claim, error)
	calls   []battery.PoolRef
}

func (b *stubBattery) ClaimVM(_ context.Context, pool battery.PoolRef) (*battery.Claim, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, pool)
	return b.claimVM(pool)
}

func (b *stubBattery) claimCalls() []battery.PoolRef {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]battery.PoolRef(nil), b.calls...)
}

// answers makes a stubBattery that answers every ClaimVM with c and err.
func answers(c *battery.Claim, err error) *stubBattery {
	return &stubBattery{claimVM: func(battery.PoolRef) (*battery.Claim, error) { return c, err }}
}

// claimKey is aClaim's key.
var claimKey = client.ObjectKey{Namespace: "ci", Name: "runner-1"}

// aClaim is the claim "runner-1" in "ci" on the Pool "small".
func aClaim(finalizers ...string) *batteryv1alpha1.MicroVMClaim {
	return &batteryv1alpha1.MicroVMClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:       claimKey.Name,
			Namespace:  claimKey.Namespace,
			Generation: 1,
			Finalizers: finalizers,
		},
		Spec: batteryv1alpha1.MicroVMClaimSpec{
			PoolRef:            batteryv1alpha1.PoolReference{Name: "small"},
			ServiceAccountName: "runner",
		},
	}
}

// newFakeClient is controller-runtime's fake client holding objs, with
// the client-go types, the MicroVMClaim status subresource and the index of
// claims by node name.
func newFakeClient(t *testing.T, objs ...client.Object) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := batteryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&batteryv1alpha1.MicroVMClaim{}).
		WithIndex(&batteryv1alpha1.MicroVMClaim{}, NodeNameField, NodeName).
		Build()
}

// newScope stores claim in a fake client and builds a scope for it as the
// controller would, with a fake clock at start.
func newScope(t *testing.T, claim *batteryv1alpha1.MicroVMClaim, b battery.Client) (*claimscope.Scope, client.Client) {
	t.Helper()
	c := newFakeClient(t, claim)
	return scopeFor(t, c, client.ObjectKeyFromObject(claim), b), c
}

// scopeFor reads the claim at key from c and builds a scope for it.
func scopeFor(t *testing.T, c client.Client, key client.ObjectKey, b battery.Client) *claimscope.Scope {
	t.Helper()
	got := &batteryv1alpha1.MicroVMClaim{}
	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatal(err)
	}
	s := claimscope.New(got)
	s.Client = c
	s.APIReader = c
	s.Battery = b
	s.Log = testr.New(t)
	s.Clock = clock.NewFake(start)
	return s
}
