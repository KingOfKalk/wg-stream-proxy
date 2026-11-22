// SPDX-License-Identifier: AGPL-3.0-or-later

package tunnel

import (
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config/wgquick"
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

// background runs a whole background request (an EPG fetch) through the
// tunnel, between BeginBackground and end.
func (l *lifecycle) background(t *testing.T) {
	t.Helper()
	end := l.tun.BeginBackground()
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
			name: "background requests are not activity",
			run: func(t *testing.T, l *lifecycle) {
				l.request(t)
				l.env.clock.waitArmed(t, testIdle)
				l.env.clock.advance(testIdle / 2)
				// Its lookup and dial are not activity either.
				l.background(t)
				l.env.clock.waitArmed(t, testIdle/2)
				l.env.clock.advance(testIdle/2 - time.Nanosecond)
				l.wantRunning(t, true, 0)
				l.env.clock.advance(time.Nanosecond)
				l.wantRunning(t, false, 1)
			},
		},
		{
			name: "background requests alone: closes when each ends",
			run: func(t *testing.T, l *lifecycle) {
				// The first one starts the tunnel and is in flight for 20m.
				end := l.tun.BeginBackground()
				if _, err := get(t, l.tun, "http://"+tunneltest.ServerAddr4.String()+"/"); err != nil {
					t.Fatalf("get: %v", err)
				}
				l.env.clock.advance(20 * time.Minute)
				l.wantRunning(t, true, 0)
				end()
				// The start is no activity: nothing in idle_timeout.
				l.env.clock.waitArmed(t, 0)
				l.env.clock.advance(0)
				l.wantRunning(t, false, 1)
				// A failing EPG source retried every 10 minutes starts it
				// for each fetch only.
				for i := range 3 {
					l.env.clock.advance(10 * time.Minute)
					l.background(t)
					l.env.clock.waitArmed(t, 0)
					l.env.clock.advance(0)
					l.wantRunning(t, false, int64(i+2))
				}
				if got := l.tun.starts.Load(); got != 4 {
					t.Errorf("devices created = %d, want 4", got)
				}
			},
		},
		{
			name: "request during a background request that started it",
			run: func(t *testing.T, l *lifecycle) {
				endBackground := l.tun.BeginBackground()
				c := l.dial(t) // starts the tunnel
				l.request(t)
				l.env.clock.advance(5 * time.Minute)
				l.request(t)
				if n := l.env.clock.pending(); n != 0 {
					t.Errorf("%d timers armed while a background request is in flight, want 0", n)
				}
				l.env.clock.advance(10 * time.Minute)
				_ = c.Close()
				endBackground()
				// The idle time runs from the end of the request.
				l.env.clock.waitArmed(t, testIdle-10*time.Minute)
				l.env.clock.advance(testIdle - 10*time.Minute - time.Nanosecond)
				l.wantRunning(t, true, 0)
				l.env.clock.advance(time.Nanosecond)
				l.wantRunning(t, false, 1)
			},
		},
		{
			name: "background request in flight keeps it up",
			run: func(t *testing.T, l *lifecycle) {
				l.request(t)
				l.env.clock.waitArmed(t, testIdle)
				end := l.tun.BeginBackground()
				c := l.dial(t)
				if n := l.env.clock.pending(); n != 0 {
					t.Errorf("%d timers armed while a request is in flight, want 0", n)
				}
				l.env.clock.advance(2 * testIdle)
				l.wantRunning(t, true, 0)
				// The idle time is over when it ends: the tunnel closes.
				_ = c.Close()
				end()
				end() // idempotent
				l.env.clock.waitArmed(t, 0)
				l.env.clock.advance(0)
				l.wantRunning(t, false, 1)
			},
		},
		{
			name: "request ends during a background request",
			run: func(t *testing.T, l *lifecycle) {
				endBackground := l.tun.BeginBackground()
				c := l.dial(t)
				end := l.tun.Begin()
				l.env.clock.advance(10 * time.Minute)
				end()
				if n := l.env.clock.pending(); n != 0 {
					t.Errorf("%d timers armed while a background request is in flight, want 0", n)
				}
				l.env.clock.advance(20 * time.Minute)
				_ = c.Close()
				endBackground()
				// The idle time runs from the end of the request.
				l.env.clock.waitArmed(t, testIdle-20*time.Minute)
				l.env.clock.advance(testIdle - 20*time.Minute - time.Nanosecond)
				l.wantRunning(t, true, 0)
				l.env.clock.advance(time.Nanosecond)
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
				if err := drain(l.m, "gr", testDrain); err != nil {
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
				go func() { done <- drain(l.m, "gr", testDrain) }()
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
			name: "waits for background requests",
			run: func(t *testing.T, l *lifecycle) {
				end := l.tun.BeginBackground()
				l.dial(t)
				done := make(chan error, 1)
				go func() { done <- drain(l.m, "gr", testDrain) }()
				l.env.clock.waitArmed(t, testDrain)
				select {
				case err := <-done:
					t.Fatalf("Drain returned with a background request in flight: %v", err)
				default:
				}
				if got := l.tun.Snapshot().ActiveRequests; got != 1 {
					t.Errorf("ActiveRequests = %d, want 1", got)
				}
				end()
				if err := <-done; err != nil {
					t.Fatalf("Drain: %v", err)
				}
				l.wantRunning(t, false, 1)
			},
		},
		{
			name: "timeout cuts a stuck stream",
			run: func(t *testing.T, l *lifecycle) {
				end := l.tun.Begin()
				defer end()
				c := l.dial(t)
				done := make(chan error, 1)
				go func() { done <- drain(l.m, "gr", testDrain) }()
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
				if err := drain(l.m, "gr", testDrain); err != nil {
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
	if err := drain(m, "missing", testDrain); err == nil {
		t.Error("Drain(missing) succeeded")
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

// startRecorder records the calls of Options.OnStart.
type startRecorder struct {
	mu    sync.Mutex
	names []string
	took  []time.Duration
}

func (r *startRecorder) record(name string, took time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.names = append(r.names, name)
	r.took = append(r.took, took)
}

func (r *startRecorder) snapshot() ([]string, []time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.names), slices.Clone(r.took)
}

// TestOnStart checks that OnStart is called once per successful start, with
// the tunnel name and the start duration measured with the tunnel clock,
// not for a failed start and not for the requests that wait for a start or
// use the running tunnel.
func TestOnStart(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})})
	peer.SetReachable(false)
	rec := &startRecorder{}
	clk := newFakeClock()
	m, err := New([]*wgquick.Tunnel{peer.ClientTunnel(t, "gr")}, Options{
		ConnectTimeout: testConnectTimeout, Clock: clk, OnStart: rec.record,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	tun := mustTunnel(t, m, "gr")
	target := "http://" + tunneltest.ServerAddr4.String() + "/"

	// A failed start (no handshake within the connect timeout) is not
	// observed.
	done := make(chan error, 1)
	go func() {
		_, err := get(t, tun, target)
		done <- err
	}()
	clk.waitTimers(1)
	peer.WaitPackets(t, 1)
	clk.advance(testConnectTimeout)
	if err := <-done; !errors.Is(err, ErrUnavailable) {
		t.Fatalf("get without handshake: %v, want ErrUnavailable", err)
	}
	if names, _ := rec.snapshot(); len(names) != 0 {
		t.Fatalf("OnStart called for a failed start: %v", names)
	}

	// Concurrent requests share one start: one call.
	peer.SetReachable(true)
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := get(t, tun, target); err != nil {
				t.Errorf("get: %v", err)
			}
		}()
	}
	wg.Wait()
	// The running tunnel serves another request without a start.
	if _, err := get(t, tun, target); err != nil {
		t.Fatalf("get on running tunnel: %v", err)
	}
	names, took := rec.snapshot()
	if !slices.Equal(names, []string{"gr"}) {
		t.Fatalf("OnStart calls = %v, want one for gr", names)
	}
	// The fake clock did not move during the start.
	if took[0] != 0 {
		t.Errorf("start duration = %s, want 0s on the fake clock", took[0])
	}
}

// drain stops a tunnel gracefully: a Cooldown that drops its result.
func drain(m *Manager, name string, timeout time.Duration) error {
	_, err := m.Cooldown(name, timeout)
	return err
}
