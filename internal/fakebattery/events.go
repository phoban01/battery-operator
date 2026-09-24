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

package fakebattery

import (
	"encoding/json"
	"sync"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// subscriberBuffer is how many events a subscriber may fall behind before
// its stream is dropped and it has to re-subscribe and replay.
const subscriberBuffer = 256

// Keys of the payload_json objects.
const (
	payloadHost    = "host"
	payloadLeaseID = "lease_id"
)

// emitLocked appends an Event for a Pool and delivers it to every
// subscriber. Ids are monotonic per Pool, as the proto says; payload is
// rendered to payload_json. It requires b.mu because it assigns the Pool's
// next id.
//
// The transitions and their events are: reservation created on a Host,
// VM_PROVISIONED; create hooks done, VM_AVAILABLE; Lease granted,
// VM_CLAIMED; ReleaseVM accepted, VM_RELEASED; the deletion that follows,
// VM_DELETED_ON_RELEASE; a Lease within one warning window of expiry,
// VM_EXPIRING_SOON; a Lease expired and its MicroVM deleted,
// VM_DELETED_DUE_TO_EXPIRY; a create or pre-lease hook failed, VM_HOOK_FAILED;
// provisioning started, POOL_REPLENISHING; a tick found a Pool short of its
// target, POOL_SIZE_BELOW_TARGET.
func (b *Battery) emitLocked(key poolKey, vmUID string, typ poolmgrv1.EventType, payload map[string]any) {
	ps, ok := b.pools[key]
	if !ok {
		return
	}
	ps.nextEvent++
	var body string
	if len(payload) > 0 {
		if raw, err := json.Marshal(payload); err == nil {
			body = string(raw)
		}
	}
	b.events.publish(key, &poolmgrv1.Event{
		Id:            ps.nextEvent,
		PoolName:      key.name,
		PoolNamespace: key.namespace,
		VmUid:         vmUID,
		Type:          typ,
		CreatedAt:     timestamppb.New(b.cfg.Clock.Now()),
		PayloadJson:   body,
	})
}

//= docs/requirements/10-battery.md#events
//= type=exception
//= reason=The fake replays only the last Config.EventReplay events of each Pool, 100 by default, where battery replays its whole outbox.
//# When a client subscribes to battery's `Events` service, battery
//# SHALL send every event its outbox holds for the Pool the subscription
//# names, or for every Pool, and then each new event as battery records it.

// eventBus fans events out to Subscribe streams and keeps the last replay
// events of every Pool for new subscribers.
type eventBus struct {
	replay int

	mu      sync.Mutex
	recent  map[poolKey][]*poolmgrv1.Event
	subs    map[int64]*subscriber
	nextSub int64
}

func newEventBus(replay int) *eventBus {
	return &eventBus{replay: replay, recent: make(map[poolKey][]*poolmgrv1.Event), subs: make(map[int64]*subscriber)}
}

// subscriber is one open Subscribe stream.
type subscriber struct {
	id     int64
	filter *poolKey
	ch     chan *poolmgrv1.Event
	// dropped is closed when the fake ends the stream: on the injected drop
	// or because the subscriber fell too far behind.
	dropped chan struct{}
	once    sync.Once
}

func (s *subscriber) matches(key poolKey) bool { return s.filter == nil || *s.filter == key }

func (s *subscriber) drop() { s.once.Do(func() { close(s.dropped) }) }

// publish records e and delivers it to matching subscribers without
// blocking; a subscriber whose buffer is full is dropped.
func (e *eventBus) publish(key poolKey, ev *poolmgrv1.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	log := append(e.recent[key], ev)
	if len(log) > e.replay {
		log = log[len(log)-e.replay:]
	}
	e.recent[key] = log
	for _, s := range e.subs {
		if !s.matches(key) {
			continue
		}
		select {
		case s.ch <- ev:
		default:
			s.drop()
		}
	}
}

// subscribe registers a subscriber and returns the events to replay to it
// before live delivery: the recent events of the filtered Pool, or of every
// Pool, in id order per Pool.
func (e *eventBus) subscribe(filter *poolKey) (*subscriber, []*poolmgrv1.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.nextSub++
	s := &subscriber{id: e.nextSub, filter: filter, ch: make(chan *poolmgrv1.Event, subscriberBuffer), dropped: make(chan struct{})}
	e.subs[s.id] = s
	var backlog []*poolmgrv1.Event
	if filter != nil {
		backlog = append(backlog, e.recent[*filter]...)
	} else {
		for _, log := range e.recent {
			backlog = append(backlog, log...)
		}
	}
	return s, backlog
}

func (e *eventBus) unsubscribe(id int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.subs, id)
}

// dropAll ends every open stream once (Faults.DropEventsStream).
func (e *eventBus) dropAll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, s := range e.subs {
		s.drop()
	}
}

// open reports how many streams are subscribed.
func (e *eventBus) open() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.subs)
}
