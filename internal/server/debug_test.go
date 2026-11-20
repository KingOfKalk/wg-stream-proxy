// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel/tunneltest"
)

// debugListener returns a listener for the debug endpoints and its base
// URL.
func debugListener(t *testing.T) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln, "http://" + ln.Addr().String()
}

// send sends a request with optional headers and returns the status and
// body.
func send(t *testing.T, c *http.Client, method, u string, h map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, u, err)
	}
	return resp.StatusCode, string(b)
}

func TestDebugOffByDefault(t *testing.T) {
	dir := writeConfig(t, map[string]string{"config.toml": ""})
	e := startRun(t, dir, 0)
	for _, p := range []string{"/debug/pprof/", "/debug/pprof/goroutine", "/debug/tunnels"} {
		if code, _ := send(t, e.client, http.MethodGet, e.url+p, nil); code != http.StatusNotFound {
			t.Errorf("GET %s on the main listener: %d, want 404", p, code)
		}
	}
}

func TestDebugProfiles(t *testing.T) {
	dir := writeConfig(t, map[string]string{"config.toml": ""})
	dl, debugURL := debugListener(t)
	e := startRun(t, dir, 0, func(o *RunOptions) { o.DebugListener = dl })

	for _, p := range []string{"/debug/pprof/", "/debug/pprof/goroutine?debug=1", "/debug/pprof/heap", "/debug/pprof/cmdline"} {
		if code, _ := send(t, e.client, http.MethodGet, debugURL+p, nil); code != http.StatusOK {
			t.Errorf("GET %s: %d, want 200", p, code)
		}
	}
	// The profiler is only on the debug listener.
	if code, _ := send(t, e.client, http.MethodGet, e.url+"/debug/pprof/", nil); code != http.StatusNotFound {
		t.Errorf("GET /debug/pprof/ on the main listener: %d, want 404", code)
	}
	if code, _ := send(t, e.client, http.MethodGet, debugURL+"/playlist.m3u8", nil); code != http.StatusNotFound {
		t.Errorf("GET /playlist.m3u8 on the debug listener: %d, want 404", code)
	}
	if code, _ := send(t, e.client, http.MethodGet, debugURL+"/debug/requests", nil); code != http.StatusNotFound {
		t.Errorf("GET /debug/requests: %d, want 404 (endpoint removed)", code)
	}
}

func TestDebugTunnelsAndCooldown(t *testing.T) {
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
	dl, debugURL := debugListener(t)
	e := startRun(t, dir, 0, func(o *RunOptions) {
		o.TunnelClock = clk
		o.DebugListener = dl
	})
	type tunnelState struct {
		Name           string    `json:"name"`
		State          string    `json:"state"`
		StartedAt      time.Time `json:"started_at"`
		LastHandshake  time.Time `json:"last_handshake"`
		ActiveRequests int       `json:"active_requests"`
	}
	tunnels := func() []tunnelState {
		t.Helper()
		code, body := send(t, e.client, http.MethodGet, debugURL+"/debug/tunnels", nil)
		if code != http.StatusOK {
			t.Fatalf("GET /debug/tunnels: %d %s", code, body)
		}
		var got struct {
			Tunnels []tunnelState `json:"tunnels"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		return got.Tunnels
	}
	stop := func(name string) (int, string) {
		t.Helper()
		return send(t, e.client, http.MethodPost, debugURL+"/debug/tunnels/"+name+"/stop", nil)
	}
	closes := func() int { return strings.Count(e.logs.String(), `msg="tunnel closed" tunnel=gr reason=cooldown`) }
	starts := func() int { return strings.Count(e.logs.String(), `msg="tunnel started" tunnel=gr`) }
	seg := func() {
		t.Helper()
		if code, body := send(t, e.client, http.MethodGet, e.url+"/c/seg", nil); code != http.StatusOK || body != "segment" {
			t.Fatalf("GET /c/seg: %d %q", code, body)
		}
	}

	if got := tunnels(); len(got) != 1 || got[0] != (tunnelState{Name: "gr", State: "stopped"}) {
		t.Errorf("tunnels before the first request = %+v, want gr stopped", got)
	}
	seg()
	// The client has the response before the handler ends the request on
	// the tunnel: wait until it has.
	eventually(t, "the request on gr ended", func() bool {
		g := tunnels()
		return len(g) == 1 && g[0].ActiveRequests == 0
	})
	got := tunnels()
	if len(got) != 1 || got[0].State != "running" || got[0].StartedAt.IsZero() || got[0].LastHandshake.IsZero() || got[0].ActiveRequests != 0 {
		t.Errorf("tunnels after a request = %+v, want gr running with start and handshake times", got)
	}

	// Cool-down of an idle tunnel: stops at once, the route's pooled
	// connection closes, the next request starts it cold.
	if code, body := stop("gr"); code != http.StatusOK || body != `{"name":"gr","was_running":true}`+"\n" {
		t.Fatalf("stop gr: %d %q", code, body)
	}
	if n := closes(); n != 1 {
		t.Fatalf("cool-down close lines = %d, want 1:\n%s", n, e.logs.String())
	}
	if got := tunnels(); got[0].State != "stopped" || !got[0].StartedAt.IsZero() {
		t.Errorf("tunnel after cool-down = %+v, want stopped", got[0])
	}
	if code, body := stop("gr"); code != http.StatusOK || body != `{"name":"gr","was_running":false}`+"\n" {
		t.Errorf("second stop gr: %d %q, want 200 and was_running false", code, body)
	}
	dials := e.tunnel("gr").dials.Load()
	seg()
	if n := starts(); n != 2 {
		t.Errorf("tunnel starts = %d, want 2", n)
	}
	if got := e.tunnel("gr").dials.Load(); got != dials+1 {
		t.Errorf("dials after cool-down = %d, want %d (no pooled connection left)", got, dials+1)
	}

	// An open stream: the cool-down drains, then the timeout cuts it.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, e.url+"/c/radio", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.ReadFull(resp.Body, make([]byte, 4096)); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	stopped := make(chan string, 1)
	go func() {
		// Not t.Fatal in this goroutine: report through the channel.
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, debugURL+"/debug/tunnels/gr/stop", nil)
		r, err := e.client.Do(req)
		if err != nil {
			stopped <- err.Error()
			return
		}
		b, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		stopped <- r.Status + " " + string(b)
	}()
	clk.waitArmed(t, tunnelDrainTimeout)
	if got := tunnels(); got[0].State != "draining" || got[0].ActiveRequests != 1 {
		t.Errorf("tunnel during cool-down = %+v, want draining with 1 request", got[0])
	}
	clk.advance(tunnelDrainTimeout)
	if got := <-stopped; got != `200 OK {"name":"gr","was_running":true}`+"\n" {
		t.Errorf("stop with an open stream: %s", got)
	}
	// The cut stream ends (an endless upstream ends only that way).
	_, _ = io.Copy(io.Discard, resp.Body)

	for _, tt := range []struct {
		method, path string
		header       map[string]string
		want         int
	}{
		{http.MethodPost, "/debug/tunnels/nope/stop", nil, http.StatusNotFound},
		{http.MethodPost, "/debug/tunnels/direct/stop", nil, http.StatusNotFound},
		{http.MethodGet, "/debug/tunnels/gr/stop", nil, http.StatusMethodNotAllowed},
		{http.MethodPost, "/debug/tunnels", nil, http.StatusMethodNotAllowed},
		// A web page in a LAN browser cannot trigger a cool-down.
		{http.MethodPost, "/debug/tunnels/gr/stop", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{http.MethodPost, "/debug/tunnels/gr/stop", map[string]string{"Origin": "http://evil.example"}, http.StatusForbidden},
	} {
		if code, body := send(t, e.client, tt.method, debugURL+tt.path, tt.header); code != tt.want {
			t.Errorf("%s %s %v: %d %q, want %d", tt.method, tt.path, tt.header, code, body, tt.want)
		}
	}
	if n := closes(); n != 2 {
		t.Errorf("cool-down close lines = %d, want 2", n)
	}
}

func TestReloadWarnsOnDebugListenChange(t *testing.T) {
	dir := writeConfig(t, map[string]string{"config.toml": ""})
	e := startRun(t, dir, 0)
	writeFiles(t, dir, map[string]string{"config.toml": "[debug]\nlisten = \"127.0.0.1:2350\"\n"})
	if err := e.reload(t); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !strings.Contains(e.logs.String(), `msg="reload: debug.listen changed, restart required; keeping the old address" debug_listen="" configured=127.0.0.1:2350`) {
		t.Errorf("no restart warning:\n%s", e.logs.String())
	}
}

func TestReloadWarnsOnWatchIntervalChange(t *testing.T) {
	dir := writeConfig(t, map[string]string{"config.toml": ""})
	e := startRun(t, dir, 0)
	writeFiles(t, dir, map[string]string{"config.toml": "[reload]\nwatch_interval = \"10s\"\n"})
	if err := e.reload(t); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !strings.Contains(e.logs.String(), `msg="reload: reload.watch_interval changed, restart required; keeping the old interval" watch_interval=2s configured=10s`) {
		t.Errorf("no restart warning:\n%s", e.logs.String())
	}
}
