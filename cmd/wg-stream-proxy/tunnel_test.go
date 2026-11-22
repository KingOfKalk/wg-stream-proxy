// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDebugBaseURL(t *testing.T) {
	tests := []struct {
		listen, want string
	}{
		{":2350", "http://127.0.0.1:2350"},
		{"0.0.0.0:2350", "http://127.0.0.1:2350"},
		{"[::]:2350", "http://[::1]:2350"},
		{"127.0.0.1:2350", "http://127.0.0.1:2350"},
		{"192.0.2.7:2350", "http://192.0.2.7:2350"},
		{"[2001:db8::1]:2350", "http://[2001:db8::1]:2350"},
		{"[fe80::1%eth0]:2350", "http://[fe80::1%25eth0]:2350"},
		{"[fe80::1%en0.1]:2350", "http://[fe80::1%25en0.1]:2350"},
	}
	for _, tt := range tests {
		t.Run(tt.listen, func(t *testing.T) {
			got, err := localURL(tt.listen, "")
			if err != nil || got != tt.want {
				t.Errorf("localURL(%q) = %q, %v; want %q", tt.listen, got, err, tt.want)
			}
		})
	}
}

// fakeDebug is a debug listener stand-in that records the requests.
type fakeDebug struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []string
}

func newFakeDebug(t *testing.T) *fakeDebug {
	t.Helper()
	f := &fakeDebug{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /debug/tunnels", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tunnels":[` +
			`{"name":"de-1","state":"stopped","active_requests":0},` +
			`{"name":"gr","state":"running","started_at":"2026-11-18T10:00:00Z","last_handshake":"2026-11-18T10:01:30Z","active_requests":2}` +
			`]}` + "\n"))
	})
	mux.HandleFunc("POST /debug/tunnels/{name}/stop", func(w http.ResponseWriter, r *http.Request) {
		switch name := r.PathValue("name"); name {
		case "gr":
			_, _ = w.Write([]byte(`{"name":"gr","was_running":true}` + "\n"))
		case "de-1":
			_, _ = w.Write([]byte(`{"name":"de-1","was_running":false}` + "\n"))
		case "broken":
			http.Error(w, "boom", http.StatusServiceUnavailable)
		default:
			http.Error(w, "no such tunnel", http.StatusNotFound)
		}
	})
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.reqs = append(f.reqs, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeDebug) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqs...)
}

// debugConfig returns a config dir whose [debug] listen is addr.
func debugConfig(t *testing.T, addr string) string {
	t.Helper()
	return writeDir(t, map[string]string{"config.toml": "[debug]\nlisten = \"" + addr + "\"\n"})
}

func TestTunnelList(t *testing.T) {
	f := newFakeDebug(t)
	dir := debugConfig(t, f.Listener.Addr().String())
	c, out, errOut := testCLI(nil)
	if code := c.run([]string{"tunnel", "list", "--config-dir", dir}); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, errOut)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	want := [][]string{
		{"NAME", "STATE", "REQUESTS", "STARTED", "LAST", "HANDSHAKE"},
		{"de-1", "stopped", "0", "-", "-"},
		{"gr", "running", "2", "2026-11-18T10:00:00Z", "2026-11-18T10:01:30Z"},
	}
	if len(lines) != len(want) {
		t.Fatalf("output:\n%s", out)
	}
	for i, w := range want {
		if got := strings.Fields(lines[i]); strings.Join(got, " ") != strings.Join(w, " ") {
			t.Errorf("line %d = %q, want fields %v", i, lines[i], w)
		}
	}
	if got := f.requests(); len(got) != 1 || got[0] != "GET /debug/tunnels" {
		t.Errorf("requests = %v", got)
	}
}

func TestTunnelStop(t *testing.T) {
	f := newFakeDebug(t)
	dir := debugConfig(t, f.Listener.Addr().String())
	tests := []struct {
		name    string
		args    []string
		code    int
		stdout  string
		stderr  string
		request string
	}{
		{"running", []string{"stop", "gr", "--config-dir", dir}, 0, "tunnel gr stopped\n", "", "POST /debug/tunnels/gr/stop"},
		{"flags first", []string{"stop", "--config-dir", dir, "gr"}, 0, "tunnel gr stopped\n", "", "POST /debug/tunnels/gr/stop"},
		{"not running", []string{"stop", "de-1", "--config-dir", dir}, 0, "tunnel de-1 was not running\n", "", "POST /debug/tunnels/de-1/stop"},
		{"unknown", []string{"stop", "nope", "--config-dir", dir}, 1, "", "wg-stream-proxy: tunnel: no tunnel named \"nope\"\n", "POST /debug/tunnels/nope/stop"},
		{"server error", []string{"stop", "broken", "--config-dir", dir}, 1, "", "wg-stream-proxy: tunnel: POST " + f.URL + "/debug/tunnels/broken/stop: status 503\n", "POST /debug/tunnels/broken/stop"},
		{"invalid name", []string{"stop", "../x", "--config-dir", dir}, 2, "", "wg-stream-proxy: tunnel: invalid tunnel name \"../x\"\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(f.requests())
			c, out, errOut := testCLI(nil)
			if code := c.run(append([]string{"tunnel"}, tt.args...)); code != tt.code {
				t.Errorf("exit code = %d, want %d; stderr: %s", code, tt.code, errOut)
			}
			if out.String() != tt.stdout || errOut.String() != tt.stderr {
				t.Errorf("stdout %q, stderr %q; want %q, %q", out, errOut, tt.stdout, tt.stderr)
			}
			reqs := f.requests()[before:]
			if tt.request == "" && len(reqs) != 0 || tt.request != "" && (len(reqs) != 1 || reqs[0] != tt.request) {
				t.Errorf("requests = %v, want %q", reqs, tt.request)
			}
		})
	}
}

func TestTunnelFailures(t *testing.T) {
	// A port nobody listens on: bind, note the address, close.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().String()
	_ = ln.Close()

	off := writeDir(t, map[string]string{"config.toml": ""})
	tests := []struct {
		name string
		args []string
		env  map[string]string
		code int
		want string
	}{
		{"debug listener off", []string{"list", "--config-dir", off}, nil, 1, "wg-stream-proxy: tunnel: the debug listener is off: set [debug] listen in config.toml\n"},
		{"stop with debug listener off", []string{"stop", "gr", "--config-dir", off}, nil, 1, "debug listener is off"},
		{"connection refused", []string{"list", "--config-dir", debugConfig(t, closed)}, nil, 1, "wg-stream-proxy: tunnel: "},
		{"invalid config", []string{"list"}, map[string]string{"WG_STREAM_PROXY_CONFIG_DIR": "/nonexistent/wsp"}, 1, "wg-stream-proxy: tunnel: "},
		{"no subcommand", nil, nil, 2, "usage: wg-stream-proxy tunnel list | tunnel stop <name>"},
		{"unknown subcommand", []string{"start", "gr"}, nil, 2, "usage: wg-stream-proxy tunnel list | tunnel stop <name>"},
		{"list with argument", []string{"list", "gr", "--config-dir", off}, nil, 2, "unexpected arguments"},
		{"stop without name", []string{"stop", "--config-dir", off}, nil, 2, "usage: wg-stream-proxy tunnel list | tunnel stop <name>"},
		{"stop with two names", []string{"stop", "a", "--config-dir", off, "b"}, nil, 2, "unexpected arguments"},
		{"unknown flag", []string{"list", "--bogus"}, nil, 2, "bogus"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, out, errOut := testCLI(tt.env)
			if code := c.run(append([]string{"tunnel"}, tt.args...)); code != tt.code {
				t.Errorf("exit code = %d, want %d; stderr: %s", code, tt.code, errOut)
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

func TestTunnelTimeout(t *testing.T) {
	hang := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-hang }))
	defer slow.Close()
	defer close(hang) // runs first: lets the handler return so Close does not block
	c, _, errOut := testCLI(nil)
	c.tunnelTimeout = 100 * time.Millisecond
	if code := c.run([]string{"tunnel", "list", "--config-dir", debugConfig(t, slow.Listener.Addr().String())}); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.HasPrefix(errOut.String(), "wg-stream-proxy: tunnel: ") || strings.Count(errOut.String(), "\n") != 1 {
		t.Errorf("stderr = %q, want one line", errOut)
	}
}

func TestServeDebugListener(t *testing.T) {
	dir := writeDir(t, map[string]string{"config.toml": "[debug]\nlisten = \"127.0.0.1:0\"\n"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, _, errOut := testCLI(nil)
	c.ctx = ctx
	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()
	var tunnels, mainPprof string
	var mainAddr net.Addr
	c.listening = func(addr net.Addr) { mainAddr = addr }
	c.debugListening = func(addr net.Addr) {
		go func() {
			defer cancel()
			tunnels = get(t, client, "http://"+addr.String()+"/debug/tunnels")
			mainPprof = get(t, client, "http://"+mainAddr.String()+"/debug/pprof/")
		}()
	}
	if code := c.run([]string{"--config-dir", dir, "--listen", "127.0.0.1:0"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut)
	}
	if tunnels != `{"tunnels":[]}`+"\n" {
		t.Errorf("/debug/tunnels = %q", tunnels)
	}
	if !strings.Contains(mainPprof, "404") {
		t.Errorf("/debug/pprof/ on the main listener = %q, want 404", mainPprof)
	}
	if !strings.Contains(errOut.String(), `msg="serving debug endpoints"`) {
		t.Errorf("logs lack the debug listener:\n%s", errOut)
	}
}

func TestServeDebugListenerOffByDefault(t *testing.T) {
	dir := writeDir(t, map[string]string{"config.toml": ""})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, _, errOut := testCLI(nil)
	c.ctx = ctx
	c.listening = func(net.Addr) { cancel() }
	c.debugListening = func(addr net.Addr) { t.Errorf("debug listener opened on %s", addr) }
	if code := c.run([]string{"--config-dir", dir, "--listen", "127.0.0.1:0"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut)
	}
	if strings.Contains(errOut.String(), "debug endpoints") {
		t.Errorf("logs mention debug endpoints:\n%s", errOut)
	}
}

func TestServeDebugListenError(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	dir := debugConfig(t, busy.Addr().String())
	c, _, errOut := testCLI(nil)
	if code := c.run([]string{"--config-dir", dir, "--listen", "127.0.0.1:0"}); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), `msg="cannot listen" addr=`+busy.Addr().String()) {
		t.Errorf("stderr lacks the debug listen error:\n%s", errOut)
	}
}

// TestLocalURLZone: the zone of a link-local listen address is escaped in
// the URL and parses back to the host the listener is bound to.
func TestLocalURLZone(t *testing.T) {
	got, err := localURL("[fe80::1%eth0]:2350", "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", got, err)
	}
	if u.Hostname() != "fe80::1%eth0" || u.Port() != "2350" || u.Path != "/healthz" {
		t.Errorf("url %q parses to host %q port %q path %q", got, u.Hostname(), u.Port(), u.Path)
	}
}
