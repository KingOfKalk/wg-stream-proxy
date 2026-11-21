// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
)

// holdHeader marks the request a test holds before an upstream fetch.
const holdHeader = "X-Test-Hold"

// fetchHold holds the request with holdHeader before its first fetch or
// its refetch, once.
type fetchHold struct {
	refetch bool
	reached chan struct{}
	release chan struct{}
	once    sync.Once
	free    sync.Once
}

func newFetchHold(t *testing.T, refetch bool) *fetchHold {
	h := &fetchHold{refetch: refetch, reached: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(h.done)
	return h
}

// before is RunOptions.beforeFetch.
func (h *fetchHold) before(r *http.Request, refetch bool) {
	if r.Header.Get(holdHeader) == "" || refetch != h.refetch {
		return
	}
	h.once.Do(func() { close(h.reached) })
	select {
	case <-h.release:
	case <-r.Context().Done():
	}
}

// wait blocks until the request is held, or fails tb after 10s.
func (h *fetchHold) wait(tb testing.TB) {
	tb.Helper()
	select {
	case <-h.reached:
	case <-time.After(10 * time.Second):
		tb.Fatal("request not held before its fetch")
	}
}

// done releases the held request.
func (h *fetchHold) done() { h.free.Do(func() { close(h.release) }) }

// TestReloadKeepsTunnelForEarlierRequests: a request that took its
// generation before a reload changed or removed its tunnel, and fetches
// only after the new generation serves (its first fetch, or the refetch
// of a 206 playlist), still gets the old tunnel: the reload swaps the
// generation before it retires the tunnel, and the request counts on the
// tunnel from its start, so the drain waits for it (spec 5.6). Only then
// does the old tunnel close.
func TestReloadKeepsTunnelForEarlierRequests(t *testing.T) {
	const playlist = "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2,\nseg.ts\n"
	// No .m3u8 in the path: the client Range is forwarded (spec 3.4).
	channels := tunnelChannels("gr") + "[[channel]]\nname = \"gr live\"\nurl = \"http://upstream.test/live\"\n"
	tests := []struct {
		name    string
		remove  bool
		refetch bool
	}{
		{name: "changed/first fetch after the swap"},
		{name: "changed/206 refetch after the swap", refetch: true},
		{name: "removed/first fetch after the swap", remove: true},
		{name: "removed/206 refetch after the swap", remove: true, refetch: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldPeer, oldUp := newPeer(t, "old")
			oldUp.add("/live", file{ctype: playlistContentType, body: playlist})
			nextPeer, _ := newPeer(t, "new")
			hold := newFetchHold(t, tt.refetch)
			dir := writeConfig(t, map[string]string{"tunnels/gr.conf": oldPeer.ClientConfig(), "channels.d/gr.toml": channels})
			clk := newTunnelClock()
			e := &tunnelEnv{runEnv: startRun(t, dir, 0, func(o *RunOptions) {
				o.TunnelClock = clk
				o.beforeFetch = hold.before
			}), dir: dir, clock: clk}
			// The tunnel runs before the reload.
			if body := e.get(t, "/c/gr-seg", http.StatusOK); body != "old" {
				t.Fatalf("before reload: %q", body)
			}

			path, header := "/c/gr-seg", http.Header{holdHeader: {"1"}}
			if tt.refetch {
				// The fixture answers Range with 206: the proxy fetches the
				// playlist again without it (spec 3.4).
				path = "/c/gr-live"
				header.Set("Range", "bytes=0-9")
			}
			type result struct {
				status int
				body   string
				err    error
			}
			done := make(chan result, 1)
			go func() {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, e.url+path, nil)
				if err != nil {
					done <- result{err: err}
					return
				}
				req.Header = header
				resp, err := e.client.Do(req)
				if err != nil {
					done <- result{err: err}
					return
				}
				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				done <- result{status: resp.StatusCode, body: string(body), err: err}
			}()
			hold.wait(t)

			files := map[string]string{"tunnels/gr.conf": nextPeer.ClientConfig()}
			if tt.remove {
				files = map[string]string{"tunnels/gr.conf": "", "channels.d/gr.toml": ""}
			}
			writeFiles(t, dir, files)
			if err := e.reload(t); err != nil {
				t.Fatalf("Reload: %v", err)
			}
			// The old tunnel drains for the held request.
			e.clock.waitArmed(t, tunnelDrainTimeout)
			// New requests use the new config.
			if tt.remove {
				e.get(t, "/c/gr-seg", http.StatusNotFound)
			} else if body := e.get(t, "/c/gr-seg", http.StatusOK); body != "new" {
				t.Errorf("new request after the reload: %q, want the new tunnel's upstream", body)
			}
			if strings.Contains(e.logs.String(), closedByReload("gr")) {
				t.Fatal("old tunnel closed while a request of the old config had not fetched yet")
			}

			hold.done()
			r := <-done
			switch {
			case r.err != nil:
				t.Fatalf("held request: %v", r.err)
			case r.status != http.StatusOK:
				t.Fatalf("held request: status %d, want 200: %s", r.status, r.body)
			case !tt.refetch && r.body != "old":
				t.Errorf("held request: %q, want the old tunnel's upstream", r.body)
			case tt.refetch && (!strings.HasPrefix(r.body, "#EXTM3U\n") || !strings.Contains(r.body, "/r/gr-live/")):
				t.Errorf("held request: %q, want the whole playlist, rewritten", r.body)
			}
			if tt.refetch {
				reqs := oldUp.requestsFor("/live")
				if len(reqs) != 2 || reqs[0].Range != "bytes=0-9" || reqs[1].Range != "" {
					t.Errorf("old upstream requests %+v, want one with Range, then the refetch without", reqs)
				}
			}
			// Its end ends the drain.
			e.logs.waitFor(t, closedByReload("gr"), 1)
			if logs := e.logs.String(); strings.Contains(logs, "replaced by a reload") {
				t.Errorf("a request hit the retired tunnel:\n%s", logs)
			}
		})
	}
}

// TestReloadFailureKeepsTunnels: a reload that fails after the tunnel
// set was updated (here building the routes) leaves the tunnels as they
// were: the old config keeps serving through the same running tunnel,
// the tunnels it would have added or changed never start, and the next
// reload applies the files.
func TestReloadFailureKeepsTunnels(t *testing.T) {
	oldPeer, _ := newPeer(t, "old")
	nextPeer, _ := newPeer(t, "new")
	ukPeer, _ := newPeer(t, "uk")
	var fail atomic.Bool
	dir := writeConfig(t, map[string]string{"tunnels/gr.conf": oldPeer.ClientConfig(), "channels.d/gr.toml": tunnelChannels("gr")})
	clk := newTunnelClock()
	e := &tunnelEnv{runEnv: startRun(t, dir, 0, func(o *RunOptions) {
		o.TunnelClock = clk
		o.WrapTunnel = func(_ string, n route.Network) route.Network {
			if fail.Load() {
				// route.NewSet rejects a tunnel without network.
				return nil
			}
			return n
		}
	}), dir: dir, clock: clk}
	if body := e.get(t, "/c/gr-seg", http.StatusOK); body != "old" {
		t.Fatalf("before reload: %q", body)
	}

	writeFiles(t, dir, map[string]string{
		"tunnels/gr.conf": nextPeer.ClientConfig(),
		"tunnels/uk.conf": ukPeer.ClientConfig(), "channels.d/uk.toml": tunnelChannels("uk"),
	})
	fail.Store(true)
	if err := e.reload(t); err == nil || !strings.Contains(err.Error(), "reload failed") {
		t.Fatalf("Reload: err = %v, want reload failed", err)
	}
	fail.Store(false)
	if body := e.get(t, "/c/gr-seg", http.StatusOK); body != "old" {
		t.Errorf("after the failed reload: %q, want the old tunnel's upstream", body)
	}
	e.get(t, "/c/uk-seg", http.StatusNotFound)
	logs := e.logs.String()
	if strings.Contains(logs, `msg="tunnel closed"`) || e.starts("gr") != 1 || e.starts("uk") != 0 {
		t.Errorf("failed reload changed the tunnels (gr started %d times, uk %d):\n%s", e.starts("gr"), e.starts("uk"), logs)
	}

	if err := e.reload(t); err != nil {
		t.Fatalf("second Reload: %v", err)
	}
	e.logs.waitFor(t, closedByReload("gr"), 1)
	if body := e.get(t, "/c/gr-seg", http.StatusOK); body != "new" {
		t.Errorf("after the second reload: %q", body)
	}
	if body := e.get(t, "/c/uk-seg", http.StatusOK); body != "uk" {
		t.Errorf("added tunnel after the second reload: %q", body)
	}
}

// countingActivity counts the requests in flight on a route.
type countingActivity struct{ n atomic.Int64 }

func (a *countingActivity) Begin() func() {
	a.n.Add(1)
	return func() { a.n.Add(-1) }
}

// TestUseCountsBeforeTheSwap: use counts a request on its route's tunnel
// before it returns the generation, and returns one only if it is still
// active after the count. A reload that swaps in between makes it take
// the next generation, undoing the count on the old one.
func TestUseCountsBeforeTheSwap(t *testing.T) {
	cfg := loadConfig(t, map[string]string{"config.toml": ""})
	newGen := func(a *countingActivity) *route.Set {
		t.Helper()
		rs, err := route.NewSet(route.Host(), map[string]route.Network{"gr": route.Host()},
			route.Options{Upstream: cfg.Upstream(), Activity: map[string]route.Activity{"gr": a}})
		if err != nil {
			t.Fatal(err)
		}
		return rs
	}
	oldAct, nextAct := &countingActivity{}, &countingActivity{}
	s, err := New(Options{Config: cfg, Routes: newGen(oldAct), Signer: newSigner(t)})
	if err != nil {
		t.Fatal(err)
	}
	old := s.active()

	picks := 0
	g, _, done := s.use(t.Context(), func(*generation) string {
		picks++
		if picks == 1 {
			// A reload swaps while this request takes the old generation.
			s.swap(cfg, newGen(nextAct))
		}
		return "gr"
	})
	switch {
	case picks != 2:
		t.Errorf("pick called %d times, want 2", picks)
	case g == old || g != s.active():
		t.Error("use returned the replaced generation")
	}
	if o, n := oldAct.n.Load(), nextAct.n.Load(); o != 0 || n != 1 {
		t.Errorf("requests counted: old tunnel %d, new tunnel %d; want 0, 1", o, n)
	}
	if u := old.users.Load(); u != 0 {
		t.Errorf("replaced generation has %d users, want 0", u)
	}
	done()
	done()
	if n, u := nextAct.n.Load(), g.users.Load(); n != 0 || u != 0 {
		t.Errorf("after done: %d counted, %d users; want 0, 0", n, u)
	}
}
