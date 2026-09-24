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
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/controller/claim"
)

// claimEvents follows battery's Events stream for the Claim Controller
// (CL-013). For each MicroVM battery reports deleted, it records the uid
// in Deleted and sends every claim that records that MicroVM to Out, whose
// channel source reconciles it; claim.ExpireDeleted then expires it.
//
// The stream is subscribed for every Pool and subscribed again whenever it
// ends. battery replays its outbox to a new subscriber (BA-050), so a
// deletion reported while the stream was down is seen on the next
// subscription; an event lost anyway is caught by the claim's expiry check
// (CL-016).
type claimEvents struct {
	Battery battery.Client
	// Reader lists claims by claim.MicroVMUIDIndex.
	Reader  client.Reader
	Deleted *claim.DeletedVMs
	Out     chan<- event.GenericEvent
	// Retry is the wait before subscribing again.
	Retry time.Duration
	Clock clock.Clock
	Log   logr.Logger
}

// Start implements manager.Runnable. It runs until ctx is done.
func (w *claimEvents) Start(ctx context.Context) error {
	for {
		stream, err := w.Battery.Subscribe(ctx, battery.EventFilter{})
		if err == nil {
			w.follow(ctx, stream)
			_ = stream.Close()
		} else {
			w.Log.V(1).Info("Failed to subscribe to battery's events", "err", err.Error())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-w.Clock.After(w.Retry):
		}
	}
}

// follow handles the stream's events until it ends.
func (w *claimEvents) follow(ctx context.Context, stream battery.EventStream) {
	for {
		e, err := stream.Recv(ctx)
		if err != nil {
			if ctx.Err() == nil {
				w.Log.V(1).Info("Lost battery's event stream", "err", err.Error())
			}
			return
		}
		w.handle(ctx, e)
	}
}

// handle records a deleted MicroVM and sends the claims that record it.
func (w *claimEvents) handle(ctx context.Context, e *battery.Event) {
	if !claim.ReportsDeletion(e) {
		return
	}
	w.Deleted.Add(e.VMUID)
	claims := &batteryv1alpha1.MicroVMClaimList{}
	if err := w.Reader.List(ctx, claims, client.MatchingFields{claim.MicroVMUIDIndex: e.VMUID}); err != nil {
		// The claim's expiry check finds the Lease gone anyway (CL-016).
		w.Log.Error(err, "Failed to list the MicroVMClaims of a deleted MicroVM", "microVM", e.VMUID)
		return
	}
	for i := range claims.Items {
		select {
		case w.Out <- event.GenericEvent{Object: &claims.Items[i]}:
		case <-ctx.Done():
			return
		}
	}
}
