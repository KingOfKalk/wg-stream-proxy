// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel/tunneltest"
)

const (
	probeHLSType = "application/vnd.apple.mpegurl"
	probeMaster  = "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\nlow/index.m3u8?token=secret\n#EXT-X-STREAM-INF:BANDWIDTH=2000\nhigh/index.m3u8\n"
	probeMedia   = "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\nseg1.ts?token=secret\n#EXTINF:4,\nseg2.ts\n"
)

// probeUpstream serves the fixtures of the probe tests and records the
// request headers per path.
type probeUpstream struct {
	mu   sync.Mutex
	seen map[string]http.Header
}

func (u *probeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	if u.seen == nil {
		u.seen = map[string]http.Header{}
	}
	u.seen[r.URL.Path] = r.Header.Clone()
	u.mu.Unlock()
	switch r.URL.Path {
	case "/live/master.m3u8":
		w.Header().Set("Content-Type", probeHLSType)
		_, _ = w.Write([]byte(probeMaster))
	case "/live/low/index.m3u8":
		w.Header().Set("Content-Type", probeHLSType)
		_, _ = w.Write([]byte(probeMedia))
	case "/live/low/seg1.ts":
		w.Header().Set("Content-Type", "video/mp2t")
		_, _ = w.Write([]byte("segment"))
	case "/radio":
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("ID3-audio-bytes"))
	case "/list.pls":
		w.Header().Set("Content-Type", "audio/x-scpls")
		_, _ = w.Write([]byte("[playlist]\nFile1=http://other.example/stream\n"))
	case "/manifest.mpd":
		w.Header().Set("Content-Type", "application/dash+xml")
		_, _ = w.Write([]byte(`<MPD><Period><AdaptationSet><SegmentTemplate media="$Number$.m4s"/></AdaptationSet></Period></MPD>`))
	default:
		http.NotFound(w, r)
	}
}

func (u *probeUpstream) header(path string) http.Header {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.seen[path]
}

// probeCLI returns a cli whose direct route may reach loopback.
func probeCLI() (*cli, *syncBuffer, *syncBuffer) {
	c, out, errOut := testCLI(nil)
	c.allowPrivate = map[string][]netip.Prefix{"direct": {netip.MustParsePrefix("127.0.0.0/8")}}
	return c, out, errOut
}

func probeDir(t *testing.T) string {
	t.Helper()
	return writeDir(t, map[string]string{"config.toml": "[upstream]\nuser_agent = \"wsp-probe-test\"\n"})
}

func TestProbeDirect(t *testing.T) {
	up := &probeUpstream{}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	dir := probeDir(t)
	tests := []struct {
		name       string
		path       string
		allow      bool
		wantCode   int
		wantStdout []string
	}{
		{
			"hls master", "/live/master.m3u8", true, 0,
			[]string{"route direct", "HLS master playlist, 2 variants", "variant playlist:", "HLS media playlist, 2 segments", "first segment:", "result: ok"},
		},
		{"raw stream", "/radio", true, 0, []string{"raw stream", "15 bytes", "audio/mpeg", "result: ok"}},
		{"refused pls", "/list.pls", true, 1, []string{"refused: pls playlist", "unsupported", "result: failed"}},
		{"dash manifest", "/manifest.mpd", true, 0, []string{"DASH manifest, 1 period, 1 adaptation set", "result: ok"}},
		{"upstream 404", "/missing", true, 1, []string{"status        404", "status: upstream status 404", "result: failed"}},
		{"guard refusal", "/radio", false, 1, []string{"guard:", "result: failed"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, out, errOut := probeCLI()
			if !tt.allow {
				c.allowPrivate = nil
			}

			code := c.run([]string{"probe", "--config-dir", dir, srv.URL + tt.path + "?token=secret"})

			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stderr %q)", code, tt.wantCode, errOut)
			}
			for _, w := range tt.wantStdout {
				if !strings.Contains(out.String(), w) {
					t.Errorf("stdout lacks %q:\n%s", w, out)
				}
			}
			if strings.Contains(out.String()+errOut.String(), "secret") {
				t.Errorf("output contains the query string:\nstdout:\n%s\nstderr:\n%s", out, errOut)
			}
			if got := up.header("/radio").Get("User-Agent"); tt.path == "/radio" && tt.allow && got != "wsp-probe-test" {
				t.Errorf("User-Agent = %q, want the [upstream] user_agent", got)
			}
		})
	}
}

// probeJSON is the part of the JSON output the tests check.
type probeJSON struct {
	Route string `json:"route"`
	OK    bool   `json:"ok"`
	Steps []struct {
		Role    string `json:"role"`
		URL     string `json:"url"`
		Status  int    `json:"status"`
		Format  string `json:"format"`
		Refused string `json:"refused"`
		Class   string `json:"class"`
		OK      bool   `json:"ok"`
		Timing  struct {
			TunnelStartMS *float64 `json:"tunnel_start_ms"`
			TTFBMS        *float64 `json:"ttfb_ms"`
			TotalMS       float64  `json:"total_ms"`
		} `json:"timing"`
	} `json:"steps"`
}

func TestProbeJSON(t *testing.T) {
	srv := httptest.NewServer(&probeUpstream{})
	t.Cleanup(srv.Close)
	dir := probeDir(t)
	tests := []struct {
		name     string
		path     string
		wantCode int
		wantOK   bool
		formats  []string
		class    string
	}{
		{"hls master", "/live/master.m3u8", 0, true, []string{"hls-master", "hls-media", ""}, ""},
		{"raw", "/radio", 0, true, []string{"raw"}, ""},
		{"refused", "/list.pls", 1, false, []string{"refused"}, "unsupported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, out, errOut := probeCLI()

			code := c.run([]string{"probe", srv.URL + tt.path, "--json", "--config-dir", dir})

			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d (stderr %q)", code, tt.wantCode, errOut)
			}
			var got probeJSON
			if err := json.Unmarshal([]byte(out.String()), &got); err != nil {
				t.Fatalf("invalid JSON: %v\n%s", err, out)
			}
			if got.Route != "direct" || got.OK != tt.wantOK || len(got.Steps) != len(tt.formats) {
				t.Fatalf("report = %+v, want ok=%v with %d steps", got, tt.wantOK, len(tt.formats))
			}
			for i, f := range tt.formats {
				s := got.Steps[i]
				if s.Format != f || s.Timing.TotalMS <= 0 || s.Timing.TTFBMS == nil {
					t.Errorf("step %d = %+v, want format %q with timing", i, s, f)
				}
			}
			if got.Steps[0].Class != tt.class {
				t.Errorf("class = %q, want %q", got.Steps[0].Class, tt.class)
			}
		})
	}
}

// TestProbeHeaders: --header goes to every upstream request, later values
// replace earlier ones, and the URL may stand anywhere among the flags.
func TestProbeHeaders(t *testing.T) {
	up := &probeUpstream{}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	dir := probeDir(t)
	c, _, errOut := probeCLI()

	code := c.run([]string{
		"probe", "--header", "Referer=https://one.example/", "--config-dir", dir, srv.URL + "/live/master.m3u8",
		"--header", "referer=https://ref.example/", "--header", "X-Token=abc=def", "--header", "User-Agent=",
	})

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, errOut)
	}
	for _, p := range []string{"/live/master.m3u8", "/live/low/index.m3u8", "/live/low/seg1.ts"} {
		h := up.header(p)
		if h.Get("Referer") != "https://ref.example/" || h.Get("X-Token") != "abc=def" {
			t.Errorf("%s: headers = %v, want Referer and X-Token", p, h)
		}
		if _, ok := h["User-Agent"]; ok {
			t.Errorf("%s: User-Agent = %v, want none (empty value)", p, h["User-Agent"])
		}
	}
}

func TestProbeUsageErrors(t *testing.T) {
	dir := probeDir(t)
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no url", []string{"probe", "--config-dir", dir}, "want exactly one URL"},
		{"two urls", []string{"probe", "--config-dir", dir, "http://a.example/x", "http://b.example/y"}, "want exactly one URL"},
		{"not a url", []string{"probe", "--config-dir", dir, "radio"}, "must be absolute with scheme http or https"},
		{"wrong scheme", []string{"probe", "--config-dir", dir, "ftp://a.example/x"}, "must be absolute with scheme http or https"},
		{"header without value", []string{"probe", "--config-dir", dir, "--header", "X-Token", "http://a.example/x"}, "want Name=Value"},
		{"invalid header name", []string{"probe", "--config-dir", dir, "--header", "X Token=1", "http://a.example/x"}, "invalid header name"},
		{"header with line break", []string{"probe", "--config-dir", dir, "--header", "X-Token=a\nb", "http://a.example/x"}, "invalid value for header"},
		{"unknown route", []string{"probe", "--config-dir", dir, "--route", "nope", "http://a.example/x"}, `unknown route "nope" (want one of: direct)`},
		{"unknown flag", []string{"probe", "--config-dir", dir, "--nope", "http://a.example/x"}, `see "wg-stream-proxy help probe"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, out, errOut := probeCLI()

			code := c.run(tt.args)

			if code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			if out.String() != "" {
				t.Errorf("stdout = %q, want empty", out)
			}
			if !strings.Contains(errOut.String(), tt.want) {
				t.Errorf("stderr = %q, want %q", errOut, tt.want)
			}
		})
	}
}

func TestProbeInvalidConfig(t *testing.T) {
	dir := writeDir(t, map[string]string{"config.toml": "[upstream]\nconnect_timeout = \"soon\"\n"})
	c, out, errOut := probeCLI()

	code := c.run([]string{"probe", "--config-dir", dir, "http://a.example/x"})

	if code != 1 || out.String() != "" || !strings.Contains(errOut.String(), "invalid config") {
		t.Errorf("exit code = %d, stdout %q, stderr %q; want 1, nothing, the config errors", code, out, errOut)
	}
}

// TestProbeInvalidConfigToml: a config.toml that does not decode fails
// the probe before any request: there is no address to trust.
func TestProbeInvalidConfigToml(t *testing.T) {
	dir := writeDir(t, map[string]string{"config.toml": "bogus = 1\n"})
	c, out, errOut := probeCLI()

	code := c.run([]string{"probe", "--config-dir", dir, "http://a.example/x"})

	if code != 1 || out.String() != "" || !strings.Contains(errOut.String(), "invalid config") || !strings.Contains(errOut.String(), "bogus") {
		t.Errorf("exit code = %d, stdout %q, stderr %q; want 1, nothing, the config errors", code, out, errOut)
	}
	if strings.Contains(errOut.String(), "standalone") {
		t.Errorf("stderr = %q, must fail before the standalone fallback", errOut)
	}
}

// TestProbeTunnelRoute: the route may be a tunnel; it starts for the
// probe (the tunnel start shows in the timing), reaches an upstream only
// the tunnel can reach, and stops afterwards (goleak checks that nothing
// is left running).
func TestProbeTunnelRoute(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{Handler: &probeUpstream{}})
	peer.SetHost("upstream.test", tunneltest.ServerAddr4)
	dir := writeDir(t, map[string]string{
		"config.toml":         "",
		"tunnels/gr.conf":     peer.ClientConfig(),
		"tunnels/broken.conf": "not a tunnel file", // not loaded: only the route's tunnel is
	})
	c, out, errOut := testCLI(nil)

	code := c.run([]string{"probe", "--config-dir", dir, "--route", "gr", "--json", "http://upstream.test/live/master.m3u8"})

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, errOut)
	}
	var got probeJSON
	if err := json.Unmarshal([]byte(out.String()), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if got.Route != "gr" || !got.OK || len(got.Steps) != 3 {
		t.Fatalf("report = %+v, want ok through gr with 3 steps", got)
	}
	if got.Steps[0].Timing.TunnelStartMS == nil || *got.Steps[0].Timing.TunnelStartMS <= 0 {
		t.Errorf("first step timing = %+v, want the tunnel start", got.Steps[0].Timing)
	}
	if got.Steps[1].Timing.TunnelStartMS != nil {
		t.Errorf("second step timing = %+v, want no tunnel start (running)", got.Steps[1].Timing)
	}
}

// TestProbeTunnelDown: a tunnel that cannot start fails the probe with
// exit code 1 and a reason.
func TestProbeTunnelDown(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{Handler: &probeUpstream{}})
	peer.SetHost("upstream.test", tunneltest.ServerAddr4)
	peer.SetReachable(false)
	dir := writeDir(t, map[string]string{
		"config.toml":     "[upstream]\nconnect_timeout = \"200ms\"\n",
		"tunnels/gr.conf": peer.ClientConfig(),
	})
	c, out, errOut := testCLI(nil)

	code := c.run([]string{"probe", "--config-dir", dir, "--route", "gr", "http://upstream.test/radio"})

	if code != 1 || !strings.Contains(out.String(), "result: failed") {
		t.Errorf("exit code = %d, stdout:\n%s\nstderr:\n%s; want 1 and a failed result", code, out, errOut)
	}
}

// fakeProbeDebug is a debug listener stand-in for POST /debug/probe: it
// records the requests and answers with status and body.
type fakeProbeDebug struct {
	*httptest.Server
	mu      sync.Mutex
	reqs    []string // method, request URI and body
	headers []http.Header
}

func newFakeProbeDebug(t *testing.T, status int, body string) *fakeProbeDebug {
	t.Helper()
	f := &fakeProbeDebug{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, r.Method+" "+r.RequestURI+" "+string(b))
		f.headers = append(f.headers, r.Header.Clone())
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeProbeDebug) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqs...)
}

// proxyReport is what the running proxy answers for a raw stream.
const proxyReport = `{"mode":"proxy","route":"gr","ok":true,"steps":[{"role":"url","url":"https://cdn.example.com/…/live.ts","status":200,"content_type":"video/mp2t","format":"raw","streaming":true,"ok":true,"timing":{"tunnel_start_ms":812.5,"ttfb_ms":40.2,"total_ms":41}}]}` + "\n"

// TestProbeModeSelection: a running proxy with a debug listener makes the
// probe; without one, or without anything listening, the command runs
// standalone and says so on stderr; --standalone forces that.
func TestProbeModeSelection(t *testing.T) {
	up := httptest.NewServer(&probeUpstream{})
	t.Cleanup(up.Close)
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().String()
	_ = closed.Close()
	target := up.URL + "/radio?token=secret"

	tests := []struct {
		name       string
		debug      string // [debug] listen; "fake" is the fake listener, "" none
		args       []string
		wantStdout string
		wantStderr string
		fakeCalls  int
	}{
		{"debug listener answers", "fake", nil, "route gr (via running proxy)", "", 1},
		{"debug listener answers, json", "fake", []string{"--json"}, `"mode": "proxy"`, "", 1},
		{"no debug listener", "", nil, "route direct (standalone)", "[debug] listen is not set, running standalone", 0},
		{"nothing listens", closedAddr, nil, "route direct (standalone)", "nothing listens on [debug] listen " + closedAddr + ", running standalone", 0},
		{"forced standalone", "fake", []string{"--standalone"}, "route direct (standalone)", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeProbeDebug(t, http.StatusOK, proxyReport)
			cfg := "[upstream.allow_private]\ndirect = [\"127.0.0.0/8\"]\n"
			switch tt.debug {
			case "fake":
				cfg += "[debug]\nlisten = \"" + f.Listener.Addr().String() + "\"\n"
			case "":
			default:
				cfg += "[debug]\nlisten = \"" + tt.debug + "\"\n"
			}
			dir := writeDir(t, map[string]string{"config.toml": cfg})
			c, out, errOut := testCLI(nil)
			args := append([]string{"probe", "--config-dir", dir}, tt.args...)
			args = append(args, target)

			code := c.run(args)

			if code != 0 {
				t.Fatalf("exit code = %d, want 0 (stderr %q)", code, errOut)
			}
			if !strings.Contains(out.String(), tt.wantStdout) {
				t.Errorf("stdout lacks %q:\n%s", tt.wantStdout, out)
			}
			if tt.wantStderr == "" && errOut.String() != "" || !strings.Contains(errOut.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want %q", errOut, tt.wantStderr)
			}
			if got := f.requests(); len(got) != tt.fakeCalls {
				t.Fatalf("debug requests = %v, want %d", got, tt.fakeCalls)
			}
		})
	}
}

// TestProbeViaProxyRequest: the URL and headers go in the body of a POST,
// never in the request line.
func TestProbeViaProxyRequest(t *testing.T) {
	f := newFakeProbeDebug(t, http.StatusOK, proxyReport)
	dir := debugConfig(t, f.Listener.Addr().String())
	c, _, errOut := testCLI(nil)

	code := c.run([]string{"probe", "--config-dir", dir, "--route", "gr", "--header", "Referer=https://ref.example/", "https://cdn.example.com/live/a.ts?token=secret"})

	if code != 0 {
		t.Fatalf("exit code = %d (stderr %q)", code, errOut)
	}
	want := `POST /debug/probe {"route":"gr","url":"https://cdn.example.com/live/a.ts?token=secret","headers":{"Referer":"https://ref.example/"}}`
	if got := f.requests(); len(got) != 1 || got[0] != want {
		t.Errorf("requests = %q, want %q", got, want)
	}
	if ct := f.headers[0].Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// TestProbeProxyErrors: an error of the debug listener is reported, not
// worked around by a standalone probe.
func TestProbeProxyErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		code   int
		want   string
	}{
		{"unknown route", http.StatusNotFound, `{"error":"unknown route \"nope\""}`, 2, `the running proxy: unknown route "nope"`},
		{"bad request", http.StatusBadRequest, `{"error":"invalid header name \"X Y\""}`, 2, "the running proxy: invalid header name"},
		{"older proxy", http.StatusNotFound, "404 page not found\n", 1, "does not know POST /debug/probe"},
		{"server error", http.StatusInternalServerError, "boom\n", 1, "answered status 500"},
		{"error text", http.StatusServiceUnavailable, `{"error":"shutting down"}`, 1, "the running proxy: shutting down"},
		{"invalid answer", http.StatusOK, "not json", 1, "invalid answer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeProbeDebug(t, tt.status, tt.body)
			dir := debugConfig(t, f.Listener.Addr().String())
			c, out, errOut := testCLI(nil)

			code := c.run([]string{"probe", "--config-dir", dir, "http://a.example/x?token=secret"})

			if code != tt.code || out.String() != "" || !strings.Contains(errOut.String(), tt.want) {
				t.Errorf("exit code %d, stdout %q, stderr %q; want %d, nothing, %q", code, out, errOut, tt.code, tt.want)
			}
			if strings.Contains(errOut.String(), "running standalone") || strings.Contains(errOut.String(), "secret") {
				t.Errorf("stderr falls back or leaks the URL: %q", errOut)
			}
			if len(f.requests()) != 1 {
				t.Errorf("debug requests = %v, want 1", f.requests())
			}
		})
	}
}

// TestProbeViaRunningProxy: the whole path against a real serve: the
// probe goes through the running proxy, whose own config (guard
// exception for loopback) decides.
func TestProbeViaRunningProxy(t *testing.T) {
	up := httptest.NewServer(&probeUpstream{})
	t.Cleanup(up.Close)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	debugAddr := ln.Addr().String()
	_ = ln.Close()
	dir := writeDir(t, map[string]string{"config.toml": "[debug]\nlisten = \"" + debugAddr + "\"\n[upstream.allow_private]\ndirect = [\"127.0.0.0/8\"]\n"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, _, srvLog := testCLI(map[string]string{"WG_STREAM_PROXY_LISTEN": "127.0.0.1:0"})
	srv.ctx = ctx
	var code int
	var out, errOut *syncBuffer
	srv.debugListening = func(net.Addr) {
		go func() {
			defer cancel()
			// No guard exception of its own: only the proxy's config counts.
			var c *cli
			c, out, errOut = testCLI(nil)
			code = c.run([]string{"probe", "--config-dir", dir, "--json", up.URL + "/live/master.m3u8?token=secret"})
		}()
	}
	if got := srv.run([]string{"--config-dir", dir}); got != 0 {
		t.Fatalf("serve exit code = %d; logs:\n%s", got, srvLog)
	}

	if code != 0 || errOut.String() != "" {
		t.Fatalf("probe exit code %d, stderr %q, stdout:\n%s", code, errOut, out)
	}
	var rep probeJSON
	if err := json.Unmarshal([]byte(out.String()), &rep); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if !rep.OK || len(rep.Steps) != 3 || !strings.Contains(out.String(), `"mode": "proxy"`) {
		t.Errorf("report = %+v\n%s", rep, out)
	}
	logs := srvLog.String()
	if !strings.Contains(logs, "msg=probe route=direct url="+up.URL+"/…/master.m3u8") {
		t.Errorf("proxy log lacks the probe line:\n%s", logs)
	}
	if strings.Contains(logs, "secret") || strings.Contains(logs, "/live/") {
		t.Errorf("proxy log leaks the query or path:\n%s", logs)
	}
}
