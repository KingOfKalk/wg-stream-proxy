// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
)

// watchClock is a manual route.Clock: a timer fires only in tick. Each
// AfterFunc call is reported on armed, so a test knows when the watcher
// has finished a step and waits for the next one.
type watchClock struct {
	mu     sync.Mutex
	timers []*watchTimer
	armed  chan time.Duration
}

type watchTimer struct {
	c      *watchClock
	f      func()
	active bool
}

func newWatchClock() *watchClock { return &watchClock{armed: make(chan time.Duration, 1000)} }

func (c *watchClock) AfterFunc(d time.Duration, f func()) route.Timer {
	c.mu.Lock()
	t := &watchTimer{c: c, f: f, active: true}
	c.timers = append(c.timers, t)
	c.mu.Unlock()
	c.armed <- d
	return t
}

func (t *watchTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.active
	t.active = false
	return was
}

func (t *watchTimer) Reset(time.Duration) bool { panic("not used") }

// waitArmed waits until the watcher has armed a timer and returns its
// duration.
func (c *watchClock) waitArmed(t *testing.T) time.Duration {
	t.Helper()
	select {
	case d := <-c.armed:
		return d
	case <-time.After(10 * time.Second):
		t.Fatal("the watcher did not arm its timer")
		return 0
	}
}

// tick fires the armed timer and waits until the watcher has finished
// its step and armed the next timer.
func (c *watchClock) tick(t *testing.T) time.Duration {
	t.Helper()
	c.mu.Lock()
	var f func()
	for _, tm := range c.timers {
		if tm.active {
			tm.active = false
			f = tm.f
		}
	}
	c.mu.Unlock()
	if f == nil {
		t.Fatal("no timer armed")
	}
	f()
	return c.waitArmed(t)
}

// watchEnv runs Watch against a scripted fingerprint.
type watchEnv struct {
	t     *testing.T
	clk   *watchClock
	mu    sync.Mutex
	fp    string
	calls int   // reloads
	err   error // result of the next reloads
	// onReload runs inside Reload, if set.
	onReload func()
	stop     context.CancelFunc
	done     chan struct{}
}

func startWatch(t *testing.T, initial string, interval time.Duration) *watchEnv {
	t.Helper()
	e := &watchEnv{t: t, clk: newWatchClock(), fp: initial, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	e.stop = cancel
	go func() {
		defer close(e.done)
		Watch(ctx, WatchOptions{
			Interval: interval,
			Clock:    e.clk,
			Initial:  initial,
			Fingerprint: func() string {
				e.mu.Lock()
				defer e.mu.Unlock()
				return e.fp
			},
			Reload: func() error {
				e.mu.Lock()
				e.calls++
				err, hook := e.err, e.onReload
				e.mu.Unlock()
				if hook != nil {
					hook()
				}
				return err
			},
			Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
	}()
	t.Cleanup(e.shutdown)
	if got := e.clk.waitArmed(t); got != interval {
		t.Fatalf("first timer = %v, want the interval %v", got, interval)
	}
	return e
}

func (e *watchEnv) shutdown() {
	e.stop()
	select {
	case <-e.done:
	case <-time.After(10 * time.Second):
		e.t.Error("Watch did not return after its context ended")
	}
}

func (e *watchEnv) set(fp string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.fp = fp
}

func (e *watchEnv) setErr(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.err = err
}

func (e *watchEnv) setHook(f func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onReload = f
}

func (e *watchEnv) reloads() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

// TestWatch scripts the fingerprint seen at each tick (the first value
// is the initial one) and checks after which ticks a reload ran.
func TestWatch(t *testing.T) {
	tests := []struct {
		name string
		fps  []string // fingerprint at the initial state, then at tick 1, 2, ...
		want []int    // total reloads after each tick
	}{
		{"unchanged", []string{"a", "a", "a", "a"}, []int{0, 0, 0}},
		{"one change reloads once it is stable", []string{"a", "b", "b", "b", "b"}, []int{0, 1, 1, 1}},
		{"burst of writes reloads once", []string{"a", "b", "c", "d", "d", "d"}, []int{0, 0, 0, 1, 1}},
		{"change and back before it is stable", []string{"a", "b", "a", "a"}, []int{0, 0, 0}},
		{"second change after a reload", []string{"a", "b", "b", "c", "c", "c"}, []int{0, 1, 1, 2, 2}},
		{"change that settles on another value", []string{"a", "b", "c", "c", "c"}, []int{0, 0, 1, 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := startWatch(t, tt.fps[0], 2*time.Second)
			for i, fp := range tt.fps[1:] {
				e.set(fp)
				e.clk.tick(t)
				if got := e.reloads(); got != tt.want[i] {
					t.Fatalf("after tick %d (fingerprint %q): %d reloads, want %d", i+1, fp, got, tt.want[i])
				}
			}
		})
	}
}

// TestWatchRearmsWithInterval: the next check is one interval after the
// previous one ended, so a slow reload does not queue ticks.
func TestWatchRearmsWithInterval(t *testing.T) {
	e := startWatch(t, "a", 750*time.Millisecond)
	e.set("b")
	for range 3 {
		if got := e.clk.tick(t); got != 750*time.Millisecond {
			t.Errorf("re-armed with %v, want 750ms", got)
		}
	}
	if got := e.reloads(); got != 1 {
		t.Fatalf("reloads = %d, want 1", got)
	}
}

// TestWatchRejectedReloadIsNotRetried: a reload that fails (broken
// file) is not repeated every interval; the next change reloads again.
func TestWatchRejectedReloadIsNotRetried(t *testing.T) {
	e := startWatch(t, "a", time.Second)
	e.setErr(errors.New("reload rejected"))
	e.set("broken")
	for range 5 {
		e.clk.tick(t)
	}
	if got := e.reloads(); got != 1 {
		t.Fatalf("reloads = %d, want 1", got)
	}
	e.setErr(nil)
	e.set("fixed")
	e.clk.tick(t)
	e.clk.tick(t)
	if got := e.reloads(); got != 2 {
		t.Errorf("reloads after the fix = %d, want 2", got)
	}
}

// TestWatchNotServing: before Run serves, the change is kept and tried
// again at the next check.
func TestWatchNotServing(t *testing.T) {
	e := startWatch(t, "a", time.Second)
	e.setErr(ErrNotServing)
	e.set("b")
	e.clk.tick(t)
	e.clk.tick(t)
	e.clk.tick(t)
	if got := e.reloads(); got != 2 {
		t.Fatalf("reloads = %d, want 2 (both stable checks)", got)
	}
	e.setErr(nil)
	e.clk.tick(t)
	e.clk.tick(t)
	if got := e.reloads(); got != 3 {
		t.Errorf("reloads = %d, want 3 (one more that succeeds, then none)", got)
	}
}

// TestWatchChangeDuringReload: a write while a reload runs is not lost.
func TestWatchChangeDuringReload(t *testing.T) {
	e := startWatch(t, "a", time.Second)
	e.setHook(func() { e.set("c") })
	e.set("b")
	e.clk.tick(t)
	e.clk.tick(t) // reloads for "b"; "c" is written meanwhile
	e.setHook(nil)
	e.clk.tick(t)
	e.clk.tick(t)
	if got := e.reloads(); got != 2 {
		t.Errorf("reloads = %d, want 2", got)
	}
}

// TestWatchStops: Watch returns when its context ends and leaves no
// timer or goroutine behind (goleak checks the latter).
func TestWatchStops(t *testing.T) {
	e := startWatch(t, "a", time.Second)
	e.shutdown()
	e.clk.mu.Lock()
	defer e.clk.mu.Unlock()
	for _, tm := range e.clk.timers {
		if tm.active {
			t.Errorf("a timer is still armed after Watch returned")
		}
	}
}
