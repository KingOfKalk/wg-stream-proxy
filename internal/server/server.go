// SPDX-License-Identifier: AGPL-3.0-or-later

// Package server is the HTTP side of wg-stream-proxy (spec 7.1): the
// channel list, channel entries under /c/, signed resources under /r/ and
// /p/, the time for DASH players under /time, error clip segments under
// /clip/, logos under /logos/, /healthz and /metrics, plus the debug
// endpoints on their own listener ([debug] listen, off by default).
// Upstream requests go through the channel's route (internal/route);
// playlists are rewritten with internal/hls, DASH manifests with
// internal/dash.
package server

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/clips"
	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/epg"
	"github.com/KingOfKalk/wg-stream-proxy/internal/hls"
	"github.com/KingOfKalk/wg-stream-proxy/internal/metrics"
	"github.com/KingOfKalk/wg-stream-proxy/internal/playlist"
	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
)

// SourceURL is the source code location reported by /healthz and the
// version command (AGPL section 13).
const SourceURL = "https://github.com/KingOfKalk/wg-stream-proxy"

// Options configure a Server.
type Options struct {
	// Config is the active configuration. Required.
	Config *config.Config
	// Routes holds the direct route and one route per tunnel. Required.
	Routes *route.Set
	// Signer signs and verifies resource URLs. Required.
	Signer *hls.Signer
	// Logger receives upstream errors and request events; nil discards.
	Logger *slog.Logger
	// Version is reported by /healthz.
	Version string
	// Metrics receives the request metrics and is served on /metrics;
	// nil means a new registry without tunnel metrics.
	Metrics *metrics.Metrics
	// RecentRequests is how many finished /c/, /r/ and /p/ requests are kept
	// for /debug/requests; 0 keeps none (no debug listener).
	RecentRequests int
	// EPGClock schedules the EPG refreshes; nil means the system clock.
	EPGClock epg.Clock
}

// Server serves the player-facing HTTP interface. It is an http.Handler.
type Server struct {
	// gen is the active generation; a reload swaps it (spec 5.6).
	gen     atomic.Pointer[generation]
	signer  *hls.Signer
	log     *slog.Logger
	version string
	mux     *http.ServeMux
	metrics *metrics.Metrics
	// requestIDs numbers the /c/, /r/ and /p/ requests.
	requestIDs atomic.Uint64
	// recent keeps the latest finished requests for /debug/requests;
	// nil while the debug listener is off.
	recent *recentRequests
	// epg fetches, filters and merges the EPG sources (spec 7.1); Run
	// runs it.
	epg *epg.Guide

	// mu guards closed and the Add calls of handlers, so drain does not
	// race with a request that is just starting.
	mu       sync.Mutex
	closed   bool
	handlers sync.WaitGroup
}

// New returns a Server.
func New(opts Options) (*Server, error) {
	switch {
	case opts.Config == nil:
		return nil, errors.New("server: no config")
	case opts.Routes == nil:
		return nil, errors.New("server: no routes")
	case opts.Signer == nil:
		return nil, errors.New("server: no signer")
	}
	s := &Server{
		signer:  opts.Signer,
		log:     opts.Logger,
		version: opts.Version,
		mux:     http.NewServeMux(),
		metrics: opts.Metrics,
	}
	if s.metrics == nil {
		s.metrics = metrics.New(metrics.Options{})
	}
	s.metrics.DeclareErrorClips(clips.Names()...)
	if opts.RecentRequests > 0 {
		s.recent = newRecentRequests(opts.RecentRequests)
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	s.epg = epg.NewGuide(epg.GuideOptions{Fetch: s.fetchEPG, Clock: opts.EPGClock, Logger: s.log})
	s.swap(opts.Config, opts.Routes)
	// GET patterns match HEAD too.
	s.mux.HandleFunc("GET /playlist.m3u8", s.handlePlaylist)
	s.mux.HandleFunc("GET "+epgPath, s.handleEPG)
	s.mux.HandleFunc("GET "+epgPath+".gz", s.handleEPG)
	s.mux.HandleFunc("GET /c/{id}", s.handleChannel)
	s.mux.HandleFunc("GET /r/", s.handleResource)
	s.mux.HandleFunc("GET /p/", s.handlePrefix)
	s.mux.HandleFunc("GET "+timePath, s.handleTime)
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /logos/{file}", s.handleLogo)
	s.mux.HandleFunc("GET /clip/{file}", s.handleClip)
	// No authentication, like the rest: LAN use (spec 8).
	s.mux.Handle("GET /metrics", s.metrics.Handler())
	return s, nil
}

// generation is one configuration with the routes built for it. It is
// immutable: a request uses the generation it started with to its end,
// while a reload swaps in the next one for new requests (spec 5.6).
type generation struct {
	cfg    *config.Config
	routes *route.Set
}

// swap makes cfg and routes the active generation and applies the EPG
// settings of cfg.
func (s *Server) swap(cfg *config.Config, routes *route.Set) {
	s.gen.Store(&generation{cfg: cfg, routes: routes})
	// After the store: the guide fetches through the active routes.
	s.epg.Update(cfg)
}

// active returns the active generation.
func (s *Server) active() *generation { return s.gen.Load() }

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	s.handlers.Add(1)
	s.mu.Unlock()
	defer s.handlers.Done()
	s.mux.ServeHTTP(w, r)
}

// drain rejects new requests and waits until running handlers return.
func (s *Server) drain() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.handlers.Wait()
}

// handleHealth answers 200 with version and source URL (spec 7.1).
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "wg-stream-proxy "+s.version+"\nsource: "+SourceURL+"\n")
}

// handlePlaylist serves the channel list (spec 6).
func (s *Server) handlePlaylist(w http.ResponseWriter, r *http.Request) {
	cfg := s.active().cfg
	base := baseURL(cfg.PublicBaseURL(), r)
	body := playlist.Render(cfg.Channels(), playlist.Options{
		BaseURL: base,
		EPGURL:  epgLink(cfg, base),
		Group:   r.URL.Query().Get("group"),
	})
	w.Header().Set("Content-Type", playlist.ContentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

// baseURL returns the base of absolute URLs in responses (spec 7.1):
// public_base_url without trailing slash; else the scheme from
// X-Forwarded-Proto and the host from X-Forwarded-Host, each falling back
// to the request (TLS or not, Host header). Of a forwarded header only
// the first comma-separated value counts; invalid values are ignored. A
// request without a usable Host gets the local address it came in on.
func baseURL(public string, r *http.Request) string {
	if public != "" {
		return strings.TrimRight(public, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := strings.ToLower(firstValue(r.Header.Get("X-Forwarded-Proto"))); p == "http" || p == "https" {
		scheme = p
	}
	host := r.Host
	if h := firstValue(r.Header.Get("X-Forwarded-Host")); validHost(h) {
		host = h
	}
	if !validHost(host) {
		host = "localhost"
		if a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
			host = a.String()
		}
	}
	return scheme + "://" + host
}

// firstValue returns the first element of a comma-separated header value,
// trimmed.
func firstValue(v string) string {
	v, _, _ = strings.Cut(v, ",")
	return strings.TrimSpace(v)
}

// validHost reports whether h looks like host[:port] with a DNS name, an
// IPv4 address or a bracketed IPv6 address. It keeps characters out of
// the URLs written into playlists that could break them.
func validHost(h string) bool {
	if h == "" || len(h) > 255 {
		return false
	}
	for i := 0; i < len(h); i++ {
		if !isHostByte(h[i]) {
			return false
		}
	}
	return true
}

func isHostByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '.' || c == '-' || c == '_' || c == ':' || c == '[' || c == ']'
}

// timePath answers the time for the UTCTiming of rewritten DASH manifests
// (spec 3.6, 7.1).
const timePath = "/time"

// handleTime answers the current UTC time in ISO 8601 with milliseconds
// (urn:mpeg:dash:utc:http-iso:2014), so DASH players never ask an
// upstream time server directly.
func (s *Server) handleTime(w http.ResponseWriter, r *http.Request) {
	body := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.WriteString(w, body)
}
