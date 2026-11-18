// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/hls"
	"github.com/KingOfKalk/wg-stream-proxy/internal/metrics"
	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
)

var update = flag.Bool("update", false, "rewrite golden files")

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// testKey is a test-only signing key.
var testKey = []byte("test-only-signing-key-0123456789")

func newSigner(t *testing.T) *hls.Signer {
	t.Helper()
	s, err := hls.NewSigner(testKey)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// writeConfig writes a config dir from relative path -> content and
// returns its path.
func writeConfig(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// loadConfig writes and loads a config dir.
func loadConfig(t *testing.T, files map[string]string) *config.Config {
	t.Helper()
	dir := writeConfig(t, files)
	cfg, err := config.Load(config.Options{Paths: config.Paths{ConfigDir: dir, TunnelsDir: filepath.Join(dir, "tunnels")}})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
	// changed is closed by the next Write; nil until waitFor needs it.
	changed chan struct{}
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.changed != nil {
		close(s.changed)
		s.changed = nil
	}
	return s.b.Write(p)
}

// waitFor blocks until the output contains substr n times. It fails tb
// after 10s.
func (s *syncBuffer) waitFor(tb testing.TB, substr string, n int) {
	tb.Helper()
	giveUp := time.After(10 * time.Second)
	for {
		s.mu.Lock()
		found := strings.Count(s.b.String(), substr) >= n
		if s.changed == nil {
			s.changed = make(chan struct{})
		}
		ch := s.changed
		s.mu.Unlock()
		if found {
			return
		}
		select {
		case <-ch:
		case <-giveUp:
			tb.Fatalf("log lacks %d x %q after 10s:\n%s", n, substr, s.String())
		}
	}
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// file is one fixture response.
type file struct {
	body     string
	ctype    string
	status   int         // 0: http.ServeContent, which answers Range with 206
	location string      // redirect target (302)
	header   http.Header // extra response headers
	endless  bool        // stream until the client goes away
	hang     bool        // send nothing until the client goes away
	stall    bool        // send the body, then nothing until the client goes away
}

// seen is a request the fixture received.
type seen struct {
	Method, Path, Query, Range, UserAgent, Referer, Icy, Remote string
	// Names lists the request header names sent, sorted, comma-separated.
	Names string
}

// upstream is a fixture upstream server handler.
type upstream struct {
	mu    sync.Mutex
	files map[string]file
	reqs  []seen
	// gone is closed by endless responses when their client went away.
	gone chan struct{}
	once sync.Once
}

func newUpstream() *upstream {
	return &upstream{files: map[string]file{}, gone: make(chan struct{})}
}

func (u *upstream) add(path string, f file) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.files[path] = f
}

func (u *upstream) requests() []seen {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]seen(nil), u.reqs...)
}

// requestsFor returns the requests for path.
func (u *upstream) requestsFor(path string) []seen {
	var out []seen
	for _, r := range u.requests() {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (u *upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.reqs = append(u.reqs, seen{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Range: r.Header.Get("Range"),
		UserAgent: r.Header.Get("User-Agent"), Referer: r.Header.Get("Referer"),
		Icy: r.Header.Get("Icy-MetaData"), Remote: r.RemoteAddr,
		Names: headerNames(r.Header),
	})
	f, ok := u.files[r.URL.Path]
	u.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	for k, v := range f.header {
		w.Header()[k] = v
	}
	if f.ctype != "" {
		w.Header().Set("Content-Type", f.ctype)
	}
	switch {
	case f.location != "":
		http.Redirect(w, r, f.location, http.StatusFound)
	case f.hang:
		<-r.Context().Done()
	case f.stall:
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, f.body)
		_ = http.NewResponseController(w).Flush()
		<-r.Context().Done()
	case f.endless:
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		// Writes block while the client does not read and fail once it
		// is gone.
		for {
			if _, err := io.WriteString(w, f.body); err != nil {
				break
			}
			if rc.Flush() != nil {
				break
			}
		}
		u.once.Do(func() { close(u.gone) })
	case f.status != 0:
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.body)
	default:
		http.ServeContent(w, r, "", time.Time{}, strings.NewReader(f.body))
	}
}

// hlsFixture adds an HLS channel to u: /live/master.m3u8 redirects to
// /cdn/v1/master.m3u8, whose variants, renditions, keys and segments use
// relative URIs, an absolute URI with a token under abs (the upstream's
// own scheme://host) and an absolute path. It returns the bodies of all
// non-playlist resources by upstream path.
func hlsFixture(u *upstream, abs string) map[string]string {
	const mpegurl = "application/vnd.apple.mpegurl"
	u.add("/live/master.m3u8", file{location: "/cdn/v1/master.m3u8"})
	u.add("/cdn/v1/master.m3u8", file{ctype: mpegurl, body: `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",LANGUAGE="el",NAME="Ελληνικά",DEFAULT=YES,AUTOSELECT=YES,URI="audio/el/index.m3u8"
#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",LANGUAGE="el",NAME="Ελληνικά",URI="subs/el/index.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,CODECS="avc1.4d401e,mp4a.40.2",AUDIO="aud",SUBTITLES="subs"
low/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2400000,RESOLUTION=1280x720,CODECS="avc1.4d401f,mp4a.40.2",AUDIO="aud",SUBTITLES="subs"
` + abs + `/cdn/v1/high/index.m3u8?token=secret
`})
	// Media playlists without a playlist Content-Type: detected by
	// #EXTM3U at the start of the body.
	u.add("/cdn/v1/low/index.m3u8", file{ctype: "text/plain", body: `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:7
#EXT-X-KEY:METHOD=AES-128,URI="../keys/k7.key",IV=0x00000000000000000000000000000007
#EXT-X-MAP:URI="init.mp4"
#EXTINF:4.000,
seg7.m4s
#EXTINF:4.000,
seg8.m4s
`})
	u.add("/cdn/v1/high/index.m3u8", file{ctype: mpegurl, body: "\ufeff#EXTM3U\r\n" +
		"#EXT-X-TARGETDURATION:4\r\n" +
		"#EXT-X-KEY:METHOD=AES-128,URI=\"/cdn/v1/keys/k7.key\"\r\n" +
		"#EXTINF:4.000,\r\n" +
		"seg7.ts\r\n" +
		"#EXT-X-ENDLIST\r\n"})
	u.add("/cdn/v1/audio/el/index.m3u8", file{ctype: mpegurl, body: "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\na7.aac\n"})
	u.add("/cdn/v1/subs/el/index.m3u8", file{ctype: mpegurl, body: "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\ns7.vtt\n"})
	res := map[string]string{
		"/cdn/v1/keys/k7.key":     "0123456789abcdef",
		"/cdn/v1/low/init.mp4":    "init segment",
		"/cdn/v1/low/seg7.m4s":    "low segment 7",
		"/cdn/v1/low/seg8.m4s":    "low segment 8",
		"/cdn/v1/high/seg7.ts":    "high segment 7",
		"/cdn/v1/audio/el/a7.aac": "audio segment 7",
		"/cdn/v1/subs/el/s7.vtt":  "WEBVTT\n",
	}
	for p, b := range res {
		u.add(p, file{ctype: "application/octet-stream", body: b})
	}
	return res
}

// do sends a request and returns the response with its body read.
func do(t *testing.T, c *http.Client, method, u string, h http.Header) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range h {
		req.Header[k] = v
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
	return resp, string(b)
}

var uriAttr = regexp.MustCompile(`URI="([^"]*)"`)

// playlistURIs returns the URI lines and URI attributes of a playlist.
func playlistURIs(body string) []string {
	var out []string
	for _, line := range strings.FieldsFunc(body, func(r rune) bool { return r == '\n' || r == '\r' }) {
		line = strings.TrimSpace(strings.TrimPrefix(line, "\ufeff"))
		switch {
		case line == "":
		case strings.HasPrefix(line, "#"):
			for _, m := range uriAttr.FindAllStringSubmatch(line, -1) {
				out = append(out, m[1])
			}
		default:
			out = append(out, line)
		}
	}
	return out
}

// upstreamOf decodes the upstream URL of a resource URL.
func upstreamOf(t *testing.T, resource string) *url.URL {
	t.Helper()
	u, err := url.Parse(resource)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(u.Path, "/")
	if len(parts) != 6 || parts[1] != "r" {
		t.Fatalf("not a resource URL: %s", resource)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[4])
	if err != nil {
		t.Fatal(err)
	}
	up, err := url.Parse(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return up
}

// crawl fetches start (a channel URL) and, recursively, every URI of
// every playlist it gets, like a player that plays all variants and
// renditions. Every URI must be a resource URL of channel under base. It
// returns the non-playlist bodies by upstream path and the number of
// playlists.
func crawl(t *testing.T, c *http.Client, base, channel, start string) (map[string]string, int) {
	t.Helper()
	got := map[string]string{}
	playlists := 0
	queue := []string{start}
	visited := map[string]bool{}
	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		if visited[u] {
			continue
		}
		visited[u] = true
		resp, body := do(t, c, http.MethodGet, u, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d: %s", u, resp.StatusCode, body)
		}
		if resp.Header.Get("Content-Type") != "application/vnd.apple.mpegurl" {
			got[upstreamOf(t, u).Path] = body
			continue
		}
		playlists++
		if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("GET %s: Cache-Control %q, want no-cache", u, cc)
		}
		for _, ref := range playlistURIs(body) {
			if !strings.HasPrefix(ref, base+"/r/"+channel+"/") {
				t.Fatalf("playlist %s has a URI that is not a resource URL of %s: %q", u, channel, ref)
			}
			queue = append(queue, ref)
		}
	}
	return got, playlists
}

// recorder wraps a route.Network and counts its use.
type recorder struct {
	n       route.Network
	lookups atomic.Int64
	dials   atomic.Int64

	mu sync.Mutex
	// changed is closed by the next lookup or dial; nil until waitUsed
	// needs it.
	changed chan struct{}
}

func (r *recorder) LookupNetIP(ctx context.Context, host string) ([]netip.Addr, error) {
	r.lookups.Add(1)
	r.notify()
	return r.n.LookupNetIP(ctx, host)
}

func (r *recorder) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	r.dials.Add(1)
	r.notify()
	return r.n.DialContext(ctx, network, address)
}

func (r *recorder) notify() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.changed != nil {
		close(r.changed)
		r.changed = nil
	}
}

// waitUsed blocks until at least n lookups and dials have called into
// the wrapped network. It fails tb after 10s.
func (r *recorder) waitUsed(tb testing.TB, n int64) {
	tb.Helper()
	giveUp := time.After(10 * time.Second)
	for {
		r.mu.Lock()
		if r.changed == nil {
			r.changed = make(chan struct{})
		}
		ch := r.changed
		r.mu.Unlock()
		if r.used() >= n {
			return
		}
		select {
		case <-ch:
		case <-giveUp:
			tb.Fatalf("network used %d times after 10s, want %d", r.used(), n)
		}
	}
}

func (r *recorder) used() int64 { return r.lookups.Load() + r.dials.Load() }

// directEnv is a proxy (New + httptest) with a direct route to one
// fixture upstream.
type directEnv struct {
	proxy   *httptest.Server
	client  *http.Client
	up      *upstream
	upURL   string
	logs    *syncBuffer
	signer  *hls.Signer
	cfg     *config.Config
	metrics *metrics.Metrics
}

// allowLoopback lets the direct route reach the loopback fixtures.
const allowLoopback = "\n[upstream.allow_private]\ndirect = [\"127.0.0.0/8\", \"::1/128\"]\n"

// newDirectEnv starts a fixture upstream and a proxy for the channel file
// channels (channels.d/tv.toml); "{UP}" in it and in configTOML is
// replaced with the upstream base URL.
func newDirectEnv(t *testing.T, configTOML, channels string) *directEnv {
	t.Helper()
	return newDirectEnvFiles(t, configTOML, map[string]string{"channels.d/tv.toml": channels})
}

// newDirectEnvFiles is newDirectEnv with any channel files, by path
// relative to the config dir; "{UP}" is replaced in each.
func newDirectEnvFiles(t *testing.T, configTOML string, channelFiles map[string]string) *directEnv {
	t.Helper()
	e := &directEnv{up: newUpstream(), logs: &syncBuffer{}, signer: newSigner(t), metrics: metrics.New(metrics.Options{})}
	upSrv := httptest.NewServer(e.up)
	t.Cleanup(upSrv.Close)
	e.upURL = upSrv.URL
	files := map[string]string{"config.toml": strings.ReplaceAll(configTOML, "{UP}", upSrv.URL)}
	for name, content := range channelFiles {
		files[name] = strings.ReplaceAll(content, "{UP}", upSrv.URL)
	}
	e.cfg = loadConfig(t, files)
	routes, err := route.NewSet(route.Host(), nil, route.Options{Upstream: e.cfg.Upstream(), AllowPrivate: e.cfg.AllowPrivate()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(routes.CloseIdleConnections)
	s, err := New(Options{Config: e.cfg, Routes: routes, Signer: e.signer, Logger: newLogger(e.logs), Version: "1.2.3", Metrics: e.metrics})
	if err != nil {
		t.Fatal(err)
	}
	e.proxy = httptest.NewServer(s)
	t.Cleanup(e.proxy.Close)
	e.client = e.proxy.Client()
	return e
}

func (e *directEnv) get(t *testing.T, path string, h http.Header) (*http.Response, string) {
	t.Helper()
	return do(t, e.client, http.MethodGet, e.proxy.URL+path, h)
}

// resource returns the resource URL of an upstream URL for channel.
func (e *directEnv) resource(channel, upstream string) string {
	return e.proxy.URL + e.signer.Path(channel, upstream)
}

// headerNames returns the names in h, sorted and comma-separated.
func headerNames(h http.Header) string {
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, k)
	}
	slices.Sort(names)
	return strings.Join(names, ",")
}
