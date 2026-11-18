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
	// ShutdownTimeout bounds the graceful shutdown; 0 means
	// DefaultShutdownTimeout.
	ShutdownTimeout time.Duration

	// Direct is the network of the direct route; nil means route.Host().
	// Tests inject a recording network (spec 9).
	Direct route.Network
	// WrapTunnel, if set, wraps the network of every tunnel; tests record
	// its use.
	WrapTunnel func(name string, n route.Network) route.Network
	// TunnelClock is the tunnels' clock (idle close, start timeouts); nil
	// means the system clock. Tests inject a manual one.
	TunnelClock tunnel.Clock
}

// Run loads the tunnels of the configuration (none is started before
// its first request; each closes after idle_timeout without requests),
// builds the routes and serves HTTP on the listener until ctx ends. Then
// it shuts down gracefully: it stops accepting, waits up to the shutdown
// timeout for in-flight requests, closes the remaining connections, and
// drains and closes the tunnels within the same timeout. It returns
// startup and serve errors; a shutdown after ctx ended returns nil.
func Run(ctx context.Context, opts RunOptions) (err error) {
	if opts.Listener == nil {
		return errors.New("server: no listener")
	}
	defer func() {
		if err != nil {
			_ = opts.Listener.Close()
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

	mgr, err := tunnel.Load(cfg.Tunnels(), tunnel.Options{
		ConnectTimeout: cfg.Upstream().ConnectTimeout,
		IdleTimeout:    cfg.IdleTimeout(),
		Logger:         log,
		Clock:          opts.TunnelClock,
	})
	if err != nil {
		return err
	}
	defer func() { _ = mgr.Close() }()

	nets := map[string]route.Network{}
	activity := map[string]route.Activity{}
	for _, t := range mgr.Tunnels() {
		var n route.Network = t
		if opts.WrapTunnel != nil {
			n = opts.WrapTunnel(t.Name(), n)
		}
		nets[t.Name()] = n
		activity[t.Name()] = t
	}
	direct := opts.Direct
	if direct == nil {
		direct = route.Host()
	}
	routes, err := route.NewSet(direct, nets, route.Options{Upstream: cfg.Upstream(), AllowPrivate: cfg.AllowPrivate(), Activity: activity})
	if err != nil {
		return err
	}
	// A stopped tunnel (idle close, drain) takes the route's pooled
	// connections with it.
	for _, t := range mgr.Tunnels() {
		if rt, ok := routes.Route(t.Name()); ok {
			t.OnStop(rt.CloseIdleConnections)
		}
	}
	// Runs before the tunnels close, after all handlers returned.
	defer routes.CloseIdleConnections()

	// The tunnel gauges read the tunnels at scrape time.
	met := metrics.New(metrics.Options{Tunnels: func() []metrics.Tunnel {
		ts := mgr.Tunnels()
		out := make([]metrics.Tunnel, len(ts))
		for i, t := range ts {
			out[i] = t
		}
		return out
	}})
	srv, err := New(Options{Config: cfg, Routes: routes, Signer: opts.Signer, Logger: log, Version: opts.Version, Metrics: met})
	if err != nil {
		return err
	}
	hs := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	served := make(chan error, 1)
	go func() { served <- hs.Serve(opts.Listener) }()
	log.Info("serving", "addr", opts.Listener.Addr().String(), "channels", len(cfg.Channels()), "tunnels", len(cfg.Tunnels()))

	select {
	case err := <-served:
		// Serve failed before any shutdown; its listener is closed.
		srv.drain()
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down", "timeout", timeout.String())
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
	_ = mgr.Shutdown(time.Until(deadline))
	log.Info("stopped")
	return nil
}
