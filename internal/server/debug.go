// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/pprof"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel"
)

// Debug endpoints (spec 7.1): served only on the separate debug listener
// ([debug] listen), never on the main listener. They have no
// authentication (spec 8).

// recentRequestsSize is how many finished requests /debug/requests keeps.
const recentRequestsSize = 256

// recentRequests is a fixed-size ring buffer of the latest finished /c/
// and /r/ requests. add copies one finishedRequest into a slot under a
// short lock; it never allocates or grows.
type recentRequests struct {
	mu   sync.Mutex
	buf  []finishedRequest
	next int
	full bool
}

func newRecentRequests(n int) *recentRequests {
	return &recentRequests{buf: make([]finishedRequest, n)}
}

// add records fr, replacing the oldest entry when full.
func (r *recentRequests) add(fr finishedRequest) {
	r.mu.Lock()
	r.buf[r.next] = fr
	r.next++
	if r.next == len(r.buf) {
		r.next = 0
		r.full = true
	}
	r.mu.Unlock()
}

// list returns a copy of the entries, newest first; never nil.
func (r *recentRequests) list() []finishedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.next
	if r.full {
		n = len(r.buf)
	}
	out := make([]finishedRequest, 0, n)
	for i := range n {
		out = append(out, r.buf[(r.next-1-i+len(r.buf))%len(r.buf)])
	}
	return out
}

// recentRequest is the JSON form of a finished request: the fields of the
// debug line (spec 8), durations in milliseconds. URL is reduced by
// route.LogURL; nothing else of the upstream URL is kept.
type recentRequest struct {
	ID              uint64   `json:"id"`
	Time            string   `json:"time"`
	Method          string   `json:"method"`
	Channel         string   `json:"channel"`
	Route           string   `json:"route"`
	Kind            string   `json:"kind"`
	Status          int      `json:"status"`
	Bytes           int64    `json:"bytes"`
	ClientGone      bool     `json:"client_gone"`
	UpstreamStatus  int      `json:"upstream_status,omitempty"`
	Reason          string   `json:"reason,omitempty"`
	TunnelStartMS   *float64 `json:"tunnel_start_ms,omitempty"`
	DNSMS           float64  `json:"dns_ms"`
	ConnectMS       float64  `json:"connect_ms"`
	ConnectAttempts int      `json:"connect_attempts"`
	TLSMS           float64  `json:"tls_ms"`
	TTFBMS          float64  `json:"ttfb_ms"`
	TotalMS         float64  `json:"total_ms"`
	ConnReused      bool     `json:"conn_reused"`
	Redirects       int      `json:"redirects"`
	URL             string   `json:"url"`
}

// recentRequestJSON converts fr for /debug/requests. time is when the
// request started.
func recentRequestJSON(fr finishedRequest) recentRequest {
	p := fr.Phases
	r := recentRequest{
		ID:              fr.ID,
		Time:            fr.Start.Format(time.RFC3339Nano),
		Method:          fr.Method,
		Channel:         fr.Channel,
		Route:           fr.Route,
		Kind:            string(fr.Kind),
		Status:          fr.Status,
		Bytes:           fr.Bytes,
		ClientGone:      fr.ClientGone,
		UpstreamStatus:  fr.UpstreamStatus,
		Reason:          string(fr.Reason),
		DNSMS:           millis(p.DNS),
		ConnectMS:       millis(p.Connect),
		ConnectAttempts: p.ConnectAttempts,
		TLSMS:           millis(p.TLS),
		TTFBMS:          millis(p.FirstByte),
		TotalMS:         millis(fr.Total),
		ConnReused:      p.Reused,
		Redirects:       p.Redirects,
		URL:             p.URL,
	}
	if p.NetworkStarted {
		ms := millis(p.NetworkStart)
		r.TunnelStartMS = &ms
	}
	return r
}

// millis returns d in milliseconds, to the microsecond.
func millis(d time.Duration) float64 {
	return math.Round(float64(d)/float64(time.Microsecond)) / 1000
}

// tunnelStatus is the JSON form of a tunnel in /debug/tunnels. The times
// are omitted while no device runs.
type tunnelStatus struct {
	Name           string       `json:"name"`
	State          tunnel.State `json:"state"`
	StartedAt      time.Time    `json:"started_at,omitzero"`
	LastHandshake  time.Time    `json:"last_handshake,omitzero"`
	ActiveRequests int          `json:"active_requests"`
}

// debugHandler serves the debug endpoints.
type debugHandler struct {
	srv *Server
	mgr *tunnel.Manager
	// drainTimeout bounds the drain of a cool-down.
	drainTimeout time.Duration
}

// newDebugHandler returns the handler of the debug listener. Requests
// whose Host is not an IP address or localhost are rejected (403), which
// stops DNS rebinding: a web page whose own name was rebound to the debug
// address can neither read the endpoints nor trigger a cool-down (its
// requests look same-origin to the browser). Unsafe methods from a
// browser on another origin are rejected as well (403).
func newDebugHandler(d *debugHandler) http.Handler {
	mux := http.NewServeMux()
	// The profiler on this mux only; its package also registers itself
	// on http.DefaultServeMux, which is never served.
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.HandleFunc("GET /debug/requests", d.handleRequests)
	mux.HandleFunc("GET /debug/tunnels", d.handleTunnels)
	mux.HandleFunc("POST /debug/tunnels/{name}/stop", d.handleStop)
	mux.HandleFunc("POST /debug/probe", d.handleProbe)
	return debugHostGuard(http.NewCrossOriginProtection().Handler(mux))
}

// debugHostGuard rejects requests whose Host debugHostAllowed refuses.
func debugHostGuard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !debugHostAllowed(r.Host) {
			http.Error(w, "host not allowed: use an IP address or localhost", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// debugHostAllowed reports whether hostport, a request's Host with or
// without port, names the debug listener by an IP address (IPv6 zones
// included) or as localhost. Any other name could have been rebound.
func debugHostAllowed(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	_, err := netip.ParseAddr(host)
	return err == nil
}

// handleRequests lists the latest finished requests, newest first.
func (d *debugHandler) handleRequests(w http.ResponseWriter, _ *http.Request) {
	var list []finishedRequest
	if d.srv.recent != nil {
		list = d.srv.recent.list()
	}
	out := make([]recentRequest, len(list))
	for i, fr := range list {
		out[i] = recentRequestJSON(fr)
	}
	writeJSON(w, http.StatusOK, struct {
		Requests []recentRequest `json:"requests"`
	}{out})
}

// handleTunnels lists the configured tunnels with their state.
func (d *debugHandler) handleTunnels(w http.ResponseWriter, _ *http.Request) {
	ts := d.mgr.Tunnels()
	out := make([]tunnelStatus, len(ts))
	for i, t := range ts {
		s := t.Snapshot()
		out[i] = tunnelStatus{
			Name: s.Name, State: s.State, StartedAt: s.StartedAt,
			LastHandshake: s.LastHandshake, ActiveRequests: s.ActiveRequests,
		}
	}
	writeJSON(w, http.StatusOK, struct {
		Tunnels []tunnelStatus `json:"tunnels"`
	}{out})
}

// handleStop cools a tunnel down: drain, stop, the next request starts
// it cold. It answers when the tunnel is stopped.
func (d *debugHandler) handleStop(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ran, err := d.mgr.Cooldown(name, d.drainTimeout)
	switch {
	case errors.Is(err, tunnel.ErrNoSuchTunnel):
		http.Error(w, "no such tunnel", http.StatusNotFound)
		return
	case err != nil:
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Name       string `json:"name"`
		WasRunning bool   `json:"was_running"`
	}{name, ran})
}

// writeJSON writes v as a JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// handlerGroup counts running handlers, so a shutdown can wait for them
// after the server stopped accepting.
type handlerGroup struct {
	mu      sync.Mutex
	stopped bool
	wg      sync.WaitGroup
}

// wrap returns h counted in g; after stop it answers 503.
func (g *handlerGroup) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		if g.stopped {
			g.mu.Unlock()
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}
		g.wg.Add(1)
		g.mu.Unlock()
		defer g.wg.Done()
		h.ServeHTTP(w, r)
	})
}

// stop rejects new handlers and waits for the running ones.
func (g *handlerGroup) stop() {
	g.mu.Lock()
	g.stopped = true
	g.mu.Unlock()
	g.wg.Wait()
}

// debugServer serves the debug endpoints on their own listener.
type debugServer struct {
	hs     *http.Server
	served chan struct{}
	group  handlerGroup
	once   sync.Once
}

// startDebug serves the debug endpoints of d on ln until stop.
func startDebug(ln net.Listener, d *debugHandler, log *slog.Logger) *debugServer {
	s := &debugServer{served: make(chan struct{})}
	s.hs = &http.Server{
		Handler:           s.group.wrap(newDebugHandler(d)),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	go func() {
		defer close(s.served)
		if err := s.hs.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			log.Warn("debug endpoints stopped", "error", err)
		}
	}()
	// No authentication: keep it on loopback or the LAN (spec 8).
	log.Info("serving debug endpoints", "addr", ln.Addr().String())
	return s
}

// close stops accepting and closes the open debug connections, which
// ends running profiles. Handlers may still run; stop waits for them.
func (s *debugServer) close() {
	s.once.Do(func() {
		_ = s.hs.Close()
		<-s.served
	})
}

// stop closes the server and waits until its handlers returned.
func (s *debugServer) stop() {
	s.close()
	s.group.stop()
}
