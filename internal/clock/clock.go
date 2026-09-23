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

// Package clock is the time source and retry policy shared by the
// Operator's components and its fakes. Every component that waits, retries
// or expires something takes a Clock and a Backoff from here rather than
// calling the time package, so that its tests advance a Fake instead of
// sleeping; the fake battery's Lease expiry is one of them. The one
// production implementation, Real, delegates to the time package.
package clock

import "time"

// Timer is a stoppable timer obtained from Clock.NewTimer. It mirrors
// time.Timer with the channel behind a method so that a fake can own it.
type Timer interface {
	// C delivers the current time once when the timer fires.
	C() <-chan time.Time
	// Stop prevents the timer from firing and reports whether it did.
	Stop() bool
	// Reset re-arms the timer for d and reports whether it had been active.
	Reset(d time.Duration) bool
}

// Clock is the time source. Now is wall-clock time; After and NewTimer
// schedule against the same source, so a fake that advances Now also fires
// the timers due by then.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
	NewTimer(d time.Duration) Timer
}

// Backoff is an exponential backoff policy. Tests inject one that returns
// zero.
type Backoff interface {
	// Next returns how long to wait before attempt n, counting from zero.
	Next(attempt int) time.Duration
}

// Real is the production Clock backed by the time package.
type Real struct{}

// Now implements Clock.
func (Real) Now() time.Time { return time.Now() }

// After implements Clock.
func (Real) After(d time.Duration) <-chan time.Time { return time.After(d) }

// NewTimer implements Clock.
func (Real) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }

// realTimer adapts *time.Timer to Timer.
type realTimer struct{ t *time.Timer }

// C implements Timer.
func (r realTimer) C() <-chan time.Time { return r.t.C }

// Stop implements Timer.
func (r realTimer) Stop() bool { return r.t.Stop() }

// Reset implements Timer.
func (r realTimer) Reset(d time.Duration) bool { return r.t.Reset(d) }

// Exponential is a Backoff that doubles Base on every attempt up to Max.
// A zero Max means no cap.
type Exponential struct {
	Base time.Duration
	Max  time.Duration
}

// Next implements Backoff.
func (e Exponential) Next(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	d := e.Base
	for range attempt {
		d *= 2
		if e.Max > 0 && d >= e.Max {
			return e.Max
		}
		if d <= 0 { // overflow
			return e.Max
		}
	}
	if e.Max > 0 && d > e.Max {
		return e.Max
	}
	return d
}

// Zero is a Backoff that never waits, for tests.
type Zero struct{}

// Next implements Backoff.
func (Zero) Next(int) time.Duration { return 0 }

// Compile-time interface checks.
var (
	_ Clock   = Real{}
	_ Backoff = Exponential{}
	_ Backoff = Zero{}
)
