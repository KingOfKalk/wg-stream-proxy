// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"

	"github.com/KingOfKalk/wg-stream-proxy/internal/clips"
	"github.com/KingOfKalk/wg-stream-proxy/internal/metrics"
	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel/tunneltest"
)

// clipChannels has one HLS channel per failure class of docs/clips.md
// 3.2, plus channels that keep their HTTP errors. {REFUSED} is a closed
// port; 127.0.0.2 is outside allow_private.
const clipChannels = `
tunnel = "direct"

[[channel]]
name = "Down"
url = "{UP}/down/master.m3u8"

[[channel]]
name = "Geo"
url = "{UP}/geo/master.m3u8"

[[channel]]
name = "Hang"
url = "{UP}/hang/master.m3u8"

[[channel]]
name = "Refused"
url = "http://{REFUSED}/live/master.m3u8"

[[channel]]
name = "Blocked"
url = "http://127.0.0.2:9/live/master.m3u8"

[[channel]]
name = "Broken"
url = "{UP}/broken/master.m3u8"

[[channel]]
name = "PLS"
url = "{UP}/pls/master.m3u8"

[[channel]]
name = "Big"
url = "{UP}/big/master.m3u8"

[[channel]]
name = "Partial"
url = "{UP}/partial/master.m3u8"

[[channel]]
name = "Forced HLS"
url = "{UP}/down/stream.bin"
[channel.upstream]
type = "hls"

[[channel]]
name = "Raw"
url = "{UP}/down/stream"

[[channel]]
name = "Live"
url = "{UP}/live/master.m3u8"
`

// clipConfig allows only 127.0.0.1 (the fixture), so 127.0.0.2 is
// blocked by the guard; small timeouts and playlist size.
const clipConfig = `
[upstream]
header_timeout = "50ms"
max_playlist_size = "1KiB"

[upstream.allow_private]
direct = ["127.0.0.1/32"]
`

// newClipEnv is a proxy for clipChannels with the fixtures of every case;
// extra is appended to config.toml.
func newClipEnv(t *testing.T, extra string) *directEnv {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := ln.Addr().String()
	_ = ln.Close()
	e := newDirectEnv(t, clipConfig+extra, strings.ReplaceAll(clipChannels, "{REFUSED}", refused))
	const mpegurl = "application/vnd.apple.mpegurl"
	e.up.add("/down/master.m3u8", file{ctype: mpegurl, status: http.StatusServiceUnavailable, body: "busy"})
	e.up.add("/down/stream.bin", file{status: http.StatusServiceUnavailable, body: "busy"})
	e.up.add("/down/stream", file{status: http.StatusServiceUnavailable, body: "busy"})
	e.up.add("/down/seg.ts", file{status: http.StatusServiceUnavailable, body: "busy"})
	e.up.add("/geo/master.m3u8", file{status: http.StatusForbidden, body: "not in your country"})
	e.up.add("/hang/master.m3u8", file{hang: true})
	e.up.add("/broken/master.m3u8", file{ctype: mpegurl, body: "#EXTM3U\n\x00\n"})
	e.up.add("/pls/master.m3u8", file{ctype: "audio/x-scpls", body: "[playlist]\nFile1=http://radio.example/stream\n"})
	e.up.add("/big/master.m3u8", file{ctype: mpegurl, body: "#EXTM3U\n" + strings.Repeat("#comment\n", 200)})
	e.up.add("/partial/master.m3u8", file{ctype: mpegurl, status: http.StatusPartialContent, body: "#EXTM3U\n"})
	e.up.add("/live/master.m3u8", file{ctype: mpegurl, body: "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2,\nseg.ts\n"})
	return e
}

// wantErrorPlaylist is the error playlist of clip under base (spec 3.4).
func wantErrorPlaylist(base, clip string) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n")
	for n := range 5 {
		b.WriteString("#EXTINF:0.960,\n" + base + "/clip/" + clip + "/" + strconv.Itoa(n) + ".ts\n")
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

// warnLines returns the warn lines of the log that name channel id.
func warnLines(logs, id string) []string {
	var out []string
	for line := range strings.SplitSeq(logs, "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, " channel="+id+" ") {
			out = append(out, line)
		}
	}
	return out
}

// TestErrorClips: GET /c/ of an HLS channel answers every failure class
// of docs/clips.md 3.2 with the error playlist of its clip; the failure is
// logged and counted as before, with the clip on the warn line.
func TestErrorClips(t *testing.T) {
	e := newClipEnv(t, "")
	tests := []struct {
		id     string
		clip   string
		reason string // upstream_errors_total reason, "" for an upstream status
	}{
		{"down", clips.ChannelNotAvailable, ""},
		{"geo", clips.ChannelNotAvailable, ""},
		{"hang", clips.ChannelNotAvailable, "timeout"},
		{"refused", clips.ChannelNotAvailable, "connect"},
		{"blocked", clips.ChannelBlocked, "guard"},
		{"broken", clips.ChannelNotAvailable, "other"},
		{"pls", clips.ChannelNotAvailable, "unsupported"},
		{"big", clips.ChannelNotAvailable, "size"},
		{"partial", clips.ChannelNotAvailable, "other"},
		{"forced-hls", clips.ChannelNotAvailable, ""},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			for _, path := range []string{"/c/" + tt.id + ".m3u8", "/c/" + tt.id} {
				resp, body := e.get(t, path, nil)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("GET %s: status %d, want 200: %s", path, resp.StatusCode, body)
				}
				h := resp.Header
				if h.Get("Content-Type") != "application/vnd.apple.mpegurl" || h.Get("Cache-Control") != "no-store" {
					t.Errorf("GET %s: Content-Type %q, Cache-Control %q", path, h.Get("Content-Type"), h.Get("Cache-Control"))
				}
				if h.Get("Content-Length") != strconv.Itoa(len(body)) {
					t.Errorf("GET %s: Content-Length %q for %d bytes", path, h.Get("Content-Length"), len(body))
				}
				if want := wantErrorPlaylist(e.proxy.URL, tt.clip); body != want {
					t.Errorf("GET %s: body\n%s\nwant\n%s", path, body, want)
				}
			}
			lines := warnLines(e.logs.String(), tt.id)
			if len(lines) != 2 {
				t.Fatalf("%d warn lines for %s, want 2:\n%s", len(lines), tt.id, e.logs.String())
			}
			for _, line := range lines {
				if !strings.Contains(line, " clip="+tt.clip) {
					t.Errorf("warn line without clip=%s: %s", tt.clip, line)
				}
			}
			_, m := e.get(t, "/metrics", nil)
			if tt.reason != "" {
				if v := metricValue(t, m, `wsp_upstream_errors_total{channel="`+tt.id+`",reason="`+tt.reason+`"}`); v != 2 {
					t.Errorf("upstream errors %s/%s = %v, want 2", tt.id, tt.reason, v)
				}
			}
		})
	}
	_, m := e.get(t, "/metrics", nil)
	for clip, want := range map[string]float64{clips.ChannelNotAvailable: 18, clips.ChannelBlocked: 2, clips.TunnelUnreachable: 0} {
		if v := metricValue(t, m, `wsp_error_clips_total{clip="`+clip+`"}`); v != want {
			t.Errorf("wsp_error_clips_total{clip=%q} = %v, want %v", clip, v, want)
		}
	}
}

// TestErrorClipsKeepHTTPErrors: HEAD, /r/, raw channels, unknown channels
// and error = false keep the HTTP errors (spec 3.4).
func TestErrorClipsKeepHTTPErrors(t *testing.T) {
	e := newClipEnv(t, "")
	off := newClipEnv(t, "\n[clips]\nerror = false\n")
	tests := []struct {
		name   string
		env    *directEnv
		method string
		path   string
		want   int
	}{
		{"head", e, http.MethodHead, "/c/down.m3u8", http.StatusBadGateway},
		{"head timeout", e, http.MethodHead, "/c/hang.m3u8", http.StatusGatewayTimeout},
		{"raw channel", e, http.MethodGet, "/c/raw", http.StatusBadGateway},
		{"resource", e, http.MethodGet, e.signer.Path("live", e.upURL+"/down/seg.ts"), http.StatusBadGateway},
		{"resource playlist", e, http.MethodGet, e.signer.Path("live", e.upURL+"/down/master.m3u8"), http.StatusBadGateway},
		{"unknown channel", e, http.MethodGet, "/c/nope.m3u8", http.StatusNotFound},
		{"off: upstream status", off, http.MethodGet, "/c/down.m3u8", http.StatusBadGateway},
		{"off: 4xx relayed", off, http.MethodGet, "/c/geo.m3u8", http.StatusForbidden},
		{"off: timeout", off, http.MethodGet, "/c/hang.m3u8", http.StatusGatewayTimeout},
		{"off: guard", off, http.MethodGet, "/c/blocked.m3u8", http.StatusBadGateway},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := do(t, tt.env.client, tt.method, tt.env.proxy.URL+tt.path, nil)
			if resp.StatusCode != tt.want {
				t.Errorf("%s %s: status %d, want %d", tt.method, tt.path, resp.StatusCode, tt.want)
			}
			if strings.Contains(body, "#EXTM3U") {
				t.Errorf("%s %s: body is a playlist: %s", tt.method, tt.path, body)
			}
		})
	}
	for _, env := range []*directEnv{e, off} {
		if strings.Contains(env.logs.String(), "clip=") {
			t.Errorf("clip logged without an error clip:\n%s", env.logs.String())
		}
		_, m := env.get(t, "/metrics", nil)
		if v := metricValue(t, m, `wsp_error_clips_total{clip="channel-not-available"}`); v != 0 {
			t.Errorf("error clips counted: %v", v)
		}
	}
	// A working channel is served as before.
	resp, body := e.get(t, "/c/live.m3u8", nil)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-cache" || !strings.Contains(body, "/r/live/") {
		t.Errorf("working channel: status %d, Cache-Control %q:\n%s", resp.StatusCode, resp.Header.Get("Cache-Control"), body)
	}
}

// TestClipSegments: /clip/{name}/{n}.ts serves the clip segment at
// position n (clips.Repeat), without any upstream request; anything else
// under /clip/ is 404.
func TestClipSegments(t *testing.T) {
	e := newClipEnv(t, "")
	for _, name := range clips.Names() {
		seg, _ := clips.Segment(name)
		for _, n := range []int{0, 4, clips.MaxPosition} {
			path := "/clip/" + name + "/" + strconv.Itoa(n) + ".ts"
			want, err := clips.Repeat(seg, n)
			if err != nil {
				t.Fatal(err)
			}
			resp, body := e.get(t, path, nil)
			if resp.StatusCode != http.StatusOK || !bytes.Equal([]byte(body), want) {
				t.Fatalf("GET %s: status %d, %d bytes, want 200 and %d bytes", path, resp.StatusCode, len(body), len(want))
			}
			if ct := resp.Header.Get("Content-Type"); ct != "video/mp2t" {
				t.Errorf("GET %s: Content-Type %q", path, ct)
			}
			if cl := resp.Header.Get("Content-Length"); cl != strconv.Itoa(len(want)) {
				t.Errorf("GET %s: Content-Length %q", path, cl)
			}
		}
	}
	resp, body := do(t, e.client, http.MethodHead, e.proxy.URL+"/clip/channel-blocked/1.ts", nil)
	if resp.StatusCode != http.StatusOK || body != "" || resp.Header.Get("Content-Length") == "" {
		t.Errorf("HEAD: status %d, %d bytes, Content-Length %q", resp.StatusCode, len(body), resp.Header.Get("Content-Length"))
	}
	for _, path := range []string{
		"/clip/nope/0.ts", "/clip/Starting/0.ts", "/clip/channel-blocked/64.ts", "/clip/channel-blocked/-1.ts",
		"/clip/channel-blocked/+1.ts", "/clip/channel-blocked/01.ts", "/clip/channel-blocked/1", "/clip/channel-blocked/1.mp4",
		"/clip/channel-blocked/.ts", "/clip/channel-blocked/", "/clip/channel-blocked/1.ts/x", "/clip/",
	} {
		if resp, _ := e.get(t, path, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", path, resp.StatusCode)
		}
	}
	if n := len(e.up.requests()); n != 0 {
		t.Errorf("upstream saw %d requests, want none", n)
	}
	if resp, _ := do(t, e.client, http.MethodPost, e.proxy.URL+"/clip/channel-blocked/0.ts", nil); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST: status %d, want 405", resp.StatusCode)
	}
}

// TestClipRequestsRecorded: /clip/ requests show up in
// the debug line with kind clip; they are not channel requests and count
// in no request metric (spec 7.3).
func TestClipRequestsRecorded(t *testing.T) {
	cfg := loadConfig(t, nil)
	routes, err := route.NewSet(route.Host(), nil, route.Options{})
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	m := metrics.New(metrics.Options{})
	s, err := New(Options{Config: cfg, Routes: routes, Signer: newSigner(t), Logger: newLogger(logs), Metrics: m})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/clip/tunnel-unreachable/2.ts", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	line := logs.String()
	for _, want := range []string{
		`msg=request id=1 method=GET channel="" route="" kind=clip status=200`,
		"bytes=" + strconv.Itoa(rec.Body.Len()),
		"url=/clip/tunnel-unreachable/2.ts",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("debug line for the clip request lacks %q:\n%s", want, line)
		}
	}
	mfs, err := m.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		switch mf.GetName() {
		case "wsp_active_requests", "wsp_upstream_requests_total", "wsp_upstream_phase_seconds":
			t.Errorf("%s has samples after a clip request", mf.GetName())
		}
	}
}

// TestErrorClipsThroughTunnel: a tunnel that does not start within
// connect_timeout gives tunnel-unreachable; a name that resolves only
// outside the tunnel's AllowedIPs gives channel-blocked (docs/clips.md
// 3.2).
func TestErrorClipsThroughTunnel(t *testing.T) {
	const channels = "tunnel = \"gr\"\n" +
		"[[channel]]\nname = \"Live\"\nurl = \"http://upstream.test/live.m3u8\"\n" +
		"[[channel]]\nname = \"Outside\"\nurl = \"http://outside.test/live.m3u8\"\n"

	t.Run("tunnel unreachable", func(t *testing.T) {
		down, _ := newPeer(t, "down")
		down.SetReachable(false)
		// The route's connect_timeout runs on the real clock; the tunnel's
		// own start timeout never fires (tunnel clock).
		const cfg = "[upstream]\nconnect_timeout = \"100ms\"\n"
		e := startTunnels(t, map[string]string{"config.toml": cfg, "tunnels/gr.conf": down.ClientConfig(), "channels.d/gr.toml": channels})
		body := e.get(t, "/c/live.m3u8", http.StatusOK)
		if want := wantErrorPlaylist(e.url, clips.TunnelUnreachable); body != want {
			t.Errorf("body\n%s\nwant\n%s", body, want)
		}
		if lines := warnLines(e.logs.String(), "live"); len(lines) != 1 || !strings.Contains(lines[0], "reason=connect") ||
			!strings.Contains(lines[0], "clip="+clips.TunnelUnreachable) {
			t.Errorf("warn lines %q, want one with reason=connect and the clip", lines)
		}
	})
	t.Run("outside AllowedIPs", func(t *testing.T) {
		up := newUpstream()
		peer := tunneltest.Start(t, tunneltest.Options{Handler: up, AllowedIPs: []netip.Prefix{netip.PrefixFrom(tunneltest.ServerAddr4, 32)}})
		peer.SetHost("outside.test", netip.MustParseAddr("198.51.100.1"))
		e := startTunnels(t, map[string]string{"tunnels/gr.conf": peer.ClientConfig(), "channels.d/gr.toml": channels})
		body := e.get(t, "/c/outside.m3u8", http.StatusOK)
		if want := wantErrorPlaylist(e.url, clips.ChannelBlocked); body != want {
			t.Errorf("body\n%s\nwant\n%s", body, want)
		}
		if n := len(up.requests()); n != 0 {
			t.Errorf("upstream saw %d requests", n)
		}
	})
}
