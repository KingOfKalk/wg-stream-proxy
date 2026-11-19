// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/epg"
	"github.com/KingOfKalk/wg-stream-proxy/internal/importer"
)

// matchCandidates is the number of suggestions per channel in epg match:
// the best one and its alternatives.
const matchCandidates = 3

// epgCmd runs epg ids and epg match (spec 7.2). Both take the config
// flags of serve and check plus --source and print to stdout; nothing is
// written.
func (c *cli) epgCmd(args []string) int {
	if len(args) > 0 && isHelpFlag(args[0]) {
		return c.help("epg")
	}
	if len(args) == 0 || args[0] != "ids" && args[0] != "match" {
		_, _ = fmt.Fprintln(c.stderr, "wg-stream-proxy: epg: want a subcommand: ids or match")
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
	var out string
	if sub == "ids" {
		out = formatIDs(x)
	} else {
		out = formatMatches(cfg.Channels(), x)
	}
	if _, err := c.stdout.Write([]byte(out)); err != nil {
		_, _ = fmt.Fprintf(c.stderr, "wg-stream-proxy: epg %s: %v\n", sub, err)
		return 1
	}
	return 0
}

// epgFlagSet returns the flags of epg ids and epg match: the config
// flags and --source.
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

// formatMatches renders the epg match output: per channel a TOML snippet
// for its [channel.playlist] table with the best suggestion, and the
// alternatives as comments. Channels whose tvg_id is in the EPG are only
// reported.
func formatMatches(chs []config.Channel, x *epg.Index) string {
	var b strings.Builder
	for i, ch := range chs {
		if i > 0 {
			b.WriteString("\n")
		}
		head := fmt.Sprintf("# %s (%s) in %s", ch.ID(), oneLine(ch.Name()), filepath.Base(ch.File()))
		cur := ch.TVGID()
		if cur != "" && x.Has(cur) {
			b.WriteString(head + ": tvg_id " + importer.Quote(cur) + " is in the EPG\n")
			continue
		}
		cands := x.Match(ch.Name(), matchCandidates)
		if len(cands) == 0 {
			b.WriteString(head + ": no match\n")
			continue
		}
		b.WriteString(head + "\n")
		if cur != "" {
			b.WriteString("# current tvg_id " + importer.Quote(cur) + " is not in the EPG\n")
		}
		b.WriteString("[channel.playlist]\n")
		b.WriteString("tvg_id = " + importer.Quote(cands[0].ID) + "  # " + describe(cands[0]) + "\n")
		if len(cands) > 1 {
			b.WriteString("# alternatives:\n")
			for _, c := range cands[1:] {
				b.WriteString("# tvg_id = " + importer.Quote(c.ID) + "  # " + describe(c) + "\n")
			}
		}
	}
	return b.String()
}

// describe returns the display name and score of a candidate for a comment.
func describe(c epg.Candidate) string {
	return fmt.Sprintf("%s (%.2f)", oneLine(c.Name), c.Score)
}

// oneLine collapses whitespace, so foreign text stays on one comment line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
