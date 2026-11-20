// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const (
	probeHLSType = "application/vnd.apple.mpegurl"
	probeMaster  = "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\nlow/index.m3u8?token=secret\n#EXT-X-STREAM-INF:BANDWIDTH=2000\nhigh/index.m3u8\n"
	probeMedia   = "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\nseg1.ts?token=secret\n#EXTINF:4,\nseg2.ts\n"
)

// probeUpstream serves the fixtures of the probe tests.
type probeUpstream struct{}

func (u *probeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

func probeDir(t *testing.T) string {
	t.Helper()
	return writeDir(t, map[string]string{"config.toml": "[upstream]\nuser_agent = \"wsp-probe-test\"\n"})
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
		{"standalone flag is gone", []string{"probe", "--config-dir", dir, "--standalone", "http://a.example/x"}, `see "wg-stream-proxy help probe"`},
		{"unknown flag", []string{"probe", "--config-dir", dir, "--nope", "http://a.example/x"}, `see "wg-stream-proxy help probe"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, out, errOut := testCLI(nil)

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

// TestProbeInvalidConfigToml: a config.toml that does not decode fails
// the probe before any request: there is no address to trust.
func TestProbeInvalidConfigToml(t *testing.T) {
	dir := writeDir(t, map[string]string{"config.toml": "bogus = 1\n"})
	c, out, errOut := testCLI(nil)

	code := c.run([]string{"probe", "--config-dir", dir, "http://a.example/x"})

	if code != 1 || out.String() != "" || !strings.Contains(errOut.String(), "invalid config") || !strings.Contains(errOut.String(), "bogus") {
		t.Errorf("exit code = %d, stdout %q, stderr %q; want 1, nothing, the config errors", code, out, errOut)
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

// probeJSON is the part of the JSON output the tests check.
type probeJSON struct {
	OK    bool `json:"ok"`
	Steps []struct {
		Role string `json:"role"`
	} `json:"steps"`
}

// proxyReport is what the running proxy answers for a raw stream.
const proxyReport = `{"mode":"proxy","route":"gr","ok":true,"steps":[{"role":"url","url":"https://cdn.example.com/…/live.ts","status":200,"content_type":"video/mp2t","format":"raw","streaming":true,"ok":true,"timing":{"tunnel_start_ms":812.5,"ttfb_ms":40.2,"total_ms":41}}]}` + "\n"

// TestProbeViaProxyOutput: the report of the running proxy is printed as
// text or JSON; the mode shows in both.
func TestProbeViaProxyOutput(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantStdout string
	}{
		{"text", nil, "route gr (via running proxy)"},
		{"json", []string{"--json"}, `"mode": "proxy"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeProbeDebug(t, http.StatusOK, proxyReport)
			dir := debugConfig(t, f.Listener.Addr().String())
			c, out, errOut := testCLI(nil)
			args := append([]string{"probe", "--config-dir", dir}, tt.args...)

			code := c.run(append(args, "https://cdn.example.com/live.ts?token=secret"))

			if code != 0 || errOut.String() != "" {
				t.Fatalf("exit code = %d, stderr %q; want 0, nothing", code, errOut)
			}
			if !strings.Contains(out.String(), tt.wantStdout) {
				t.Errorf("stdout lacks %q:\n%s", tt.wantStdout, out)
			}
			if got := f.requests(); len(got) != 1 {
				t.Errorf("debug requests = %v, want 1", got)
			}
		})
	}
}

// TestProbeNeedsRunningProxy: without [debug] listen, or with nothing
// listening there, probe exits with an error that says what to do; it
// makes no request of its own.
func TestProbeNeedsRunningProxy(t *testing.T) {
	up := httptest.NewServer(&probeUpstream{})
	t.Cleanup(up.Close)
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().String()
	_ = closed.Close()

	tests := []struct {
		name string
		cfg  string
		want []string
	}{
		{"no debug listener", "", []string{"[debug] listen is not set", "set it in config.toml and start the proxy"}},
		{"nothing listens", "[debug]\nlisten = \"" + closedAddr + "\"\n", []string{"nothing listens on [debug] listen " + closedAddr, "start the proxy"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeDir(t, map[string]string{"config.toml": tt.cfg + "[upstream.allow_private]\ndirect = [\"127.0.0.0/8\"]\n"})
			c, out, errOut := testCLI(nil)

			code := c.run([]string{"probe", "--config-dir", dir, up.URL + "/radio?token=secret"})

			if code != 1 || out.String() != "" {
				t.Errorf("exit code = %d, stdout %q; want 1, nothing", code, out)
			}
			for _, w := range tt.want {
				if !strings.Contains(errOut.String(), w) {
					t.Errorf("stderr lacks %q:\n%s", w, errOut)
				}
			}
			if strings.Contains(errOut.String(), "standalone") || strings.Contains(errOut.String(), "secret") {
				t.Errorf("stderr mentions standalone or leaks the URL: %q", errOut)
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
// not worked around.
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
			if strings.Contains(errOut.String(), "secret") {
				t.Errorf("stderr leaks the URL: %q", errOut)
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
	srv, _, srvLog := testCLI(nil)
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
	if got := srv.run([]string{"--config-dir", dir, "--listen", "127.0.0.1:0"}); got != 0 {
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
