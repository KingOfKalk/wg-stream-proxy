// SPDX-License-Identifier: AGPL-3.0-or-later

package tunnel

import (
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// fakeClock is a manually advanced Clock.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
	added  chan struct{}
}

type fakeTimer struct {
	c       *fakeClock
	when    time.Time
	f       func()
	stopped bool
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Now(), added: make(chan struct{}, 1)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: c, when: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	select {
	case c.added <- struct{}{}:
	default:
	}
	return t
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	if t.stopped {
		return false
	}
	t.stopped = true
	return true
}

// pending returns the number of armed timers.
func (c *fakeClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if !t.stopped {
			n++
		}
	}
	return n
}

// next returns the time until the earliest armed timer is due, or -1.
func (c *fakeClock) next() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	d := time.Duration(-1)
	for _, t := range c.timers {
		if !t.stopped && (d < 0 || t.when.Sub(c.now) < d) {
			d = t.when.Sub(c.now)
		}
	}
	return d
}

// waitTimers blocks until at least n timers are armed.
func (c *fakeClock) waitTimers(n int) {
	for c.pending() < n {
		<-c.added
	}
}

// armedIn reports whether a timer is armed that is due d from now.
func (c *fakeClock) armedIn(d time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.timers {
		if !t.stopped && t.when.Sub(c.now) == d {
			return true
		}
	}
	return false
}

// waitArmed blocks until a timer is armed that is due d from now. It
// fails tb if none is armed within 10s.
func (c *fakeClock) waitArmed(tb testing.TB, d time.Duration) {
	tb.Helper()
	giveUp := time.After(10 * time.Second)
	for !c.armedIn(d) {
		select {
		case <-c.added:
		case <-giveUp:
			tb.Fatalf("no timer armed for %s", d)
		}
	}
}

// advance moves the clock forward and runs the timers that are due.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []func()
	for _, t := range c.timers {
		if !t.stopped && !t.when.After(c.now) {
			t.stopped = true
			due = append(due, t.f)
		}
	}
	c.mu.Unlock()
	for _, f := range due {
		f()
	}
}
