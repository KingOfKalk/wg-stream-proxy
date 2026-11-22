// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/clips"
	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
)

// reloadChannels has a live channel whose URL is a media playlist, one
// whose URL is a master playlist, and an hls channel whose URL does not
// end in .m3u8, so the client's Range reaches the upstream (the refetch
// of spec 3.4).
const reloadChannels = `
tunnel = "direct"

[[channel]]
name = "Live"
url = "{UP}/live/index.m3u8"

[[channel]]
name = "Master"
url = "{UP}/master/master.m3u8"

[[channel]]
name = "Ranged"
url = "{UP}/ranged/playlist"
[channel.upstream]
type = "hls"
`

const (
	mediaPlaylist  = "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:7\n#EXTINF:2,\nseg7.ts\n"
	masterPlaylist = "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000\nlow/index.m3u8\n"
	mpegurl        = "application/vnd.apple.mpegurl"
)

var (
	livePlaylist = file{ctype: mpegurl, body: mediaPlaylist}
	busy         = file{status: http.StatusServiceUnavailable, body: "busy"}
)

// manualNow is a clock that moves only when the test advances it.
type manualNow struct {
	mu sync.Mutex
	t  time.Time
}

func newManualNow() *manualNow {
	return &manualNow{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *manualNow) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *manualNow) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// reloadEnv is a proxy for reloadChannels with a manual clock; requests
// go to the Server directly, so each can come from its own address.
type reloadEnv struct {
	*directEnv
	clock *manualNow
	// warns is the number of warn lines of channel live seen so far.
	warns int
}

func newReloadEnv(t *testing.T) *reloadEnv {
	t.Helper()
	clock := newManualNow()
	e := &reloadEnv{clock: clock, directEnv: newDirectEnvFiles(t, clipConfig,
		map[string]string{"channels.d/tv.toml": reloadChannels}, func(o *Options) { o.now = clock.now })}
	e.up.add("/live/index.m3u8", livePlaylist)
	e.up.add("/master/master.m3u8", file{ctype: mpegurl, body: masterPlaylist})
	e.up.add("/ranged/playlist", livePlaylist)
	return e
}

// reloadBase is the base URL of the answers to reloadEnv.send.
const reloadBase = "http://wsp.test"

// send sends a request for path from remote.
func (e *reloadEnv) send(t *testing.T, method, remote, path string, h http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, reloadBase+path, nil)
	req.RemoteAddr = remote
	for k, v := range h {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	e.server.ServeHTTP(rec, req)
	return rec
}

// newWarns returns the warn lines of channel id logged since the last
// call.
func (e *reloadEnv) newWarns(id string) []string {
	lines := warnLines(e.logs.String(), id)
	out := lines[e.warns:]
	e.warns = len(lines)
	return out
}

// wantPlaylist checks that rec is the rewritten playlist of channel id.
func wantPlaylist(t *testing.T, rec *httptest.ResponseRecorder, id string) {
	t.Helper()
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-cache" || !strings.Contains(rec.Body.String(), "/r/"+id+"/") {
		t.Fatalf("status %d, Cache-Control %q, want the rewritten playlist:\n%s", rec.Code, rec.Header().Get("Cache-Control"), rec.Body.String())
	}
}

// wantClip checks that rec is the error playlist of clip.
func wantClip(t *testing.T, rec *httptest.ResponseRecorder, clip string) {
	t.Helper()
	if want := wantErrorPlaylist(reloadBase, clip); rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Errorf("status %d, body\n%s\nwant 200 and\n%s", rec.Code, rec.Body.String(), want)
	}
}

// wantPlain checks that rec is the HTTP status code, not a playlist.
func wantPlain(t *testing.T, rec *httptest.ResponseRecorder, code int) {
	t.Helper()
	if rec.Code != code || strings.Contains(rec.Body.String(), "#EXTM3U") {
		t.Errorf("status %d, body %q, want %d without a playlist", rec.Code, rec.Body.String(), code)
	}
}

// clipsServed returns wsp_error_clips_total of channel-not-available.
func (e *reloadEnv) clipsServed(t *testing.T) float64 {
	t.Helper()
	_, m := e.get(t, "/metrics", nil)
	return metricValue(t, m, `wsp_error_clips_total{clip="`+clips.ChannelNotAvailable+`"}`)
}

// TestErrorClipsOnReload: a GET of /c/ within 30 s after the proxy
// answered the same client with the channel's media playlist is a reload
// and gets the HTTP status of every failure, as with error = false; after
// 30 s it gets the clip again (spec 3.4). Only a served clip has clip= on
// its warn line and counts in error_clips_total.
func TestErrorClipsOnReload(t *testing.T) {
	e := newReloadEnv(t)
	failures := []struct {
		name string
		fail file
		want int
	}{
		{"upstream 5xx", busy, http.StatusBadGateway},
		{"upstream 4xx relayed", file{status: http.StatusForbidden, body: "not in your country"}, http.StatusForbidden},
		{"timeout", file{hang: true}, http.StatusGatewayTimeout},
		{"rejected playlist", file{ctype: mpegurl, body: "#EXTM3U\n\x00\n"}, http.StatusBadGateway},
		{"refused format", file{ctype: "audio/x-scpls", body: "[playlist]\nFile1=http://radio.example/stream\n"}, http.StatusBadGateway},
		{"206 without Range", file{ctype: mpegurl, status: http.StatusPartialContent, body: mediaPlaylist}, http.StatusBadGateway},
	}
	for i, tt := range failures {
		t.Run(tt.name, func(t *testing.T) {
			remote := "192.0.2." + strconv.Itoa(i+1) + ":40000"
			e.up.add("/live/index.m3u8", livePlaylist)
			wantPlaylist(t, e.send(t, http.MethodGet, remote, "/c/live.m3u8", nil), "live")
			e.up.add("/live/index.m3u8", tt.fail)
			before := e.clipsServed(t)
			e.newWarns("live")
			for _, d := range []time.Duration{10 * time.Second, 20 * time.Second} {
				e.clock.advance(d)
				wantPlain(t, e.send(t, http.MethodGet, remote, "/c/live.m3u8", nil), tt.want)
			}
			for _, line := range e.newWarns("live") {
				if strings.Contains(line, "clip=") {
					t.Errorf("clip on the warn line of a plain answer: %s", line)
				}
			}
			if v := e.clipsServed(t); v != before {
				t.Errorf("error clips %v after plain answers, want %v", v, before)
			}
			// 31 s after the playlist: a start again.
			e.clock.advance(time.Second)
			wantClip(t, e.send(t, http.MethodGet, remote, "/c/live.m3u8", nil), clips.ChannelNotAvailable)
			if lines := e.newWarns("live"); len(lines) != 1 || !strings.Contains(lines[0], " clip="+clips.ChannelNotAvailable) {
				t.Errorf("warn lines %q, want one with the clip", lines)
			}
			if v := e.clipsServed(t); v != before+1 {
				t.Errorf("error clips %v, want %v", v, before+1)
			}
		})
	}
}

// TestErrorClipsOnReloadScope: only a served media playlist of /c/ starts
// the reload window, per channel and client address (spec 3.4).
func TestErrorClipsOnReloadScope(t *testing.T) {
	const a, b = "192.0.2.10:40000", "192.0.2.20:40000"
	t.Run("start failure", func(t *testing.T) {
		e := newReloadEnv(t)
		e.up.add("/live/index.m3u8", busy)
		// A clip starts no window.
		for range 2 {
			wantClip(t, e.send(t, http.MethodGet, a, "/c/live.m3u8", nil), clips.ChannelNotAvailable)
		}
	})
	t.Run("client address", func(t *testing.T) {
		e := newReloadEnv(t)
		wantPlaylist(t, e.send(t, http.MethodGet, a, "/c/live.m3u8", nil), "live")
		e.up.add("/live/index.m3u8", busy)
		wantClip(t, e.send(t, http.MethodGet, b, "/c/live.m3u8", nil), clips.ChannelNotAvailable)
		// Another port of the same address: the same client.
		wantPlain(t, e.send(t, http.MethodGet, "192.0.2.10:50000", "/c/live.m3u8", nil), http.StatusBadGateway)
		wantClip(t, e.send(t, http.MethodGet, "[2001:db8::1]:40000", "/c/live.m3u8", nil), clips.ChannelNotAvailable)
	})
	t.Run("channel", func(t *testing.T) {
		e := newReloadEnv(t)
		wantPlaylist(t, e.send(t, http.MethodGet, a, "/c/live.m3u8", nil), "live")
		e.up.add("/ranged/playlist", busy)
		wantClip(t, e.send(t, http.MethodGet, a, "/c/ranged.m3u8", nil), clips.ChannelNotAvailable)
	})
	t.Run("master playlist", func(t *testing.T) {
		e := newReloadEnv(t)
		wantPlaylist(t, e.send(t, http.MethodGet, a, "/c/master.m3u8", nil), "master")
		e.up.add("/master/master.m3u8", busy)
		wantClip(t, e.send(t, http.MethodGet, a, "/c/master.m3u8", nil), clips.ChannelNotAvailable)
	})
	t.Run("HEAD", func(t *testing.T) {
		e := newReloadEnv(t)
		if rec := e.send(t, http.MethodHead, a, "/c/live.m3u8", nil); rec.Code != http.StatusOK {
			t.Fatalf("HEAD: status %d", rec.Code)
		}
		e.up.add("/live/index.m3u8", busy)
		wantClip(t, e.send(t, http.MethodGet, a, "/c/live.m3u8", nil), clips.ChannelNotAvailable)
		// HEAD keeps its status, also on a reload.
		e.up.add("/live/index.m3u8", livePlaylist)
		wantPlaylist(t, e.send(t, http.MethodGet, a, "/c/live.m3u8", nil), "live")
		e.up.add("/live/index.m3u8", busy)
		wantPlain(t, e.send(t, http.MethodHead, a, "/c/live.m3u8", nil), http.StatusBadGateway)
	})
	t.Run("resource", func(t *testing.T) {
		e := newReloadEnv(t)
		wantPlaylist(t, e.send(t, http.MethodGet, a, "/c/live.m3u8", nil), "live")
		e.up.add("/live/seg7.ts", busy)
		wantPlain(t, e.send(t, http.MethodGet, a, e.signer.Path("live", e.upURL+"/live/seg7.ts"), nil), http.StatusBadGateway)
		// The resource failure does not end the window.
		e.up.add("/live/index.m3u8", busy)
		wantPlain(t, e.send(t, http.MethodGet, a, "/c/live.m3u8", nil), http.StatusBadGateway)
	})
	t.Run("refetch", func(t *testing.T) {
		e := newReloadEnv(t)
		// Range reaches the upstream, which answers 206: the playlist is
		// fetched again whole.
		wantPlaylist(t, e.send(t, http.MethodGet, a, "/c/ranged.m3u8", http.Header{"Range": {"bytes=0-9"}}), "ranged")
		if reqs := e.up.requestsFor("/ranged/playlist"); len(reqs) != 2 || reqs[0].Range == "" || reqs[1].Range != "" {
			t.Fatalf("upstream requests %+v, want one with Range, then the refetch without", reqs)
		}
		e.up.add("/ranged/playlist", busy)
		wantPlain(t, e.send(t, http.MethodGet, a, "/c/ranged.m3u8", http.Header{"Range": {"bytes=0-9"}}), http.StatusBadGateway)
	})
	t.Run("error clips off", func(t *testing.T) {
		e := newReloadEnv(t)
		wantPlaylist(t, e.send(t, http.MethodGet, a, "/c/live.m3u8", nil), "live")
		if n := len(e.server.players.m); n != 1 {
			t.Errorf("%d remembered players, want 1", n)
		}
		off := loadReloadConfig(t, e, "\n[clips]\nerror = false\n", reloadChannels)
		e.server.swap(off, e.server.active().routes)
		wantPlaylist(t, e.send(t, http.MethodGet, b, "/c/live.m3u8", nil), "live")
		if n := len(e.server.players.m); n != 1 {
			t.Errorf("%d remembered players with error = false, want 1", n)
		}
	})
	t.Run("config reload", func(t *testing.T) {
		e := newReloadEnv(t)
		wantPlaylist(t, e.send(t, http.MethodGet, a, "/c/live.m3u8", nil), "live")
		wantPlaylist(t, e.send(t, http.MethodGet, a, "/c/ranged.m3u8", nil), "ranged")
		// Live changes, ranged stays the same.
		next := loadReloadConfig(t, e, "", strings.Replace(reloadChannels, "/live/index.m3u8", "/live2/index.m3u8", 1))
		e.server.swap(next, e.server.active().routes)
		e.up.add("/live2/index.m3u8", busy)
		e.up.add("/ranged/playlist", busy)
		wantClip(t, e.send(t, http.MethodGet, a, "/c/live.m3u8", nil), clips.ChannelNotAvailable)
		wantPlain(t, e.send(t, http.MethodGet, a, "/c/ranged.m3u8", nil), http.StatusBadGateway)
	})
}

// loadReloadConfig loads the config of a reloadEnv with extra appended
// to config.toml and channels as the channel file.
func loadReloadConfig(t *testing.T, e *reloadEnv, extra, channels string) *config.Config {
	t.Helper()
	return loadConfig(t, map[string]string{
		"config.toml":        clipConfig + extra,
		"channels.d/tv.toml": strings.ReplaceAll(channels, "{UP}", e.upURL),
	})
}

// TestPlayersBounded: the remembered players expire after the window and
// are capped, the oldest evicted first.
func TestPlayersBounded(t *testing.T) {
	clock := newManualNow()
	p := newPlayers(clock.now)
	var ch config.Channel
	addr := func(i int) netip.Addr {
		return netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)})
	}
	for i := range maxPlayers {
		p.served(addr(i), ch)
		clock.advance(time.Millisecond)
	}
	p.served(addr(maxPlayers), ch)
	if n := len(p.m); n != maxPlayers {
		t.Fatalf("%d players, want %d", n, maxPlayers)
	}
	if p.reload(addr(0), ch) {
		t.Error("the oldest player was not evicted")
	}
	for _, i := range []int{1, maxPlayers - 1, maxPlayers} {
		if !p.reload(addr(i), ch) {
			t.Errorf("player %d forgotten", i)
		}
	}
	// A served playlist again moves a player to the end.
	p.served(addr(1), ch)
	p.served(addr(maxPlayers+1), ch)
	if !p.reload(addr(1), ch) || p.reload(addr(2), ch) {
		t.Error("want player 2 evicted, not player 1")
	}
	// After the window every entry has expired: the next one sweeps them.
	clock.advance(reloadWindow + time.Millisecond)
	if p.reload(addr(1), ch) {
		t.Error("expired player is a reload")
	}
	for i := range maxPlayers {
		p.served(addr(i), ch)
	}
	clock.advance(reloadWindow + time.Millisecond)
	p.served(addr(maxPlayers+5), ch)
	if n := len(p.m); n != 1 {
		t.Errorf("%d players after the window, want 1", n)
	}
	// A request without a usable client address is never a reload.
	p.served(netip.Addr{}, ch)
	if p.reload(netip.Addr{}, ch) {
		t.Error("invalid address is a reload")
	}
}
