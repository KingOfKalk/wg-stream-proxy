// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/epg"
	"github.com/KingOfKalk/wg-stream-proxy/internal/metrics"
	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
)

// epgPath is the served guide (spec 7.1); epgPath+".gz" is its gzip form,
// the one the channel list links.
const epgPath = "/epg.xml"

// epgLink returns the EPG URL of the channel list header (spec 6): the
// proxy's guide when it has sources, else [epg] url (link mode).
func epgLink(cfg *config.Config, base string) string {
	if len(cfg.EPGSources()) > 0 {
		return base + epgPath + ".gz"
	}
	return cfg.EPGURL()
}

// handleEPG serves the merged guide (spec 6.1, 7.1): 404 without
// [[epg.source]], 503 until the first source was fetched.
func (s *Server) handleEPG(w http.ResponseWriter, r *http.Request) {
	if len(s.active().cfg.EPGSources()) == 0 {
		http.NotFound(w, r)
		return
	}
	out := s.epg.Output()
	if out == nil {
		http.Error(w, "EPG not fetched yet", http.StatusServiceUnavailable)
		return
	}
	body, etag, ctype := out.Plain, out.ETag, "application/xml; charset=utf-8"
	if strings.HasSuffix(r.URL.Path, ".gz") {
		body, etag, ctype = out.Gzip, out.GzipETag, "application/gzip"
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("ETag", etag)
	h.Set("Cache-Control", "no-cache")
	// Conditional, HEAD and range requests.
	http.ServeContent(w, r, "", out.Modified, bytes.NewReader(body))
}

// fetchEPG fetches an EPG source through its route in the active
// generation (spec 3.4 rules: guard, [upstream] timeouts, redirects);
// a running tunnel is used, a stopped one starts. The fetch is not
// activity for the idle close (spec 2.1, 6.1).
func (s *Server) fetchEPG(ctx context.Context, src config.EPGSource) (*http.Response, error) {
	g, ctx, done := s.useBackground(ctx, func(*generation) string { return src.Route })
	rt, ok := g.routes.Route(src.Route)
	if !ok {
		done()
		return nil, fmt.Errorf("epg: unknown route %q", src.Route)
	}
	resp, err := rt.Fetch(ctx, route.Request{URL: src.URL})
	if err != nil {
		done()
		return nil, err
	}
	// The guide reads the body later: the use ends when it closes it.
	resp.Body = &genBody{ReadCloser: resp.Body, done: done}
	return resp, nil
}

// genBody is a response body that ends its use of a generation (done)
// when it is closed; done runs once.
type genBody struct {
	io.ReadCloser
	done func()
}

// Close implements io.Closer.
func (b *genBody) Close() error {
	err := b.ReadCloser.Close()
	b.done()
	return err
}

// startEPG runs the guide's refreshes until the returned function is
// called; it waits for a fetch in progress to end and may be called
// more than once.
func startEPG(g *epg.Guide) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.Run(ctx)
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}

// epgStatus returns the state of the EPG sources for the metrics.
func (s *Server) epgStatus() []metrics.EPGSource {
	st := s.epg.Status()
	out := make([]metrics.EPGSource, len(st))
	for i, ss := range st {
		out[i] = metrics.EPGSource{
			Name: ss.Name, Fetches: ss.Fetches, Updated: ss.Updated,
		}
	}
	return out
}
