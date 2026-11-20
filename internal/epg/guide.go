// SPDX-License-Identifier: AGPL-3.0-or-later

package epg

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
)

const (
	// RetryInterval is the time until a failed source is fetched again,
	// unless [epg] refresh is shorter.
	RetryInterval = 10 * time.Minute
	// FetchTimeout bounds one fetch of a source, download and filter,
	// on top of the [upstream] timeouts (which have no total limit).
	FetchTimeout = 5 * time.Minute
)

// Fetch results, the result label of wsp_epg_fetches_total (spec 7.3):
// ResultOK, the route.Reason values connect, timeout, guard, tls,
// redirects, size and other, and these.
const (
	ResultOK = "ok"
	// ResultStatus: the source answered with a status other than 200.
	ResultStatus = "status"
	// ResultInvalid: the file is no gzip or XMLTV the filter accepts.
	ResultInvalid = "invalid"
)

// Results lists every fetch result.
var Results = []string{
	ResultOK, string(route.ReasonConnect), string(route.ReasonTimeout), string(route.ReasonGuard),
	string(route.ReasonTLS), string(route.ReasonRedirects), string(route.ReasonSize),
	ResultStatus, ResultInvalid, string(route.ReasonOther),
}

// errFetchTimeout: a fetch took longer than FetchTimeout.
var errFetchTimeout = errors.New("epg: fetch took longer than 5m")

// Clock is the clock of a Guide; tests inject a manual one (spec 9).
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) route.Timer
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) AfterFunc(d time.Duration, f func()) route.Timer { return time.AfterFunc(d, f) }

// FetchFunc fetches the URL of a source through its route (spec 3.4
// rules: guard, [upstream] timeouts, redirects). The caller closes the
// body.
type FetchFunc func(ctx context.Context, src config.EPGSource) (*http.Response, error)

// GuideOptions configure a Guide.
type GuideOptions struct {
	// Fetch fetches a source. Required.
	Fetch FetchFunc
	// Clock schedules the refreshes; nil means the system clock.
	Clock Clock
	// Logger receives one line per fetch; nil discards.
	Logger *slog.Logger
}

// Output is a served guide: the merged XMLTV document, plain and gzip.
type Output struct {
	Plain, Gzip []byte
	// ETag is the strong validator of Plain, quoted; the one of Gzip is
	// GzipETag.
	ETag, GzipETag string
	// Modified is when the content last changed.
	Modified   time.Time
	Channels   int
	Programmes int
}

// SourceStatus is the state of one source for the metrics (spec 7.3).
type SourceStatus struct {
	Name string
	// Fetches counts the fetches by result (Results).
	Fetches map[string]uint64
	// Updated is when the copy in use was fetched; zero without one.
	Updated time.Time
}

// Guide fetches the EPG sources, filters and merges them and keeps the
// served guide (spec 6.1): each source is fetched one after the
// other, every [epg] refresh, a failed one again after RetryInterval;
// a failed source keeps its last good copy. Update applies a reloaded
// config; Run does the work.
type Guide struct {
	fetch FetchFunc
	clock Clock
	log   *slog.Logger
	wake  chan struct{}
	out   atomic.Pointer[Output]

	mu      sync.Mutex
	sources []config.EPGSource
	wants   []Want
	refresh time.Duration
	state   map[string]*sourceState
	// dirty: the guide must be merged again (a new copy, a new config).
	dirty bool
}

// sourceState is what the guide knows about one source.
type sourceState struct {
	src  config.EPGSource
	copy *Copy
	// copyAt is when copy was fetched.
	copyAt time.Time
	// tried is the start of the last fetch, zero before the first;
	// failed tells whether it failed.
	tried  time.Time
	failed bool
	// force: fetch at once (after a config change); epoch counts the
	// changes, so a fetch that ran during one does not clear force.
	force bool
	epoch uint64

	fetches map[string]uint64
}

// NewGuide returns a Guide without sources; Update sets them.
func NewGuide(opts GuideOptions) *Guide {
	g := &Guide{
		fetch: opts.Fetch,
		clock: opts.Clock,
		log:   opts.Logger,
		wake:  make(chan struct{}, 1),
		state: map[string]*sourceState{},
	}
	if g.clock == nil {
		g.clock = systemClock{}
	}
	if g.log == nil {
		g.log = slog.New(slog.DiscardHandler)
	}
	return g
}

// Output returns the served guide, nil until the first source was
// fetched (or after the sources were removed).
func (g *Guide) Output() *Output { return g.out.Load() }

// Update applies the EPG settings of cfg (spec 5.6): when the sources or
// the wanted ids changed, every source is fetched again at once; a
// removed source is forgotten and a changed one (URL, route) loses its
// copy. A changed refresh applies to the next fetch times.
func (g *Guide) Update(cfg *config.Config) {
	sources := cfg.EPGSources()
	wants := Wants(cfg.Channels())
	g.mu.Lock()
	changed := !slices.Equal(sources, g.sources) || !slices.Equal(wants, g.wants)
	wake := changed || cfg.EPGRefresh() != g.refresh
	g.refresh = cfg.EPGRefresh()
	if changed {
		keep := map[string]bool{}
		for _, s := range sources {
			keep[s.Name] = true
			st := g.state[s.Name]
			if st == nil {
				st = &sourceState{fetches: map[string]uint64{}}
				g.state[s.Name] = st
			}
			if st.src != s {
				st.src, st.copy, st.copyAt = s, nil, time.Time{}
			}
			st.force = true
			st.epoch++
		}
		maps.DeleteFunc(g.state, func(name string, _ *sourceState) bool { return !keep[name] })
		g.sources, g.wants = sources, wants
		g.dirty = true
		if len(sources) == 0 {
			g.out.Store(nil) // at once: Run has nothing to fetch
		}
	}
	g.mu.Unlock()
	if wake {
		g.signal()
	}
}

// signal wakes Run.
func (g *Guide) signal() {
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

// Status returns the state of each source in config order.
func (g *Guide) Status() []SourceStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]SourceStatus, 0, len(g.sources))
	for _, s := range g.sources {
		st := g.state[s.Name]
		ss := SourceStatus{Name: s.Name, Fetches: maps.Clone(st.fetches)}
		if st.copy != nil {
			ss.Updated = st.copyAt
		}
		out = append(out, ss)
	}
	return out
}

// Run fetches the sources when they are due and merges the guide, until
// ctx ends.
func (g *Guide) Run(ctx context.Context) {
	tick := make(chan struct{}, 1)
	for {
		// This round reads the state Update left; a pending wake would
		// only start another one.
		select {
		case <-g.wake:
		default:
		}
		for {
			src, epoch, ids, ok := g.due()
			if !ok {
				break
			}
			g.fetchSource(ctx, src, epoch, ids)
			if ctx.Err() != nil {
				return
			}
		}
		g.merge()

		var timer route.Timer
		if d, ok := g.untilNext(); ok {
			timer = g.clock.AfterFunc(d, func() {
				select {
				case tick <- struct{}{}:
				default:
				}
			})
		}
		select {
		case <-ctx.Done():
		case <-tick:
		case <-g.wake:
		}
		if timer != nil {
			timer.Stop()
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// interval is the time from a fetch to the next one of a source.
func (g *Guide) interval(st *sourceState) time.Duration {
	if st.failed {
		return min(g.refresh, RetryInterval)
	}
	return g.refresh
}

// due returns the first source in config order that is due, with the
// ids to filter it for.
func (g *Guide) due() (config.EPGSource, uint64, map[string]bool, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock.Now()
	for _, s := range g.sources {
		st := g.state[s.Name]
		if st.force || st.tried.IsZero() || !now.Before(st.tried.Add(g.interval(st))) {
			return s, st.epoch, idsFor(s.Name, g.wants), true
		}
	}
	return config.EPGSource{}, 0, nil, false
}

// untilNext returns the time until the next source is due; false
// without sources.
func (g *Guide) untilNext() (time.Duration, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.sources) == 0 {
		return 0, false
	}
	now := g.clock.Now()
	var next time.Time
	for _, s := range g.sources {
		st := g.state[s.Name]
		at := st.tried.Add(g.interval(st))
		if next.IsZero() || at.Before(next) {
			next = at
		}
	}
	return max(next.Sub(now), 0), true
}

// fetchSource fetches and filters one source and stores the result.
func (g *Guide) fetchSource(ctx context.Context, src config.EPGSource, epoch uint64, ids map[string]bool) {
	start := g.clock.Now()
	c, err := g.download(ctx, src, ids, start)
	d := g.clock.Now().Sub(start)
	if ctx.Err() != nil {
		return // shutting down
	}
	result := classify(err)

	g.mu.Lock()
	st := g.state[src.Name]
	if st == nil || st.src != src {
		// Removed or changed while it was fetched.
		g.mu.Unlock()
		return
	}
	st.tried, st.failed = start, err != nil
	st.fetches[result]++
	if st.epoch == epoch {
		st.force = false
	}
	var age time.Duration
	hadCopy := st.copy != nil
	if err == nil {
		st.copy, st.copyAt = c, start
		g.dirty = true
	} else if hadCopy {
		age = start.Sub(st.copyAt)
	}
	g.mu.Unlock()

	if err != nil {
		attrs := []any{"source", src.Name, "route", src.Route, "url", route.LogURL(src.URL), "result", result, "error", err, "duration", d}
		if hadCopy {
			attrs = append(attrs, "serving_copy_age", age.Round(time.Second))
		} else {
			attrs = append(attrs, "serving_copy_age", "none")
		}
		g.log.Warn("epg source failed: keeping the last good copy", attrs...)
		return
	}
	g.log.Info("epg source fetched", "source", src.Name, "route", src.Route, "url", route.LogURL(src.URL),
		"bytes", c.Read, "filtered_bytes", c.Size, "channels", c.Channels(), "programmes", c.Programmes, "duration", d)
}

// download fetches src and filters it for ids at now, within
// FetchTimeout.
func (g *Guide) download(ctx context.Context, src config.EPGSource, ids map[string]bool, now time.Time) (*Copy, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	timer := g.clock.AfterFunc(FetchTimeout, func() { cancel(errFetchTimeout) })
	defer timer.Stop()
	c, err := g.downloadCtx(ctx, src, ids, now)
	if err != nil && errors.Is(context.Cause(ctx), errFetchTimeout) {
		return nil, errFetchTimeout
	}
	return c, err
}

func (g *Guide) downloadCtx(ctx context.Context, src config.EPGSource, ids map[string]bool, now time.Time) (*Copy, error) {
	resp, err := g.fetch(ctx, src)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, &statusError{code: resp.StatusCode}
	}
	r, err := Open(resp.Body, MaxSize)
	if err != nil {
		return nil, err
	}
	return Filter(r, ids, now)
}

// statusError: the source answered with a status other than 200.
type statusError struct{ code int }

func (e *statusError) Error() string { return fmt.Sprintf("epg: status %d", e.code) }

// classify returns the fetch result of err (Results).
func classify(err error) string {
	var (
		se *statusError
		ce flate.CorruptInputError
	)
	switch {
	case err == nil:
		return ResultOK
	case errors.As(err, &se):
		return ResultStatus
	case errors.Is(err, ErrTooLarge):
		return string(route.ReasonSize)
	case errors.Is(err, errFetchTimeout):
		return string(route.ReasonTimeout)
	case errors.Is(err, ErrInvalid), errors.Is(err, gzip.ErrHeader), errors.Is(err, gzip.ErrChecksum),
		errors.As(err, &ce), errors.Is(err, io.ErrUnexpectedEOF):
		// Not XMLTV, or broken or cut gzip data.
		return ResultInvalid
	}
	return string(route.Classify(err))
}

// merge builds the guide from the copies if anything changed and
// publishes it; an unchanged document keeps its ETag and time. Without
// any copy there is no guide.
func (g *Guide) merge() {
	g.mu.Lock()
	if !g.dirty {
		g.mu.Unlock()
		return
	}
	g.dirty = false
	order := make([]string, 0, len(g.sources))
	copies := map[string]*Copy{}
	for _, s := range g.sources {
		order = append(order, s.Name)
		if c := g.state[s.Name].copy; c != nil {
			copies[s.Name] = c
		}
	}
	wants := g.wants
	g.mu.Unlock()

	if len(copies) == 0 {
		g.out.Store(nil)
		return
	}
	doc, channels, programmes := Merge(order, copies, wants)
	prev := g.out.Load()
	if prev != nil && bytes.Equal(prev.Plain, doc) {
		return
	}
	var zb bytes.Buffer
	zw := gzip.NewWriter(&zb)
	_, _ = zw.Write(doc)
	_ = zw.Close()
	sum := sha256.Sum256(doc)
	tag := hex.EncodeToString(sum[:16])
	g.out.Store(&Output{
		Plain: doc, Gzip: zb.Bytes(),
		ETag: `"` + tag + `"`, GzipETag: `"` + tag + `.gz"`,
		Modified: g.clock.Now(), Channels: channels, Programmes: programmes,
	})
	g.log.Info("epg guide updated", "channels", channels, "programmes", programmes, "bytes", len(doc), "gzip_bytes", zb.Len())
}
