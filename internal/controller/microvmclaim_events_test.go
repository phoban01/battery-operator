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

	"github.com/go-logr/logr/testr"
	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/controller/claim"
)

// scriptedStream gives its events, then ends with ErrUnavailable.
type scriptedStream struct {
	events []*battery.Event
}

func (s *scriptedStream) Recv(context.Context) (*battery.Event, error) {
	if len(s.events) == 0 {
		return nil, battery.ErrUnavailable
	}
	e := s.events[0]
	s.events = s.events[1:]
	return e, nil
}

func (s *scriptedStream) Close() error { return nil }

// subscribeStub is a battery.Client whose first Subscribe gives the
// scripted stream, and whose later ones fail in transit. It counts them.
type subscribeStub struct {
	battery.Client

	mu     sync.Mutex
	first  *scriptedStream
	calls  int
	filter battery.EventFilter
}

func (s *subscribeStub) Subscribe(_ context.Context, f battery.EventFilter) (battery.EventStream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.filter = f
	if s.calls == 1 {
		return s.first, nil
	}
	return nil, battery.ErrUnavailable
}

func (s *subscribeStub) subscribes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// claimOn is a Bound claim named name on the MicroVM uid.
func claimOn(name, uid string) *batteryv1alpha1.MicroVMClaim {
	return &batteryv1alpha1.MicroVMClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ci"},
		Spec: batteryv1alpha1.MicroVMClaimSpec{
			PoolRef:            batteryv1alpha1.PoolReference{Name: "small"},
			ServiceAccountName: "runner",
		},
		Status: batteryv1alpha1.MicroVMClaimStatus{
			Phase:   batteryv1alpha1.MicroVMClaimBound,
			LeaseID: "lease-" + name,
			MicroVM: &batteryv1alpha1.MicroVMReference{UID: uid},
		},
	}
}

//= docs/requirements/02-claims.md#renewal
//= type=test
//# When battery's `Events` stream reports that the MicroVM of a
//# Bound claim was deleted, the Claim Controller SHALL set the claim's phase
//# to `Expired` and its condition `Bound` false with the reason
//# `LeaseExpired`.

// TestClaimEventsSendsTheClaimOfADeletedMicroVM covers CL-013 from the
// watcher's side: it subscribes for every Pool, records a deleted MicroVM
// and sends the claim that records it, and no other, to the controller;
// events that are not a deletion are ignored. When the stream ends it
// subscribes again. claim.ExpireDeleted's test covers the expiry itself.
func TestClaimEventsSendsTheClaimOfADeletedMicroVM(t *testing.T) {
	s := runtime.NewScheme()
	if err := batteryv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(claimOn(testClaimName, testClaimVM), claimOn("runner-2", "vm-2")).
		WithStatusSubresource(&batteryv1alpha1.MicroVMClaim{}).
		WithIndex(&batteryv1alpha1.MicroVMClaim{}, claim.MicroVMUIDIndex, func(o client.Object) []string {
			return claim.MicroVMUID(o.(*batteryv1alpha1.MicroVMClaim))
		}).
		Build()
	b := &subscribeStub{first: &scriptedStream{events: []*battery.Event{
		{Type: poolmgrv1.EventType_VM_EXPIRING_SOON, VMUID: "vm-2"},
		{Type: poolmgrv1.EventType_VM_DELETED_DUE_TO_EXPIRY, VMUID: testClaimVM},
	}}}
	out := make(chan event.GenericEvent, 4)
	w := &claimEvents{
		Battery: b,
		Reader:  c,
		Deleted: &claim.DeletedVMs{},
		Out:     out,
		Retry:   10 * time.Millisecond,
		Clock:   clock.Real{},
		Log:     testr.New(t),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Start(ctx) }()

	select {
	case e := <-out:
		if e.Object.GetName() != testClaimName {
			t.Errorf("sent claim %s, want runner-1", e.Object.GetName())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no claim was sent for the deleted MicroVM")
	}
	deadline := time.Now().Add(10 * time.Second)
	for b.subscribes() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the watcher did not subscribe again after the stream ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Start = %v", err)
	}

	select {
	case e := <-out:
		t.Errorf("also sent %s, want only runner-1", e.Object.GetName())
	default:
	}
	if !w.Deleted.Has(testClaimVM) || w.Deleted.Has("vm-2") {
		t.Errorf("Deleted has vm-1, vm-2 = %t, %t, want true, false", w.Deleted.Has(testClaimVM), w.Deleted.Has("vm-2"))
	}
	if b.filter.Pool != nil {
		t.Errorf("subscribed for Pool %v, want every Pool", b.filter.Pool)
	}
}

// testClaimVM is the MicroVM of the claim these tests watch.
const testClaimVM = "vm-1"
