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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
)

// What claims.qnt fixes, and how the replay maps its values to the Go
// ones.
const (
	replayNamespace = "ci"
	// claims.qnt's POOL.
	replayPool = "pool"
	// One of claims.qnt's ticks.
	replayTick = time.Second
	// claims.qnt's EXPIRY, the Pool's heartbeat_expiry_threshold, in
	// ticks.
	modelExpiry = 3
)

// replayStart is the fake clock at the model's time 0, on a whole second
// because metav1.Time keeps whole seconds.
var replayStart = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

// renewBase is spec.renewTime at the model's renewTime 0. The model counts
// renewals, and the Go claim takes base plus one second per renewal; 0
// itself is no renewTime at all, as a new claim has.
var renewBase = replayStart.Add(-24 * time.Hour)

// modelVMHost is claims.qnt's VM_HOST: the Host each MicroVM lands on.
var modelVMHost = map[string]string{
	"v1": "h1", "v2": "h2", "v3": "h1",
	"v4": "h2", "v5": "h1", "v6": "h2",
}

// tickOf is the model's time of t.
func tickOf(t time.Time) int64 { return int64(t.Sub(replayStart) / replayTick) }

// leaseName is the Go lease id of the model's lease id n.
func leaseName(n int64) string { return "lease-" + strconv.FormatInt(n, 10) }

// leaseNum is the model's lease id of the Go lease id: 0 for none, and -1
// for one the model could not have chosen.
func leaseNum(id string) int64 {
	if id == "" {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(id, "lease-"), 10, 64)
	if err != nil || !strings.HasPrefix(id, "lease-") {
		return -1
	}
	return n
}

// The battery.Client methods the claim controller calls.
const (
	methodClaimVM    = "ClaimVM"
	methodHeartbeat  = "Heartbeat"
	methodReleaseVM  = "ReleaseVM"
	methodListLeases = "ListLeases"
)

// callMode is how the scripted battery answers the controller's calls in
// one model step.
type callMode int

const (
	// answer: battery acts and answers, as ctlClaimVM, ctlHeartbeat and
	// the like.
	answer callMode = iota
	// lose: battery acts and the answer is lost in transit, as the model's
	// *Lost steps.
	lose
	// drop: the call fails in transit before battery acts, as the model's
	// *Unanswered steps.
	drop
)

// scriptedBattery is a battery.Client that holds claims.qnt's
// BatteryState and takes battery's steps when the trace does.
//
// The fake battery (internal/fakebattery) cannot stand in for it: it
// provisions and sweeps on its own control loop, picks the oldest warm
// MicroVM and random lease ids, loses its state when it stops, and cannot
// lose an answer after acting. A trace fixes each of those choices, so the
// replay has battery make them as the trace says. The fake battery's
// agreement with battery on each of them is its own tests' concern
// (BA-*, 08-test-doubles.md).
//
// Every method the claim controller does not call panics, through the nil
// embedded Client.
type scriptedBattery struct {
	battery.Client

	clock clock.Clock
	up    bool
	warm  map[string]bool
	// events are the MicroVM deletions on the Events stream that the
	// controller has not received.
	events    map[string]bool
	leases    map[string]*battery.LeaseRecord
	nextLease int64

	// mode and vm are how the calls of the current step are answered,
	// and the warm MicroVM a ClaimVM leases.
	mode callMode
	vm   string
	// calls are the methods called in the current step.
	calls []string
}

func newScriptedBattery(c clock.Clock) *scriptedBattery {
	return &scriptedBattery{
		clock:     c,
		up:        true,
		warm:      map[string]bool{},
		events:    map[string]bool{},
		leases:    map[string]*battery.LeaseRecord{},
		nextLease: 1,
	}
}

// begin sets how the calls of the next step are answered, and forgets the
// calls of the last.
func (b *scriptedBattery) begin(mode callMode, vm string) {
	b.mode, b.vm, b.calls = mode, vm, nil
}

// call records a call and says whether battery acts on it, and what the
// controller receives if it does not answer.
func (b *scriptedBattery) call(method string) (acts bool, err error) {
	b.calls = append(b.calls, method)
	switch {
	case !b.up:
		return false, fmt.Errorf("scripted battery is down: %w", battery.ErrUnavailable)
	case b.mode == drop:
		return false, fmt.Errorf("scripted %s failed in transit: %w", method, battery.ErrUnavailable)
	case b.mode == lose:
		return true, fmt.Errorf("scripted %s answer lost in transit: %w", method, battery.ErrUnavailable)
	}
	return true, nil
}

// ClaimVM leases b.vm, which must be warm, or answers RESOURCE_EXHAUSTED
// if nothing is.
func (b *scriptedBattery) ClaimVM(_ context.Context, _ battery.PoolRef) (*battery.Claim, error) {
	acts, lost := b.call(methodClaimVM)
	if !acts {
		return nil, lost
	}
	if len(b.warm) == 0 {
		return nil, fmt.Errorf("scripted ClaimVM: %w", battery.ErrExhausted)
	}
	if !b.warm[b.vm] {
		return nil, fmt.Errorf("scripted ClaimVM: the trace leases %q, which is not warm", b.vm)
	}
	now := b.clock.Now()
	id := leaseName(b.nextLease)
	b.nextLease++
	delete(b.warm, b.vm)
	b.leases[id] = &battery.LeaseRecord{
		LeaseID:         id,
		VMUID:           b.vm,
		Pool:            battery.PoolRef{Name: replayPool, Namespace: replayNamespace},
		ClaimedAt:       now,
		LastHeartbeatAt: now,
		ExpiresAt:       now.Add(modelExpiry * replayTick),
	}
	if lost != nil {
		return nil, lost
	}
	return &battery.Claim{LeaseID: id, VMUID: b.vm, Host: battery.HostRef{Name: modelVMHost[b.vm]}}, nil
}

// Heartbeat renews a Lease battery holds, expired or not (BA-010).
func (b *scriptedBattery) Heartbeat(_ context.Context, leaseID string) (time.Time, error) {
	acts, lost := b.call(methodHeartbeat)
	if !acts {
		return time.Time{}, lost
	}
	l, ok := b.leases[leaseID]
	if !ok {
		return time.Time{}, fmt.Errorf("scripted Heartbeat of %s: %w", leaseID, battery.ErrNotFound)
	}
	now := b.clock.Now()
	l.LastHeartbeatAt = now
	l.ExpiresAt = now.Add(modelExpiry * replayTick)
	if lost != nil {
		return time.Time{}, lost
	}
	return l.ExpiresAt, nil
}

// ReleaseVM releases a Lease battery holds and deletes its MicroVM, or
// answers NOT_FOUND.
func (b *scriptedBattery) ReleaseVM(_ context.Context, leaseID string) error {
	acts, lost := b.call(methodReleaseVM)
	if !acts {
		return lost
	}
	if _, ok := b.leases[leaseID]; !ok {
		if lost != nil {
			return lost
		}
		return fmt.Errorf("scripted ReleaseVM of %s: %w", leaseID, battery.ErrNotFound)
	}
	b.drop(leaseID)
	return lost
}

// ListLeases lists every Lease battery holds, by lease id.
func (b *scriptedBattery) ListLeases(_ context.Context, _ *battery.PoolRef) ([]*battery.LeaseRecord, error) {
	acts, lost := b.call(methodListLeases)
	if !acts {
		return nil, lost
	}
	out := make([]*battery.LeaseRecord, 0, len(b.leases))
	for _, l := range b.leases {
		c := *l
		out = append(out, &c)
	}
	slices.SortFunc(out, func(x, y *battery.LeaseRecord) int { return strings.Compare(x.LeaseID, y.LeaseID) })
	return out, lost
}

// drop deletes a Lease and its MicroVM, which the Events stream reports.
func (b *scriptedBattery) drop(leaseID string) {
	b.events[b.leases[leaseID].VMUID] = true
	delete(b.leases, leaseID)
}

// replenish is claims.qnt's replenish(vm).
func (b *scriptedBattery) replenish(vm string) { b.warm[vm] = true }

// sweep is claims.qnt's batteryExpire: every Lease whose expiry is at or
// before now goes.
func (b *scriptedBattery) sweep() {
	now := b.clock.Now()
	for id, l := range b.leases {
		if !l.ExpiresAt.After(now) {
			b.drop(id)
		}
	}
}

// stop is claims.qnt's batteryStop: the events not yet delivered are lost
// with the stream.
func (b *scriptedBattery) stop() {
	b.up = false
	b.events = map[string]bool{}
}

// start is claims.qnt's batteryStart.
func (b *scriptedBattery) start() { b.up = true }
