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
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/dash"
	"github.com/KingOfKalk/wg-stream-proxy/internal/hls"
	"github.com/KingOfKalk/wg-stream-proxy/internal/metrics"
	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel"
)

// Content-Types of rewritten HLS playlists and DASH manifests (spec
// 3.4).
const (
	playlistContentType = "application/vnd.apple.mpegurl"
	manifestContentType = "application/dash+xml"
)

// copyBufferSize is the chunk size of piped bodies; every chunk is
// flushed to the client.
const copyBufferSize = 32 << 10

// handleChannel serves /c/{id}, /c/{id}.m3u8 and /c/{id}.mpd (spec 3.1,
// 6): the channel URL with the configured upstream type.
func (s *Server) handleChannel(w http.ResponseWriter, r *http.Request) {
	g := s.active()
	id, ok := strings.CutSuffix(r.PathValue("id"), ".m3u8")
	if !ok {
		id = strings.TrimSuffix(id, ".mpd")
	}
	ch, ok := g.channel(id)
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
	// responses unchanged, so an auto channel that turns out to be HLS or
	// DASH works too. HLS and DASH channels use the normal pool.
	s.proxy(w, r, g, ch, fetch{
		upstream: ch.URL(), configured: typ, icy: typ == hls.Auto || typ == hls.Raw, other: metrics.KindRaw,
		errorClips: errorClips(r, g.cfg, ch),
	})
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

// handlePrefix serves /p/{channel}/{sig}/{mode}/{b64prefix}/{path} (spec
// 4.1): the signed upstream prefix plus the path below it, which the
// player built from a rewritten DASH manifest. Route and headers come
// from the channel config, never from the URL.
func (s *Server) handlePrefix(w http.ResponseWriter, r *http.Request) {
	pr, err := s.signer.ParsePrefix(r.URL)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	g := s.active()
	ch, ok := g.channel(pr.Channel)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Upstream redirects are followed only below the prefix: the client
	// chooses the path, so an open redirect on the prefix host would
	// otherwise reach any URL through the channel's route.
	s.proxy(w, r, g, ch, fetch{upstream: pr.URL, configured: hls.Auto, other: metrics.KindSegment, redirect: pr.Below})
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
	// errorClips: a failure is answered with an error clip instead of an
	// HTTP error (spec 3.4).
	errorClips bool
	// redirect, if set, is route.Request.Redirect: which redirects are
	// followed (prefix-signed resources, spec 4.1).
	redirect func(*url.URL) bool
}

// kind returns the metrics kind of the request until the response type
// is known, and for responses whose type is never detected (errors,
// non-2xx): playlist for an hls or dash channel or, without a configured
// type, an upstream path ending in .m3u8, .m3u or .mpd; else f.other.
func (f fetch) kind() metrics.Kind {
	switch f.configured {
	case hls.HLS, hls.DASH:
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
	// Every request is recorded, at every log level: the trace of its
	// upstream requests (one ClientTrace each) and the tunnel start it
	// waited for. When it ends, requestFinished gets what was recorded.
	rec := &requestRecord{ResponseWriter: w, id: s.requestIDs.Add(1), start: time.Now()}
	w = rec
	ctx := tunnel.WithStartTrace(r.Context(), rec.trace.NetworkWait)
	defer func() { s.requestFinished(rec.finish(r, ch, rq)) }()
	client := r.Header
	for refetch := false; ; refetch = true {
		resp, err := rt.Fetch(ctx, route.Request{URL: f.upstream, Header: ch.Header(), Client: client, ICY: f.icy, Trace: &rec.trace, Redirect: f.redirect})
		if err != nil {
			s.fail(w, r, g, ch, f, err, rq.FetchError)
			return
		}
		retry := s.respond(w, r, g, ch, rq, resp, f, refetch)
		_ = resp.Body.Close()
		if !retry {
			return
		}
		// A playlist, or a body with a content coding, answered with 206:
		// fetch it whole (spec 3.4).
		client = client.Clone()
		client.Del("Range")
		client.Del("If-Range")
	}
}

// respond answers the client from an upstream response. It returns true
// if the response is a partial playlist, or a partial response with a
// content coding, that must be fetched again without Range; refetch
// tells that this already happened.
func (s *Server) respond(w http.ResponseWriter, r *http.Request, g *generation, ch config.Channel, rq *metrics.Request, resp *http.Response, f fetch, refetch bool) bool {
	final := resp.Request.URL
	recordOf(w).upstream(resp.StatusCode)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rq.Response(resp.StatusCode)
		code := route.ClientStatus(resp.StatusCode)
		if clip := f.clip(nil); code != resp.StatusCode || clip != "" {
			// A 4xx is relayed; with an error clip it is logged like a 5xx.
			s.log.Warn("upstream error status", withClip([]any{
				"channel", ch.ID(), "route", ch.Route(),
				"url", route.LogURL(final.String()), "upstream_status", resp.StatusCode, "status", code,
			}, clip)...)
			s.answerFailure(w, r, g, code, clip)
			return false
		}
		// 4xx: relayed like any other non-playlist response, decoded
		// like one (spec 3.4).
		if err := route.DecodeBody(resp); err != nil {
			s.fail(w, r, g, ch, f, err, rq.Error)
			return false
		}
		s.stream(w, r, ch, rq, resp, resp.Body)
		return false
	}

	// A Content-Encoding the transport did not undo is decoded or refused
	// before the body is classified (spec 3.4): a compressed playlist is
	// rewritten or refused like the plain one, and no body reaches the
	// client encoded without its Content-Encoding. A 206 with a coding is
	// fetched again whole, like a partial playlist.
	if err := route.DecodeBody(resp); err != nil {
		rq.Response(resp.StatusCode)
		if errors.Is(err, route.ErrEncodedPartial) && !refetch {
			return true
		}
		s.fail(w, r, g, ch, f, err, rq.Error)
		return false
	}

	// Unless the type is known, look at the start of the body: an auto
	// channel or resource may be HLS or DASH (spec 3.1 steps 2.3, 2.4),
	// and no channel may pipe a playlist the proxy does not rewrite (spec
	// 3.3). A HEAD request closes the upstream body unread (spec 3.4); it
	// sends no URLs.
	typ, format, body, err := hls.Classify(resp.Header.Get("Content-Type"),
		bufio.NewReaderSize(resp.Body, hls.SniffLen), f.configured, r.Method != http.MethodHead)
	switch {
	case err != nil:
		rq.Response(resp.StatusCode)
		s.fail(w, r, g, ch, f, err, rq.Error)
		return false
	case format != "":
		s.refuse(w, r, g, ch, f, rq, resp, format)
		return false
	case typ == hls.Raw:
		rq.SetKind(f.other)
		rq.Response(resp.StatusCode)
		s.stream(w, r, ch, rq, resp, body)
		return false
	}
	rq.SetKind(metrics.KindPlaylist)
	rq.Response(resp.StatusCode)
	if resp.StatusCode == http.StatusPartialContent {
		if refetch {
			clip := f.clip(nil)
			s.log.Warn("upstream answered a playlist with 206 without Range", withClip([]any{
				"channel", ch.ID(),
				"url", route.LogURL(final.String()),
			}, clip)...)
			rq.Error(string(route.ReasonOther))
			recordOf(w).failed(route.ReasonOther)
			s.answerFailure(w, r, g, http.StatusBadGateway, clip)
			return false
		}
		return true
	}
	s.rewrite(w, r, g, ch, f, rq, resp, body, typ)
	return false
}

// refuse answers a playlist in a format the proxy does not rewrite with
// 502 (spec 3.3): piped, it would hand the player upstream URLs to fetch
// past the channel's tunnel. Nothing of the body reaches the client.
func (s *Server) refuse(w http.ResponseWriter, r *http.Request, g *generation, ch config.Channel, f fetch, rq *metrics.Request, resp *http.Response, format string) {
	rq.SetKind(metrics.KindPlaylist)
	rq.Response(resp.StatusCode)
	clip := f.clip(nil)
	s.log.Warn("unsupported playlist format refused", withClip([]any{
		"channel", ch.ID(), "route", ch.Route(),
		"url", route.LogURL(resp.Request.URL.String()), "format", format,
	}, clip)...)
	rq.Error(string(route.ReasonUnsupported))
	recordOf(w).failed(route.ReasonUnsupported)
	s.answerFailure(w, r, g, http.StatusBadGateway, clip)
}

// rewrite answers with the rewritten HLS playlist (spec 3.2, 3.4), every
// URI an absolute resource URL of the channel, or with the rewritten DASH
// manifest (typ DASH, spec 3.6), every URL below an absolute prefix URL of
// the channel.
func (s *Server) rewrite(w http.ResponseWriter, r *http.Request, g *generation, ch config.Channel, f fetch, rq *metrics.Request, resp *http.Response, body io.Reader, typ hls.Type) {
	h := w.Header()
	route.CopyResponseHeader(h, resp, true)
	// The rewritten body is a new, whole representation.
	h.Del("Accept-Ranges")
	h.Del("Content-Range")
	h.Set("Content-Type", playlistContentType)
	if typ == hls.DASH {
		h.Set("Content-Type", manifestContentType)
	}
	h.Set("Cache-Control", "no-cache")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}

	final := resp.Request.URL
	data, err := route.ReadPlaylist(body, g.cfg.Upstream().MaxPlaylistSize)
	if err != nil {
		clearHeader(h)
		s.fail(w, r, g, ch, f, err, rq.Error)
		return
	}
	base := baseURL(g.cfg.PublicBaseURL(), r)
	var out []byte
	if typ == hls.DASH {
		out, err = dash.Rewrite(data, dash.Options{
			Final: final,
			PrefixURL: func(prefix string, query bool) (string, error) {
				p, err := s.signer.PrefixPath(ch.ID(), prefix, query)
				return base + p, err
			},
			TimeURL: base + timePath,
		})
	} else {
		paths := s.signer.PathBuilder(ch.ID())
		out, err = hls.Rewrite(data, final, func(dst, abs []byte) []byte {
			return paths.AppendPath(append(dst, base...), abs)
		})
	}
	if err != nil {
		// hls and dash errors never contain URLs from the playlist.
		clip := f.clip(nil)
		msg, reason := "playlist rejected", route.ReasonOther
		switch {
		case errors.Is(err, dash.ErrDRM):
			msg, reason = "DRM manifest refused", route.ReasonDRM
		case errors.Is(err, dash.ErrTooLarge), errors.Is(err, hls.ErrPlaylistTooLarge):
			reason = route.ReasonSize
		}
		s.log.Warn(msg, withClip([]any{"channel", ch.ID(), "url", route.LogURL(final.String()), "error", err}, clip)...)
		rq.Error(string(reason))
		recordOf(w).failed(reason)
		s.answerFailure(w, r, g, http.StatusBadGateway, clip)
		return
	}
	h.Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
	s.log.Debug("playlist", "channel", ch.ID(), "url", route.LogURL(final.String()))
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
			recordOf(w).failed(route.Classify(src.err))
		}
		s.log.Debug("stream ended", "channel", ch.ID(), "url", route.LogURL(resp.Request.URL.String()),
			"bytes", n, "reason", route.Classify(err))
		return
	}
	s.log.Debug("stream done", "channel", ch.ID(), "url", route.LogURL(resp.Request.URL.String()), "bytes", n)
}

// fail answers an upstream error: 504 for timeouts, else 502, or an error
// clip if f allows it (spec 3.4). Errors from route never contain query
// strings (spec 8). count records the error reason in the metrics; an
// error after the client went away is not an upstream error and counts
// nothing.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, g *generation, ch config.Channel, f fetch, err error, count func(reason string)) {
	if r.Context().Err() != nil {
		s.log.Debug("client gone", "channel", ch.ID())
		return
	}
	reason := route.Classify(err)
	count(string(reason))
	recordOf(w).failed(reason)
	code := route.ErrorStatus(err)
	clip := f.clip(err)
	s.log.Warn("upstream request failed", withClip([]any{
		"channel", ch.ID(), "route", ch.Route(),
		"reason", reason, "status", code, "error", err,
	}, clip)...)
	s.answerFailure(w, r, g, code, clip)
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
