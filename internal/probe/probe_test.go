// SPDX-License-Identifier: AGPL-3.0-or-later

package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
)

// file is one fixture response.
type file struct {
	ctype  string
	body   string
	status int
	// header holds extra response headers.
	header http.Header
	// size hides the length: the body is sent chunked.
	chunked bool
}

// fixture serves files by path and records the request headers.
type fixture struct {
	srv   *httptest.Server
	mu    sync.Mutex
	files map[string]file
	seen  map[string]http.Header
}

func newFixture(t *testing.T, files map[string]file) *fixture {
	t.Helper()
	f := &fixture{files: files, seen: map[string]http.Header{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		fl, ok := f.files[r.URL.Path]
		f.seen[r.URL.Path] = r.Header.Clone()
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		if fl.ctype != "" {
			w.Header().Set("Content-Type", fl.ctype)
		}
		for k, v := range fl.header {
			w.Header()[k] = v
		}
		if fl.chunked {
			w.(http.Flusher).Flush()
		}
		if fl.status != 0 {
			w.WriteHeader(fl.status)
		}
		_, _ = w.Write([]byte(fl.body))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fixture) url(path string) string { return f.srv.URL + path }

func (f *fixture) header(path string) http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[path]
}

var loopback = netip.MustParsePrefix("127.0.0.0/8")

// directRoute returns the direct route; allow adds the loopback range
// to the private-range guard exceptions.
func directRoute(t *testing.T, allow bool, clk route.Clock) *route.Route {
	t.Helper()
	opts := route.Options{
		Upstream: config.Upstream{UserAgent: "wsp-test", ConnectTimeout: 5 * time.Second, HeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, MaxPlaylistSize: 1 << 20},
		Clock:    clk,
	}
	if allow {
		opts.AllowPrivate = map[string][]netip.Prefix{config.RouteDirect: {loopback}}
	}
	set, err := route.NewSet(route.Host(), nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(set.CloseIdleConnections)
	rt, _ := set.Route(config.RouteDirect)
	return rt
}

func run(t *testing.T, rt *route.Route, url string) *Report {
	t.Helper()
	return Run(t.Context(), url, Options{Route: rt, MaxPlaylistSize: 1 << 20})
}

const (
	hlsType = "application/vnd.apple.mpegurl"
	master  = "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\nlow/index.m3u8?token=secret\n#EXT-X-STREAM-INF:BANDWIDTH=2000\nhigh/index.m3u8\n"
	media   = "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\nseg1.ts?token=secret\n#EXTINF:4,\nseg2.ts\n"
)

// TestRunFormats: what the proxy would do with a response, per format
// and failure.
func TestRunFormats(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]file
		path  string
		allow bool
		// want
		ok       bool
		steps    []string // role of each step
		format   string
		refused  string
		class    string
		status   int
		variants int
		segments int
		periods  int
		// adaptationSets counts the AdaptationSet elements of a DASH
		// manifest.
		adaptationSets int
	}{
		{
			name:  "hls media playlist and its first segment",
			files: map[string]file{"/live/index.m3u8": {ctype: hlsType, body: media}, "/live/seg1.ts": {ctype: "video/mp2t", body: "segment"}},
			path:  "/live/index.m3u8", allow: true, ok: true, steps: []string{"url", "segment"}, format: "hls-media", status: 200, segments: 2,
		},
		{
			name: "hls master playlist: first variant and first segment",
			files: map[string]file{
				"/live/master.m3u8":     {ctype: hlsType, body: master},
				"/live/low/index.m3u8":  {ctype: hlsType, body: media},
				"/live/low/seg1.ts":     {ctype: "video/mp2t", body: "segment"},
				"/live/high/index.m3u8": {ctype: hlsType, body: media},
			},
			path: "/live/master.m3u8", allow: true, ok: true, steps: []string{"url", "variant", "segment"}, format: "hls-master", status: 200, variants: 2,
		},
		{
			name:  "hls detected by body, wrong content type",
			files: map[string]file{"/p": {ctype: "text/plain", body: media}, "/seg1.ts": {body: "x"}},
			path:  "/p", allow: true, ok: true, steps: []string{"url", "segment"}, format: "hls-media", status: 200, segments: 2,
		},
		{
			name:  "raw stream",
			files: map[string]file{"/radio": {ctype: "audio/mpeg", body: "ID3-bytes"}},
			path:  "/radio", allow: true, ok: true, steps: []string{"url"}, format: "raw", status: 200,
		},
		{
			name:  "raw stream with icy headers",
			files: map[string]file{"/radio": {ctype: "audio/mpeg", body: "bytes", header: http.Header{"Icy-Name": {"Radio"}}}},
			path:  "/radio", allow: true, ok: true, steps: []string{"url"}, format: "icy", status: 200,
		},
		{
			name:  "refused pls",
			files: map[string]file{"/r.pls": {ctype: "audio/x-scpls", body: "[playlist]\nFile1=http://x/\n"}},
			path:  "/r.pls", allow: true, steps: []string{"url"}, format: "refused", refused: "pls", class: "unsupported", status: 200,
		},
		{
			name:  "dash manifest",
			files: map[string]file{"/m.mpd": {ctype: "application/dash+xml", body: dashManifest}},
			path:  "/m.mpd", allow: true, ok: true, steps: []string{"url"}, format: "dash", status: 200, periods: 2, adaptationSets: 3,
		},
		{
			name:  "dash manifest detected by body after a long comment",
			files: map[string]file{"/play": {ctype: "application/octet-stream", body: "<!--" + strings.Repeat("x", 1000) + "-->" + strings.TrimPrefix(dashManifest, "<?xml version=\"1.0\"?>\n")}},
			path:  "/play", allow: true, ok: true, steps: []string{"url"}, format: "dash", status: 200, periods: 2, adaptationSets: 3,
		},
		{
			name:  "dash manifest with DRM",
			files: map[string]file{"/m.mpd": {ctype: "application/dash+xml", body: `<MPD><Period><AdaptationSet><ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011"/></AdaptationSet></Period></MPD>`}},
			path:  "/m.mpd", allow: true, steps: []string{"url"}, format: "dash", class: "drm", status: 200,
		},
		{
			name:  "dash manifest the rewriter rejects",
			files: map[string]file{"/m.mpd": {ctype: "application/dash+xml", body: `<MPD><Period xlink:href="https://x.example/p.xml" xmlns:xlink="http://www.w3.org/1999/xlink"/></MPD>`}},
			path:  "/m.mpd", allow: true, steps: []string{"url"}, format: "dash", class: "other", status: 200,
		},
		{
			name:  "refused utf-16 xml",
			files: map[string]file{"/x": {ctype: "text/xml", body: "\xff\xfe<\x00M\x00P\x00D\x00>\x00"}},
			path:  "/x", allow: true, steps: []string{"url"}, format: "refused", refused: "unknown", class: "unsupported", status: 200,
		},
		{
			name:  "refused plain m3u by body",
			files: map[string]file{"/list": {ctype: "text/plain", body: "http://a.example/stream\n"}},
			path:  "/list", allow: true, steps: []string{"url"}, format: "refused", refused: "m3u", class: "unsupported", status: 200,
		},
		{
			name:  "upstream 404",
			files: map[string]file{},
			path:  "/missing", allow: true, steps: []string{"url"}, class: "status", status: 404,
		},
		{
			name:  "upstream 500",
			files: map[string]file{"/x": {status: 500, body: "boom"}},
			path:  "/x", allow: true, steps: []string{"url"}, class: "status", status: 500,
		},
		{
			name:  "private range guard",
			files: map[string]file{"/radio": {ctype: "audio/mpeg", body: "x"}},
			path:  "/radio", allow: false, steps: []string{"url"}, class: "guard",
		},
		{
			name:  "playlist the rewriter rejects",
			files: map[string]file{"/bad.m3u8": {ctype: hlsType, body: "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=http://k.example/key\nseg.ts\n"}},
			path:  "/bad.m3u8", allow: true, steps: []string{"url"}, class: "other", status: 200,
		},
		{
			name: "variant playlist missing",
			files: map[string]file{
				"/m.m3u8": {ctype: hlsType, body: master},
			},
			path: "/m.m3u8", allow: true, steps: []string{"url", "variant"}, format: "hls-master", class: "status", variants: 2, status: 200,
		},
		{
			name: "segment missing",
			files: map[string]file{
				"/i.m3u8": {ctype: hlsType, body: media},
			},
			path: "/i.m3u8", allow: true, steps: []string{"url", "segment"}, format: "hls-media", class: "status", segments: 2, status: 200,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, tt.files)
			rep := run(t, directRoute(t, tt.allow, nil), f.url(tt.path))

			if rep.OK != tt.ok {
				t.Errorf("OK = %v, want %v (report %+v)", rep.OK, tt.ok, rep)
			}
			if got := roles(rep); strings.Join(got, ",") != strings.Join(tt.steps, ",") {
				t.Fatalf("steps = %v, want %v", got, tt.steps)
			}
			s := rep.Steps[0]
			if tt.format != "" && s.Format != tt.format {
				t.Errorf("format = %q, want %q", s.Format, tt.format)
			}
			if s.Refused != tt.refused {
				t.Errorf("refused = %q, want %q", s.Refused, tt.refused)
			}
			if s.Status != tt.status {
				t.Errorf("status = %d, want %d", s.Status, tt.status)
			}
			if s.Variants != tt.variants || s.Segments != tt.segments {
				t.Errorf("variants/segments = %d/%d, want %d/%d", s.Variants, s.Segments, tt.variants, tt.segments)
			}
			if s.Periods != tt.periods || s.AdaptationSets != tt.adaptationSets {
				t.Errorf("periods/adaptation sets = %d/%d, want %d/%d", s.Periods, s.AdaptationSets, tt.periods, tt.adaptationSets)
			}
			last := rep.Steps[len(rep.Steps)-1]
			if tt.class == "" {
				if !last.OK || last.Class != "" || last.Error != "" {
					t.Errorf("last step = %+v, want ok", last)
				}
				return
			}
			failed := failedStep(rep)
			if failed == nil || failed.Class != tt.class || failed.Error == "" {
				t.Errorf("failed step = %+v, want class %q with an error", failed, tt.class)
			}
		})
	}
}

func roles(r *Report) []string {
	var out []string
	for _, s := range r.Steps {
		out = append(out, s.Role)
	}
	return out
}

func failedStep(r *Report) *Step {
	for i := range r.Steps {
		if !r.Steps[i].OK {
			return &r.Steps[i]
		}
	}
	return nil
}

// TestRunSizeAndTiming: size of a playlist and of a raw response, and the
// timing fields of every step.
func TestRunSizeAndTiming(t *testing.T) {
	f := newFixture(t, map[string]file{
		"/i.m3u8":  {ctype: hlsType, body: media},
		"/seg1.ts": {ctype: "video/mp2t", body: "0123456789"},
		"/radio":   {ctype: "audio/mpeg", body: "bytes"},
		"/live":    {ctype: "audio/mpeg", body: "bytes", chunked: true},
	})
	rt := directRoute(t, true, nil)

	pl := run(t, rt, f.url("/i.m3u8"))
	if s := pl.Steps[0]; s.Size == nil || *s.Size != int64(len(media)) || s.Streaming {
		t.Errorf("playlist size = %v streaming %v, want %d", s.Size, s.Streaming, len(media))
	}
	if s := pl.Steps[1]; s.Size == nil || *s.Size != 10 {
		t.Errorf("segment size = %v, want 10", s.Size)
	}
	if s := run(t, rt, f.url("/radio")).Steps[0]; s.Size == nil || *s.Size != 5 || s.Streaming {
		t.Errorf("raw size = %v streaming %v, want 5", s.Size, s.Streaming)
	}
	if s := run(t, rt, f.url("/live")).Steps[0]; s.Size != nil || !s.Streaming {
		t.Errorf("chunked raw: size = %v streaming %v, want streaming", s.Size, s.Streaming)
	}

	for i, s := range pl.Steps {
		tm := s.Timing
		// A pooled connection (best effort, it returns to the pool
		// asynchronously) has no connect phase.
		if tm.TTFBMS == nil || tm.TotalMS <= 0 || tm.ConnectMS == nil != tm.ConnReused || i == 0 && tm.ConnectMS == nil {
			t.Errorf("%s timing = %+v, want ttfb, total and a connect unless reused", s.Role, tm)
		}
		if tm.DNSMS != nil || tm.TLSMS != nil || tm.TunnelStartMS != nil {
			t.Errorf("%s timing = %+v: no lookup (IP literal), no TLS, no tunnel start expected", s.Role, tm)
		}
	}
}

// TestRunHeadersAndRedirects: configured headers are sent on every
// request, and the final URL after a redirect is reported in the logged
// form: no query, no path but the file name.
func TestRunHeadersAndRedirects(t *testing.T) {
	f := newFixture(t, map[string]file{
		"/start":                {status: http.StatusFound, header: http.Header{"Location": {"/deep/path/final.m3u8?token=secret"}}},
		"/deep/path/final.m3u8": {ctype: hlsType, body: media},
		"/deep/path/seg1.ts":    {body: "x"},
	})
	rep := Run(t.Context(), f.url("/start?auth=secret"), Options{
		Route:  directRoute(t, true, nil),
		Header: http.Header{"Referer": {"https://ref.example/"}},
	})

	if !rep.OK {
		t.Fatalf("report = %+v, want ok", rep)
	}
	if got := f.header("/deep/path/seg1.ts").Get("Referer"); got != "https://ref.example/" {
		t.Errorf("Referer of the segment request = %q, want the configured one", got)
	}
	if got := f.header("/start").Get("User-Agent"); got != "wsp-test" {
		t.Errorf("User-Agent = %q, want the [upstream] one", got)
	}
	s := rep.Steps[0]
	if want := "http://" + f.srv.Listener.Addr().String() + "/…/final.m3u8"; s.URL != want {
		t.Errorf("URL = %q, want %q", s.URL, want)
	}
	if s.Redirects != 1 {
		t.Errorf("redirects = %d, want 1", s.Redirects)
	}
	var buf bytes.Buffer
	if err := rep.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	if err := rep.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "secret") || strings.Contains(buf.String(), "deep") {
		t.Errorf("output leaks the query or the path:\n%s", buf.String())
	}
}

// TestRunMalformedRedirectTarget: a redirect to a scheme-relative
// Location that does not parse as an http URL fails the step without its
// path or query in the report (spec 7.2, 8, #157); the same for such a
// variant URI.
func TestRunMalformedRedirectTarget(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]file
		start string
		class string
	}{
		{
			"redirect",
			map[string]file{"/start": {status: http.StatusFound, header: http.Header{"Location": {"//::/secretdir/file.m3u8?token=secret"}}}},
			"/start", "other",
		},
		{
			"redirect with a URL in the authority",
			map[string]file{"/start": {status: http.StatusFound, header: http.Header{"Location": {"//:http://x/secretdir/y?token=secret"}}}},
			"/start", "other",
		},
		{
			"variant",
			map[string]file{"/m.m3u8": {ctype: hlsType, body: "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\n//::/secretdir/v.m3u8?token=secret\n"}},
			"/m.m3u8", "other",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, tt.files)
			rep := run(t, directRoute(t, true, nil), f.url(tt.start))
			if rep.OK {
				t.Fatalf("report = %+v, want a failed step", rep)
			}
			if s := failedStep(rep); s == nil || s.Class != tt.class || s.Error == "" {
				t.Errorf("failed step = %+v, want class %s with an error", s, tt.class)
			}
			var buf bytes.Buffer
			if err := rep.WriteJSON(&buf); err != nil {
				t.Fatal(err)
			}
			if err := rep.WriteText(&buf); err != nil {
				t.Fatal(err)
			}
			for _, s := range []string{"secretdir", "secret", "token"} {
				if strings.Contains(buf.String(), s) {
					t.Errorf("report contains %q:\n%s", s, buf.String())
				}
			}
		})
	}
}

// TestRunPlaylistTooLarge: the size limit of the proxy applies.
func TestRunPlaylistTooLarge(t *testing.T) {
	f := newFixture(t, map[string]file{"/i.m3u8": {ctype: hlsType, body: media}})
	rep := Run(t.Context(), f.url("/i.m3u8"), Options{Route: directRoute(t, true, nil), MaxPlaylistSize: 10})

	if rep.OK {
		t.Fatal("OK, want the size limit to fail")
	}
	if s := failedStep(rep); s == nil || s.Class != "size" {
		t.Errorf("failed step = %+v, want class size", s)
	}
}

// TestRunInvalidURL: a URL the route cannot fetch is a failed probe, not
// a panic.
func TestRunInvalidURL(t *testing.T) {
	for _, u := range []string{"ftp://example.com/x", "not a url", ""} {
		t.Run(u, func(t *testing.T) {
			rep := Run(t.Context(), u, Options{Route: directRoute(t, true, nil)})
			if rep.OK || failedStep(rep) == nil {
				t.Errorf("report = %+v, want a failed step", rep)
			}
		})
	}
}

// TestRunICY: a Shoutcast v1 status line is accepted through the ICY
// transport, like the proxy does for a raw or auto channel.
func TestRunICY(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		buf := make([]byte, 1024)
		_, _ = c.Read(buf)
		_, _ = c.Write([]byte("ICY 200 OK\r\nicy-name: Radio\r\ncontent-type: audio/mpeg\r\n\r\nstreamdata"))
	})
	t.Cleanup(func() { _ = ln.Close(); wg.Wait() })

	rep := run(t, directRoute(t, true, nil), "http://"+ln.Addr().String()+"/stream")

	if !rep.OK || rep.Steps[0].Format != "icy" || rep.Steps[0].Status != 200 {
		t.Errorf("report = %+v, want ok, format icy, status 200", rep.Steps[0])
	}
}

// manualClock is a route.Clock whose timers fire only in advance.
type manualClock struct {
	mu     sync.Mutex
	now    time.Duration
	timers []*manualTimer
	added  chan struct{}
}

type manualTimer struct {
	c      *manualClock
	when   time.Duration
	f      func()
	active bool
}

func newManualClock() *manualClock { return &manualClock{added: make(chan struct{}, 16)} }

func (c *manualClock) AfterFunc(d time.Duration, f func()) route.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &manualTimer{c: c, when: c.now + d, f: f, active: true}
	c.timers = append(c.timers, t)
	c.added <- struct{}{}
	return t
}

func (t *manualTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.active
	t.active = false
	return was
}

func (t *manualTimer) Reset(d time.Duration) bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.active
	t.active = true
	t.when = t.c.now + d
	return was
}

func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now += d
	var due []func()
	for _, t := range c.timers {
		if t.active && t.when <= c.now {
			t.active = false
			due = append(due, t.f)
		}
	}
	c.mu.Unlock()
	for _, f := range due {
		f()
	}
}

// TestRunHeaderTimeout: [upstream] header_timeout applies; the class is
// timeout.
func TestRunHeaderTimeout(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	clk := newManualClock()
	rt := directRoute(t, true, clk)

	done := make(chan *Report, 1)
	go func() { done <- Run(context.Background(), srv.URL+"/slow", Options{Route: rt}) }()
	<-reached
	clk.advance(time.Hour)
	rep := <-done

	if s := failedStep(rep); rep.OK || s == nil || s.Class != "timeout" {
		t.Errorf("report = %+v, want a timeout", rep)
	}
}

// TestJSON: the JSON form has the documented fields.
func TestJSON(t *testing.T) {
	f := newFixture(t, map[string]file{"/i.m3u8": {ctype: hlsType, body: media}, "/seg1.ts": {body: "x"}})
	rep := run(t, directRoute(t, true, nil), f.url("/i.m3u8"))

	var buf bytes.Buffer
	if err := rep.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Route string `json:"route"`
		OK    bool   `json:"ok"`
		Steps []struct {
			Role        string `json:"role"`
			URL         string `json:"url"`
			Status      int    `json:"status"`
			ContentType string `json:"content_type"`
			Format      string `json:"format"`
			Segments    int    `json:"segments"`
			OK          bool   `json:"ok"`
			Timing      struct {
				TotalMS float64 `json:"total_ms"`
			} `json:"timing"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	if got.Route != "direct" || !got.OK || len(got.Steps) != 2 {
		t.Fatalf("report = %+v", got)
	}
	s := got.Steps[0]
	if s.Role != "url" || s.Status != 200 || s.ContentType != hlsType || s.Format != "hls-media" || s.Segments != 2 || !s.OK || s.Timing.TotalMS <= 0 {
		t.Errorf("step 0 = %+v", s)
	}
}

// TestTextOutput: the human form names the format, the status and the
// outcome.
func TestTextOutput(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]file
		path  string
		allow bool
		want  []string
	}{
		{
			"master",
			map[string]file{"/m.m3u8": {ctype: hlsType, body: master}, "/low/index.m3u8": {ctype: hlsType, body: media}, "/low/seg1.ts": {body: "x"}},
			"/m.m3u8", true,
			[]string{"route direct", "HLS master playlist, 2 variants", "HLS media playlist, 2 segments", "first segment", "status", "200", "first byte", "result: ok"},
		},
		{
			"raw",
			map[string]file{"/radio": {ctype: "audio/mpeg", body: "bytes"}},
			"/radio", true,
			[]string{"raw stream", "5 bytes", "audio/mpeg", "result: ok"},
		},
		{
			"refused",
			map[string]file{"/r.pls": {ctype: "audio/x-scpls", body: "[playlist]\n"}},
			"/r.pls", true,
			[]string{"refused", "pls", "result: failed"},
		},
		{
			"guard",
			map[string]file{"/radio": {body: "x"}},
			"/radio", false,
			[]string{"guard", "result: failed"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, tt.files)
			rep := run(t, directRoute(t, tt.allow, nil), f.url(tt.path))

			var buf bytes.Buffer
			if err := rep.WriteText(&buf); err != nil {
				t.Fatal(err)
			}
			for _, w := range tt.want {
				if !strings.Contains(buf.String(), w) {
					t.Errorf("output lacks %q:\n%s", w, buf.String())
				}
			}
		})
	}
}

// TestSanitize: foreign text never carries control characters into the
// output.
func TestSanitize(t *testing.T) {
	f := newFixture(t, map[string]file{"/r": {ctype: "audio/mpeg;\x1b[31m x=\x07", body: "x"}})
	rep := run(t, directRoute(t, true, nil), f.url("/r"))

	var buf bytes.Buffer
	if err := rep.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(buf.String(), "\x1b\x07") {
		t.Errorf("output contains control characters: %q", buf.String())
	}
}

// dashManifest is a live DASH manifest with two periods and three
// adaptation sets.
const dashManifest = `<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic">
  <Period id="1"><BaseURL>dash/</BaseURL>
    <AdaptationSet mimeType="video/mp4"><SegmentTemplate media="v-$Number$.m4s"/><Representation id="v" bandwidth="1"/></AdaptationSet>
    <AdaptationSet mimeType="audio/mp4"><SegmentTemplate media="a-$Number$.m4s"/><Representation id="a" bandwidth="1"/></AdaptationSet>
  </Period>
  <Period id="2"><AdaptationSet mimeType="video/mp4"><SegmentTemplate media="v-$Number$.m4s"/><Representation id="v" bandwidth="1"/></AdaptationSet></Period>
</MPD>
`
