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
	"fmt"
	"strconv"
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
// records its calls, with the request id each carried. Every other method
// panics through the nil embedded Client: binding must not call them.
type stubBattery struct {
	battery.Client

	mu         sync.Mutex
	claimVM    func(battery.PoolRef) (*battery.Claim, error)
	calls      []battery.PoolRef
	requestIDs []string
}

func (b *stubBattery) ClaimVM(_ context.Context, pool battery.PoolRef, requestID string) (*battery.Claim, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, pool)
	b.requestIDs = append(b.requestIDs, requestID)
	return b.claimVM(pool)
}

func (b *stubBattery) claimCalls() []battery.PoolRef {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]battery.PoolRef(nil), b.calls...)
}

// claimRequestIDs is the request id of each ClaimVM, in order.
func (b *stubBattery) claimRequestIDs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.requestIDs...)
}

// replayingBattery is a battery.Client whose ClaimVM keeps a Lease per
// request id, as battery v0.4.0 does (BA-007, BA-008): the first call for
// a request id leases the next MicroVM, and every later call for it gets
// the same Lease back. lose makes the next call lose its answer in
// transit after battery has acted, as a dropped connection does.
type replayingBattery struct {
	battery.Client

	mu     sync.Mutex
	leases map[string]*battery.Claim
	next   int
	lose   bool
	calls  []string
}

func newReplayingBattery() *replayingBattery {
	return &replayingBattery{leases: map[string]*battery.Claim{}}
}

func (b *replayingBattery) ClaimVM(_ context.Context, _ battery.PoolRef, requestID string) (*battery.Claim, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, requestID)
	l, ok := b.leases[requestID]
	if !ok {
		b.next++
		l = &battery.Claim{
			LeaseID: "lease-" + strconv.Itoa(b.next),
			VMUID:   "vm-" + strconv.Itoa(b.next),
			Host:    battery.HostRef{Name: nodeA},
		}
		b.leases[requestID] = l
	}
	if b.lose {
		b.lose = false
		return nil, fmt.Errorf("answer lost in transit: %w", battery.ErrUnavailable)
	}
	return l, nil
}

// leased is how many Leases battery created.
func (b *replayingBattery) leased() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.next
}

// requestIDs is the request id of each ClaimVM, in order.
func (b *replayingBattery) requestIDs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...)
}

// answers makes a stubBattery that answers every ClaimVM with c and err.
func answers(c *battery.Claim, err error) *stubBattery {
	return &stubBattery{claimVM: func(battery.PoolRef) (*battery.Claim, error) { return c, err }}
}

// claimKey is aClaim's key.
var claimKey = client.ObjectKey{Namespace: "ci", Name: "runner-1"}

// testClaimUID is aClaim's uid, which the API server would give it, and
// the request id Bind sends for it (CL-060).
const testClaimUID = "6f1c2d3e-0000-4000-8000-000000000001"

// aClaim is the claim "runner-1" in "ci" on the Pool testPool.
func aClaim(finalizers ...string) *batteryv1alpha1.MicroVMClaim {
	return &batteryv1alpha1.MicroVMClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:       claimKey.Name,
			Namespace:  claimKey.Namespace,
			UID:        testClaimUID,
			Generation: 1,
			Finalizers: finalizers,
		},
		Spec: batteryv1alpha1.MicroVMClaimSpec{
			PoolRef:            batteryv1alpha1.PoolReference{Name: testPool},
			ServiceAccountName: "runner",
		},
	}
}

// newFakeClient is controller-runtime's fake client holding objs, with
// the client-go types, the MicroVMClaim status subresource and the index of
// claims by node name.
func newFakeClient(t *testing.T, objs ...client.Object) client.WithWatch {
	t.Helper()
	//= docs/requirements/08-test-doubles.md#test-environments
	//# The unit tests SHALL test each subreconciler, and the rest of
	//# the logic of the controllers and the Exec Agent, against the fake battery,
	//# the fake `flintlockd` and a fake Kubernetes client, without a Kubernetes
	//# API server.
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

// The names the claim tests use: aClaim's Pool, and the MicroVM of
// battery's answer to its ClaimVM (testLease is in release_test.go, nodeA
// in agentaddress_test.go).
const (
	testPool = "small"
	testVM   = "vm-1"
)
