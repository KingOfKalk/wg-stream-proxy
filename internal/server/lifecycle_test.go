// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel"
	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel/tunneltest"
)

// tunnelClock is a manual tunnel.Clock: timers fire only in advance.
type tunnelClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*tunnelTimer
	added  chan struct{}
}

type tunnelTimer struct {
	c       *tunnelClock
	when    time.Time
	f       func()
	stopped bool
}

func newTunnelClock() *tunnelClock {
	return &tunnelClock{now: time.Now(), added: make(chan struct{}, 1)}
}

func (c *tunnelClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *tunnelClock) AfterFunc(d time.Duration, f func()) tunnel.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &tunnelTimer{c: c, when: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	select {
	case c.added <- struct{}{}:
	default:
	}
	return t
}

func (t *tunnelTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := !t.stopped
	t.stopped = true
	return was
}

// armed returns the durations from now of the armed timers.
func (c *tunnelClock) armed() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []time.Duration
	for _, t := range c.timers {
		if !t.stopped {
			out = append(out, t.when.Sub(c.now))
		}
	}
	return out
}

// waitArmed blocks until a timer is armed that is due d from now. It
// fails tb if none is armed within 10s.
func (c *tunnelClock) waitArmed(tb testing.TB, d time.Duration) {
	tb.Helper()
	giveUp := time.After(10 * time.Second)
	for !slices.Contains(c.armed(), d) {
		select {
		case <-c.added:
		case <-giveUp:
			tb.Fatalf("no tunnel timer armed for %s", d)
		}
	}
}

// advance moves the clock forward and runs the timers that are due.
func (c *tunnelClock) advance(d time.Duration) {
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

// TestRunTunnelIdleClose is the L3 lifecycle path of testing.md: pooled
// upstream connections do not keep a tunnel up, an open stream does, and
// the next request after an idle close starts the tunnel again.
func TestRunTunnelIdleClose(t *testing.T) {
	const idle = 10 * time.Minute
	up := newUpstream()
	up.add("/seg.ts", file{ctype: "video/mp2t", body: "segment"})
	up.add("/radio", file{ctype: "audio/mpeg", body: binaryBody('a', 1024), endless: true})
	peer := tunneltest.Start(t, tunneltest.Options{Handler: up})
	peer.SetHost("upstream.test", tunneltest.ServerAddr4)
	dir := writeConfig(t, map[string]string{
		"config.toml":     "[tunnels]\nidle_timeout = \"10m\"\n",
		"tunnels/gr.conf": peer.ClientConfig(),
		"channels.d/gr.toml": `
[[channel]]
name = "Seg"
url = "http://upstream.test/seg.ts"
[[channel]]
name = "Radio"
url = "http://upstream.test/radio"
`,
	})
	clk := newTunnelClock()
	e := startRun(t, dir, 0, func(o *RunOptions) { o.TunnelClock = clk })
	idleCloses := func() int { return strings.Count(e.logs.String(), `msg="tunnel closed" tunnel=gr reason=idle`) }
	starts := func() int { return strings.Count(e.logs.String(), `msg="tunnel started" tunnel=gr`) }

	// A finished request leaves a pooled connection behind; the tunnel
	// closes anyway.
	if resp, body := do(t, e.client, http.MethodGet, e.url+"/c/seg", nil); resp.StatusCode != http.StatusOK || body != "segment" {
		t.Fatalf("GET /c/seg: status %d, body %q", resp.StatusCode, body)
	}
	clk.waitArmed(t, idle)
	clk.advance(idle - time.Second)
	if n := idleCloses(); n != 0 {
		t.Fatalf("tunnel closed before idle_timeout: %d", n)
	}
	clk.advance(time.Second)
	if n := idleCloses(); n != 1 {
		t.Fatalf("idle closes = %d, want 1\n%s", n, e.logs.String())
	}

	// The next request starts it again, over a new connection.
	if resp, body := do(t, e.client, http.MethodGet, e.url+"/c/seg", nil); resp.StatusCode != http.StatusOK || body != "segment" {
		t.Fatalf("GET /c/seg after idle close: status %d, body %q", resp.StatusCode, body)
	}
	if n := starts(); n != 2 {
		t.Errorf("tunnel starts = %d, want 2", n)
	}

	// An open stream keeps it up past idle_timeout.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, e.url+"/c/radio", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(resp.Body, make([]byte, 4096)); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	clk.advance(3 * idle)
	if n := idleCloses(); n != 1 {
		t.Errorf("idle closes with an open stream = %d, want 1", n)
	}
	if _, err := io.ReadFull(resp.Body, make([]byte, 4096)); err != nil {
		t.Fatalf("stream broken after idle_timeout: %v", err)
	}
	// The player leaves: the idle time starts.
	_ = resp.Body.Close()
	clk.waitArmed(t, idle)
	clk.advance(idle)
	if n := idleCloses(); n != 2 {
		t.Errorf("idle closes after the stream ended = %d, want 2", n)
	}
}
