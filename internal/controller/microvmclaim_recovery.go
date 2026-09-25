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
	"fmt"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/controller/claim"
)

// claimRecovery reconciles every Bound claim against battery's Leases
// (CL-030). It lists the Bound claims, then every Lease with one
// unfiltered ListLeases, records in Leases what battery said of each
// claim's Lease, and sends every Bound claim to Out, whose channel source
// reconciles it; claim.Recover then settles the claim.
//
// claimEvents, the Claim Controller's side of BatteryEvents, runs it after
// each subscription to battery's Events stream succeeds, before the stream
// is read. The first subscription is the Claim Controller's start. Every later one follows a
// stream that ended because battery went away, or restarted (DP-006: the
// gated client holds the Subscribe until the restart is over), so it is
// the connection being restored. No other signal is needed, and a
// subscription whose recovery fails is dropped and made again, so every
// connection is recovered once.
//
// The claims are read before the Leases on purpose: see
// claim.RecoveredLeases. They come from the manager's cache, which the
// manager has synced before it starts the watcher.
type claimRecovery struct {
	Battery battery.Client
	// Reader lists the claims.
	Reader client.Reader
	Leases *claim.RecoveredLeases
	Out    chan<- event.GenericEvent
	Log    logr.Logger
}

// run recovers once. It returns an error if the claims or the Leases could
// not be read, and then records and sends nothing.
func (r *claimRecovery) run(ctx context.Context) error {
	//= docs/requirements/02-claims.md#recovery
	//# When the Claim Controller starts, and when its connection to
	//# battery is restored, the Claim Controller SHALL reconcile every Bound claim that is not being deleted against
	//# battery's Leases.
	claims := &batteryv1alpha1.MicroVMClaimList{}
	if err := r.Reader.List(ctx, claims); err != nil {
		return fmt.Errorf("listing MicroVMClaims: %w", err)
	}
	var bound []*batteryv1alpha1.MicroVMClaim
	var recorded []string
	for i := range claims.Items {
		if c := &claims.Items[i]; claim.HoldsLease(c) {
			bound = append(bound, c)
			recorded = append(recorded, c.Status.LeaseID)
		}
	}
	leases, err := r.Battery.ListLeases(ctx, nil)
	if err != nil {
		return fmt.Errorf("listing battery's Leases: %w", err)
	}
	r.Leases.Record(recorded, leases)
	r.Log.Info("Recovered MicroVMClaims against battery's Leases", "bound", len(bound), "leases", len(leases))
	for _, c := range bound {
		select {
		case r.Out <- event.GenericEvent{Object: c}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
