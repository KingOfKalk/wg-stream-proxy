// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"encoding/json"
	"errors"
	"log/slog"
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
