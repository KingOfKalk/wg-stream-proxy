// SPDX-License-Identifier: AGPL-3.0-or-later

package tunnel

import (
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel/tunneltest"
)

const (
	testIdle  = time.Hour
	testDrain = 30 * time.Second
)

// lifecycle is a started tunnel with an idle timeout and a counting stop
// hook.
type lifecycle struct {
	m     *Manager
	env   *testEnv
	tun   *Tunnel
	stops atomic.Int64
}

func startLifecycle(t *testing.T) *lifecycle {
	t.Helper()
	peer := tunneltest.Start(t, tunneltest.Options{})
	m, env := newManagerIdle(t, testIdle, peer.ClientTunnel(t, "gr"))
	l := &lifecycle{m: m, env: env, tun: mustTunnel(t, m, "gr")}
	l.tun.OnStop(func() { l.stops.Add(1) })
	return l
}

// request runs a whole request through the tunnel, like the route layer
// does between Begin and end.
func (l *lifecycle) request(t *testing.T) {
	t.Helper()
	end := l.tun.Begin()
	defer end()
	if _, err := get(t, l.tun, "http://"+tunneltest.ServerAddr4.String()+"/"); err != nil {
		t.Fatalf("get: %v", err)
	}
}

// dial opens a connection to the peer's HTTP port that stays open: a
// stream that never ends on its own.
func (l *lifecycle) dial(t *testing.T) net.Conn {
	t.Helper()
	c, err := l.tun.DialContext(t.Context(), "tcp4", tunneltest.ServerAddr4.String()+":80")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func (l *lifecycle) wantRunning(t *testing.T, want bool, stops int64) {
	t.Helper()
	if got := l.tun.Running(); got != want {
		t.Errorf("Running = %v, want %v", got, want)
	}
	if got := l.stops.Load(); got != stops {
		t.Errorf("stop hook ran %d times, want %d", got, stops)
	}
}

// readFails reads from c until the connection ends, or fails the test
// after 5s.
func readFails(t *testing.T, c net.Conn) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, c)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("connection still open after the tunnel closed")
	}
}

func TestIdleClose(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T, l *lifecycle)
	}{
		{
			name: "closes after idle_timeout without activity",
			run: func(t *testing.T, l *lifecycle) {
				l.request(t)
				l.env.clock.waitArmed(t, testIdle)
				l.env.clock.advance(testIdle - time.Nanosecond)
				l.wantRunning(t, true, 0)
				l.env.clock.advance(time.Nanosecond)
				l.wantRunning(t, false, 1)
				if l.tun.Up() || !l.tun.LastHandshake().IsZero() {
					t.Errorf("Up=%v LastHandshake=%v after idle close", l.tun.Up(), l.tun.LastHandshake())
				}
			},
		},
		{
			name: "open stream keeps it alive",
			run: func(t *testing.T, l *lifecycle) {
				end := l.tun.Begin()
				c := l.dial(t)
				if n := l.env.clock.pending(); n != 0 {
					t.Errorf("%d timers armed while a request is in flight, want 0", n)
				}
				l.env.clock.advance(3 * testIdle)
				l.wantRunning(t, true, 0)
				// The stream ends: the idle time starts now.
				_ = c.Close()
				end()
				end() // idempotent
				l.env.clock.waitArmed(t, testIdle)
				l.env.clock.advance(testIdle - time.Nanosecond)
				l.wantRunning(t, true, 0)
				l.env.clock.advance(time.Nanosecond)
				l.wantRunning(t, false, 1)
			},
		},
		{
			name: "activity restarts the idle time",
			run: func(t *testing.T, l *lifecycle) {
				l.request(t)
				l.env.clock.waitArmed(t, testIdle)
				l.env.clock.advance(testIdle / 2)
				l.request(t)
				l.env.clock.advance(testIdle / 2)
				l.wantRunning(t, true, 0)
				l.env.clock.advance(testIdle / 2)
				l.wantRunning(t, false, 1)
			},
		},
		{
			name: "uncounted lookups are activity too",
			run: func(t *testing.T, l *lifecycle) {
				l.request(t)
				l.env.clock.waitArmed(t, testIdle)
				l.env.clock.advance(testIdle - time.Minute)
				if _, err := l.tun.LookupNetIP(t.Context(), tunneltest.ServerAddr4.String()); err != nil {
					t.Fatalf("lookup: %v", err)
				}
				l.env.clock.advance(time.Minute)
				l.wantRunning(t, true, 0)
				l.env.clock.advance(testIdle - time.Minute)
				l.wantRunning(t, false, 1)
			},
		},
		{
			name: "next request starts it again",
			run: func(t *testing.T, l *lifecycle) {
				l.request(t)
				l.env.clock.waitArmed(t, testIdle)
				l.env.clock.advance(testIdle)
				l.wantRunning(t, false, 1)
				l.request(t)
				l.wantRunning(t, true, 1)
				if got := l.tun.starts.Load(); got != 2 {
					t.Errorf("devices created = %d, want 2", got)
				}
				l.env.clock.waitArmed(t, testIdle)
				l.env.clock.advance(testIdle)
				l.wantRunning(t, false, 2)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, startLifecycle(t))
		})
	}
}

func TestIdleCloseDisabled(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	m, env := newManager(t, peer.ClientTunnel(t, "gr"))
	tun := mustTunnel(t, m, "gr")
	end := tun.Begin()
	if _, err := get(t, tun, "http://"+tunneltest.ServerAddr4.String()+"/"); err != nil {
		t.Fatalf("get: %v", err)
	}
	end()
	if n := env.clock.pending(); n != 0 {
		t.Errorf("%d timers armed without idle timeout, want 0", n)
	}
	env.clock.advance(1000 * time.Hour)
	if !tun.Running() {
		t.Error("tunnel closed without idle timeout")
	}
}

func TestDrain(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T, l *lifecycle)
	}{
		{
			name: "nothing in flight closes at once",
			run: func(t *testing.T, l *lifecycle) {
				l.request(t)
				if err := l.m.Drain("gr", testDrain); err != nil {
					t.Fatalf("Drain: %v", err)
				}
				l.wantRunning(t, false, 1)
				if n := l.env.clock.pending(); n != 0 {
					t.Errorf("%d timers armed after drain, want 0", n)
				}
				// A drained tunnel starts again on the next request.
				l.request(t)
				l.wantRunning(t, true, 1)
			},
		},
		{
			name: "waits for in-flight requests",
			run: func(t *testing.T, l *lifecycle) {
				end := l.tun.Begin()
				c := l.dial(t)
				done := make(chan error, 1)
				go func() { done <- l.m.Drain("gr", testDrain) }()
				l.env.clock.waitArmed(t, testDrain)
				l.env.clock.advance(testDrain - time.Nanosecond)
				select {
				case err := <-done:
					t.Fatalf("Drain returned with a request in flight: %v", err)
				default:
				}
				l.wantRunning(t, true, 0)
				end()
				if err := <-done; err != nil {
					t.Fatalf("Drain: %v", err)
				}
				l.wantRunning(t, false, 1)
				readFails(t, c)
				if n := l.env.clock.pending(); n != 0 {
					t.Errorf("%d timers armed after drain, want 0", n)
				}
			},
		},
		{
			name: "timeout cuts a stuck stream",
			run: func(t *testing.T, l *lifecycle) {
				end := l.tun.Begin()
				defer end()
				c := l.dial(t)
				done := make(chan error, 1)
				go func() { done <- l.m.Drain("gr", testDrain) }()
				l.env.clock.waitArmed(t, testDrain)
				l.env.clock.advance(testDrain)
				if err := <-done; err != nil {
					t.Fatalf("Drain: %v", err)
				}
				l.wantRunning(t, false, 1)
				readFails(t, c)
			},
		},
		{
			name: "stopped tunnel",
			run: func(t *testing.T, l *lifecycle) {
				if err := l.m.Drain("gr", testDrain); err != nil {
					t.Fatalf("Drain: %v", err)
				}
				l.wantRunning(t, false, 0)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, startLifecycle(t))
		})
	}
}

func TestDrainUnknownTunnel(t *testing.T) {
	m, _ := newManager(t)
	if err := m.Drain("missing", testDrain); err == nil {
		t.Error("Drain(missing) succeeded")
	}
}

func TestShutdown(t *testing.T) {
	a := tunneltest.Start(t, tunneltest.Options{})
	b := tunneltest.Start(t, tunneltest.Options{})
	m, env := newManagerIdle(t, testIdle, a.ClientTunnel(t, "a"), b.ClientTunnel(t, "b"))
	ta, tb := mustTunnel(t, m, "a"), mustTunnel(t, m, "b")
	var stops atomic.Int64
	ta.OnStop(func() { stops.Add(1) })
	tb.OnStop(func() { stops.Add(1) })

	endA := ta.Begin()
	ca, err := ta.DialContext(t.Context(), "tcp4", tunneltest.ServerAddr4.String()+":80")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ca.Close() }()
	endB := tb.Begin()
	cb, err := tb.DialContext(t.Context(), "tcp4", tunneltest.ServerAddr4.String()+":80")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cb.Close() }()

	done := make(chan error, 1)
	go func() { done <- m.Shutdown(testDrain) }()
	env.clock.waitArmed(t, testDrain)
	endA()
	select {
	case err := <-done:
		t.Fatalf("Shutdown returned with a request in flight on b: %v", err)
	default:
	}
	// b's stream is stuck: the shared deadline cuts it.
	env.clock.advance(testDrain)
	if err := <-done; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	endB()
	readFails(t, cb)
	if ta.Running() || tb.Running() || stops.Load() != 2 {
		t.Errorf("after Shutdown: a running %v, b running %v, stop hooks %d; want stopped, 2", ta.Running(), tb.Running(), stops.Load())
	}
	if _, err := ta.DialContext(t.Context(), "tcp", tunneltest.ServerAddr4.String()+":80"); !errors.Is(err, ErrClosed) {
		t.Errorf("dial after Shutdown: err = %v, want ErrClosed", err)
	}
	if n := env.clock.pending(); n != 0 {
		t.Errorf("%d timers armed after Shutdown, want 0", n)
	}
}

func TestCloseRunsStopHook(t *testing.T) {
	l := startLifecycle(t)
	l.request(t)
	if err := l.m.Close(); err != nil {
		t.Fatal(err)
	}
	l.wantRunning(t, false, 1)
	if n := l.env.clock.pending(); n != 0 {
		t.Errorf("%d timers armed after Close, want 0", n)
	}
	// Activity after Close arms nothing.
	l.tun.Begin()()
	if n := l.env.clock.pending(); n != 0 {
		t.Errorf("%d timers armed after Close and a request, want 0", n)
	}
}
