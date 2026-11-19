// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/probe"
	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel"
)

// headerList is the repeatable --header flag: Name=Value pairs.
type headerList struct{ h http.Header }

func (l *headerList) String() string { return "" }

// Set adds one Name=Value pair; a later value of a name replaces an
// earlier one. Like in a channel file (spec 5.4), an empty value drops
// the header, except for User-Agent, where it sends none.
func (l *headerList) Set(s string) error {
	name, value, ok := strings.Cut(s, "=")
	switch {
	case !ok:
		return fmt.Errorf("want Name=Value")
	case !config.ValidHeaderName(name):
		return fmt.Errorf("invalid header name %q", name)
	case strings.ContainsAny(value, "\r\n\x00"):
		return fmt.Errorf("invalid value for header %q", name)
	}
	if l.h == nil {
		l.h = http.Header{}
	}
	name = http.CanonicalHeaderKey(name)
	if value == "" && name != "User-Agent" {
		delete(l.h, name)
		return nil
	}
	l.h[name] = []string{value}
	return nil
}

// probeFlags are the flags of probe.
type probeFlags struct {
	fset    *flag.FlagSet
	config  *config.Flags
	route   string
	headers headerList
	json    bool
}

func newProbeFlags() *probeFlags {
	fs, cf := configFlagSet("probe")
	f := &probeFlags{fset: fs, config: cf}
	fs.StringVar(&f.route, "route", config.RouteDirect, "route to test: direct or a tunnel name")
	fs.Var(&f.headers, "header", "upstream request header Name=Value, may be repeated (like upstream.headers of a channel)")
	fs.BoolVar(&f.json, "json", false, "print the result as JSON")
	return f
}

// parse parses args; the URL may come before, between or after the
// flags. It returns the URL and the exit code and ok=false if the
// command must stop.
func (f *probeFlags) parse(c *cli, args []string) (target string, code int, ok bool) {
	f.fset.SetOutput(c.stderr)
	var rest []string
	for {
		if err := f.fset.Parse(args); err != nil {
			return "", c.parseFailed("probe", err), false
		}
		if f.fset.NArg() == 0 {
			break
		}
		rest = append(rest, f.fset.Arg(0))
		args = f.fset.Args()[1:]
	}
	if len(rest) != 1 {
		_, _ = fmt.Fprintln(c.stderr, "wg-stream-proxy: probe: want exactly one URL")
		return "", 2, false
	}
	u, err := url.Parse(rest[0])
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		// The argument is not echoed: it may carry a token.
		_, _ = fmt.Fprintln(c.stderr, "wg-stream-proxy: probe: the URL must be absolute with scheme http or https")
		return "", 2, false
	}
	return rest[0], 0, true
}

// probeCmd tests a URL through a route (spec 7.2): it fetches it like the
// proxy would and reports the detected format and the timing. Exit code 0
// if the proxy would serve the URL, 1 if not, 2 for usage errors.
func (c *cli) probeCmd(args []string) int {
	f := newProbeFlags()
	target, code, ok := f.parse(c, args)
	if !ok {
		return code
	}
	opts, err := config.Resolve(*f.config, c.env)
	if err != nil {
		c.printErrors(err)
		return 1
	}
	cfg, err := config.Load(opts)
	if err != nil {
		c.printErrors(err)
		return 1
	}
	if !f.routeExists(cfg) {
		names := []string{config.RouteDirect}
		for _, t := range cfg.Tunnels() {
			names = append(names, t.Name)
		}
		_, _ = fmt.Fprintf(c.stderr, "wg-stream-proxy: probe: unknown route %q (want one of: %s)\n", f.route, strings.Join(names, ", "))
		return 2
	}

	// Only the tunnel of the route is loaded; it starts with the first
	// request and stops when the command ends.
	log := newLogger(c.stderr, cfg.LogFormat(), probeLogLevel(cfg))
	nets := map[string]route.Network{}
	activity := map[string]route.Activity{}
	if f.route != config.RouteDirect {
		mgr, err := tunnel.Load(f.tunnelFiles(cfg), tunnel.Options{
			ConnectTimeout: cfg.Upstream().ConnectTimeout,
			IdleTimeout:    cfg.IdleTimeout(),
			Logger:         log,
		})
		if err != nil {
			c.printErrors(err)
			return 1
		}
		defer func() { _ = mgr.Close() }()
		t, _ := mgr.Tunnel(f.route)
		nets[f.route], activity[f.route] = t, t
	}
	allow := cfg.AllowPrivate()
	for name, prefixes := range c.allowPrivate {
		allow[name] = append(allow[name], prefixes...)
	}
	set, err := route.NewSet(route.Host(), nets, route.Options{Upstream: cfg.Upstream(), AllowPrivate: allow, Activity: activity})
	if err != nil {
		_, _ = fmt.Fprintf(c.stderr, "wg-stream-proxy: probe: %v\n", err)
		return 1
	}
	defer set.CloseIdleConnections()
	rt, _ := set.Route(f.route)

	rep := probe.Run(c.context(), target, probe.Options{Route: rt, Header: f.headers.h, MaxPlaylistSize: cfg.Upstream().MaxPlaylistSize})
	write := rep.WriteText
	if f.json {
		write = rep.WriteJSON
	}
	if err := write(c.stdout); err != nil {
		_, _ = fmt.Fprintf(c.stderr, "wg-stream-proxy: probe: %v\n", err)
		return 1
	}
	if !rep.OK {
		return 1
	}
	return 0
}

// routeExists reports whether the route flag names direct or a tunnel.
func (f *probeFlags) routeExists(cfg *config.Config) bool {
	return f.route == config.RouteDirect || len(f.tunnelFiles(cfg)) == 1
}

// tunnelFiles returns the tunnel file of the route flag, if it is a
// tunnel.
func (f *probeFlags) tunnelFiles(cfg *config.Config) []config.Tunnel {
	for _, t := range cfg.Tunnels() {
		if t.Name == f.route {
			return []config.Tunnel{t}
		}
	}
	return nil
}

// probeLogLevel is the log level of the probe: warnings and errors
// (a tunnel that fails to start), debug if the config asks for it. The
// start of the tunnel shows in the timing, not as a log line.
func probeLogLevel(cfg *config.Config) slog.Level {
	if cfg.LogLevel() <= slog.LevelDebug {
		return slog.LevelDebug
	}
	return slog.LevelWarn
}
