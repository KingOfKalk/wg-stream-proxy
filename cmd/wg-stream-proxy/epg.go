// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/epg"
	"github.com/KingOfKalk/wg-stream-proxy/internal/importer"
	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
)

// matchCandidates is the number of suggestions per channel in epg match:
// the best one and its alternatives.
const matchCandidates = 3

// epgCmd runs epg ids and epg match (spec 7.2). Both take the config
// flags of serve and check and print to stdout; nothing is written.
func (c *cli) epgCmd(args []string) int {
	if len(args) == 0 || args[0] != "ids" && args[0] != "match" {
		_, _ = fmt.Fprintln(c.stderr, "wg-stream-proxy: epg: want a subcommand: ids or match")
		return 2
	}
	sub := args[0]
	opts, code, ok := c.parseFlags("epg "+sub, args[1:])
	if !ok {
		return code
	}
	cfg, err := config.Load(opts)
	if err != nil {
		c.printErrors(err)
		return 1
	}
	x, err := c.loadEPG(cfg)
	if err != nil {
		_, _ = fmt.Fprintf(c.stderr, "wg-stream-proxy: epg %s: %v\n", sub, err)
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

// loadEPG downloads [epg] url over the direct route (spec 7.2): with the
// private-range guard (and the allow_private exceptions of direct) and
// the [upstream] timeouts.
func (c *cli) loadEPG(cfg *config.Config) (*epg.Index, error) {
	if cfg.EPGURL() == "" {
		return nil, fmt.Errorf("no [epg] url in config.toml")
	}
	allow := cfg.AllowPrivate()
	for name, prefixes := range c.epgAllowPrivate {
		allow[name] = append(allow[name], prefixes...)
	}
	set, err := route.NewSet(route.Host(), nil, route.Options{Upstream: cfg.Upstream(), AllowPrivate: allow})
	if err != nil {
		return nil, err
	}
	defer set.CloseIdleConnections()
	direct, _ := set.Route(config.RouteDirect)
	return epg.Download(c.context(), direct, cfg.EPGURL())
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
