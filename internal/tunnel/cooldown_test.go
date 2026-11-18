// SPDX-License-Identifier: AGPL-3.0-or-later

package tunnel

import (
	"errors"
	"log/slog"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config/wgquick"
	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel/tunneltest"
)

// cooldownSetup is a tunnel with an idle timeout, a counting stop hook
// and a log buffer.
type cooldownSetup struct {
	*lifecycle
	peer *tunneltest.Peer
	logs *lockedBuffer
}

func startCooldown(t *testing.T) *cooldownSetup {
	t.Helper()
	peer := tunneltest.Start(t, tunneltest.Options{})
	peer.SetHost("upstream.test", tunneltest.ServerAddr4)
	log, buf := testLogger(slog.LevelInfo)
	env := &testEnv{clock: newFakeClock(), resolver: &recordingResolver{hosts: map[string][]netip.Addr{}}}
	m, err := New([]*wgquick.Tunnel{peer.ClientTunnel(t, "gr")}, Options{
		ConnectTimeout: testConnectTimeout,
		IdleTimeout:    testIdle,
		Clock:          env.clock,
		HostResolver:   env.resolver,
		Logger:         log,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	l := &lifecycle{m: m, env: env, tun: mustTunnel(t, m, "gr")}
	l.tun.OnStop(func() { l.stops.Add(1) })
	return &cooldownSetup{lifecycle: l, peer: peer, logs: buf}
}

// cooldownLines counts the close lines of a cool-down.
func (s *cooldownSetup) cooldownLines() int {
	return strings.Count(s.logs.String(), `msg="tunnel closed" tunnel=gr reason=cooldown`)
}

func TestCooldown(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T, s *cooldownSetup)
	}{
		{
			name: "idle tunnel stops at once and starts cold",
			run: func(t *testing.T, s *cooldownSetup) {
				lookup := func() {
					t.Helper()
					if _, err := s.tun.LookupNetIP(t.Context(), "upstream.test"); err != nil {
						t.Fatalf("lookup: %v", err)
					}
				}
				lookup()
				lookup()
				if got := len(s.peer.Queries()); got != 1 {
					t.Fatalf("queries before cool-down = %d, want 1 (cached)", got)
				}
				stopped, err := s.m.Cooldown("gr", testDrain)
				if err != nil || !stopped {
					t.Fatalf("Cooldown = %v, %v; want true, nil", stopped, err)
				}
				s.wantRunning(t, false, 1)
				if n := s.cooldownLines(); n != 1 {
					t.Errorf("cool-down close lines = %d, want 1:\n%s", n, s.logs.String())
				}
				if n := s.env.clock.pending(); n != 0 {
					t.Errorf("%d timers armed after cool-down, want 0", n)
				}
				// The next request starts the device again, with an empty
				// DNS cache.
				lookup()
				if got := s.tun.starts.Load(); got != 2 {
					t.Errorf("starts = %d, want 2", got)
				}
				if got := len(s.peer.Queries()); got != 2 {
					t.Errorf("queries after cool-down = %d, want 2 (cache emptied)", got)
				}
			},
		},
		{
			name: "stopped tunnel is a no-op",
			run: func(t *testing.T, s *cooldownSetup) {
				stopped, err := s.m.Cooldown("gr", testDrain)
				if err != nil || stopped {
					t.Fatalf("Cooldown = %v, %v; want false, nil", stopped, err)
				}
				s.wantRunning(t, false, 0)
				if n := s.cooldownLines(); n != 0 {
					t.Errorf("cool-down close lines = %d, want 0", n)
				}
			},
		},
		{
			name: "waits for in-flight requests",
			run: func(t *testing.T, s *cooldownSetup) {
				end := s.tun.Begin()
				c := s.dial(t)
				type result struct {
					stopped bool
					err     error
				}
				done := make(chan result, 1)
				go func() {
					stopped, err := s.m.Cooldown("gr", testDrain)
					done <- result{stopped, err}
				}()
				s.env.clock.waitArmed(t, testDrain)
				if got := s.tun.Snapshot().State; got != StateDraining {
					t.Errorf("state while draining = %q, want %q", got, StateDraining)
				}
				s.env.clock.advance(testDrain - time.Nanosecond)
				select {
				case r := <-done:
					t.Fatalf("Cooldown returned with a request in flight: %+v", r)
				default:
				}
				s.wantRunning(t, true, 0)
				end()
				if r := <-done; r.err != nil || !r.stopped {
					t.Fatalf("Cooldown = %+v, want stopped", r)
				}
				s.wantRunning(t, false, 1)
				readFails(t, c)
				if got := s.tun.Snapshot().State; got != StateStopped {
					t.Errorf("state after cool-down = %q, want %q", got, StateStopped)
				}
			},
		},
		{
			name: "timeout cuts a stuck stream",
			run: func(t *testing.T, s *cooldownSetup) {
				end := s.tun.Begin()
				defer end()
				c := s.dial(t)
				done := make(chan error, 1)
				go func() {
					_, err := s.m.Cooldown("gr", testDrain)
					done <- err
				}()
				s.env.clock.waitArmed(t, testDrain)
				s.env.clock.advance(testDrain)
				if err := <-done; err != nil {
					t.Fatalf("Cooldown: %v", err)
				}
				s.wantRunning(t, false, 1)
				readFails(t, c)
				if n := s.cooldownLines(); n != 1 {
					t.Errorf("cool-down close lines = %d, want 1", n)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, startCooldown(t))
		})
	}
}

func TestCooldownErrors(t *testing.T) {
	s := startCooldown(t)
	if _, err := s.m.Cooldown("missing", testDrain); !errors.Is(err, ErrNoSuchTunnel) {
		t.Errorf("Cooldown(missing) error = %v, want ErrNoSuchTunnel", err)
	}
	if err := s.m.Drain("missing", testDrain); !errors.Is(err, ErrNoSuchTunnel) {
		t.Errorf("Drain(missing) error = %v, want ErrNoSuchTunnel", err)
	}
	if err := s.m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.Cooldown("gr", testDrain); !errors.Is(err, ErrClosed) {
		t.Errorf("Cooldown after Close error = %v, want ErrClosed", err)
	}
}

func TestSnapshot(t *testing.T) {
	t.Run("stopped and running", func(t *testing.T) {
		s := startCooldown(t)
		want := Snapshot{Name: "gr", State: StateStopped}
		if got := s.tun.Snapshot(); got != want {
			t.Errorf("fresh Snapshot = %+v, want %+v", got, want)
		}
		started := s.env.clock.Now()
		s.request(t)
		s.env.clock.advance(time.Minute)
		end := s.tun.Begin()
		defer end()
		got := s.tun.Snapshot()
		if got.State != StateRunning || !got.StartedAt.Equal(started) || got.LastHandshake.IsZero() || got.ActiveRequests != 1 {
			t.Errorf("running Snapshot = %+v, want running since %v with a handshake and 1 request", got, started)
		}
	})

	t.Run("starting", func(t *testing.T) {
		s := startCooldown(t)
		s.peer.SetReachable(false)
		done := make(chan error, 1)
		go func() {
			_, err := s.tun.LookupNetIP(t.Context(), "upstream.test")
			done <- err
		}()
		s.env.clock.waitTimers(1)
		if got := s.tun.Snapshot().State; got != StateStarting {
			t.Errorf("state during start = %q, want %q", got, StateStarting)
		}
		s.env.clock.advance(testConnectTimeout)
		if err := <-done; !errors.Is(err, ErrUnavailable) {
			t.Fatalf("lookup err = %v, want ErrUnavailable", err)
		}
		if got := s.tun.Snapshot().State; got != StateStopped {
			t.Errorf("state after failed start = %q, want %q", got, StateStopped)
		}
	})
}
