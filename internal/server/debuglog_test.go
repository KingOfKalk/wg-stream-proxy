// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/netip"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel/tunneltest"
)

// traceCheck is a network that records whether a lookup or dial carried
// an httptrace.ClientTrace.
type traceCheck struct {
	n      route.Network
	traced atomic.Bool
}

func (c *traceCheck) LookupNetIP(ctx context.Context, host string) ([]netip.Addr, error) {
	if httptrace.ContextClientTrace(ctx) != nil {
		c.traced.Store(true)
	}
	return c.n.LookupNetIP(ctx, host)
}

func (c *traceCheck) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if httptrace.ContextClientTrace(ctx) != nil {
		c.traced.Store(true)
	}
	return c.n.DialContext(ctx, network, address)
}

var logAttr = regexp.MustCompile(`(\w+)=("(?:[^"\\]|\\.)*"|\S*)`)

// requestLines returns the attributes of every "request" debug line.
func requestLines(logs string) []map[string]string {
	var out []map[string]string
	for line := range strings.SplitSeq(logs, "\n") {
		if !strings.Contains(line, " msg=request ") {
			continue
		}
		attrs := map[string]string{}
		for _, m := range logAttr.FindAllStringSubmatch(line, -1) {
			attrs[m[1]] = strings.Trim(m[2], `"`)
		}
		out = append(out, attrs)
	}
	return out
}

// checkLine compares the attributes of a request line with want; "*"
// means present, "-" absent, ">0" a positive duration.
func checkLine(t *testing.T, name string, got map[string]string, want map[string]string) {
	t.Helper()
	for k, w := range want {
		v, ok := got[k]
		switch {
		case w == "-":
			if ok {
				t.Errorf("%s: %s=%s, want absent", name, k, v)
			}
		case !ok:
			t.Errorf("%s: no %s in %v", name, k, got)
		case w == "*":
		case w == ">0":
			if d, err := time.ParseDuration(v); err != nil || d <= 0 {
				t.Errorf("%s: %s=%s, want a positive duration", name, k, v)
			}
		case v != w:
			t.Errorf("%s: %s=%s, want %s", name, k, v, w)
		}
	}
}

// TestRequestDebugLog runs a cold and a warm request through a tunnel, a
// direct one and a failing one at debug level: each writes one request
// line with the fields of spec 8, and no log line contains a query string
// or a key.
func TestRequestDebugLog(t *testing.T) {
	up := newUpstream()
	up.add("/seg.ts", file{ctype: "video/mp2t", body: "segment"})
	peer := tunneltest.Start(t, tunneltest.Options{Handler: up})
	peer.SetHost("upstream.test", tunneltest.ServerAddr4)
	directSrv := httptest.NewServer(up)
	t.Cleanup(directSrv.Close)
	// A port nothing listens on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().String()
	_ = ln.Close()

	dir := writeConfig(t, map[string]string{
		"config.toml":     allowLoopback,
		"tunnels/gr.conf": peer.ClientConfig(),
		"channels.d/gr.toml": `
[[channel]]
name = "Seg"
url = "http://upstream.test/seg.ts?token=secret-token"
`,
		"channels.d/de.toml": `
tunnel = "direct"
[[channel]]
name = "Direct"
url = "` + directSrv.URL + `/seg.ts?token=secret-token"
[[channel]]
name = "Broken"
url = "http://` + closed + `/seg.ts?token=secret-token"
`,
	})
	var direct *traceCheck
	e := startRun(t, dir, 0, func(o *RunOptions) {
		direct = &traceCheck{n: o.Direct}
		o.Direct = direct
	})

	get := func(path string, status, lines int) {
		t.Helper()
		if resp, _ := do(t, e.client, http.MethodGet, e.url+path, nil); resp.StatusCode != status {
			t.Fatalf("GET %s: status %d, want %d", path, resp.StatusCode, status)
		}
		e.logs.waitFor(t, " msg=request ", lines)
	}
	get("/c/seg", http.StatusOK, 1)
	get("/c/seg", http.StatusOK, 2)
	get("/c/direct", http.StatusOK, 3)
	get("/c/broken", http.StatusBadGateway, 4)

	logs := e.logs.String()
	lines := requestLines(logs)
	if len(lines) != 4 {
		t.Fatalf("%d request lines, want 4:\n%s", len(lines), logs)
	}
	common := map[string]string{
		"id": "*", "method": "GET", "kind": "raw", "client_gone": "false",
		"dns": "*", "connect": "*", "connect_attempts": "*", "tls": "0s", "ttfb": "*", "total": ">0",
		"conn_reused": "*", "redirects": "0", "url": "*",
	}
	tests := []struct {
		name string
		want map[string]string
	}{
		{"cold", map[string]string{
			"channel": "seg", "route": "gr", "status": "200", "bytes": "7", "tunnel_start": ">0",
			"connect_attempts": "1", "ttfb": ">0", "conn_reused": "false", "url": "http://upstream.test/seg.ts",
			"reason": "-", "upstream_status": "200",
		}},
		{"warm", map[string]string{
			"channel": "seg", "route": "gr", "status": "200", "bytes": "7", "tunnel_start": "-",
			// A raw channel uses the ICY transport: no keep-alive.
			"connect_attempts": "1", "conn_reused": "false", "ttfb": ">0",
		}},
		{"direct", map[string]string{
			"channel": "direct", "route": "direct", "status": "200", "bytes": "7", "tunnel_start": "-",
			"url": directSrv.URL + "/seg.ts",
		}},
		{"error", map[string]string{
			"channel": "broken", "route": "direct", "status": "502", "reason": "connect", "tunnel_start": "-",
			"connect_attempts": "1", "ttfb": "0s", "upstream_status": "-", "url": "http://" + closed + "/seg.ts",
		}},
	}
	for i, tt := range tests {
		checkLine(t, tt.name, lines[i], common)
		checkLine(t, tt.name, lines[i], tt.want)
	}
	if lines[0]["id"] == lines[1]["id"] {
		t.Errorf("cold and warm request share id %s", lines[0]["id"])
	}
	if !direct.traced.Load() {
		t.Error("direct request at debug level carried no httptrace")
	}

	for _, s := range []string{"secret-token", "token="} {
		if strings.Contains(logs, s) {
			t.Errorf("log contains %q:\n%s", s, logs)
		}
	}
	for line := range strings.SplitSeq(peer.ClientConfig(), "\n") {
		k, v, ok := strings.Cut(line, " = ")
		if !ok || !strings.HasSuffix(k, "Key") {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		for _, f := range []string{v, hex.EncodeToString(b), v[0:4] + "…", "…" + v[39:43]} {
			if strings.Contains(logs, f) {
				t.Errorf("log contains %q of %s", f, k)
			}
		}
	}
}

// TestRequestDebugLogDisabled: above debug level a request writes no
// request line, but is traced all the same: the timing metrics need it.
func TestRequestDebugLogDisabled(t *testing.T) {
	up := newUpstream()
	up.add("/seg.ts", file{ctype: "video/mp2t", body: "segment"})
	directSrv := httptest.NewServer(up)
	t.Cleanup(directSrv.Close)
	dir := writeConfig(t, map[string]string{
		"config.toml": allowLoopback,
		"channels.d/de.toml": `
tunnel = "direct"
[[channel]]
name = "Direct"
url = "` + directSrv.URL + `/seg.ts"
`,
	})
	logs := &syncBuffer{}
	var direct *traceCheck
	e := startRun(t, dir, 0, func(o *RunOptions) {
		o.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
		direct = &traceCheck{n: o.Direct}
		o.Direct = direct
	})
	if resp, body := do(t, e.client, http.MethodGet, e.url+"/c/direct", nil); resp.StatusCode != http.StatusOK || body != "segment" {
		t.Fatalf("GET /c/direct: status %d, body %q", resp.StatusCode, body)
	}
	if !direct.traced.Load() {
		t.Error("request above debug level carried no httptrace")
	}
	if strings.Contains(logs.String(), "msg=request") {
		t.Errorf("request line above debug level:\n%s", logs.String())
	}
}

// TestLogsHideUpstreamURLPaths: an upstream URL with a token in the path
// and one in the query reaches no log line (request, playlist, stream,
// upstream status and connect error lines): only scheme, host and file
// name are logged (spec 8).
func TestLogsHideUpstreamURLPaths(t *testing.T) {
	const idToken = "0123456789abcdef0123456789abcdef"
	up := newUpstream()
	up.add("/live/"+idToken+"/master.m3u8", file{ctype: "application/vnd.apple.mpegurl", body: "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\nseg.ts\n"})
	up.add("/gone/"+idToken+"/x.m3u8", file{status: http.StatusServiceUnavailable})
	up.add("/raw/"+idToken+"/seg.ts", file{ctype: "video/mp2t", body: "segment"})
	directSrv := httptest.NewServer(up)
	t.Cleanup(directSrv.Close)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().String()
	_ = ln.Close()

	dir := writeConfig(t, map[string]string{
		"config.toml": allowLoopback,
		"channels.d/de.toml": `
tunnel = "direct"
[[channel]]
name = "Hls"
url = "` + directSrv.URL + `/live/` + idToken + `/master.m3u8?token=secret-token"
[[channel]]
name = "Raw"
url = "` + directSrv.URL + `/raw/` + idToken + `/seg.ts?token=secret-token"
[[channel]]
name = "Gone"
url = "` + directSrv.URL + `/gone/` + idToken + `/x.m3u8?token=secret-token"
[[channel]]
name = "Broken"
url = "http://` + closed + `/broken/` + idToken + `/seg.ts?token=secret-token"
`,
	})
	e := startRun(t, dir, 0)
	for i, tt := range []struct {
		path   string
		status int
	}{{"/c/hls", http.StatusOK}, {"/c/raw", http.StatusOK}, {"/c/gone", http.StatusBadGateway}, {"/c/broken", http.StatusBadGateway}} {
		if resp, _ := do(t, e.client, http.MethodGet, e.url+tt.path, nil); resp.StatusCode != tt.status {
			t.Fatalf("GET %s: status %d, want %d", tt.path, resp.StatusCode, tt.status)
		}
		e.logs.waitFor(t, " msg=request ", i+1)
	}

	logs := e.logs.String()
	for _, s := range []string{idToken, "secret-token", "token=", "/live/", "/raw/", "/gone/", "/broken/"} {
		if strings.Contains(logs, s) {
			t.Errorf("log contains %q:\n%s", s, logs)
		}
	}
	// Each line type is present, with the reduced URL.
	for _, want := range []string{
		`msg=playlist channel=hls url=` + directSrv.URL + "/…/master.m3u8",
		`msg="stream done" channel=raw url=` + directSrv.URL + "/…/seg.ts",
		`msg="upstream error status" channel=gone route=direct url=` + directSrv.URL + "/…/x.m3u8",
		`msg="upstream request failed"`,
		"error=\"upstream http://" + closed + "/…/seg.ts: ",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}
	for _, l := range requestLines(logs) {
		if !strings.Contains(l["url"], "/…/") {
			t.Errorf("request line url = %q, want the reduced form", l["url"])
		}
	}
}
