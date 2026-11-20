// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KingOfKalk/wg-stream-proxy/internal/probe"
	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel/tunneltest"
)

const (
	probeMasterBody = "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\nlow/index.m3u8?token=secret-token\n"
	probeMediaBody  = "#EXTM3U\n#EXTINF:4,\nseg1.ts?token=secret-token\n"
)

// probeEnv is a running proxy with a tunnel gr (the in-process peer reaches
// upstream.test) and the direct route (loopback allowed), with the
// debug listener on.
type probeEnv struct {
	*runEnv
	debugURL string
	direct   *upstream
	directU  string
	tunneled *upstream
}

func newProbeEnv(t *testing.T) *probeEnv {
	t.Helper()
	tun := newUpstream()
	tun.add("/radio", file{ctype: "audio/mpeg", body: "radio-bytes"})
	tun.add("/live/master.m3u8", file{ctype: "application/vnd.apple.mpegurl", body: probeMasterBody})
	tun.add("/live/low/index.m3u8", file{ctype: "application/vnd.apple.mpegurl", body: probeMediaBody})
	tun.add("/live/low/seg1.ts", file{ctype: "video/mp2t", body: "segment"})
	peer := tunneltest.Start(t, tunneltest.Options{Handler: tun})
	peer.SetHost("upstream.test", tunneltest.ServerAddr4)

	dir := tun2dir(t, peer.ClientConfig())
	dir2 := newUpstream()
	dir2.add("/radio", file{ctype: "audio/mpeg", body: "direct-radio"})
	dir2.add("/list.pls", file{ctype: "audio/x-scpls", body: "[playlist]\nFile1=http://x.example/\n"})
	srv := httptest.NewServer(dir2)
	t.Cleanup(srv.Close)

	dl, debugURL := debugListener(t)
	e := startRun(t, dir, 0, func(o *RunOptions) { o.DebugListener = dl })
	return &probeEnv{runEnv: e, debugURL: debugURL, direct: dir2, directU: srv.URL, tunneled: tun}
}

// tun2dir writes the config dir of the probe tests.
func tun2dir(t *testing.T, tunnelFile string) string {
	t.Helper()
	return writeConfig(t, map[string]string{
		"config.toml":     allowLoopback,
		"tunnels/gr.conf": tunnelFile,
	})
}

// post sends body to POST /debug/probe.
func (e *probeEnv) post(t *testing.T, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, e.debugURL+"/debug/probe", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, sb.String()
}

func probeBody(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestDebugProbeTunnelRoute: the probe goes through the running tunnel:
// it starts once, a second probe finds it up, and no second device starts.
func TestDebugProbeTunnelRoute(t *testing.T) {
	e := newProbeEnv(t)
	req := map[string]any{"route": "gr", "url": "http://upstream.test/live/master.m3u8?token=secret-token", "headers": map[string]string{"Referer": "https://ref.example/"}}

	var firstStart *float64
	for i := range 2 {
		code, body := e.post(t, probeBody(t, req))
		if code != http.StatusOK {
			t.Fatalf("probe %d: status %d: %s", i, code, body)
		}
		var rep probe.Report
		if err := json.Unmarshal([]byte(body), &rep); err != nil {
			t.Fatalf("invalid JSON: %v\n%s", err, body)
		}
		if !rep.OK || rep.Mode != probe.ModeProxy || rep.Route != "gr" || len(rep.Steps) != 3 {
			t.Fatalf("probe %d: report = %+v, want ok through gr via the proxy with 3 steps\n%s", i, rep, body)
		}
		if i == 0 {
			firstStart = rep.Steps[0].Timing.TunnelStartMS
			if firstStart == nil {
				t.Errorf("first probe: no tunnel start in the timing: %s", body)
			}
		} else if rep.Steps[0].Timing.TunnelStartMS != nil {
			t.Errorf("second probe: tunnel start %v, want none (tunnel up)", *rep.Steps[0].Timing.TunnelStartMS)
		}
		if strings.Contains(body, "secret-token") || strings.Contains(body, "/live/") {
			t.Errorf("answer leaks the query or path:\n%s", body)
		}
	}
	if got := strings.Count(e.logs.String(), `msg="tunnel started" tunnel=gr`); got != 1 {
		t.Errorf("tunnel started %d times, want once", got)
	}
	for _, r := range e.tunneled.requestsFor("/live/master.m3u8") {
		if r.Referer != "https://ref.example/" {
			t.Errorf("Referer upstream = %q, want the header of the request", r.Referer)
		}
	}
	if got := len(e.tunneled.requestsFor("/live/low/seg1.ts")); got != 2 {
		t.Errorf("segment requests = %d, want 2", got)
	}
}

// TestDebugProbeDirect: the default route is direct; formats and refusals
// are those of serving; a failed probe is still a 200 answer.
func TestDebugProbeDirect(t *testing.T) {
	e := newProbeEnv(t)
	tests := []struct {
		name   string
		req    map[string]any
		ok     bool
		format string
	}{
		{"raw, default route", map[string]any{"url": e.directU + "/radio"}, true, "raw"},
		{"refused pls", map[string]any{"route": "direct", "url": e.directU + "/list.pls"}, false, "refused"},
		{"upstream 404", map[string]any{"url": e.directU + "/missing"}, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, body := e.post(t, probeBody(t, tt.req))
			if code != http.StatusOK {
				t.Fatalf("status %d: %s", code, body)
			}
			var rep probe.Report
			if err := json.Unmarshal([]byte(body), &rep); err != nil {
				t.Fatal(err)
			}
			if rep.OK != tt.ok || rep.Route != "direct" || rep.Steps[0].Format != tt.format {
				t.Errorf("report = %+v, want ok=%v format %q", rep, tt.ok, tt.format)
			}
		})
	}
}

// TestDebugProbeGuard: the private-range guard of the running proxy
// applies: the tunnel route has no loopback exception.
func TestDebugProbeGuard(t *testing.T) {
	e := newProbeEnv(t)
	code, body := e.post(t, probeBody(t, map[string]any{"route": "gr", "url": "http://127.0.0.1:9/x"}))
	var rep probe.Report
	if code != http.StatusOK || json.Unmarshal([]byte(body), &rep) != nil || rep.OK || rep.Steps[0].Class != "guard" {
		t.Errorf("status %d, report %+v, want a 200 answer with a failed guard step\n%s", code, rep, body)
	}
}

func TestDebugProbeErrors(t *testing.T) {
	e := newProbeEnv(t)
	tests := []struct {
		name string
		body string
		code int
		want string
	}{
		{"unknown route", `{"route":"nope","url":"http://a.example/x"}`, http.StatusNotFound, `unknown route "nope"`},
		{"no url", `{"route":"direct"}`, http.StatusBadRequest, "absolute"},
		{"wrong scheme", `{"url":"ftp://a.example/x"}`, http.StatusBadRequest, "absolute"},
		{"not json", `url=http://a.example/x`, http.StatusBadRequest, "JSON object"},
		{"unknown field", `{"url":"http://a.example/x","extra":1}`, http.StatusBadRequest, "JSON object"},
		{"two objects", `{"url":"http://a.example/x"}{"url":"http://a.example/x"}`, http.StatusBadRequest, "JSON object"},
		{"bad header name", `{"url":"http://a.example/x","headers":{"X Bad":"1"}}`, http.StatusBadRequest, "invalid header name"},
		{"bad header value", `{"url":"http://a.example/x","headers":{"X-Ok":"a\nb"}}`, http.StatusBadRequest, "invalid value for header"},
		{"too large", `{"url":"http://a.example/` + strings.Repeat("a", maxProbeBody) + `"}`, http.StatusRequestEntityTooLarge, "16 KiB"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, body := e.post(t, tt.body)
			var got probeError
			if code != tt.code || json.Unmarshal([]byte(body), &got) != nil || !strings.Contains(got.Error, tt.want) {
				t.Errorf("status %d, body %q; want %d with an error containing %q", code, body, tt.code, tt.want)
			}
			if strings.Contains(body, "a.example") {
				t.Errorf("error echoes the URL: %s", body)
			}
		})
	}
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		if code, _ := send(t, e.client, m, e.debugURL+"/debug/probe", nil); code != http.StatusMethodNotAllowed {
			t.Errorf("%s /debug/probe: %d, want 405", m, code)
		}
	}
	// Only on the debug listener.
	if code, _ := send(t, e.client, http.MethodPost, e.url+"/debug/probe", nil); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
		t.Errorf("POST /debug/probe on the main listener: %d, want 404 or 405", code)
	}
}

// TestDebugProbeHostAndOrigin: the guards of the debug listener cover the
// probe: a host name, and a cross-origin browser request, are rejected
// before the handler runs.
func TestDebugProbeHostAndOrigin(t *testing.T) {
	h := newDebugHandler(&debugHandler{})
	tests := []struct {
		name    string
		host    string
		headers map[string]string
		want    int
	}{
		{"host name", "rebound.example:2350", map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusForbidden},
		{"localhost, cross-site", "localhost:2350", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"ip, foreign origin", "127.0.0.1:2350", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		// Rejected by the handler (no server in this test): it was reached.
		{"ip, no browser headers", "127.0.0.1:2350", nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/debug/probe", strings.NewReader(`{"url":"ftp://x"}`))
			r.Host = tt.host
			for k, v := range tt.headers {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			if tt.want == 0 {
				// The handler needs a server for a valid request; an invalid
				// URL is refused before it is used.
				h.ServeHTTP(w, r)
				if w.Code != http.StatusBadRequest {
					t.Errorf("status %d, want 400 from the handler", w.Code)
				}
				return
			}
			h.ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Errorf("status %d, want %d", w.Code, tt.want)
			}
		})
	}
}

// TestDebugProbeIsNoChannelRequest: a probe counts in no request metric and
// logs one line with the logged URL form only.
func TestDebugProbeIsNoChannelRequest(t *testing.T) {
	e := newProbeEnv(t)
	url := "http://upstream.test/live/master.m3u8?token=secret-token"
	if code, body := e.post(t, probeBody(t, map[string]any{"route": "gr", "url": url})); code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}

	_, metrics := send(t, e.client, http.MethodGet, e.url+"/metrics", nil)
	for _, name := range []string{"wsp_upstream_requests_total{", "wsp_active_requests{", "wsp_upstream_phase_seconds_count{", "wsp_upstream_errors_total{"} {
		if strings.Contains(metrics, name) {
			t.Errorf("/metrics has a %s series after a probe", name)
		}
	}
	logs := e.logs.String()
	if got := strings.Count(logs, "msg=probe "); got != 1 {
		t.Errorf("probe log lines = %d, want 1:\n%s", got, logs)
	}
	if !strings.Contains(logs, "route=gr url=http://upstream.test/…/master.m3u8") {
		t.Errorf("logs lack the probe line with the logged URL:\n%s", logs)
	}
	for _, leak := range []string{"secret-token", "token=", "/live/"} {
		if strings.Contains(logs, leak) {
			t.Errorf("logs contain %q:\n%s", leak, logs)
		}
	}
}
