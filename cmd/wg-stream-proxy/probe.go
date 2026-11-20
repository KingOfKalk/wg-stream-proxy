// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/probe"
	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel"
)

// probeCallTimeout bounds the request to POST /debug/probe: the server
// stops a probe after 60s.
const probeCallTimeout = 75 * time.Second

// headerList is the repeatable --header flag: Name=Value pairs.
type headerList struct{ h http.Header }

func (l *headerList) String() string { return "" }

// Set adds one Name=Value pair (probe.SetHeader has the rules).
func (l *headerList) Set(s string) error {
	name, value, ok := strings.Cut(s, "=")
	if !ok {
		return errors.New("want Name=Value")
	}
	if l.h == nil {
		l.h = http.Header{}
	}
	return probe.SetHeader(l.h, name, value)
}

// probeFlags are the flags of probe.
type probeFlags struct {
	fset       *flag.FlagSet
	config     *config.Flags
	route      string
	headers    headerList
	json       bool
	standalone bool
}

func newProbeFlags() *probeFlags {
	fs, cf := configFlagSet("probe")
	f := &probeFlags{fset: fs, config: cf}
	fs.StringVar(&f.route, "route", config.RouteDirect, "route to test: direct or a tunnel name")
	fs.Var(&f.headers, "header", "upstream request header Name=Value, may be repeated (like upstream.headers of a channel)")
	fs.BoolVar(&f.json, "json", false, "print the result as JSON")
	fs.BoolVar(&f.standalone, "standalone", false, "do not use the running proxy: make the requests here and start the tunnel of the route")
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
	if err := probe.CheckURL(rest[0]); err != nil {
		// The argument is not echoed: it may carry a token.
		_, _ = fmt.Fprintln(c.stderr, "wg-stream-proxy: probe: "+err.Error())
		return "", 2, false
	}
	return rest[0], 0, true
}

// probeCmd tests a URL through a route (spec 7.2): it fetches it like the
// proxy would and reports the detected format and the timing. If the
// proxy runs and its debug listener answers, the running proxy makes the
// requests (POST /debug/probe); else the command does it itself. Exit
// code 0 if the proxy would serve the URL, 1 if not, 2 for usage errors.
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
	if !f.standalone {
		// The mode needs only the debug listener address, read without
		// the tunnel and channel files (spec 7.2): the running proxy
		// answers with its own loaded config, so a broken file of the
		// newer version does not send the command standalone.
		_, debugListen, err := config.LoadAddresses(opts)
		if err != nil {
			c.printErrors(err)
			return 1
		}
		if debugListen == "" {
			c.standaloneNote(f, "[debug] listen is not set")
		} else if code, done := c.probeViaProxy(debugListen, f, target); done {
			return code
		}
	}
	cfg, err := config.Load(opts)
	if err != nil {
		c.printErrors(err)
		return 1
	}
	return c.probeStandalone(cfg, f, target)
}

// probeViaProxy sends the probe to the debug listener of the running
// proxy. It returns done=false if there is none to use: nothing listens
// there (a note on stderr says so). An answer with an error is
// reported, not worked around.
func (c *cli) probeViaProxy(debugListen string, f *probeFlags, target string) (code int, done bool) {
	base, err := localURL(debugListen, "")
	if err != nil {
		return c.probeFail(1, err.Error()), true
	}
	req := struct {
		Route   string            `json:"route"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers,omitempty"`
	}{Route: f.route, URL: target}
	if len(f.headers.h) > 0 {
		req.Headers = make(map[string]string, len(f.headers.h))
		for k, v := range f.headers.h {
			req.Headers[k] = v[0]
		}
	}
	var rep probe.Report
	err = c.debugCall(http.MethodPost, base+"/debug/probe", probeCallTimeout, req, &rep)
	var se *statusError
	var de *dialError
	switch {
	case errors.As(err, &de):
		c.standaloneNote(f, "nothing listens on [debug] listen "+debugListen)
		return 0, false
	case errors.As(err, &se):
		return c.probeStatusFailed(se), true
	case err != nil:
		// The URL is in the request body: it is not in this error.
		return c.probeFail(1, "the running proxy: "+err.Error()), true
	}
	return c.probeReport(&rep, f), true
}

// probeStatusFailed reports an error answer of the debug listener.
func (c *cli) probeStatusFailed(se *statusError) int {
	switch {
	case se.code == http.StatusNotFound && se.msg == "":
		return c.probeFail(1, "the running proxy does not know POST /debug/probe (older version?); use --standalone")
	case se.msg == "":
		return c.probeFail(1, fmt.Sprintf("the running proxy answered status %d", se.code))
	case se.code == http.StatusBadRequest || se.code == http.StatusNotFound:
		return c.probeFail(2, "the running proxy: "+se.msg)
	}
	return c.probeFail(1, "the running proxy: "+se.msg)
}

// standaloneNote tells on stderr that the probe runs standalone and why.
func (c *cli) standaloneNote(f *probeFlags, why string) {
	msg := "wg-stream-proxy: probe: " + why + ", running standalone"
	if f.route != config.RouteDirect {
		msg += fmt.Sprintf(": starting its own tunnel %s; if a proxy with the same tunnel key runs, its streams on that tunnel may stall until the next handshake", f.route)
	}
	_, _ = fmt.Fprintln(c.stderr, msg)
}

func (c *cli) probeFail(code int, msg string) int {
	_, _ = fmt.Fprintln(c.stderr, "wg-stream-proxy: probe: "+msg)
	return code
}

// probeReport prints rep and returns the exit code.
func (c *cli) probeReport(rep *probe.Report, f *probeFlags) int {
	write := rep.WriteText
	if f.json {
		write = rep.WriteJSON
	}
	if err := write(c.stdout); err != nil {
		return c.probeFail(1, err.Error())
	}
	if !rep.OK {
		return 1
	}
	return 0
}

// probeStandalone makes the probe in this process. Only the tunnel of the
// route is loaded; it starts with the first request and stops when the
// command ends.
func (c *cli) probeStandalone(cfg *config.Config, f *probeFlags, target string) int {
	_, isTunnel := f.tunnelFile(cfg)
	if f.route != config.RouteDirect && !isTunnel {
		names := []string{config.RouteDirect}
		for _, t := range cfg.Tunnels() {
			names = append(names, t.Name)
		}
		return c.probeFail(2, fmt.Sprintf("unknown route %q (want one of: %s)", f.route, strings.Join(names, ", ")))
	}

	rt, closeRoute, err := c.standaloneRoute(cfg, f.route)
	if err != nil {
		var cerr configError
		if errors.As(err, &cerr) {
			c.printErrors(cerr.error)
			return 1
		}
		return c.probeFail(1, err.Error())
	}
	defer closeRoute()

	rep := probe.Run(c.context(), target, probe.Options{Route: rt, Header: f.headers.h, MaxPlaylistSize: cfg.Upstream().MaxPlaylistSize})
	rep.Mode = probe.ModeStandalone
	return c.probeReport(rep, f)
}

// tunnelFile returns the tunnel file of the route flag, if it is a
// tunnel.
func (f *probeFlags) tunnelFile(cfg *config.Config) (config.Tunnel, bool) {
	for _, t := range cfg.Tunnels() {
		if t.Name == f.route {
			return t, true
		}
	}
	return config.Tunnel{}, false
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

// configError is an invalid tunnel file found by standaloneRoute.
type configError struct{ error }

// standaloneRoute returns the route name (direct or a tunnel of cfg) in
// this process, for commands that run without the proxy (probe
// --standalone, epg). Only the tunnel of the route is loaded; it starts
// with the first request. close stops it and closes the connections.
// An invalid tunnel file gives a configError.
func (c *cli) standaloneRoute(cfg *config.Config, name string) (rt *route.Route, closeRoute func(), err error) {
	log := newLogger(c.stderr, cfg.LogFormat(), probeLogLevel(cfg))
	nets := map[string]route.Network{}
	activity := map[string]route.Activity{}
	closeTunnel := func() {}
	for _, tf := range cfg.Tunnels() {
		if tf.Name != name {
			continue
		}
		mgr, err := tunnel.Load([]config.Tunnel{tf}, tunnel.Options{
			ConnectTimeout: cfg.Upstream().ConnectTimeout,
			IdleTimeout:    cfg.IdleTimeout(),
			Logger:         log,
		})
		if err != nil {
			return nil, nil, configError{err}
		}
		closeTunnel = func() { _ = mgr.Close() }
		t, _ := mgr.Tunnel(name)
		nets[name], activity[name] = t, t
	}
	allow := cfg.AllowPrivate()
	for n, prefixes := range c.allowPrivate {
		allow[n] = append(allow[n], prefixes...)
	}
	set, err := route.NewSet(route.Host(), nets, route.Options{Upstream: cfg.Upstream(), AllowPrivate: allow, Activity: activity})
	if err != nil {
		closeTunnel()
		return nil, nil, err
	}
	rt, ok := set.Route(name)
	if !ok {
		closeTunnel()
		return nil, nil, fmt.Errorf("unknown route %q", name)
	}
	return rt, func() {
		set.CloseIdleConnections()
		closeTunnel()
	}, nil
}
