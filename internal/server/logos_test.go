// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
)

const (
	secretBody = "TOP-SECRET"
	pngBody    = "\x89PNG fake"
)

// logoEnv is a proxy whose config dir has a logos dir and, next to it,
// a file that must never be served.
type logoEnv struct {
	dir   string // config dir
	proxy *httptest.Server
}

// newLogoEnv writes a config dir with the given logos (name -> content)
// and starts a proxy on it. makeLogosDir false leaves assets/logos out.
func newLogoEnv(t *testing.T, logos map[string]string, makeLogosDir bool) *logoEnv {
	t.Helper()
	files := map[string]string{
		"config.toml": "",
		"secret.txt":  secretBody,
	}
	dir := writeConfig(t, files)
	if makeLogosDir {
		if err := os.MkdirAll(filepath.Join(dir, "assets", "logos"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range logos {
		if err := os.WriteFile(filepath.Join(dir, "assets", "logos", name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &logoEnv{dir: dir, proxy: startLogoProxy(t, dir)}
}

func startLogoProxy(t *testing.T, dir string) *httptest.Server {
	t.Helper()
	cfg := loadConfigDir(t, dir)
	routes, err := route.NewSet(route.Host(), nil, route.Options{Upstream: cfg.Upstream(), AllowPrivate: cfg.AllowPrivate()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(routes.CloseIdleConnections)
	s, err := New(Options{Config: cfg, Routes: routes, Signer: newSigner(t), Logger: newLogger(&syncBuffer{}), Version: "1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv
}

// rawGet sends a request with the exact request target, bypassing any
// path normalisation of the HTTP client.
func rawGet(t *testing.T, srv *httptest.Server, method, target string) (*http.Response, string) {
	t.Helper()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, method+" "+target+" HTTP/1.1\r\nHost: example.test\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: method})
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(b)
}

func TestLogoServed(t *testing.T) {
	e := newLogoEnv(t, map[string]string{
		"skai.png":    pngBody,
		"news 24.png": pngBody,
		"a.jpg":       "jpg",
		"b.JPEG":      "jpeg",
		"c.gif":       "gif",
		"d.webp":      "webp",
		"e.svg":       "<svg/>",
		"f.ico":       "ico",
		"g.avif":      "avif",
		"noext":       "raw",
		"h.unknown1":  "raw",
	}, true)
	tests := []struct {
		target, ctype string
	}{
		{"/logos/skai.png", "image/png"},
		{"/logos/news%2024.png", "image/png"},
		{"/logos/a.jpg", "image/jpeg"},
		{"/logos/b.JPEG", "image/jpeg"},
		{"/logos/c.gif", "image/gif"},
		{"/logos/d.webp", "image/webp"},
		{"/logos/e.svg", "image/svg+xml"},
		{"/logos/f.ico", "image/x-icon"},
		{"/logos/g.avif", "image/avif"},
		{"/logos/noext", "application/octet-stream"},
		{"/logos/h.unknown1", "application/octet-stream"},
	}
	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			resp, body := rawGet(t, e.proxy, http.MethodGet, tt.target)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d, want 200", resp.StatusCode)
			}
			if got := resp.Header.Get("Content-Type"); got != tt.ctype {
				t.Errorf("Content-Type = %q, want %q", got, tt.ctype)
			}
			if got := resp.Header.Get("Cache-Control"); got != "public, max-age=3600" {
				t.Errorf("Cache-Control = %q", got)
			}
			if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q", got)
			}
			if resp.Header.Get("Last-Modified") == "" {
				t.Error("no Last-Modified")
			}
			if body == "" {
				t.Error("empty body")
			}
		})
	}
}

func TestLogoSVGLockedDown(t *testing.T) {
	e := newLogoEnv(t, map[string]string{"e.svg": "<svg/>"}, true)
	resp, _ := rawGet(t, e.proxy, http.MethodGet, "/logos/e.svg")
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "sandbox") {
		t.Errorf("Content-Security-Policy = %q", csp)
	}
}

func TestLogoHead(t *testing.T) {
	e := newLogoEnv(t, map[string]string{"skai.png": pngBody}, true)
	resp, body := rawGet(t, e.proxy, http.MethodHead, "/logos/skai.png")
	if resp.StatusCode != http.StatusOK || body != "" {
		t.Errorf("HEAD: status %d, body %q", resp.StatusCode, body)
	}
	if got, want := resp.Header.Get("Content-Length"), "9"; got != want {
		t.Errorf("Content-Length = %q, want %q", got, want)
	}
	if got := resp.Header.Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q", got)
	}
	resp, _ = rawGet(t, e.proxy, http.MethodHead, "/logos/missing.png")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("HEAD missing: status %d, want 404", resp.StatusCode)
	}
}

func TestLogoConditionalAndMethod(t *testing.T) {
	e := newLogoEnv(t, map[string]string{"skai.png": pngBody}, true)
	resp, _ := rawGet(t, e.proxy, http.MethodGet, "/logos/skai.png")
	lm := resp.Header.Get("Last-Modified")
	req, err := http.NewRequest(http.MethodGet, e.proxy.URL+"/logos/skai.png", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("If-Modified-Since", lm)
	got, err := e.proxy.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = got.Body.Close()
	if got.StatusCode != http.StatusNotModified {
		t.Errorf("If-Modified-Since: status %d, want 304", got.StatusCode)
	}
	resp, _ = rawGet(t, e.proxy, http.MethodPost, "/logos/skai.png")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST: status %d, want 405", resp.StatusCode)
	}
}

func TestLogoNotFound(t *testing.T) {
	e := newLogoEnv(t, map[string]string{"skai.png": pngBody}, true)
	if err := os.Mkdir(filepath.Join(e.dir, "assets", "logos", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, "assets", "logos", ".hidden.png"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/logos/missing.png", "/logos/sub", "/logos/.hidden.png", "/logos/", "/logos"} {
		t.Run(target, func(t *testing.T) {
			resp, body := rawGet(t, e.proxy, http.MethodGet, target)
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("status %d, want 404", resp.StatusCode)
			}
			if strings.Contains(body, "x") && target == "/logos/.hidden.png" {
				t.Errorf("body %q leaks the hidden file", body)
			}
		})
	}
}

func TestLogoMissingDir(t *testing.T) {
	e := newLogoEnv(t, nil, false)
	resp, _ := rawGet(t, e.proxy, http.MethodGet, "/logos/skai.png")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status %d, want 404", resp.StatusCode)
	}
}

// TestLogoTraversal is the L3 security test of spec 5.4: nothing outside
// the logos dir is served, whatever the encoding.
func TestLogoTraversal(t *testing.T) {
	e := newLogoEnv(t, map[string]string{"skai.png": pngBody}, true)
	targets := []string{
		"/logos/../../secret.txt",
		"/logos/..%2f..%2fsecret.txt",
		"/logos/..%2F..%2Fsecret.txt",
		"/logos/%2e%2e/%2e%2e/secret.txt",
		"/logos/%2e%2e%2f%2e%2e%2fsecret.txt",
		"/logos/%2E%2E%2F%2E%2E%2Fsecret.txt",
		"/logos/..%5c..%5csecret.txt",
		"/logos/..\\..\\secret.txt",
		"/logos/%252e%252e%252f%252e%252e%252fsecret.txt",
		"/logos/....//....//secret.txt",
		"/logos/skai.png/../../../secret.txt",
		"/logos/skai.png/../../secret.txt",
		"/logos/%2e%2e",
		"/logos/..",
		"/logos/.",
		"/logos/%00",
		"/logos/skai.png%00.txt",
		"/logos/..%00/secret.txt",
		"/logos//secret.txt",
		"/logos/%2fsecret.txt",
		"/logos/%2f..%2f..%2fsecret.txt",
		"//logos/../../secret.txt",
		"/logos/..;/secret.txt",
		"/logos/%c0%ae%c0%ae/secret.txt",
		"/logos/..%c0%af..%c0%afsecret.txt",
		"/logos/skai.png/",
		"/logos/skai.png/x",
		"/logos/..%2fsecret.txt",
		"/logos/..%5csecret.txt",
	}
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			resp, body := rawGet(t, e.proxy, http.MethodGet, target)
			if resp.StatusCode == http.StatusOK {
				t.Errorf("status 200, want a refusal; body %q", body)
			}
			if strings.Contains(body, secretBody) {
				t.Errorf("response leaks the secret: %q", body)
			}
			if resp.StatusCode >= 500 {
				t.Errorf("status %d, want a client error or redirect", resp.StatusCode)
			}
		})
	}
}

func TestLogoSymlinks(t *testing.T) {
	e := newLogoEnv(t, map[string]string{"skai.png": pngBody}, true)
	logos := filepath.Join(e.dir, "assets", "logos")
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "out.png"), []byte(secretBody), 0o600); err != nil {
		t.Fatal(err)
	}
	links := map[string]string{
		"file-abs.png":  filepath.Join(e.dir, "secret.txt"),
		"file-rel.png":  "../../secret.txt",
		"file-out.png":  filepath.Join(outside, "out.png"),
		"dir-out":       outside,
		"inside.png":    "skai.png",
		"dangling.png":  "nope.png",
		"loop-a.png":    "loop-b.png",
		"loop-b.png":    "loop-a.png",
		"dir-self-link": ".",
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(logos, name)); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		target string
		want   int
	}{
		{"/logos/file-abs.png", http.StatusNotFound},
		{"/logos/file-rel.png", http.StatusNotFound},
		{"/logos/file-out.png", http.StatusNotFound},
		{"/logos/dir-out", http.StatusNotFound},
		{"/logos/dir-out/out.png", http.StatusNotFound},
		{"/logos/dangling.png", http.StatusNotFound},
		{"/logos/loop-a.png", http.StatusNotFound},
		{"/logos/dir-self-link", http.StatusNotFound},
		// A link that stays inside the logos dir is fine.
		{"/logos/inside.png", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			resp, body := rawGet(t, e.proxy, http.MethodGet, tt.target)
			if resp.StatusCode != tt.want {
				t.Errorf("status %d, want %d", resp.StatusCode, tt.want)
			}
			if strings.Contains(body, secretBody) {
				t.Errorf("response leaks the secret: %q", body)
			}
		})
	}
}

// TestLogoLinkedLogosDir: a logos dir that is itself a symlink (e.g. a
// mounted volume) is the configured location and works.
func TestLogoLinkedLogosDir(t *testing.T) {
	dir := writeConfig(t, map[string]string{"config.toml": ""})
	real := t.TempDir()
	if err := os.WriteFile(filepath.Join(real, "skai.png"), []byte(pngBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(dir, "assets", "logos")); err != nil {
		t.Fatal(err)
	}
	srv := startLogoProxy(t, dir)
	resp, body := rawGet(t, srv, http.MethodGet, "/logos/skai.png")
	if resp.StatusCode != http.StatusOK || body != pngBody {
		t.Errorf("status %d, body %q", resp.StatusCode, body)
	}
}

// loadConfigDir loads the config dir dir.
func loadConfigDir(t *testing.T, dir string) *config.Config {
	t.Helper()
	cfg, err := config.Load(config.Options{Paths: config.Paths{ConfigDir: dir, TunnelsDir: filepath.Join(dir, "tunnels")}})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}
