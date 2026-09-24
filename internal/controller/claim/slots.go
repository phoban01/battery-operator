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
	"fmt"
	"time"
)

// DefaultSlotWait is how long a claim that found every BindSlots slot
// taken waits before it tries again.
const DefaultSlotWait = time.Second

// BindSlots bounds how many of the Claim Controller's reconciles call
// ClaimVM at once. It is safe for concurrent use.
//
// battery runs the Pool's pre-lease hooks inside ClaimVM, so a ClaimVM can
// take up to its own deadline (DP-013), much longer than any other call.
// The controller runs n reconciles at once and BindSlots lets n-1 of them
// call ClaimVM, so at least one is always free for renewals, expiries and
// releases, and never waits for a ClaimVM (CL-018, #89). A claim that finds
// no slot is not queued behind the others: Bind asks for a retry and
// calls nothing.
type BindSlots struct {
	slots chan struct{}
}

// NewBindSlots gives the ClaimVM slots of a controller that runs
// reconciles reconciles at once. It needs at least two: one to bind and
// one for everything else.
func NewBindSlots(reconciles int) (*BindSlots, error) {
	if reconciles < 2 {
		return nil, fmt.Errorf("the Claim Controller needs at least 2 concurrent reconciles, one of them kept from ClaimVM; got %d", reconciles)
	}
	return &BindSlots{slots: make(chan struct{}, reconciles-1)}, nil
}

// tryAcquire takes a slot if one is free, without waiting.
func (b *BindSlots) tryAcquire() bool {
	select {
	case b.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// release gives back a slot tryAcquire took.
func (b *BindSlots) release() { <-b.slots }
