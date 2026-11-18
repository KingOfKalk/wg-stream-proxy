// SPDX-License-Identifier: AGPL-3.0-or-later

package capacitytest

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// clock is a manual clock for the harness tests.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestLive(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	l := newLive(time.Second, 3, 64, c.now)
	srv := httptest.NewServer(l)
	t.Cleanup(srv.Close)

	if got := l.newest(); got != -1 {
		t.Fatalf("newest() before the first segment = %d, want -1", got)
	}
	for range 5 {
		l.advance()
		c.t = c.t.Add(time.Second)
	}
	if got := l.newest(); got != 4 {
		t.Fatalf("newest() = %d, want 4", got)
	}
	if at, ok := l.publishedAt(2); !ok || !at.Equal(time.Unix(1002, 0)) {
		t.Errorf("publishedAt(2) = %v, %v; want %v", at, ok, time.Unix(1002, 0))
	}
	if _, ok := l.publishedAt(5); ok {
		t.Error("publishedAt(5) reports an unpublished segment")
	}

	tests := []struct {
		path   string
		status int
		ctype  string
		body   string
	}{
		{"/one/master.m3u8", http.StatusOK, playlistType, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-STREAM-INF:BANDWIDTH=2000000\nindex.m3u8\n"},
		{"/one/index.m3u8", http.StatusOK, playlistType, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:2\n" +
			"#EXTINF:1.000,\nseg2.ts\n#EXTINF:1.000,\nseg3.ts\n#EXTINF:1.000,\nseg4.ts\n"},
		{"/two/seg4.ts", http.StatusOK, "video/mp2t", string(segment("two", 4, 64))},
		// Segments that left the window stay available, like on a CDN.
		{"/two/seg0.ts", http.StatusOK, "video/mp2t", string(segment("two", 0, 64))},
		{"/two/seg5.ts", http.StatusNotFound, "", ""},
		{"/two/segx.ts", http.StatusNotFound, "", ""},
		{"/two/other.txt", http.StatusNotFound, "", ""},
		{"/master.m3u8", http.StatusNotFound, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			resp, err := srv.Client().Get(srv.URL + tt.path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tt.status {
				t.Fatalf("status %d, want %d", resp.StatusCode, tt.status)
			}
			if tt.status != http.StatusOK {
				return
			}
			if ct := resp.Header.Get("Content-Type"); ct != tt.ctype {
				t.Errorf("Content-Type %q, want %q", ct, tt.ctype)
			}
			if string(b) != tt.body {
				t.Errorf("body:\n%q\nwant:\n%q", b, tt.body)
			}
		})
	}
}

func TestSegment(t *testing.T) {
	a := segment("one", 7, 100)
	if len(a) != 100 {
		t.Fatalf("len = %d, want 100", len(a))
	}
	if !bytes.Equal(a, segment("one", 7, 100)) {
		t.Error("segment is not deterministic")
	}
	for _, other := range [][]byte{segment("one", 8, 100), segment("two", 7, 100)} {
		if bytes.Equal(a, other) {
			t.Error("different segments have the same content")
		}
	}
}

// corrupt flips one byte of every segment of the wrapped handler.
type corrupt struct{ h http.Handler }

func (c corrupt) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, r)
	b := rec.Body.Bytes()
	if strings.HasSuffix(r.URL.Path, ".ts") && len(b) > 0 {
		b[len(b)-1] ^= 0xff
	}
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(b)
}

func TestViewer(t *testing.T) {
	const window = 3
	tests := []struct {
		name string
		// wrap wraps the fixture handler.
		wrap func(http.Handler) http.Handler
		// lag is how far the viewer's clock is ahead of the fixture's.
		lag time.Duration
		// play advances the fixture and steps the viewer after start.
		play         func(ctx context.Context, l *live, v *viewer)
		wantSegments int
		wantFirst    int
		wantLast     int
		wantErr      string
	}{
		{
			name: "every segment in order",
			play: func(ctx context.Context, l *live, v *viewer) {
				v.step(ctx)
				for range 4 {
					l.advance()
					v.step(ctx)
					// A poll without a new segment changes nothing.
					v.step(ctx)
				}
			},
			// Starts at the live edge (seg 2), then 3-6.
			wantSegments: 5, wantFirst: 2, wantLast: 6,
		},
		{
			name: "two new segments in one poll",
			play: func(ctx context.Context, l *live, v *viewer) {
				v.step(ctx)
				l.advance()
				l.advance()
				v.step(ctx)
			},
			wantSegments: 3, wantFirst: 2, wantLast: 4,
		},
		{
			name: "segments left the window",
			play: func(ctx context.Context, l *live, v *viewer) {
				v.step(ctx)
				for range window + 2 {
					l.advance()
				}
				v.step(ctx)
			},
			wantSegments: 4, wantFirst: 2, wantLast: 7,
			wantErr: "segments 3-4 left the window",
		},
		{
			name:         "corrupt segment",
			wrap:         func(h http.Handler) http.Handler { return corrupt{h} },
			play:         func(ctx context.Context, _ *live, v *viewer) { v.step(ctx) },
			wantSegments: 1, wantFirst: 2, wantLast: 2,
			wantErr: "segment 2: wrong content",
		},
		{
			name:         "stall",
			lag:          10 * time.Second,
			play:         func(ctx context.Context, _ *live, v *viewer) { v.step(ctx) },
			wantSegments: 1, wantFirst: 2, wantLast: 2,
			wantErr: "segment 2: stall: arrived 10s after it was published",
		},
		{
			name: "upstream gone",
			play: func(ctx context.Context, l *live, v *viewer) {
				v.media = strings.Replace(v.media, "index.m3u8", "gone.m3u8", 1)
				v.step(ctx)
			},
			wantFirst: -1, wantLast: -1,
			wantErr: "status 404",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &clock{t: time.Unix(1000, 0)}
			l := newLive(time.Second, window, 32, c.now)
			for range window {
				l.advance()
			}
			var h http.Handler = l
			if tt.wrap != nil {
				h = tt.wrap(h)
			}
			srv := httptest.NewServer(h)
			t.Cleanup(srv.Close)
			v := newViewer("two", srv.URL+"/two/master.m3u8", srv.Client(), l, 3*time.Second)
			v.now = func() time.Time { return c.t.Add(tt.lag) }
			ctx := t.Context()
			if err := v.start(ctx); err != nil {
				t.Fatalf("start: %v", err)
			}
			if want := srv.URL + "/two/index.m3u8"; v.media != want {
				t.Fatalf("media playlist %q, want %q", v.media, want)
			}
			tt.play(ctx, l, v)
			if v.segments != tt.wantSegments || v.first != tt.wantFirst || v.last != tt.wantLast {
				t.Errorf("segments %d (%d-%d), want %d (%d-%d)", v.segments, v.first, v.last,
					tt.wantSegments, tt.wantFirst, tt.wantLast)
			}
			errs := v.errors()
			switch {
			case tt.wantErr == "" && len(errs) > 0:
				t.Errorf("errors: %v", errs)
			case tt.wantErr != "" && (len(errs) == 0 || !strings.Contains(errs[0].Error(), tt.wantErr)):
				t.Errorf("errors: %v, want one with %q", errs, tt.wantErr)
			}
		})
	}
}

func TestViewerIgnoresErrorsAfterCancel(t *testing.T) {
	l := newLive(time.Second, 3, 32, time.Now)
	l.advance()
	srv := httptest.NewServer(l)
	t.Cleanup(srv.Close)
	v := newViewer("one", srv.URL+"/one/master.m3u8", srv.Client(), l, time.Second)
	if err := v.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	v.step(ctx)
	if errs := v.errors(); len(errs) != 0 {
		t.Errorf("errors after cancel: %v", errs)
	}
}

func TestViewerStartErrors(t *testing.T) {
	l := newLive(time.Second, 3, 32, time.Now)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/empty.m3u8" {
			_, _ = io.WriteString(w, "#EXTM3U\n")
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	for _, tt := range []struct{ path, want string }{
		{"/missing.m3u8", "status 404"},
		{"/empty.m3u8", "no variant"},
	} {
		t.Run(tt.path, func(t *testing.T) {
			v := newViewer("one", srv.URL+tt.path, srv.Client(), l, time.Second)
			if err := v.start(t.Context()); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("start() = %v, want error with %q", err, tt.want)
			}
		})
	}
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		env     string
		want    time.Duration
		wantRun bool
		wantErr bool
	}{
		{"", 0, false, false},
		{"0", 0, false, false},
		{"false", 0, false, false},
		{"1", defaultDuration, true, false},
		{"true", defaultDuration, true, false},
		{"30s", 30 * time.Second, true, false},
		{"5m", 5 * time.Minute, true, false},
		{"5s", 0, false, true},
		{"soon", 0, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.env, func(t *testing.T) {
			d, run, err := parseDuration(tt.env)
			if d != tt.want || run != tt.wantRun || (err != nil) != tt.wantErr {
				t.Errorf("parseDuration(%q) = %v, %v, %v; want %v, %v, error %v", tt.env, d, run, err, tt.want, tt.wantRun, tt.wantErr)
			}
		})
	}
}

func TestCheckGrowth(t *testing.T) {
	const mib = 1 << 20
	s := func(at time.Duration, g int, heap uint64) sample {
		return sample{at: at, goroutines: g, heap: heap * mib}
	}
	tests := []struct {
		name    string
		samples []sample
		want    []string
	}{
		{"steady", []sample{s(5*time.Second, 300, 90), s(10*time.Second, 100, 10), s(15*time.Second, 110, 20), s(20*time.Second, 100, 12)}, nil},
		{"goroutines grow", []sample{s(10*time.Second, 100, 10), s(15*time.Second, 120, 10), s(20*time.Second, 121, 10)}, []string{"goroutines grew from 100 to 121 at 20s"}},
		{"heap grows", []sample{s(10*time.Second, 100, 10), s(20*time.Second, 100, 43)}, []string{"heap in use grew from 10.0 MiB to 43.0 MiB at 20s"}},
		{"no sample after warm-up", []sample{s(5*time.Second, 100, 10)}, []string{"no sample after the warm-up"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkGrowth(tt.samples, 10*time.Second)
			if strings.Join(got, "\n") != strings.Join(tt.want, "\n") {
				t.Errorf("checkGrowth() = %q, want %q", got, tt.want)
			}
		})
	}
}
