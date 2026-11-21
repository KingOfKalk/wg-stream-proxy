// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/config/wgquick"
	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel"
)

// ErrNotServing: Reload was called while Run does not serve.
var ErrNotServing = errors.New("server: not serving")

// Reloader reloads the configuration of a running server (spec 5.6); the
// SIGHUP handler calls Reload. Pass it to Run in RunOptions.Reloader.
type Reloader struct {
	load func() (*config.Config, error)

	// mu serializes reloads and guards fn.
	mu sync.Mutex
	// fn reloads the running server; nil while Run does not serve.
	fn func(*config.Config, error) error
}

// NewReloader returns a Reloader that reads and validates the
// configuration files with load (config.Load, after the checks of
// settings that need a restart).
func NewReloader(load func() (*config.Config, error)) *Reloader {
	return &Reloader{load: load}
}

// Reload reloads all files and waits for the result: nil if the new
// configuration serves, else why it was rejected; the old one keeps
// serving then. Concurrent calls run one after the other. Before Run
// serves and after it stopped, Reload returns ErrNotServing.
func (r *Reloader) Reload() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fn == nil {
		return ErrNotServing
	}
	cfg, err := r.load()
	return r.fn(cfg, err)
}

// attach sets the reload function of the running server; nil detaches
// it, after a reload in progress has finished.
func (r *Reloader) attach(fn func(*config.Config, error) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fn = fn
}

// runState is the part of Run that reloads change. Reloads are
// serialized by the Reloader; the shutdown starts after the last one.
type runState struct {
	opts   RunOptions
	log    *slog.Logger
	mgr    *tunnel.Manager
	srv    *Server
	direct route.Network
	// fixed are the settings that need a restart, as in effect since the
	// start (spec 5.6).
	fixed fixedSettings
}

// fixedSettings are the settings a reload cannot change (spec 5.6); the
// Reloader's loader checks the signing key.
type fixedSettings struct {
	listen        string
	logFormat     config.LogFormat
	debugListen   string
	watchInterval time.Duration
}

// fixedOf returns the settings of cfg that need a restart.
func fixedOf(cfg *config.Config) fixedSettings {
	return fixedSettings{
		listen:        cfg.Listen(),
		logFormat:     cfg.LogFormat(),
		debugListen:   cfg.DebugListen(),
		watchInterval: cfg.WatchInterval(),
	}
}

// buildRoutes builds the routes of cfg over the manager's current
// tunnels. Every call builds new routes, so a reload starts with empty
// connection pools (spec 5.6).
func (st *runState) buildRoutes(cfg *config.Config) (*route.Set, error) {
	tunnels := st.mgr.Tunnels()
	nets := make(map[string]route.Network, len(tunnels))
	activity := make(map[string]route.Activity, len(tunnels))
	for _, t := range tunnels {
		var n route.Network = t
		if st.opts.WrapTunnel != nil {
			n = st.opts.WrapTunnel(t.Name(), n)
		}
		nets[t.Name()] = n
		activity[t.Name()] = t
	}
	routes, err := route.NewSet(st.direct, nets, route.Options{Upstream: cfg.Upstream(), AllowPrivate: cfg.AllowPrivate(), Activity: activity})
	if err != nil {
		return nil, err
	}
	// A stopped tunnel (idle close, drain) takes the route's pooled
	// connections with it.
	for _, t := range tunnels {
		if rt, ok := routes.Route(t.Name()); ok {
			t.OnStop(rt.CloseIdleConnections)
		}
	}
	return routes, nil
}

// reload swaps in the configuration cfg that the Reloader loaded with
// error err (spec 5.6). The tunnel files are parsed first; any error
// rejects the whole reload and the old generation keeps serving.
func (st *runState) reload(cfg *config.Config, err error) error {
	var tunnels []*wgquick.Tunnel
	if err == nil {
		tunnels, err = tunnel.Parse(cfg.Tunnels(), st.log)
	}
	if err != nil {
		for line := range strings.SplitSeq(err.Error(), "\n") {
			if line != "" {
				st.log.Error("reload: invalid config", "error", line)
			}
		}
		st.log.Error("reload rejected: the old config keeps serving")
		return fmt.Errorf("reload rejected: %w", err)
	}

	// Compared with the values in effect, not with the last loaded config:
	// a reload back to them needs no restart.
	fx := st.fixed
	if cfg.Listen() != fx.listen {
		st.log.Warn("reload: listen changed, restart required; keeping the old address",
			"listen", fx.listen, "configured", cfg.Listen())
	}
	if cfg.LogFormat() != fx.logFormat {
		st.log.Warn("reload: log_format changed, restart required; keeping the old format",
			"log_format", string(fx.logFormat), "configured", string(cfg.LogFormat()))
	}
	if cfg.DebugListen() != fx.debugListen {
		st.log.Warn("reload: debug.listen changed, restart required; keeping the old address",
			"debug_listen", fx.debugListen, "configured", cfg.DebugListen())
	}
	if cfg.WatchInterval() != fx.watchInterval {
		st.log.Warn("reload: reload.watch_interval changed, restart required; keeping the old interval",
			"watch_interval", fx.watchInterval, "configured", cfg.WatchInterval())
	}

	changes, err := st.mgr.Apply(tunnels, tunnelDrainTimeout)
	if err == nil {
		err = st.mgr.SetTimeouts(cfg.Upstream().ConnectTimeout, cfg.IdleTimeout())
	}
	var routes *route.Set
	if err == nil {
		routes, err = st.buildRoutes(cfg)
	}
	if err != nil {
		// Only after a shutdown began, or with a config the loader should
		// have rejected.
		st.log.Error("reload failed", "error", err)
		return fmt.Errorf("reload failed: %w", err)
	}
	// Reloads are serialized: old is the generation this one replaces.
	old := st.srv.active()
	st.srv.swap(cfg, routes)
	old.retire()
	if st.opts.LogLevel != nil {
		st.opts.LogLevel.Set(cfg.LogLevel())
	}
	chAdded, chChanged, chRemoved := diffChannels(old.cfg.Channels(), cfg.Channels())
	// One info line with counts; the channel ids are debug only, and no
	// URLs ever (they may carry tokens).
	st.log.Info("reloaded", "channels", len(cfg.Channels()), "tunnels", len(cfg.Tunnels()),
		"channels_added", len(chAdded), "channels_changed", len(chChanged), "channels_removed", len(chRemoved),
		"tunnels_added", changes.Added, "tunnels_changed", changes.Changed, "tunnels_removed", changes.Removed)
	if len(chAdded)+len(chChanged)+len(chRemoved) > 0 {
		st.log.Debug("reload: channel changes", "added", chAdded, "changed", chChanged, "removed", chRemoved)
	}
	return nil
}

// closeIdleConnections closes the idle upstream connections of the
// active routes. Called after all requests and fetches ended: the routes
// reloads replaced were closed by their last user (generation.done) or
// by the reload itself (generation.retire).
func (st *runState) closeIdleConnections() {
	st.srv.active().routes.CloseIdleConnections()
}

// diffChannels compares two generations of channels by id and returns the
// ids of the added, changed (any difference of the merged model, see
// config.Channel.Equal) and removed channels, each sorted and never nil.
func diffChannels(old, next []config.Channel) (added, changed, removed []string) {
	added, changed, removed = []string{}, []string{}, []string{}
	prev := make(map[string]config.Channel, len(old))
	for _, ch := range old {
		prev[ch.ID()] = ch
	}
	seen := make(map[string]bool, len(next))
	for _, ch := range next {
		seen[ch.ID()] = true
		switch p, ok := prev[ch.ID()]; {
		case !ok:
			added = append(added, ch.ID())
		case !p.Equal(ch):
			changed = append(changed, ch.ID())
		}
	}
	for _, ch := range old {
		if !seen[ch.ID()] {
			removed = append(removed, ch.ID())
		}
	}
	slices.Sort(added)
	slices.Sort(changed)
	slices.Sort(removed)
	return added, changed, removed
}
