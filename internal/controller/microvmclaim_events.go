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

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/controller/claim"
)

// claimEvents is the Claim Controller's side of BatteryEvents (CL-013).
// For each MicroVM battery reports deleted, it records the uid in Deleted
// and sends every claim that records that MicroVM to Out, whose channel
// source reconciles it; claim.ExpireDeleted then expires it.
//
// BatteryEvents subscribes for every Pool and subscribes again whenever
// the stream ends. battery replays its outbox to a new subscriber
// (BA-050), so a deletion reported while the stream was down is seen on
// the next subscription; an event lost anyway is caught by the claim's
// expiry check (CL-016).
//
// After each subscription succeeds, and before BatteryEvents reads the
// stream, it runs Recovery if it has one (CL-030): see claimRecovery for
// why a successful subscription is the Claim Controller's start or its
// connection to battery being restored. A recovery that fails drops the
// subscription, which BatteryEvents makes again after its backoff.
type claimEvents struct {
	// Reader lists claims by claim.MicroVMUIDIndex.
	Reader  client.Reader
	Deleted *claim.DeletedVMs
	Out     chan<- event.GenericEvent
	// Recovery, when set, runs after every successful subscription.
	Recovery *claimRecovery
	Log      logr.Logger
}

var _ eventConsumer = (*claimEvents)(nil)

// subscribed implements eventConsumer: it runs Recovery.
func (w *claimEvents) subscribed(ctx context.Context) error {
	if w.Recovery == nil {
		return nil
	}
	if err := w.Recovery.run(ctx); err != nil {
		if ctx.Err() == nil {
			w.Log.Error(err, "Failed to recover MicroVMClaims against battery's Leases; subscribing again")
		}
		return err
	}
	return nil
}

// resync implements eventConsumer: a claim needs none, since its own
// expiry check reads battery while the stream is down (CL-016).
func (w *claimEvents) resync(context.Context) {}

// handle implements eventConsumer: it records a deleted MicroVM and sends
// the claims that record it.
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
