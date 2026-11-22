// SPDX-License-Identifier: AGPL-3.0-or-later

package epg

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
)

// writeFiles writes a config dir: relative path -> content.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// manualClock is a Clock that moves only when the test advances it. It
// reports every armed timer on armed.
type manualClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*manualTimer
	armed  chan time.Duration
}

type manualTimer struct {
	c       *manualClock
	at      time.Time
	f       func()
	stopped bool
}

func newManualClock() *manualClock {
	return &manualClock{now: filterNow, armed: make(chan time.Duration, 100)}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) AfterFunc(d time.Duration, f func()) route.Timer {
	c.mu.Lock()
	t := &manualTimer{c: c, at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	c.mu.Unlock()
	c.armed <- d
	return t
}

func (t *manualTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := !t.stopped
	t.stopped = true
	return was
}

func (t *manualTimer) Reset(time.Duration) bool { panic("not used") }

// advance moves the clock and fires the timers that are due.
func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []func()
	keep := c.timers[:0]
	for _, t := range c.timers {
		switch {
		case t.stopped:
		case !t.at.After(c.now):
			t.stopped = true
			due = append(due, t.f)
		default:
			keep = append(keep, t)
		}
	}
	c.timers = keep
	c.mu.Unlock()
	for _, f := range due {
		go f()
	}
}

// waitIdle waits until Run waits for its next refresh and returns the
// time until it; the fetch timeouts are skipped.
func (c *manualClock) waitIdle(t *testing.T) time.Duration {
	t.Helper()
	for {
		select {
		case d := <-c.armed:
			if d != FetchTimeout {
				return d
			}
		case <-time.After(10 * time.Second):
			t.Fatal("guide does not become idle")
		}
	}
}

// fakeSources answers the fetches of a guide test per source name.
type fakeSources struct {
	mu      sync.Mutex
	answers map[string]func(ctx context.Context) (*http.Response, error)
	fetched []string
}

func (f *fakeSources) set(name string, a func(ctx context.Context) (*http.Response, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[name] = a
}

func (f *fakeSources) fetch(ctx context.Context, src config.EPGSource) (*http.Response, error) {
	f.mu.Lock()
	a := f.answers[src.Name]
	f.fetched = append(f.fetched, src.Name)
	f.mu.Unlock()
	if a == nil {
		return nil, errors.New("no answer")
	}
	return a(ctx)
}

// takeFetched returns and clears the names fetched so far.
func (f *fakeSources) takeFetched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.fetched
	f.fetched = nil
	return out
}

func body(status int, b []byte) func(context.Context) (*http.Response, error) {
	return func(context.Context) (*http.Response, error) {
		return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(b))}, nil
	}
}

// guideEnv is a running Guide with a manual clock and fake sources.
type guideEnv struct {
	g     *Guide
	clock *manualClock
	src   *fakeSources
	logs  *syncBuf
	dir   string
}

const guideChannels = `tunnel = "direct"
[[channel]]
name = "A"
url = "https://live.example.com/a.m3u8"
playlist.tvg_id = "a"
[[channel]]
name = "B"
url = "https://live.example.com/b.m3u8"
playlist.tvg_id = "b"
`

func twoSources(refresh string) string {
	return "[epg]\nrefresh = \"" + refresh + "\"\n" +
		"[[epg.source]]\nname = \"gr\"\nurl = \"https://epg.example.org/gr.xml.gz\"\n" +
		"[[epg.source]]\nname = \"de\"\nurl = \"https://epg.example.net/de.xml\"\n"
}

func (e *guideEnv) load(t *testing.T, configTOML, channels string) *config.Config {
	t.Helper()
	writeFiles(t, e.dir, map[string]string{"config.toml": configTOML, "channels.d/tv.toml": channels})
	cfg, err := config.Load(config.Options{Paths: config.Paths{ConfigDir: e.dir, TunnelsDir: filepath.Join(e.dir, "tunnels")}})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// startGuide runs a guide for the config until the test ends; it
// returns once the first round of fetches is done.
func startGuide(t *testing.T, configTOML, channels string, answers map[string]func(context.Context) (*http.Response, error)) *guideEnv {
	t.Helper()
	e := startGuideNoWait(t, configTOML, channels, answers)
	e.clock.waitIdle(t)
	return e
}

// startGuideNoWait runs a guide like startGuide but returns at once.
func startGuideNoWait(t *testing.T, configTOML, channels string, answers map[string]func(context.Context) (*http.Response, error)) *guideEnv {
	t.Helper()
	e := &guideEnv{clock: newManualClock(), src: &fakeSources{answers: answers}, logs: &syncBuf{}, dir: t.TempDir()}
	log := slog.New(slog.NewTextHandler(e.logs, nil))
	e.g = NewGuide(GuideOptions{Fetch: e.src.fetch, Clock: e.clock, Logger: log})
	e.g.Update(e.load(t, configTOML, channels))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.g.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return e
}

func gzipped(t *testing.T, s string) []byte { return gz(t, []byte(s)) }

func TestGuideFirstFetch(t *testing.T) {
	e := startGuide(t, twoSources("6h"), guideChannels, map[string]func(context.Context) (*http.Response, error){
		"gr": body(200, gzipped(t, source("gr", "a"))),
		"de": body(200, []byte(source("de", "a", "b"))),
	})
	if got := e.src.takeFetched(); strings.Join(got, ",") != "gr,de" {
		t.Errorf("fetched %v, want gr then de", got)
	}
	out := e.g.Output()
	if out == nil {
		t.Fatal("no output")
	}
	if !strings.Contains(string(out.Plain), "<title>gr a</title>") || !strings.Contains(string(out.Plain), "<title>de b</title>") ||
		strings.Contains(string(out.Plain), "<title>de a</title>") || out.Channels != 2 || out.Programmes != 2 {
		t.Errorf("guide:\n%s", out.Plain)
	}
	zr, err := gzip.NewReader(bytes.NewReader(out.Gzip))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil || !bytes.Equal(plain, out.Plain) {
		t.Errorf("gzip does not hold the plain guide: %v", err)
	}
	if out.ETag == "" || out.GzipETag == out.ETag || !out.Modified.Equal(filterNow) {
		t.Errorf("etags %q %q modified %v", out.ETag, out.GzipETag, out.Modified)
	}
	st := e.g.Status()
	if len(st) != 2 || st[0].Name != "gr" || st[0].Fetches[ResultOK] != 1 || !st[0].Updated.Equal(filterNow) {
		t.Errorf("status %+v", st)
	}
	if !strings.Contains(e.logs.String(), "epg source fetched") || !strings.Contains(e.logs.String(), "url=https://epg.example.org/gr.xml.gz") {
		t.Errorf("logs:\n%s", e.logs)
	}
}

// A slow source must not hold back the guide: it is served once the
// first source is fetched, while the second is still in progress.
func TestGuideServedWhileASourceIsSlow(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	slow := func(ctx context.Context) (*http.Response, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return body(200, []byte(source("de", "b")))(ctx)
	}
	e := startGuideNoWait(t, twoSources("6h"), guideChannels, map[string]func(context.Context) (*http.Response, error){
		"gr": body(200, []byte(source("gr", "a"))),
		"de": slow,
	})
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the second source is not fetched")
	}
	first := e.g.Output()
	if first == nil {
		t.Fatal("no guide while the second source is in progress")
	}
	if s := string(first.Plain); !strings.Contains(s, "<title>gr a</title>") || strings.Contains(s, "<title>de b</title>") ||
		!first.Modified.Equal(filterNow) {
		t.Errorf("guide (modified %v):\n%s", first.Modified, first.Plain)
	}

	e.clock.advance(time.Minute)
	close(release)
	e.clock.waitIdle(t)
	out := e.g.Output()
	if s := string(out.Plain); !strings.Contains(s, "<title>gr a</title>") || !strings.Contains(s, "<title>de b</title>") {
		t.Errorf("guide:\n%s", s)
	}
	if out.ETag == first.ETag || !out.Modified.Equal(filterNow.Add(time.Minute)) {
		t.Errorf("etag %s modified %v", out.ETag, out.Modified)
	}
}

// With a refresh shorter than a fetch the first source is due again as
// soon as it ends; the others are still fetched, the oldest due time
// first.
func TestGuideShortRefreshDoesNotStarve(t *testing.T) {
	var e *guideEnv
	grFetches := 0
	ready := make(chan struct{}) // e is set
	slowGR := func(ctx context.Context) (*http.Response, error) {
		<-ready
		grFetches++ // only the guide goroutine fetches
		if grFetches <= 3 {
			e.clock.advance(2 * time.Minute) // longer than the refresh
		}
		return body(200, []byte(source("gr", "a")))(ctx)
	}
	e = startGuideNoWait(t, twoSources("1m"), guideChannels, map[string]func(context.Context) (*http.Response, error){
		"gr": slowGR,
		"de": body(200, []byte(source("de", "b"))),
	})
	close(ready)
	e.clock.waitIdle(t)
	got := e.src.takeFetched()
	if len(got) < 2 || got[0] != "gr" || got[1] != "de" {
		t.Fatalf("fetched %v, want gr then de before gr again", got)
	}
	n := 0
	for _, name := range got {
		if name == "de" {
			n++
		}
	}
	if n < 2 {
		t.Errorf("fetched %v: de is fetched less often than it is due", got)
	}
	out := e.g.Output()
	if out == nil || !strings.Contains(string(out.Plain), "<title>gr a</title>") || !strings.Contains(string(out.Plain), "<title>de b</title>") {
		t.Errorf("guide: %v", out)
	}
}

// syncBuf is a log buffer safe for the guide goroutine and the test.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestGuideRefreshAndFailure(t *testing.T) {
	e := startGuide(t, twoSources("1h"), guideChannels, map[string]func(context.Context) (*http.Response, error){
		"gr": body(200, []byte(source("gr", "a"))),
		"de": body(200, []byte(source("de", "b"))),
	})
	e.src.takeFetched()
	first := e.g.Output()

	// Unchanged sources: the guide keeps its ETag and time.
	e.clock.advance(time.Hour)
	if d := e.clock.waitIdle(t); d != time.Hour {
		t.Fatalf("next refresh in %v, want 1h", d)
	}
	if got := e.src.takeFetched(); strings.Join(got, ",") != "gr,de" {
		t.Errorf("fetched %v", got)
	}
	if e.g.Output() != first {
		t.Error("an unchanged guide was replaced")
	}

	// gr fails, de changes: gr keeps its last good copy, de updates,
	// and gr is tried again after RetryInterval.
	e.src.set("gr", body(http.StatusInternalServerError, nil))
	e.src.set("de", body(200, []byte(source("de2", "b"))))
	e.clock.advance(time.Hour)
	if d := e.clock.waitIdle(t); d != RetryInterval {
		t.Fatalf("next fetch in %v, want the retry interval", d)
	}
	out := e.g.Output()
	if s := string(out.Plain); !strings.Contains(s, "<title>gr a</title>") || !strings.Contains(s, "<title>de2 b</title>") {
		t.Errorf("guide:\n%s", s)
	}
	if out.ETag == first.ETag || !out.Modified.Equal(filterNow.Add(2*time.Hour)) {
		t.Errorf("etag %s modified %v", out.ETag, out.Modified)
	}
	st := e.g.Status()
	if st[0].Fetches[ResultStatus] != 1 || st[0].Fetches[ResultOK] != 2 || !st[0].Updated.Equal(filterNow.Add(time.Hour)) {
		t.Errorf("gr status %+v", st[0])
	}
	if logs := e.logs.String(); !strings.Contains(logs, "epg source failed: keeping the last good copy") ||
		!strings.Contains(logs, "result=status") || !strings.Contains(logs, "serving_copy_age=1h0m0s") {
		t.Errorf("logs:\n%s", logs)
	}

	// Only gr is due after the retry interval.
	e.src.takeFetched()
	e.src.set("gr", body(200, []byte(source("gr2", "a"))))
	e.clock.advance(RetryInterval)
	if d := e.clock.waitIdle(t); d != time.Hour-RetryInterval {
		t.Fatalf("next fetch in %v, want de's refresh", d)
	}
	if got := e.src.takeFetched(); strings.Join(got, ",") != "gr" {
		t.Errorf("fetched %v, want only gr", got)
	}
	if s := string(e.g.Output().Plain); !strings.Contains(s, "<title>gr2 a</title>") {
		t.Errorf("guide:\n%s", s)
	}
}

func TestGuideStartsWithoutGuide(t *testing.T) {
	e := startGuide(t, twoSources("6h"), guideChannels, map[string]func(context.Context) (*http.Response, error){
		"gr": body(200, []byte("<html>not xmltv</html>")),
		"de": body(200, []byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 0, 1, 2, 3}),
	})
	if out := e.g.Output(); out != nil {
		t.Fatalf("guide without any good copy: %s", out.Plain)
	}
	st := e.g.Status()
	if st[0].Fetches[ResultInvalid] != 1 || st[1].Fetches[ResultInvalid] != 1 || !st[0].Updated.IsZero() {
		t.Errorf("status %+v", st)
	}
	if logs := e.logs.String(); !strings.Contains(logs, "serving_copy_age=none") || strings.Contains(logs, "not xmltv") {
		t.Errorf("logs:\n%s", logs)
	}
	e.src.set("de", body(200, []byte(source("de", "b"))))
	e.clock.advance(RetryInterval)
	e.clock.waitIdle(t)
	if out := e.g.Output(); out == nil || !strings.Contains(string(out.Plain), "<title>de b</title>") {
		t.Fatalf("guide after the retry: %v", out)
	}
}

func TestGuideUpdate(t *testing.T) {
	e := startGuide(t, twoSources("6h"), guideChannels, map[string]func(context.Context) (*http.Response, error){
		"gr": body(200, []byte(source("gr", "a", "c"))),
		"de": body(200, []byte(source("de", "b", "c"))),
	})
	e.src.takeFetched()

	// The same config fetches nothing; a shorter refresh moves the next
	// fetch without one.
	e.g.Update(e.load(t, twoSources("6h"), guideChannels))
	e.clock.advance(time.Hour)
	e.g.Update(e.load(t, twoSources("2h"), guideChannels))
	if d := e.clock.waitIdle(t); d != time.Hour {
		t.Fatalf("next refresh in %v", d)
	}
	if got := e.src.takeFetched(); len(got) != 0 {
		t.Errorf("fetched %v without a change", got)
	}

	// A new tvg_id: every source again, at once.
	more := guideChannels + "[[channel]]\nname = \"C\"\nurl = \"https://live.example.com/c.m3u8\"\nplaylist = { tvg_id = \"c\", epg_source = \"de\" }\n"
	e.g.Update(e.load(t, twoSources("2h"), more))
	if d := e.clock.waitIdle(t); d != 2*time.Hour {
		t.Fatalf("next refresh in %v", d)
	}
	if got := e.src.takeFetched(); strings.Join(got, ",") != "gr,de" {
		t.Errorf("fetched %v", got)
	}
	if s := string(e.g.Output().Plain); !strings.Contains(s, "<title>de c</title>") || strings.Contains(s, "<title>gr c</title>") {
		t.Errorf("guide:\n%s", s)
	}

	// A removed source is forgotten with its copy; a source whose URL
	// changed loses its copy until the new one is fetched.
	one := "[[epg.source]]\nname = \"gr\"\nurl = \"https://epg.example.org/other.xml\"\n"
	e.src.set("gr", body(http.StatusNotFound, nil))
	e.g.Update(e.load(t, one, guideChannels))
	e.clock.waitIdle(t)
	if got := e.src.takeFetched(); strings.Join(got, ",") != "gr" {
		t.Errorf("fetched %v", got)
	}
	if out := e.g.Output(); out != nil {
		t.Errorf("guide from removed or changed sources:\n%s", out.Plain)
	}
	if st := e.g.Status(); len(st) != 1 || st[0].Fetches[ResultOK] != 2 || st[0].Fetches[ResultStatus] != 1 {
		t.Errorf("status %+v", st)
	}

	// No sources: no guide, Run waits for the next Update.
	e.g.Update(e.load(t, "", guideChannels))
	if out := e.g.Output(); out != nil {
		t.Error("guide without sources")
	}
}

func TestGuideFetchTimeout(t *testing.T) {
	started := make(chan struct{}, 1)
	slow := func(ctx context.Context) (*http.Response, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	e := &guideEnv{clock: newManualClock(), src: &fakeSources{answers: map[string]func(context.Context) (*http.Response, error){"gr": slow}}, logs: &syncBuf{}, dir: t.TempDir()}
	e.g = NewGuide(GuideOptions{Fetch: e.src.fetch, Clock: e.clock})
	e.g.Update(e.load(t, "[[epg.source]]\nname = \"gr\"\nurl = \"https://epg.example.org/gr.xml\"\n", guideChannels))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.g.Run(ctx)
	}()
	<-started
	if d := <-e.clock.armed; d != FetchTimeout {
		t.Fatalf("armed %v", d)
	}
	e.clock.advance(FetchTimeout)
	if d := <-e.clock.armed; d != RetryInterval-FetchTimeout {
		t.Fatalf("next fetch in %v, want the retry interval from the start of the fetch", d)
	}
	if st := e.g.Status(); st[0].Fetches["timeout"] != 1 {
		t.Errorf("status %+v", st)
	}

	// Shutdown during a fetch ends Run.
	e.clock.advance(RetryInterval)
	<-started
	cancel()
	<-done
}

func TestClassify(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{nil, ResultOK},
		{&statusError{code: 404}, ResultStatus},
		{ErrTooLarge, "size"},
		{errFetchTimeout, "timeout"},
		{invalid("x"), ResultInvalid},
		{gzip.ErrChecksum, ResultInvalid},
		{io.ErrUnexpectedEOF, ResultInvalid},
		{&route.Error{URL: "https://epg.example.org", Err: route.ErrBlocked}, "guard"},
		{&route.Error{URL: "https://epg.example.org", Err: route.ErrReadTimeout}, "timeout"},
		{errors.New("boom"), "other"},
	}
	for _, tt := range tests {
		if got := classify(tt.err); got != tt.want {
			t.Errorf("classify(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
	for _, r := range Results {
		if r == "" {
			t.Error("empty result")
		}
	}
}
