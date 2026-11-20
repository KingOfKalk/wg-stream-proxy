// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/epg"
)

// epgCmd runs epg ids (spec 7.2). It takes the config flags of serve and
// check plus --source and prints to stdout; nothing is written.
func (c *cli) epgCmd(args []string) int {
	if len(args) > 0 && isHelpFlag(args[0]) {
		return c.help("epg")
	}
	if len(args) == 0 || args[0] != "ids" {
		_, _ = fmt.Fprintln(c.stderr, "wg-stream-proxy: epg: want the subcommand ids")
		return 2
	}
	sub := args[0]
	fs, fp, source := epgFlagSet("epg " + sub)
	fs.SetOutput(c.stderr)
	if err := fs.Parse(args[1:]); err != nil {
		return c.parseFailed("epg "+sub, err)
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(c.stderr, "wg-stream-proxy: epg %s: unexpected arguments: %s\n", sub, strings.Join(fs.Args(), " "))
		return 2
	}
	opts, err := config.Resolve(*fp, c.env)
	if err != nil {
		c.printErrors(err)
		return 1
	}
	cfg, err := config.Load(opts)
	if err != nil {
		c.printErrors(err)
		return 1
	}
	x, err := c.loadEPG(cfg, *source)
	if err != nil {
		_, _ = fmt.Fprintf(c.stderr, "wg-stream-proxy: epg %s: %v\n", sub, err)
		if errors.Is(err, errUnknownSource) {
			return 2
		}
		return 1
	}
	if _, err := c.stdout.Write([]byte(formatIDs(x))); err != nil {
		_, _ = fmt.Fprintf(c.stderr, "wg-stream-proxy: epg %s: %v\n", sub, err)
		return 1
	}
	return 0
}

// epgFlagSet returns the flags of epg ids: the config flags and --source.
func epgFlagSet(name string) (*flag.FlagSet, *config.Flags, *string) {
	fs, fp := configFlagSet(name)
	source := fs.String("source", "", "name of the [[epg.source]] to read (default: the first one, or [epg] url)")
	return fs, fp, source
}

// errUnknownSource: --source names no [[epg.source]].
var errUnknownSource = errors.New("unknown EPG source")

// epgSource returns the EPG file the commands read (spec 7.2): the
// [[epg.source]] named name, by default the first one, or [epg] url
// over the direct route.
func epgSource(cfg *config.Config, name string) (config.EPGSource, error) {
	sources := cfg.EPGSources()
	if name != "" {
		names := make([]string, len(sources))
		for i, s := range sources {
			if s.Name == name {
				return s, nil
			}
			names[i] = s.Name
		}
		if len(names) == 0 {
			return config.EPGSource{}, fmt.Errorf("%w %q: config.toml has no [[epg.source]]", errUnknownSource, name)
		}
		return config.EPGSource{}, fmt.Errorf("%w %q (want one of: %s)", errUnknownSource, name, strings.Join(names, ", "))
	}
	switch {
	case len(sources) > 0:
		return sources[0], nil
	case cfg.EPGURL() != "":
		return config.EPGSource{URL: cfg.EPGURL(), Route: config.RouteDirect}, nil
	}
	return config.EPGSource{}, errors.New("no [[epg.source]] or [epg] url in config.toml")
}

// loadEPG downloads the EPG file of epgSource through its route (spec
// 7.2): with the private-range guard (and the allow_private exceptions of
// the route) and the [upstream] timeouts. A tunnel route starts its
// tunnel in this process.
func (c *cli) loadEPG(cfg *config.Config, name string) (*epg.Index, error) {
	src, err := epgSource(cfg, name)
	if err != nil {
		return nil, err
	}
	if src.Route != config.RouteDirect {
		_, _ = fmt.Fprintf(c.stderr, "wg-stream-proxy: epg: source %q uses tunnel %q: starting it in this process; a running proxy with the same tunnel key may stall until its next handshake\n", src.Name, src.Route)
	}
	rt, closeRoute, err := c.standaloneRoute(cfg, src.Route)
	if err != nil {
		return nil, err
	}
	defer closeRoute()
	return epg.Download(c.context(), rt, src.URL)
}

// context returns the command context.
func (c *cli) context() context.Context {
	if c.ctx == nil {
		return context.Background()
	}
	return c.ctx
}

// formatIDs lists the EPG channels sorted by id: the id, then the display
// names, separated by tabs.
func formatIDs(x *epg.Index) string {
	var b strings.Builder
	for _, ch := range x.Channels() {
		b.WriteString(ch.ID)
		for _, n := range ch.Names {
			b.WriteString("\t" + n)
		}
		b.WriteString("\n")
	}
	return b.String()
}
