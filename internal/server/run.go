// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/epg"
	"github.com/KingOfKalk/wg-stream-proxy/internal/hls"
	"github.com/KingOfKalk/wg-stream-proxy/internal/metrics"
	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel"
)

const (
	// DefaultShutdownTimeout bounds the graceful shutdown: in-flight
	// requests get this long before their connections are closed (spec
	// 2.1, drain on shutdown).
	DefaultShutdownTimeout = 30 * time.Second
	// tunnelDrainTimeout bounds the drain of a tunnel a reload changed or
	// removed (spec 2.1), and of a cool-down (spec 7.1).
	tunnelDrainTimeout = 30 * time.Second
	// readHeaderTimeout bounds reading a player's request headers. There
	// is no write timeout: streams have no time limit (spec 9).
	readHeaderTimeout = 10 * time.Second
	// idleTimeout closes idle keep-alive connections of players.
	idleTimeout = 2 * time.Minute
)

// RunOptions configure Run.
type RunOptions struct {
	// Config is the validated configuration. Required.
	Config *config.Config
	// Signer signs and verifies resource URLs. Required.
	Signer *hls.Signer
	// Logger receives all log output; nil discards it.
	Logger *slog.Logger
	// Version is reported by /healthz.
	Version string
	// Listener accepts player connections, normally on Config.Listen().
	// Required. Run closes it.
	Listener net.Listener
	// DebugListener, if set, serves the debug endpoints (spec 7.1),
	// normally on Config.DebugListen(). nil: no debug endpoints. Run closes it.
	DebugListener net.Listener
	// ShutdownTimeout bounds the graceful shutdown; 0 means
	// DefaultShutdownTimeout.
	ShutdownTimeout time.Duration
	// Reloader, if set, reloads the configuration while Run serves
	// (spec 5.6).
	Reloader *Reloader
	// LogLevel, if set, is the level of Logger; a reload sets it to the
	// new log_level.
	LogLevel *slog.LevelVar

	// Direct is the network of the direct route; nil means route.Host().
	// Tests inject a recording network (spec 9).
	Direct route.Network
	// WrapTunnel, if set, wraps the network of every tunnel; tests record
	// its use.
	WrapTunnel func(name string, n route.Network) route.Network
	// TunnelClock is the tunnels' clock (idle close, start timeouts); nil
	// means the system clock. Tests inject a manual one.
	TunnelClock tunnel.Clock
	// EPGClock schedules the EPG refreshes; nil means the system clock.
	EPGClock epg.Clock
}

// Run loads the tunnels of the configuration (none is started before
// its first request; each closes after idle_timeout without requests),
// builds the routes and serves HTTP on the listener until ctx ends. Then
// it shuts down gracefully: it stops accepting, waits up to the shutdown
// timeout for in-flight requests, closes the remaining connections, and
// drains and closes the tunnels within the same timeout. It returns
// startup and serve errors; a shutdown after ctx ended returns nil.
//
// While serving, opts.Reloader reloads the configuration (spec 5.6).
func Run(ctx context.Context, opts RunOptions) (err error) {
	if opts.Listener == nil {
		if opts.DebugListener != nil {
			_ = opts.DebugListener.Close()
		}
		return errors.New("server: no listener")
	}
	defer func() {
		if err != nil {
			_ = opts.Listener.Close()
			if opts.DebugListener != nil {
				_ = opts.DebugListener.Close()
			}
		}
	}()
	switch {
	case opts.Config == nil:
		return errors.New("server: no config")
	case opts.Signer == nil:
		return errors.New("server: no signer")
	}
	cfg := opts.Config
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	timeout := opts.ShutdownTimeout
	if timeout <= 0 {
		timeout = DefaultShutdownTimeout
	}

	// One registry for the process: reloads keep it. The tunnel gauges
	// and EPG metrics read the current state at scrape time (mgr and srv
	// are set before the first scrape); the manager reports every tunnel
	// start to it.
	var (
		mgr *tunnel.Manager
		srv *Server
	)
	met := metrics.New(metrics.Options{
		Tunnels: func() []metrics.Tunnel {
			ts := mgr.Tunnels()
			out := make([]metrics.Tunnel, len(ts))
			for i, t := range ts {
				out[i] = t
			}
			return out
		},
		EPG: func() []metrics.EPGSource { return srv.epgStatus() },
	})
	mgr, err = tunnel.Load(cfg.Tunnels(), tunnel.Options{
		ConnectTimeout: cfg.Upstream().ConnectTimeout,
		IdleTimeout:    cfg.IdleTimeout(),
		Logger:         log,
		Clock:          opts.TunnelClock,
		OnStart:        met.ObserveTunnelStart,
	})
	if err != nil {
		return err
	}
	defer func() { _ = mgr.Close() }()

	st := &runState{opts: opts, log: log, mgr: mgr, direct: opts.Direct}
	if st.direct == nil {
		st.direct = route.Host()
	}
	routes, _, err := st.buildRoutes(nil, cfg)
	if err != nil {
		return err
	}
	srv, err = New(Options{
		Config: cfg, Routes: routes, Signer: opts.Signer, Logger: log, Version: opts.Version, Metrics: met,
		EPGClock: opts.EPGClock,
	})
	if err != nil {
		return err
	}
	st.srv = srv
	// Runs before the tunnels close, after all handlers returned.
	defer st.closeIdleConnections()
	// The EPG refreshes (spec 7.1); stopped before the tunnels drain, so
	// a fetch in progress does not hold a tunnel up.
	stopEPG := startEPG(srv.epg)
	defer stopEPG()

	hs := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	if opts.Reloader != nil {
		// Before serving: a served request means reloads work.
		opts.Reloader.attach(st.reload)
	}
	served := make(chan error, 1)
	go func() { served <- hs.Serve(opts.Listener) }()
	log.Info("serving", "addr", opts.Listener.Addr().String(), "channels", len(cfg.Channels()), "tunnels", len(cfg.Tunnels()))
	var dbg *debugServer
	if opts.DebugListener != nil {
		dbg = startDebug(opts.DebugListener, &debugHandler{srv: srv, mgr: mgr, drainTimeout: tunnelDrainTimeout}, log)
		// Runs before the deferred mgr.Close: waits for the debug handlers;
		// a cool-down ends within its drain timeout.
		defer dbg.stop()
	}

	select {
	case err := <-served:
		// Serve failed before any shutdown; its listener is closed.
		if opts.Reloader != nil {
			opts.Reloader.attach(nil)
		}
		srv.drain()
		return err
	case <-ctx.Done():
	}

	// No reload runs during the shutdown; one in progress finishes first.
	if opts.Reloader != nil {
		opts.Reloader.attach(nil)
	}
	log.Info("shutting down", "timeout", timeout.String())
	if dbg != nil {
		// Debug requests (profiles) end at once; a cool-down in progress
		// ends with the tunnels.
		dbg.close()
	}
	deadline := time.Now().Add(timeout)
	sctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if err := hs.Shutdown(sctx); err != nil {
		log.Warn("shutdown timeout: closing open connections", "error", err)
		_ = hs.Close()
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		log.Warn("serve", "error", err)
	}
	srv.drain()
	// The tunnels drain within the same deadline, then close (spec 2.1).
	stopEPG()
	_ = mgr.Shutdown(time.Until(deadline))
	log.Info("stopped")
	return nil
}
