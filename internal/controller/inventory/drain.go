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
	"fmt"
	"slices"
	"time"

	"github.com/phoban01/battery-operator/internal/battery"
)

// PoolLister lists the Pools battery holds: the Operator's battery client.
type PoolLister interface {
	// ListPools lists the Pools in a namespace; empty lists every
	// namespace.
	ListPools(ctx context.Context, namespace string) ([]*battery.Pool, error)
}

// drainPoll is how often Drain looks at battery's Pools again while it
// waits for them.
const drainPoll = time.Second

// Drain takes the Hosts the next restart removes out of every Pool before
// battery restarts without them (#73). battery v0.3.3 does not check a
// Pool's flintlock_hosts against its Hosts, and replenishes a Pool on the
// Host in flintlock_hosts with the fewest of its MicroVMs: a removed Host
// has none, so battery would pick it every time and fail, and the Pool
// would not replenish until the Pool Controller dropped it.
//
// Drain publishes the Hosts that remain, which the Pool Controller watches
// and resolves every Pool's selector against, so the Pools drop the
// leaving Hosts with UpdatePool. Drain then waits, looking at battery's
// Pools every drainPoll, until none names a leaving Host, and lets Apply
// restart battery. It waits at most Timeout: a Pool the Pool Controller
// cannot update (battery refused its spec, it is being deleted and battery
// refuses to delete it, or battery holds it and the cluster does not)
// would otherwise hold battery's Hosts back for good.
//
// A Host joining needs no drain: HostSet gains it only once battery has
// restarted with it (restart), so no Pool names it before battery knows
// it.
//
// Changes that settle while Drain waits go into the same restart.
type Drain struct {
	// Timeout is the longest Drain waits; zero does not wait.
	Timeout time.Duration
}

func (d Drain) Reconcile(ctx context.Context, s *Scope) (Result, error) {
	remaining := Hosts{}
	var leaving []string
	for name, address := range s.Config.Hosts {
		if _, ok := s.Desired[name]; ok {
			remaining[name] = address
		} else {
			leaving = append(leaving, name)
		}
	}
	if len(leaving) == 0 {
		s.State.drainStarted = time.Time{}
		return Result{}, nil
	}
	slices.Sort(leaving)

	//= docs/requirements/04-inventory.md#applying
	//# When a restart of battery would remove a Host, the Inventory
	//# Controller SHALL first give the Pool Controller the Hosts that remain,
	//# and SHALL restart battery only once no Pool in battery names the removed
	//# Host in its `flintlock_hosts` or the configured drain timeout has passed.
	s.HostSet.publish(remaining)
	if d.Timeout <= 0 {
		return Result{}, nil
	}
	now := s.Clock.Now()
	if s.State.drainStarted.IsZero() {
		s.State.drainStarted = now
		s.Log.Info("Waiting for Pools to drop Hosts before restarting battery", "leaving", leaving)
	}
	waited := now.Sub(s.State.drainStarted)
	naming, err := poolsNaming(ctx, s.Pools, leaving)
	switch {
	case err == nil && len(naming) == 0:
		s.Log.Info("Pools dropped the Hosts battery is restarting without", "leaving", leaving, "waited", waited)
		return Result{}, nil
	case waited >= d.Timeout:
		msg := fmt.Sprintf("%v", naming)
		if err != nil {
			msg = err.Error()
		}
		s.Log.Info("Restarting battery while Pools may still name Hosts it removes",
			"leaving", leaving, "pools", msg, "waited", waited)
		return Result{}, nil
	case err != nil:
		s.Log.Error(err, "Failed to list battery's Pools", "leaving", leaving)
	}
	return Result{Stop: true, RequeueAfter: min(drainPoll, d.Timeout-waited)}, nil
}

// poolsNaming lists the Pools in battery whose flintlock_hosts name any
// of hosts.
func poolsNaming(ctx context.Context, pools PoolLister, hosts []string) ([]string, error) {
	held, err := pools.ListPools(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("listing battery's Pools: %w", err)
	}
	var naming []string
	for _, p := range held {
		if slices.ContainsFunc(p.Spec.FlintlockHosts, func(h string) bool { return slices.Contains(hosts, h) }) {
			naming = append(naming, p.Spec.Ref.String())
		}
	}
	return naming, nil
}
