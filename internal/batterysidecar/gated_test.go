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

package batterysidecar_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/batterysidecar"
)

// stubClient stands in for the battery connection behind Gated: it records
// that a call reached it. Its embedded Client is nil, so a method Gated
// passes through that the test did not expect panics.
type stubClient struct {
	battery.Client
	events *eventLog
	calls  int
}

func (s *stubClient) record() {
	s.calls++
	if s.events != nil {
		s.events.add("call")
	}
}

func (s *stubClient) CreatePool(context.Context, battery.PoolSpec) (*battery.Pool, error) {
	s.record()
	return &battery.Pool{}, nil
}

func (s *stubClient) UpdatePool(context.Context, battery.PoolSpec) (*battery.Pool, error) {
	s.record()
	return &battery.Pool{}, nil
}

func (s *stubClient) DeletePool(context.Context, battery.PoolRef) error { s.record(); return nil }

func (s *stubClient) GetPool(context.Context, battery.PoolRef) (*battery.Pool, error) {
	s.record()
	return &battery.Pool{}, nil
}

func (s *stubClient) ListPools(context.Context, string) ([]*battery.Pool, error) {
	s.record()
	return nil, nil
}

func (s *stubClient) ClaimVM(context.Context, battery.PoolRef) (*battery.Claim, error) {
	s.record()
	return &battery.Claim{}, nil
}

func (s *stubClient) Heartbeat(context.Context, string) (time.Time, error) {
	s.record()
	return time.Time{}, nil
}

func (s *stubClient) ReleaseVM(context.Context, string) error { s.record(); return nil }

func (s *stubClient) ListLeases(context.Context, *battery.PoolRef) ([]*battery.LeaseRecord, error) {
	s.record()
	return nil, nil
}

func (s *stubClient) Subscribe(context.Context, battery.EventFilter) (battery.EventStream, error) {
	s.record()
	return nil, nil
}

// closedGate is a restart in progress that never ends.
type closedGate struct{}

func (closedGate) Wait(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// openGate is no restart in progress.
type openGate struct{}

func (openGate) Wait(context.Context) error { return nil }

// gatedCalls calls every method of battery.Client on g.
func gatedCalls(g batterysidecar.Gated) map[string]func(context.Context) error {
	return map[string]func(context.Context) error{
		"CreatePool": func(ctx context.Context) error { _, err := g.CreatePool(ctx, battery.PoolSpec{}); return err },
		"UpdatePool": func(ctx context.Context) error { _, err := g.UpdatePool(ctx, battery.PoolSpec{}); return err },
		"DeletePool": func(ctx context.Context) error { return g.DeletePool(ctx, battery.PoolRef{}) },
		"GetPool":    func(ctx context.Context) error { _, err := g.GetPool(ctx, battery.PoolRef{}); return err },
		"ListPools":  func(ctx context.Context) error { _, err := g.ListPools(ctx, ""); return err },
		"ClaimVM":    func(ctx context.Context) error { _, err := g.ClaimVM(ctx, battery.PoolRef{}); return err },
		"Heartbeat":  func(ctx context.Context) error { _, err := g.Heartbeat(ctx, "l"); return err },
		"ReleaseVM":  func(ctx context.Context) error { return g.ReleaseVM(ctx, "l") },
		"ListLeases": func(ctx context.Context) error { _, err := g.ListLeases(ctx, nil); return err },
		"Subscribe": func(ctx context.Context) error {
			_, err := g.Subscribe(ctx, battery.EventFilter{})
			return err
		},
	}
}

// TestGatedHoldsEveryCall checks that every method waits for the gate: none
// reaches battery while a restart is in progress, and each does once none
// is.
func TestGatedHoldsEveryCall(t *testing.T) {
	t.Parallel()
	held := &stubClient{}
	for name, call := range gatedCalls(batterysidecar.Gated{Client: held, Gate: closedGate{}}) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		if err := call(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s during a restart: %v, want the caller's deadline", name, err)
		}
		cancel()
	}
	if held.calls != 0 {
		t.Errorf("%d calls reached battery during a restart", held.calls)
	}

	open := &stubClient{}
	calls := gatedCalls(batterysidecar.Gated{Client: open, Gate: openGate{}})
	for name, call := range calls {
		if err := call(context.Background()); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if open.calls != len(calls) {
		t.Errorf("%d of %d calls reached battery with no restart in progress", open.calls, len(calls))
	}
}
