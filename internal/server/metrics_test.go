// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel/tunneltest"
)

// Help and type lines of the request metrics (spec 7.3).
const (
	requestsHead = "# HELP wsp_upstream_requests_total Upstream requests by status code, or error if no response came.\n" +
		"# TYPE wsp_upstream_requests_total counter\n"
	errorsHead = "# HELP wsp_upstream_errors_total Upstream request failures by reason.\n" +
		"# TYPE wsp_upstream_errors_total counter\n"
	activeHead = "# HELP wsp_active_requests Requests in flight under /c/, /r/ and /p/, open raw streams included.\n" +
		"# TYPE wsp_active_requests gauge\n"
)

// compareMetrics compares the series of one metric in g with want, the
// sample lines without help and type; "" means no series.
func compareMetrics(t *testing.T, g prometheus.Gatherer, name, head, want string) {
	t.Helper()
	want = strings.TrimSpace(want)
	if want != "" {
		want = head + want + "\n"
	}
	if err := testutil.GatherAndCompare(g, strings.NewReader(want), name); err != nil {
		t.Errorf("%s:\n%v", name, err)
	}
}

// activeRequests returns the non-zero wsp_active_requests series as
// "channel/kind" -> value.
func activeRequests(t *testing.T, g prometheus.Gatherer) map[string]float64 {
	t.Helper()
	mfs, err := g.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() != "wsp_active_requests" {
			continue
		}
		for _, m := range mf.GetMetric() {
			if v := m.GetGauge().GetValue(); v != 0 {
				var channel, kind string
				for _, l := range m.GetLabel() {
					switch l.GetName() {
					case "channel":
						channel = l.GetValue()
					case "kind":
						kind = l.GetValue()
					}
				}
				out[channel+"/"+kind] = v
			}
		}
	}
	return out
}

// TestRequestMetrics checks kind, code and reason of the request metrics
// (spec 7.3) for each kind of response and every error reason, and that
// wsp_active_requests is back to 0 once the requests ended.
func TestRequestMetrics(t *testing.T) {
	const mpegurl = "application/vnd.apple.mpegurl"
	tests := []struct {
		name string
		// config is config.toml; the fixtures need allowLoopback.
		config string
		// run sends the requests.
		run            func(t *testing.T, e *directEnv)
		requests, errs string
	}{
		{
			name:   "hls crawl",
			config: allowLoopback,
			run: func(t *testing.T, e *directEnv) {
				hlsFixture(e.up, e.upURL)
				got, playlists := crawl(t, e.client, e.proxy.URL, "news", e.proxy.URL+"/c/news.m3u8")
				if playlists != 5 || len(got) != 7 {
					t.Fatalf("crawl: %d playlists, %d resources", playlists, len(got))
				}
			},
			requests: `
wsp_upstream_requests_total{channel="news",code="200",kind="playlist"} 5
wsp_upstream_requests_total{channel="news",code="200",kind="segment"} 7
`,
		},
		{
			name:   "head",
			config: allowLoopback,
			run: func(t *testing.T, e *directEnv) {
				hlsFixture(e.up, e.upURL)
				for _, p := range []string{"/c/news", e.signer.Path("news", e.upURL+"/cdn/v1/low/seg7.m4s")} {
					if resp, _ := do(t, e.client, http.MethodHead, e.proxy.URL+p, nil); resp.StatusCode != http.StatusOK {
						t.Errorf("HEAD %s: status %d", p, resp.StatusCode)
					}
				}
			},
			requests: `
wsp_upstream_requests_total{channel="news",code="200",kind="playlist"} 1
wsp_upstream_requests_total{channel="news",code="200",kind="segment"} 1
`,
		},
		{
			name:   "configured types",
			config: allowLoopback,
			run: func(t *testing.T, e *directEnv) {
				hlsFixture(e.up, e.upURL)
				e.up.add("/radio", file{ctype: "audio/mpeg", body: "radio"})
				e.up.add("/odd/playlist.bin", file{ctype: "application/octet-stream", body: "#EXTM3U\n#EXTINF:4,\nseg1.ts\n"})
				// A stream behind a playlist Content-Type: raw pipes it.
				e.up.add("/cdn/v1/master.m3u8", file{ctype: "application/vnd.apple.mpegurl", body: "\x47\x40\x00\x10 not a playlist"})
				for _, p := range []string{"/c/radio", "/c/forced-raw", "/c/forced-hls"} {
					if resp, _ := e.get(t, p, nil); resp.StatusCode != http.StatusOK {
						t.Errorf("GET %s: status %d", p, resp.StatusCode)
					}
				}
			},
			requests: `
wsp_upstream_requests_total{channel="forced-hls",code="200",kind="playlist"} 1
wsp_upstream_requests_total{channel="forced-raw",code="200",kind="raw"} 1
wsp_upstream_requests_total{channel="radio",code="200",kind="raw"} 1
`,
		},
		{
			name:   "upstream status",
			config: allowLoopback,
			run: func(t *testing.T, e *directEnv) {
				e.up.add("/status", file{ctype: "text/plain", status: http.StatusNotFound, body: "gone"})
				e.up.add("/live/master.m3u8", file{ctype: mpegurl, status: http.StatusServiceUnavailable})
				e.up.add("/cdn/seg.ts", file{ctype: "video/mp2t", status: http.StatusInternalServerError})
				e.get(t, "/c/status", nil)
				e.get(t, "/c/news", nil)
				e.get(t, e.signer.Path("news", e.upURL+"/cdn/seg.ts"), nil)
				e.get(t, e.signer.Path("news", e.upURL+"/cdn/v1/low/index.m3u8"), nil) // 404
			},
			// Statuses only count as requests: the kind comes from the
			// configured type or the path, nothing is sniffed.
			requests: `
wsp_upstream_requests_total{channel="news",code="404",kind="playlist"} 1
wsp_upstream_requests_total{channel="news",code="500",kind="segment"} 1
wsp_upstream_requests_total{channel="news",code="503",kind="playlist"} 1
wsp_upstream_requests_total{channel="status",code="404",kind="raw"} 1
`,
		},
		{
			name:   "playlist partial content",
			config: allowLoopback,
			run: func(t *testing.T, e *directEnv) {
				const pl = "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\nseg1.ts\n"
				e.up.add("/live/playlist", file{ctype: mpegurl, body: pl})
				e.up.add("/live/partial", file{ctype: mpegurl, status: http.StatusPartialContent, body: pl})
				rng := http.Header{"Range": {"bytes=0-9"}}
				if resp, _ := e.get(t, "/c/partial", rng); resp.StatusCode != http.StatusOK {
					t.Errorf("partial: status %d", resp.StatusCode)
				}
				if resp, _ := e.get(t, "/c/always-partial", rng); resp.StatusCode != http.StatusBadGateway {
					t.Errorf("always partial: status %d", resp.StatusCode)
				}
			},
			// A refetch is a second upstream request.
			requests: `
wsp_upstream_requests_total{channel="always-partial",code="206",kind="playlist"} 2
wsp_upstream_requests_total{channel="partial",code="200",kind="playlist"} 1
wsp_upstream_requests_total{channel="partial",code="206",kind="playlist"} 1
`,
			errs: `
wsp_upstream_errors_total{channel="always-partial",reason="other"} 1
`,
		},
		{
			name:   "guard",
			config: "",
			run: func(t *testing.T, e *directEnv) {
				e.get(t, "/c/news", nil)
				e.get(t, "/c/radio", nil)
			},
			requests: `
wsp_upstream_requests_total{channel="news",code="error",kind="playlist"} 1
wsp_upstream_requests_total{channel="radio",code="error",kind="raw"} 1
`,
			errs: `
wsp_upstream_errors_total{channel="news",reason="guard"} 1
wsp_upstream_errors_total{channel="radio",reason="guard"} 1
`,
		},
		{
			name:   "connect",
			config: allowLoopback,
			run: func(t *testing.T, e *directEnv) {
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				addr := ln.Addr().String()
				_ = ln.Close()
				e.get(t, e.signer.Path("news", "http://"+addr+"/x.ts?token=secret"), nil)
			},
			requests: `wsp_upstream_requests_total{channel="news",code="error",kind="segment"} 1`,
			errs:     `wsp_upstream_errors_total{channel="news",reason="connect"} 1`,
		},
		{
			name:   "timeouts",
			config: allowLoopback + "\n[upstream]\nheader_timeout = \"20ms\"\nread_timeout = \"20ms\"\n",
			run: func(t *testing.T, e *directEnv) {
				e.up.add("/hang", file{hang: true})
				e.up.add("/radio", file{ctype: "audio/mpeg", body: binaryBody('a', 1024), stall: true})
				if resp, _ := e.get(t, "/c/hang", nil); resp.StatusCode != http.StatusGatewayTimeout {
					t.Errorf("header timeout: status %d", resp.StatusCode)
				}
				// The stream ends after the read timeout.
				if resp, body := e.get(t, "/c/radio", nil); resp.StatusCode != http.StatusOK || len(body) != 1024 {
					t.Errorf("read timeout: status %d, %d bytes", resp.StatusCode, len(body))
				}
			},
			requests: `
wsp_upstream_requests_total{channel="hang",code="error",kind="raw"} 1
wsp_upstream_requests_total{channel="radio",code="200",kind="raw"} 1
`,
			errs: `
wsp_upstream_errors_total{channel="hang",reason="timeout"} 1
wsp_upstream_errors_total{channel="radio",reason="timeout"} 1
`,
		},
		{
			name:   "tls",
			config: allowLoopback,
			run: func(t *testing.T, e *directEnv) {
				srv := httptest.NewUnstartedServer(http.NotFoundHandler())
				srv.Config.ErrorLog = log.New(io.Discard, "", 0)
				srv.StartTLS()
				t.Cleanup(srv.Close)
				// The test server's certificate is not trusted.
				e.get(t, e.signer.Path("news", srv.URL+"/seg.ts"), nil)
			},
			requests: `wsp_upstream_requests_total{channel="news",code="error",kind="segment"} 1`,
			errs:     `wsp_upstream_errors_total{channel="news",reason="tls"} 1`,
		},
		{
			name:   "redirects",
			config: allowLoopback,
			run: func(t *testing.T, e *directEnv) {
				for i := range 12 {
					e.up.add(fmt.Sprintf("/loop/%d", i), file{location: fmt.Sprintf("/loop/%d", i+1)})
				}
				e.get(t, e.signer.Path("news", e.upURL+"/loop/0"), nil)
			},
			requests: `wsp_upstream_requests_total{channel="news",code="error",kind="segment"} 1`,
			errs:     `wsp_upstream_errors_total{channel="news",reason="redirects"} 1`,
		},
		{
			name: "rejected playlists",
			// HTTP errors instead of error clips, as before (spec 3.4).
			config: allowLoopback + "\n[upstream]\nmax_playlist_size = \"2MiB\"\n[clips]\nerror = false\n",
			run: func(t *testing.T, e *directEnv) {
				// Auto channel /c/radio turns out to be a playlist.
				e.up.add("/radio", file{ctype: mpegurl, body: "#EXTM3U\n\x00\n"})
				big := "#EXTM3U\n" + strings.Repeat("#"+strings.Repeat("x", 1023)+"\n", 2049)
				e.up.add("/live/master.m3u8", file{ctype: mpegurl, body: big})
				for _, p := range []string{"/c/radio", "/c/news"} {
					if resp, _ := e.get(t, p, nil); resp.StatusCode != http.StatusBadGateway {
						t.Errorf("GET %s: status %d, want 502", p, resp.StatusCode)
					}
				}
			},
			requests: `
wsp_upstream_requests_total{channel="news",code="200",kind="playlist"} 1
wsp_upstream_requests_total{channel="radio",code="200",kind="playlist"} 1
`,
			errs: `
wsp_upstream_errors_total{channel="news",reason="size"} 1
wsp_upstream_errors_total{channel="radio",reason="other"} 1
`,
		},
		{
			name:   "unsupported playlists",
			config: allowLoopback + "\n[clips]\nerror = false\n",
			run: func(t *testing.T, e *directEnv) {
				// Spec 3.3: refused on an auto channel, a raw channel
				// and a resource, before anything is streamed.
				hlsFixture(e.up, e.upURL)
				e.up.add("/radio", file{ctype: "audio/x-scpls", body: "[playlist]\nFile1=http://radio.example/s\n"})
				e.up.add("/cdn/seg.ts", file{ctype: "application/xspf+xml", body: "<playlist/>"})
				for _, p := range []string{"/c/radio", "/c/forced-raw", e.signer.Path("news", e.upURL+"/cdn/seg.ts")} {
					if resp, _ := e.get(t, p, nil); resp.StatusCode != http.StatusBadGateway {
						t.Errorf("GET %s: status %d, want 502", p, resp.StatusCode)
					}
				}
			},
			requests: `
wsp_upstream_requests_total{channel="forced-raw",code="200",kind="playlist"} 1
wsp_upstream_requests_total{channel="news",code="200",kind="playlist"} 1
wsp_upstream_requests_total{channel="radio",code="200",kind="playlist"} 1
`,
			errs: `
wsp_upstream_errors_total{channel="forced-raw",reason="unsupported"} 1
wsp_upstream_errors_total{channel="news",reason="unsupported"} 1
wsp_upstream_errors_total{channel="radio",reason="unsupported"} 1
`,
		},
		{
			name:   "client disconnect",
			config: allowLoopback,
			run: func(t *testing.T, e *directEnv) {
				e.up.add("/radio", file{ctype: "audio/mpeg", body: binaryBody('a', 4096), endless: true})
				e.up.add("/hang", file{hang: true})
				resp := open(t, e.client, e.proxy.URL+"/c/radio")
				if _, err := io.ReadFull(resp.Body, make([]byte, 64<<10)); err != nil {
					t.Fatalf("read stream: %v", err)
				}
				if got := activeRequests(t, e.metrics.Gatherer()); len(got) != 1 || got["radio/raw"] != 1 {
					t.Errorf("active requests with an open stream: %v, want radio/raw 1", got)
				}
				_ = resp.Body.Close()
				<-e.up.gone

				// Gone before the response: no upstream request counted.
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, e.proxy.URL+"/c/hang", nil)
				if err != nil {
					t.Fatal(err)
				}
				c := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 50 * time.Millisecond}}
				defer c.CloseIdleConnections()
				if resp, err := c.Do(req); err == nil {
					_ = resp.Body.Close()
					t.Fatalf("GET /c/hang: status %d, want a client timeout", resp.StatusCode)
				}
			},
			requests: `wsp_upstream_requests_total{channel="radio",code="200",kind="raw"} 1`,
		},
		{
			name:   "unknown channels",
			config: allowLoopback,
			run: func(t *testing.T, e *directEnv) {
				for _, p := range []string{
					"/c/nope", "/c/off",
					"/r/news/AAAAAAAAAAAAAAAAAAAAAA/aHR0cDovL3guZXhhbXBsZS8/data",
					e.signer.Path("nope", e.upURL+"/seg.ts"),
				} {
					if resp, _ := e.get(t, p, nil); resp.StatusCode != http.StatusNotFound {
						t.Errorf("GET %s: status %d, want 404", p, resp.StatusCode)
					}
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newDirectEnv(t, tt.config, channelsTOML)
			tt.run(t, e)
			// Close waits for all handlers, streams included.
			e.client.CloseIdleConnections()
			e.proxy.Close()
			g := e.metrics.Gatherer()
			compareMetrics(t, g, "wsp_upstream_requests_total", requestsHead, tt.requests)
			compareMetrics(t, g, "wsp_upstream_errors_total", errorsHead, tt.errs)
			if got := activeRequests(t, g); len(got) != 0 {
				t.Errorf("active requests after all requests ended: %v", got)
			}
			if strings.Contains(e.logs.String(), "secret") {
				t.Errorf("query string logged:\n%s", e.logs.String())
			}
		})
	}
}

// open starts a GET and returns the response with its body unread.
func open(t *testing.T, c *http.Client, u string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestActiveRequestKinds: an active request counts under its final kind
// once the response type is known.
func TestActiveRequestKinds(t *testing.T) {
	e := newDirectEnv(t, allowLoopback, channelsTOML)
	e.up.add("/radio", file{ctype: "audio/mpeg", body: binaryBody('a', 4096), endless: true})
	e.up.add("/cdn/seg.ts", file{ctype: "video/mp2t", body: binaryBody('s', 4096), endless: true})
	// A segment whose path looks like a playlist.
	e.up.add("/cdn/odd.m3u8", file{ctype: "video/mp2t", body: binaryBody('o', 4096), endless: true})

	var open1 []*http.Response
	for _, p := range []string{"/c/radio", e.signer.Path("news", e.upURL+"/cdn/seg.ts"), e.signer.Path("news", e.upURL+"/cdn/odd.m3u8"), "/c/radio"} {
		resp := open(t, e.client, e.proxy.URL+p)
		if _, err := io.ReadFull(resp.Body, make([]byte, 4096)); err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		open1 = append(open1, resp)
	}
	want := map[string]float64{"radio/raw": 2, "news/segment": 2}
	if got := activeRequests(t, e.metrics.Gatherer()); !equalMaps(got, want) {
		t.Errorf("active requests = %v, want %v", got, want)
	}
	for _, resp := range open1 {
		_ = resp.Body.Close()
	}
	e.client.CloseIdleConnections()
	e.proxy.Close()
	if got := activeRequests(t, e.metrics.Gatherer()); len(got) != 0 {
		t.Errorf("active requests after the streams ended: %v", got)
	}
}

func equalMaps(a, b map[string]float64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// TestMetricsEndpoint: /metrics serves the registry without
// authentication (spec 7.1, 8).
func TestMetricsEndpoint(t *testing.T) {
	e := newDirectEnv(t, allowLoopback, channelsTOML)
	e.up.add("/radio", file{ctype: "audio/mpeg", body: "radio"})
	e.get(t, "/c/radio", nil)
	resp, body := e.get(t, "/metrics", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type %q, want text/plain", ct)
	}
	want := []string{
		`wsp_upstream_requests_total{channel="radio",code="200",kind="raw"} 1`,
		"go_goroutines ",
	}
	if runtime.GOOS == "linux" {
		want = append(want, "process_start_time_seconds ")
	}
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("/metrics lacks %q", w)
		}
	}
	// Without Options.Metrics the server has its own registry.
	s, err := New(Options{Config: e.cfg, Routes: mustRoutes(t, e), Signer: e.signer})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "go_goroutines ") {
		t.Errorf("default registry: status %d", rec.Code)
	}
}

// metricValue returns the value of the sample line name in an exposition
// body, or -1 without one.
func metricValue(t *testing.T, body, name string) float64 {
	t.Helper()
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + ` (\S+)$`).FindStringSubmatch(body)
	if m == nil {
		return -1
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestRunTunnelMetrics: the tunnel gauges follow the tunnel through
// start, handshake age and idle close; Run serves /metrics with them.
func TestRunTunnelMetrics(t *testing.T) {
	const idle = 10 * time.Minute
	up := newUpstream()
	up.add("/seg.ts", file{ctype: "video/mp2t", body: "segment"})
	peer := tunneltest.Start(t, tunneltest.Options{Handler: up})
	peer.SetHost("upstream.test", tunneltest.ServerAddr4)
	dir := writeConfig(t, map[string]string{
		"config.toml":     "[tunnels]\nidle_timeout = \"10m\"\n",
		"tunnels/gr.conf": peer.ClientConfig(),
		"channels.d/gr.toml": `
[[channel]]
name = "Seg"
url = "http://upstream.test/seg.ts"
`,
	})
	clk := newTunnelClock()
	e := startRun(t, dir, 0, func(o *RunOptions) { o.TunnelClock = clk })
	const (
		upName = `wsp_tunnel_up{tunnel="gr"}`
		hsName = `wsp_tunnel_last_handshake_timestamp_seconds{tunnel="gr"}`
	)
	scrape := func() string {
		t.Helper()
		resp, body := do(t, e.client, http.MethodGet, e.url+"/metrics", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /metrics: status %d", resp.StatusCode)
		}
		return body
	}

	// Not started yet.
	body := scrape()
	if up, hs := metricValue(t, body, upName), metricValue(t, body, hsName); up != 0 || hs != 0 {
		t.Errorf("before the first request: up %v, handshake %v, want 0 and 0", up, hs)
	}

	before := time.Now()
	if resp, body := do(t, e.client, http.MethodGet, e.url+"/c/seg", nil); resp.StatusCode != http.StatusOK || body != "segment" {
		t.Fatalf("GET /c/seg: status %d, body %q", resp.StatusCode, body)
	}
	body = scrape()
	hs := metricValue(t, body, hsName)
	if up := metricValue(t, body, upName); up != 1 {
		t.Errorf("after a request: up %v, want 1", up)
	}
	// Handshake times have second resolution in the UAPI dump.
	if lo, hi := float64(before.Unix()-1), float64(time.Now().Unix()+1); hs < lo || hs > hi {
		t.Errorf("handshake %v, want between %v and %v", hs, lo, hi)
	}
	if !slices.Contains(strings.Split(body, "\n"), `wsp_upstream_requests_total{channel="seg",code="200",kind="raw"} 1`) {
		t.Errorf("request metrics missing:\n%s", body)
	}

	// The handshake grows stale: no traffic for more than 180s.
	clk.waitArmed(t, idle)
	clk.advance(5 * time.Minute)
	body = scrape()
	if up, got := metricValue(t, body, upName), metricValue(t, body, hsName); up != 0 || got != hs {
		t.Errorf("after 5m: up %v, handshake %v, want 0 and %v", up, got, hs)
	}

	// Idle close: the device is gone.
	clk.advance(idle)
	body = scrape()
	if up, got := metricValue(t, body, upName), metricValue(t, body, hsName); up != 0 || got != 0 {
		t.Errorf("after idle close: up %v, handshake %v, want 0 and 0", up, got)
	}
}

// mustRoutes returns a new direct route set for e's config.
func mustRoutes(t *testing.T, e *directEnv) *route.Set {
	t.Helper()
	routes, err := route.NewSet(route.Host(), nil, route.Options{Upstream: e.cfg.Upstream(), AllowPrivate: e.cfg.AllowPrivate()})
	if err != nil {
		t.Fatal(err)
	}
	return routes
}
