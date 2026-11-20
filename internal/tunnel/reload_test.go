// SPDX-License-Identifier: AGPL-3.0-or-later

package tunnel

import (
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config/wgquick"
	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel/tunneltest"
)

// withComment returns the tunnel file text with comments and blank lines
// added: a different file with the same parsed content.
func withComment(text string) string {
	return "# edited\n\n" + strings.ReplaceAll(text, "\n", "  # note\n")
}

// withMTU returns the tunnel file text with an MTU line: a changed tunnel.
func withMTU(text string) string {
	return strings.Replace(text, "[Interface]\n", "[Interface]\nMTU = 1280\n", 1)
}

// names returns the names of tunnels.
func names(ts []*Tunnel) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name()
	}
	return out
}

// stopped returns a channel that is closed when t's device stops.
func stopped(t *Tunnel) <-chan struct{} {
	ch := make(chan struct{})
	var once sync.Once
	t.OnStop(func() { once.Do(func() { close(ch) }) })
	return ch
}

// waitClosed waits for ch, or fails tb after 10s.
func waitClosed(tb testing.TB, ch <-chan struct{}, what string) {
	tb.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		tb.Fatalf("%s: not within 10s", what)
	}
}

func TestApplyChanges(t *testing.T) {
	peers := map[string]*tunneltest.Peer{}
	for _, name := range []string{"a", "b", "c", "d"} {
		peers[name] = tunneltest.Start(t, tunneltest.Options{})
	}
	parse := func(name string, edit func(string) string) *wgquick.Tunnel {
		return tunneltest.ParseTunnel(t, name, edit(peers[name].ClientConfig()))
	}
	same := func(s string) string { return s }
	tests := []struct {
		name string
		next []*wgquick.Tunnel
		want Changes
		// kept are the tunnels that must keep their *Tunnel.
		kept []string
	}{
		{
			name: "only comments changed",
			next: []*wgquick.Tunnel{parse("a", withComment), parse("b", withComment), parse("c", same)},
			kept: []string{"a", "b", "c"},
		},
		{
			name: "changed",
			next: []*wgquick.Tunnel{parse("a", same), parse("b", withMTU), parse("c", same)},
			want: Changes{Changed: []string{"b"}},
			kept: []string{"a", "c"},
		},
		{
			name: "removed",
			next: []*wgquick.Tunnel{parse("c", same), parse("a", same)},
			want: Changes{Removed: []string{"b"}},
			kept: []string{"a", "c"},
		},
		{
			name: "added",
			next: []*wgquick.Tunnel{parse("d", same), parse("a", same), parse("b", same), parse("c", same)},
			want: Changes{Added: []string{"d"}},
			kept: []string{"a", "b", "c"},
		},
		{
			name: "all at once",
			next: []*wgquick.Tunnel{parse("a", withMTU), parse("d", same)},
			want: Changes{Added: []string{"d"}, Changed: []string{"a"}, Removed: []string{"b", "c"}},
		},
		{
			name: "empty",
			want: Changes{Removed: []string{"a", "b", "c"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _ := newManager(t, parse("a", same), parse("b", same), parse("c", same))
			before := map[string]*Tunnel{}
			for _, tun := range m.Tunnels() {
				before[tun.Name()] = tun
			}
			got, err := m.Apply(tt.next, testDrain)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if !slices.Equal(got.Added, tt.want.Added) || !slices.Equal(got.Changed, tt.want.Changed) || !slices.Equal(got.Removed, tt.want.Removed) {
				t.Errorf("Apply = %+v, want %+v", got, tt.want)
			}
			var wantNames []string
			for _, cfg := range tt.next {
				wantNames = append(wantNames, cfg.Name)
			}
			slices.Sort(wantNames)
			if gotNames := names(m.Tunnels()); !slices.Equal(gotNames, wantNames) {
				t.Errorf("Tunnels = %v, want %v", gotNames, wantNames)
			}
			for _, cfg := range tt.next {
				tun := mustTunnel(t, m, cfg.Name)
				if keep := slices.Contains(tt.kept, cfg.Name); (tun == before[cfg.Name]) != keep {
					t.Errorf("tunnel %s kept its Tunnel: %v, want %v", cfg.Name, !keep, keep)
				}
			}
			for _, name := range tt.want.Removed {
				if _, ok := m.Tunnel(name); ok {
					t.Errorf("removed tunnel %s still present", name)
				}
			}
		})
	}
}

func TestApplyKeepsUnchangedTunnelRunning(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	m, _ := newManagerIdle(t, testIdle, peer.ClientTunnel(t, "gr"))
	tun := mustTunnel(t, m, "gr")
	c, err := tun.DialContext(t.Context(), "tcp4", tunneltest.ServerAddr4.String()+":80")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	stops := stopped(tun)

	if _, err := m.Apply([]*wgquick.Tunnel{tunneltest.ParseTunnel(t, "gr", withComment(peer.ClientConfig()))}, testDrain); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := mustTunnel(t, m, "gr"); got != tun {
		t.Fatal("unchanged tunnel replaced")
	}
	select {
	case <-stops:
		t.Fatal("unchanged tunnel stopped")
	default:
	}
	// The open connection still works: same device, no new start.
	if _, err := io.WriteString(c, "GET / HTTP/1.0\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if b, err := io.ReadAll(c); err != nil || !strings.Contains(string(b), tunneltest.ClientAddr4.String()) {
		t.Errorf("response over the kept connection: %q, %v", b, err)
	}
	if n := tun.starts.Load(); n != 1 {
		t.Errorf("starts = %d, want 1", n)
	}
}

func TestApplyRetiresChangedAndRemovedTunnels(t *testing.T) {
	type env struct {
		m        *Manager
		env      *testEnv
		old      *Tunnel
		stops    <-chan struct{}
		nextPeer *tunneltest.Peer // peer of the changed config
		changeTo []*wgquick.Tunnel
	}
	setup := func(t *testing.T, remove bool) *env {
		t.Helper()
		peer := tunneltest.Start(t, tunneltest.Options{})
		other := tunneltest.Start(t, tunneltest.Options{Handler: tunneltest.EchoRemoteAddr})
		m, te := newManagerIdle(t, testIdle, peer.ClientTunnel(t, "gr"))
		e := &env{m: m, env: te, old: mustTunnel(t, m, "gr"), nextPeer: other}
		e.stops = stopped(e.old)
		if !remove {
			e.changeTo = []*wgquick.Tunnel{other.ClientTunnel(t, "gr")}
		}
		return e
	}
	target := tunneltest.ServerAddr4.String() + ":80"
	for _, remove := range []bool{false, true} {
		name := map[bool]string{false: "changed", true: "removed"}[remove]
		t.Run(name+"/idle tunnel closes at once and starts no more", func(t *testing.T) {
			e := setup(t, remove)
			if _, err := get(t, e.old, "http://"+target+"/"); err != nil {
				t.Fatal(err)
			}
			if _, err := e.m.Apply(e.changeTo, testDrain); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			waitClosed(t, e.stops, "retired tunnel stop")
			if e.old.Running() {
				t.Error("retired tunnel still running")
			}
			// The old Tunnel never starts again: a route of an old
			// configuration fails closed.
			_, err := e.old.DialContext(t.Context(), "tcp4", target)
			if !errors.Is(err, ErrUnavailable) {
				t.Errorf("dial on retired tunnel: err = %v, want ErrUnavailable", err)
			}
			if n := e.old.starts.Load(); n != 1 {
				t.Errorf("retired tunnel started %d times, want 1", n)
			}
			if remove {
				return
			}
			// The new tunnel starts lazily, with the new config.
			nt := mustTunnel(t, e.m, "gr")
			if nt.Running() {
				t.Error("new tunnel started before its first request")
			}
			body, err := get(t, nt, "http://"+target+"/")
			if err != nil || !strings.HasPrefix(body, tunneltest.ClientAddr4.String()+":") {
				t.Fatalf("request through new tunnel: %q, %v", body, err)
			}
			if e.nextPeer.Packets() == 0 {
				t.Error("new tunnel did not use the new peer")
			}
		})
		t.Run(name+"/in-flight request finishes, then the tunnel closes", func(t *testing.T) {
			e := setup(t, remove)
			end := e.old.Begin()
			c, err := e.old.DialContext(t.Context(), "tcp4", target)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			if _, err := e.m.Apply(e.changeTo, testDrain); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			e.env.clock.waitArmed(t, testDrain)
			e.env.clock.advance(testDrain - time.Nanosecond)
			// The stream goes on through the old device.
			if _, err := io.WriteString(c, "GET / HTTP/1.0\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 4)
			if _, err := io.ReadFull(c, buf); err != nil {
				t.Fatalf("in-flight connection broken by the reload: %v", err)
			}
			if !e.old.Running() {
				t.Fatal("retired tunnel closed with a request in flight")
			}
			end()
			waitClosed(t, e.stops, "retired tunnel stop after the request ended")
			if n := e.env.clock.pending(); n != 0 {
				t.Errorf("%d timers armed after the drain, want 0", n)
			}
		})
		t.Run(name+"/drain timeout cuts a stuck stream", func(t *testing.T) {
			e := setup(t, remove)
			end := e.old.Begin()
			defer end()
			c, err := e.old.DialContext(t.Context(), "tcp4", target)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			if _, err := e.m.Apply(e.changeTo, testDrain); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			e.env.clock.waitArmed(t, testDrain)
			e.env.clock.advance(testDrain)
			waitClosed(t, e.stops, "retired tunnel stop at the drain timeout")
			readFails(t, c)
		})
	}
}

func TestApplyCancelsStart(t *testing.T) {
	down := tunneltest.Start(t, tunneltest.Options{})
	down.SetReachable(false)
	up := tunneltest.Start(t, tunneltest.Options{})
	m, env := newManager(t, down.ClientTunnel(t, "gr"))
	old := mustTunnel(t, m, "gr")
	target := tunneltest.ServerAddr4.String() + ":80"

	const waiters = 3
	errs := make(chan error, waiters)
	for range waiters {
		go func() {
			c, err := old.DialContext(t.Context(), "tcp4", target)
			if c != nil {
				_ = c.Close()
			}
			errs <- err
		}()
	}
	// The start waits for a handshake that never comes.
	env.clock.waitTimers(1)
	if _, err := m.Apply([]*wgquick.Tunnel{up.ClientTunnel(t, "gr")}, testDrain); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for range waiters {
		select {
		case err := <-errs:
			if !errors.Is(err, ErrUnavailable) {
				t.Errorf("waiting request: err = %v, want ErrUnavailable", err)
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				t.Errorf("cancelled start is a timeout (would map to 504): %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("waiting request not released by the reload")
		}
	}
	if old.Running() {
		t.Error("cancelled tunnel running")
	}
	// The new tunnel starts lazily on the next request.
	nt := mustTunnel(t, m, "gr")
	if nt.Running() {
		t.Error("new tunnel started by the reload")
	}
	if _, err := get(t, nt, "http://"+target+"/"); err != nil {
		t.Fatalf("request after the reload: %v", err)
	}
}

func TestApplyErrorsChangeNothing(t *testing.T) {
	a := tunneltest.Start(t, tunneltest.Options{})
	b := tunneltest.Start(t, tunneltest.Options{})
	tests := []struct {
		name string
		next func(t *testing.T) []*wgquick.Tunnel
		want string
	}{
		{"duplicate name", func(t *testing.T) []*wgquick.Tunnel {
			return []*wgquick.Tunnel{a.ClientTunnel(t, "a"), b.ClientTunnel(t, "a")}
		}, "duplicate name"},
		{"same private key", func(t *testing.T) []*wgquick.Tunnel {
			return []*wgquick.Tunnel{a.ClientTunnel(t, "a"), a.ClientTunnel(t, "b")}
		}, "same PrivateKey"},
		{"nil", func(*testing.T) []*wgquick.Tunnel { return []*wgquick.Tunnel{nil} }, "nil tunnel"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _ := newManager(t, a.ClientTunnel(t, "a"))
			before := mustTunnel(t, m, "a")
			_, err := m.Apply(tt.next(t), testDrain)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Apply: err = %v, want %q", err, tt.want)
			}
			if got := m.Tunnels(); len(got) != 1 || got[0] != before {
				t.Errorf("Tunnels after a failed Apply = %v", names(got))
			}
		})
	}
}

func TestApplyAfterClose(t *testing.T) {
	m, _ := newManager(t)
	_ = m.Close()
	if _, err := m.Apply(nil, testDrain); !errors.Is(err, ErrClosed) {
		t.Errorf("Apply after Close: err = %v, want ErrClosed", err)
	}
}

func TestCloseCoversDrainingTunnels(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	m, _ := newManager(t, peer.ClientTunnel(t, "gr"))
	old := mustTunnel(t, m, "gr")
	stops := stopped(old)
	end := old.Begin()
	defer end()
	c, err := old.DialContext(t.Context(), "tcp4", tunneltest.ServerAddr4.String()+":80")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := m.Apply(nil, testDrain); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, stops, "draining tunnel stop")
	readFails(t, c)
}

func TestSetTimeouts(t *testing.T) {
	tests := []struct {
		name      string
		idle      time.Duration
		elapsed   time.Duration
		wantArmed time.Duration // -1: no timer
	}{
		{"shorter idle timeout counts from the last activity", 10 * time.Minute, 4 * time.Minute, 6 * time.Minute},
		{"longer idle timeout", 2 * testIdle, 4 * time.Minute, 2*testIdle - 4*time.Minute},
		{"already idle longer than the new timeout", 10 * time.Minute, 20 * time.Minute, 0},
		{"idle close disabled", 0, 4 * time.Minute, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := startLifecycle(t)
			l.request(t)
			l.env.clock.waitArmed(t, testIdle)
			l.env.clock.advance(tt.elapsed)
			if err := l.m.SetTimeouts(testConnectTimeout, tt.idle); err != nil {
				t.Fatal(err)
			}
			if got := l.env.clock.next(); got != tt.wantArmed || l.env.clock.pending() > 1 {
				t.Errorf("next timer in %s (%d armed), want %s", got, l.env.clock.pending(), tt.wantArmed)
			}
			if tt.wantArmed < 0 {
				return
			}
			l.env.clock.advance(tt.wantArmed)
			l.wantRunning(t, false, 1)
		})
	}
}

func TestSetTimeoutsConnect(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	peer.SetReachable(false)
	m, env := newManager(t, peer.ClientTunnel(t, "gr"))
	if err := m.SetTimeouts(time.Minute, 0); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := mustTunnel(t, m, "gr").DialContext(t.Context(), "tcp4", tunneltest.ServerAddr4.String()+":80")
		done <- err
	}()
	env.clock.waitArmed(t, time.Minute)
	env.clock.advance(time.Minute)
	if err := <-done; !errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
}

func TestSetTimeoutsErrors(t *testing.T) {
	m, _ := newManager(t)
	for _, tt := range []struct{ connect, idle time.Duration }{{0, 0}, {-time.Second, 0}, {time.Second, -time.Second}} {
		if err := m.SetTimeouts(tt.connect, tt.idle); err == nil {
			t.Errorf("SetTimeouts(%s, %s) succeeded", tt.connect, tt.idle)
		}
	}
}
