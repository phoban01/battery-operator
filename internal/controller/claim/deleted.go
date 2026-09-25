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
	"sync"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/controller/claimscope"
)

// ReportsDeletion reports whether battery's event e says that a leased
// MicroVM was deleted: battery records these only once flintlockd has
// confirmed the deletion (BA-051), after the Lease row is gone.
func ReportsDeletion(e *battery.Event) bool {
	return e.VMUID != "" &&
		(e.Type == poolmgrv1.EventType_VM_DELETED_DUE_TO_EXPIRY ||
			e.Type == poolmgrv1.EventType_VM_DELETED_ON_RELEASE)
}

// DeletedVMs is the set of MicroVMs battery's Events stream has reported
// deleted, which the Operator's event watcher fills and ExpireDeleted
// reads. It is safe for concurrent use.
//
// It is held in memory only, and an event the stream drops never reaches
// it: that is safe, because CheckExpiry finds the Lease gone with
// ListLeases once the claim's expiry passes (CL-016). battery never reuses
// a MicroVM's uid, so an entry never expires the wrong claim; entries are
// forgotten after Retention so that the set stays small.
type DeletedVMs struct {
	// Retention is how long an entry is kept; zero is an hour.
	Retention time.Duration
	// Clock defaults to clock.Real.
	Clock clock.Clock

	mu   sync.Mutex
	seen map[string]time.Time
}

// Add records that battery deleted the MicroVM uid.
func (d *DeletedVMs) Add(uid string) {
	now := d.now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.seen == nil {
		d.seen = make(map[string]time.Time)
	}
	retention := d.Retention
	if retention <= 0 {
		retention = time.Hour
	}
	for u, at := range d.seen {
		if now.Sub(at) > retention {
			delete(d.seen, u)
		}
	}
	d.seen[uid] = now
}

// Has reports whether battery has reported the MicroVM uid deleted.
func (d *DeletedVMs) Has(uid string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.seen[uid]
	return ok
}

func (d *DeletedVMs) now() time.Time {
	if d.Clock == nil {
		return time.Now()
	}
	return d.Clock.Now()
}

// ExpireDeleted expires a Bound claim whose MicroVM battery's Events
// stream has reported deleted. It calls nothing: the event is battery's
// answer. A renewal pending on the claim does not hold it back, because
// battery deletes the Lease before the MicroVM (BA-023), and a Heartbeat
// could only be answered NOT_FOUND.
type ExpireDeleted struct {
	// Deleted is the set the event watcher fills; nil expires nothing.
	Deleted *DeletedVMs
}

var _ claimscope.Subreconciler = ExpireDeleted{}

// Reconcile implements claimscope.Subreconciler.
func (e ExpireDeleted) Reconcile(_ context.Context, s *claimscope.Scope) (claimscope.Result, error) {
	c := s.Claim
	if e.Deleted == nil || !holdsLease(c) || c.Status.MicroVM == nil {
		return claimscope.Result{}, nil
	}
	uid := c.Status.MicroVM.UID
	//= docs/requirements/02-claims.md#renewal
	//# When battery's `Events` stream reports the deletion of the
	//# MicroVM of a Bound claim that is not being deleted, the Claim Controller
	//# SHALL set the claim's phase to `Expired` and its condition `Bound` false
	//# with the reason `LeaseExpired`.
	if !e.Deleted.Has(uid) {
		return claimscope.Result{}, nil
	}
	expire(s, fmt.Sprintf("battery deleted MicroVM %s", uid))
	return claimscope.Result{Stop: true}, nil
}

// MicroVMUIDIndex is the field index of MicroVMClaims by
// status.microVM.uid, by which the event watcher finds a deleted
// MicroVM's claim.
const MicroVMUIDIndex = "status.microVM.uid"

// MicroVMUID is the indexer of MicroVMUIDIndex.
func MicroVMUID(c *batteryv1alpha1.MicroVMClaim) []string {
	if c.Status.MicroVM == nil || c.Status.MicroVM.UID == "" {
		return nil
	}
	return []string{c.Status.MicroVM.UID}
}
