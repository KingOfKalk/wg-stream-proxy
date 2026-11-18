// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/hls"
	"github.com/KingOfKalk/wg-stream-proxy/internal/metrics"
	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
)

// playlistContentType is the Content-Type of rewritten playlists (spec
// 3.4).
const playlistContentType = "application/vnd.apple.mpegurl"

// copyBufferSize is the chunk size of piped bodies; every chunk is
// flushed to the client.
const copyBufferSize = 32 << 10

// handleChannel serves /c/{id} and /c/{id}.m3u8 (spec 3.1): the channel
// URL with the configured upstream type.
func (s *Server) handleChannel(w http.ResponseWriter, r *http.Request) {
	g := s.active()
	ch, ok := g.channel(strings.TrimSuffix(r.PathValue("id"), ".m3u8"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	typ, err := hls.ParseType(string(ch.Type()))
	if err != nil {
		// The config validates the type; fail closed anyway.
		s.log.Error("invalid upstream type", "channel", ch.ID(), "error", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	// Raw and auto channels may be Shoutcast v1 radios: fetch them through
	// the route's ICY transport (spec 3.3). The type of an auto channel is
	// known only from the response, and the ICY wrapper leaves HTTP
	// responses unchanged, so an auto channel that turns out to be HLS
	// works too.
	s.proxy(w, r, g, ch, fetch{upstream: ch.URL(), configured: typ, icy: typ != hls.HLS, other: metrics.KindRaw})
}

// handleResource serves /r/{channel}/{sig}/{b64url}/{name} (spec 4).
// Route and headers come from the channel config, never from the URL.
func (s *Server) handleResource(w http.ResponseWriter, r *http.Request) {
	id, upstream, err := s.signer.Parse(r.URL.EscapedPath())
	if err != nil {
		http.NotFound(w, r)
		return
	}
	g := s.active()
	ch, ok := g.channel(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Resources (segments, keys, sub-playlists) are HTTP: normal pool.
	s.proxy(w, r, g, ch, fetch{upstream: upstream, configured: hls.Auto, other: metrics.KindSegment})
}

// channel returns the enabled channel with the given id.
func (g *generation) channel(id string) (config.Channel, bool) {
	ch, ok := g.cfg.Channel(id)
	if !ok || !ch.Enabled() {
		return config.Channel{}, false
	}
	return ch, true
}

// fetch describes the upstream side of a /c/ or /r/ request.
type fetch struct {
	// upstream is the upstream URL.
	upstream string
	// configured is the channel's upstream type for /c/ and Auto for /r/.
	configured hls.Type
	// icy selects the route's ICY transport (route.Request.ICY).
	icy bool
	// other is the metrics kind of a response that is not a playlist:
	// raw for /c/, segment for /r/.
	other metrics.Kind
}

// kind returns the metrics kind of the request until the response type
// is known, and for responses whose type is never detected (errors,
// non-2xx): playlist for an hls channel or, without a configured type,
// an upstream path ending in .m3u8 or .m3u; else f.other.
func (f fetch) kind() metrics.Kind {
	switch f.configured {
	case hls.HLS:
		return metrics.KindPlaylist
	case hls.Auto:
		if u, err := url.Parse(f.upstream); err == nil && route.IsPlaylistPath(u.Path) {
			return metrics.KindPlaylist
		}
	}
	return f.other
}

// proxy fetches upstream through the channel's route in generation g and
// answers with a rewritten playlist or the piped body (spec 3.1-3.4). The
// request context is the upstream context: a client disconnect cancels
// the upstream request.
func (s *Server) proxy(w http.ResponseWriter, r *http.Request, g *generation, ch config.Channel, f fetch) {
	rt, ok := g.routes.Route(ch.Route())
	if !ok {
		// The config only has channels with existing routes; fail closed.
		s.log.Error("no route for channel", "channel", ch.ID(), "route", ch.Route())
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	// Only configured channel ids become labels, never URLs.
	rq := s.metrics.Begin(ch.ID(), f.kind())
	defer rq.End()
	client := r.Header
	for refetch := false; ; refetch = true {
		resp, err := rt.Fetch(r.Context(), route.Request{URL: f.upstream, Header: ch.Header(), Client: client, ICY: f.icy})
		if err != nil {
			s.fail(w, r, ch, err, rq.FetchError)
			return
		}
		retry := s.respond(w, r, g, ch, rq, resp, f, refetch)
		_ = resp.Body.Close()
		if !retry {
			return
		}
		// A playlist answered with 206: fetch it whole (spec 3.4).
		client = client.Clone()
		client.Del("Range")
		client.Del("If-Range")
	}
}

// respond answers the client from an upstream response. It returns true
// if the response is a partial playlist that must be fetched again
// without Range; refetch tells that this already happened.
func (s *Server) respond(w http.ResponseWriter, r *http.Request, g *generation, ch config.Channel, rq *metrics.Request, resp *http.Response, f fetch, refetch bool) bool {
	final := resp.Request.URL
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rq.Response(resp.StatusCode)
		if code := route.ClientStatus(resp.StatusCode); code != resp.StatusCode {
			s.log.Warn("upstream error status", "channel", ch.ID(), "route", ch.Route(),
				"url", route.RedactURL(final.String()), "upstream_status", resp.StatusCode, "status", code)
			http.Error(w, http.StatusText(code), code)
			return false
		}
		// 4xx: relayed like any other non-playlist response.
		s.stream(w, r, ch, rq, resp, resp.Body)
		return false
	}

	body := bufio.NewReaderSize(resp.Body, hls.SniffLen)
	typ := hls.Detect(resp.Header.Get("Content-Type"), nil, f.configured)
	if typ != hls.HLS && f.configured == hls.Auto && r.Method != http.MethodHead {
		// Look at the start of the body (spec 3.1 step 2.3). A HEAD
		// request closes the upstream body unread (spec 3.4).
		prefix, err := body.Peek(hls.SniffLen)
		if err != nil && !errors.Is(err, io.EOF) {
			rq.Response(resp.StatusCode)
			s.fail(w, r, ch, err, rq.Error)
			return false
		}
		typ = hls.Detect(resp.Header.Get("Content-Type"), prefix, f.configured)
	}
	if typ != hls.HLS {
		rq.SetKind(f.other)
		rq.Response(resp.StatusCode)
		s.stream(w, r, ch, rq, resp, body)
		return false
	}
	rq.SetKind(metrics.KindPlaylist)
	rq.Response(resp.StatusCode)
	if resp.StatusCode == http.StatusPartialContent {
		if refetch {
			s.log.Warn("upstream answered a playlist with 206 without Range", "channel", ch.ID(),
				"url", route.RedactURL(final.String()))
			rq.Error(string(route.ReasonOther))
			http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
			return false
		}
		return true
	}
	s.rewrite(w, r, g, ch, rq, resp, body)
	return false
}

// rewrite answers with the rewritten playlist (spec 3.2, 3.4): every URI
// becomes an absolute resource URL of the channel.
func (s *Server) rewrite(w http.ResponseWriter, r *http.Request, g *generation, ch config.Channel, rq *metrics.Request, resp *http.Response, body io.Reader) {
	h := w.Header()
	route.CopyResponseHeader(h, resp, true)
	// The rewritten body is a new, whole representation.
	h.Del("Accept-Ranges")
	h.Del("Content-Range")
	h.Set("Content-Type", playlistContentType)
	h.Set("Cache-Control", "no-cache")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}

	final := resp.Request.URL
	data, err := route.ReadPlaylist(body)
	if err != nil {
		clearHeader(h)
		s.fail(w, r, ch, err, rq.Error)
		return
	}
	base := baseURL(g.cfg.PublicBaseURL(), r)
	out, err := hls.Rewrite(data, final, func(abs *url.URL) string {
		return base + s.signer.Path(ch.ID(), abs.String())
	})
	if err != nil {
		// hls errors never contain URIs from the playlist.
		clearHeader(h)
		s.log.Warn("playlist rejected", "channel", ch.ID(), "url", route.RedactURL(final.String()), "error", err)
		rq.Error(string(route.ReasonOther))
		http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		return
	}
	h.Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
	s.log.Debug("playlist", "channel", ch.ID(), "url", route.RedactURL(final.String()))
}

// stream relays a non-playlist response: headers per spec 3.4, the
// upstream status, and the body piped with io.Copy and a flush after
// every chunk, without size or time limit (spec 3.3). HEAD sends no body.
//
// Raw channels (radio, MPEG-TS) take this path too: the route forwarded
// Icy-MetaData and turned an ICY status line into HTTP/1.0; icy-*
// headers are copied back, metadata in the body passes unchanged.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, ch config.Channel, rq *metrics.Request, resp *http.Response, body io.Reader) {
	route.CopyResponseHeader(w.Header(), resp, false)
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	fw := flushWriter{w: w, rc: http.NewResponseController(w)}
	src := &upstreamReader{r: body}
	n, err := io.CopyBuffer(fw, src, make([]byte, copyBufferSize))
	if err != nil {
		// The client went away or the upstream failed mid-stream; the
		// status is sent already. Only upstream failures count, not the
		// upstream read a client disconnect cancelled.
		if src.err != nil && r.Context().Err() == nil {
			rq.Error(string(route.Classify(src.err)))
		}
		s.log.Debug("stream ended", "channel", ch.ID(), "url", route.RedactURL(resp.Request.URL.String()),
			"bytes", n, "reason", route.Classify(err))
		return
	}
	s.log.Debug("stream done", "channel", ch.ID(), "url", route.RedactURL(resp.Request.URL.String()), "bytes", n)
}

// fail answers an upstream error: 504 for timeouts, else 502 (spec 3.4).
// Errors from route never contain query strings (spec 8). count records
// the error reason in the metrics; an error after the client went away
// is not an upstream error and counts nothing.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, ch config.Channel, err error, count func(reason string)) {
	if r.Context().Err() != nil {
		s.log.Debug("client gone", "channel", ch.ID())
		return
	}
	reason := route.Classify(err)
	count(string(reason))
	code := route.ErrorStatus(err)
	s.log.Warn("upstream request failed", "channel", ch.ID(), "route", ch.Route(),
		"reason", reason, "status", code, "error", err)
	http.Error(w, http.StatusText(code), code)
}

// clearHeader removes all headers set so far.
func clearHeader(h http.Header) {
	for k := range h {
		delete(h, k)
	}
}

// flushWriter flushes the response after every write.
type flushWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err != nil {
		return n, err
	}
	if err := f.rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return n, err
	}
	return n, nil
}

// upstreamReader records the read error of the upstream body other than
// io.EOF, so a failed stream can tell an upstream error from a client
// write error. It also hides all methods of the reader but Read:
// bufio.Reader.WriteTo would copy in SniffLen-sized chunks.
type upstreamReader struct {
	r   io.Reader
	err error
}

func (u *upstreamReader) Read(p []byte) (int, error) {
	n, err := u.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		u.err = err
	}
	return n, err
}
