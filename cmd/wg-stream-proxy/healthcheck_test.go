// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHealthcheckURL(t *testing.T) {
	tests := []struct {
		listen, want string
	}{
		{":2349", "http://127.0.0.1:2349/healthz"},
		{"0.0.0.0:80", "http://127.0.0.1:80/healthz"},
		{"[::]:2349", "http://[::1]:2349/healthz"},
		{"127.0.0.1:8080", "http://127.0.0.1:8080/healthz"},
		{"192.0.2.7:8080", "http://192.0.2.7:8080/healthz"},
		{"[2001:db8::1]:8080", "http://[2001:db8::1]:8080/healthz"},
		{"localhost:9", "http://localhost:9/healthz"},
	}
	for _, tt := range tests {
		t.Run(tt.listen, func(t *testing.T) {
			got, err := healthcheckURL(tt.listen)
			if err != nil || got != tt.want {
				t.Errorf("healthcheckURL(%q) = %q, %v; want %q", tt.listen, got, err, tt.want)
			}
		})
	}
	if _, err := healthcheckURL("nope"); err == nil {
		t.Error("healthcheckURL(\"nope\") = nil error, want error")
	}
}

func TestHealthcheck(t *testing.T) {
	dir := writeDir(t, map[string]string{"config.toml": ""})
	var calls atomic.Int64
	var path atomic.Value
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		path.Store(r.Method + " " + r.URL.Path)
		if status == http.StatusFound {
			http.Redirect(w, r, "/healthz", status)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte("body"))
	}))
	defer srv.Close()

	t.Run("ok is silent", func(t *testing.T) {
		status = http.StatusOK
		c, out, errOut := testCLI(map[string]string{"WG_STREAM_PROXY_LISTEN": srv.Listener.Addr().String()})
		if code := c.run([]string{"healthcheck", "--config-dir", dir}); code != 0 {
			t.Fatalf("exit code = %d, want 0; stderr: %s", code, errOut)
		}
		if out.String() != "" || errOut.String() != "" {
			t.Errorf("output on success: stdout %q, stderr %q", out, errOut)
		}
		if got := path.Load(); got != "GET /healthz" {
			t.Errorf("request = %v, want GET /healthz", got)
		}
	})

	t.Run("flag beats env", func(t *testing.T) {
		status = http.StatusOK
		c, _, errOut := testCLI(map[string]string{"WG_STREAM_PROXY_LISTEN": "127.0.0.1:1"})
		if code := c.run([]string{"healthcheck", "--config-dir", dir, "--listen", srv.Listener.Addr().String()}); code != 0 {
			t.Fatalf("exit code = %d, want 0; stderr: %s", code, errOut)
		}
	})

	t.Run("listen from config.toml", func(t *testing.T) {
		status = http.StatusOK
		d := writeDir(t, map[string]string{"config.toml": "listen = \"" + srv.Listener.Addr().String() + "\"\n"})
		c, _, errOut := testCLI(nil)
		if code := c.run([]string{"healthcheck", "--config-dir", d}); code != 0 {
			t.Fatalf("exit code = %d, want 0; stderr: %s", code, errOut)
		}
	})

	for _, st := range []int{http.StatusServiceUnavailable, http.StatusNotFound, http.StatusFound} {
		t.Run(http.StatusText(st), func(t *testing.T) {
			status = st
			c, out, errOut := testCLI(map[string]string{"WG_STREAM_PROXY_LISTEN": srv.Listener.Addr().String()})
			if code := c.run([]string{"healthcheck", "--config-dir", dir}); code != 1 {
				t.Fatalf("exit code = %d, want 1", code)
			}
			assertOneLine(t, out.String(), errOut.String())
		})
	}
}

func TestHealthcheckFailures(t *testing.T) {
	// A port nobody listens on: bind, note the address, close.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().String()
	_ = ln.Close()

	hang := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-hang }))
	defer slow.Close()
	defer close(hang) // runs first: lets the handler return so Close does not block

	dir := writeDir(t, map[string]string{"config.toml": ""})
	badConfig := writeDir(t, map[string]string{"config.toml": "listen = \"nope\"\n"})
	tests := []struct {
		name string
		args []string
		env  map[string]string
	}{
		{"closed port", []string{"--config-dir", dir}, map[string]string{"WG_STREAM_PROXY_LISTEN": closed}},
		{"timeout", []string{"--config-dir", dir}, map[string]string{"WG_STREAM_PROXY_LISTEN": slow.Listener.Addr().String()}},
		{"invalid config", []string{"--config-dir", badConfig}, nil},
		{"missing config dir", nil, map[string]string{"WG_STREAM_PROXY_CONFIG_DIR": "/nonexistent/wsp"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, out, errOut := testCLI(tt.env)
			c.healthTimeout = 100 * time.Millisecond
			if code := c.run(append([]string{"healthcheck"}, tt.args...)); code != 1 {
				t.Fatalf("exit code = %d, want 1", code)
			}
			assertOneLine(t, out.String(), errOut.String())
		})
	}
}

func TestHealthcheckArguments(t *testing.T) {
	c, _, errOut := testCLI(nil)
	if code := c.run([]string{"healthcheck", "extra"}); code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "unexpected arguments") {
		t.Errorf("stderr = %q", errOut)
	}
}

// assertOneLine checks that a failed healthcheck wrote nothing to stdout
// and exactly one line to stderr.
func assertOneLine(t *testing.T, stdout, stderr string) {
	t.Helper()
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.HasPrefix(stderr, "wg-stream-proxy: healthcheck: ") || strings.Count(stderr, "\n") != 1 || !strings.HasSuffix(stderr, "\n") {
		t.Errorf("stderr = %q, want one line starting with the command", stderr)
	}
}
