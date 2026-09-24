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

package batterysidecar

import (
	"context"
	"time"

	"github.com/phoban01/battery-operator/internal/battery"
)

// Gate is what Gated waits on; *Restarter implements it.
type Gate interface {
	// Wait returns nil once no restart is in progress, or ctx's error.
	Wait(ctx context.Context) error
}

// Gated is the battery client the controllers use: every call first waits
// for Gate, so none reaches battery while it restarts (DP-006).
//
// It spells out every method of battery.Client rather than embedding one,
// so that a method added to the interface fails to compile here instead of
// passing through ungated.
type Gated struct {
	Client battery.Client
	Gate   Gate
}

var _ battery.Client = Gated{}

// CreatePool implements battery.Client.
func (g Gated) CreatePool(ctx context.Context, spec battery.PoolSpec) (*battery.Pool, error) {
	if err := g.Gate.Wait(ctx); err != nil {
		return nil, err
	}
	return g.Client.CreatePool(ctx, spec)
}

// UpdatePool implements battery.Client.
func (g Gated) UpdatePool(ctx context.Context, spec battery.PoolSpec) (*battery.Pool, error) {
	if err := g.Gate.Wait(ctx); err != nil {
		return nil, err
	}
	return g.Client.UpdatePool(ctx, spec)
}

// DeletePool implements battery.Client.
func (g Gated) DeletePool(ctx context.Context, ref battery.PoolRef) error {
	if err := g.Gate.Wait(ctx); err != nil {
		return err
	}
	return g.Client.DeletePool(ctx, ref)
}

// GetPool implements battery.Client.
func (g Gated) GetPool(ctx context.Context, ref battery.PoolRef) (*battery.Pool, error) {
	if err := g.Gate.Wait(ctx); err != nil {
		return nil, err
	}
	return g.Client.GetPool(ctx, ref)
}

// ListPools implements battery.Client.
func (g Gated) ListPools(ctx context.Context, namespace string) ([]*battery.Pool, error) {
	if err := g.Gate.Wait(ctx); err != nil {
		return nil, err
	}
	return g.Client.ListPools(ctx, namespace)
}

// ClaimVM implements battery.Client.
func (g Gated) ClaimVM(ctx context.Context, pool battery.PoolRef) (*battery.Claim, error) {
	if err := g.Gate.Wait(ctx); err != nil {
		return nil, err
	}
	return g.Client.ClaimVM(ctx, pool)
}

// Heartbeat implements battery.Client.
func (g Gated) Heartbeat(ctx context.Context, leaseID string) (time.Time, error) {
	if err := g.Gate.Wait(ctx); err != nil {
		return time.Time{}, err
	}
	return g.Client.Heartbeat(ctx, leaseID)
}

// ReleaseVM implements battery.Client.
func (g Gated) ReleaseVM(ctx context.Context, leaseID string) error {
	if err := g.Gate.Wait(ctx); err != nil {
		return err
	}
	return g.Client.ReleaseVM(ctx, leaseID)
}

// Subscribe implements battery.Client. A stream opened before a restart
// ends with battery.ErrUnavailable when battery exits, and its owner
// subscribes again, which waits.
func (g Gated) Subscribe(ctx context.Context, filter battery.EventFilter) (battery.EventStream, error) {
	if err := g.Gate.Wait(ctx); err != nil {
		return nil, err
	}
	return g.Client.Subscribe(ctx, filter)
}

// ListLeases implements battery.Client.
func (g Gated) ListLeases(ctx context.Context, pool *battery.PoolRef) ([]*battery.LeaseRecord, error) {
	if err := g.Gate.Wait(ctx); err != nil {
		return nil, err
	}
	return g.Client.ListLeases(ctx, pool)
}
