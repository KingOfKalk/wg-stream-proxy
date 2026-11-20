// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
)

// description is the canonical one-line description of the program.
const description = "Route each IPTV and internet radio channel through its own userspace WireGuard tunnel (or direct) and serve them as one M3U playlist for Kodi, VLC and other players."

// usage is the help of the program: the commands with one line each.
const usage = description + `

usage: wg-stream-proxy [command] [flags]

commands:
  serve        run the proxy until SIGINT or SIGTERM; SIGHUP reloads (default)
  check        validate config, channel and tunnel files; start nothing
  probe        test a <url> through a route of the running proxy (needs [debug] listen)
  epg          epg ids: list the EPG ids of a source of config.toml
  healthcheck  query /healthz on the listen address
  tunnel       tunnel list, tunnel stop <name>: through the debug endpoints (needs [debug] listen)
  version      print version and source URL
  help         help [command]: show this text, or the flags of a command

Run "wg-stream-proxy help <command>" for the flags of a command.
`

// topics maps a command path ("epg ids") to its usage line (after
// "wg-stream-proxy ") and its flags, nil if it has none.
var topics = map[string]struct {
	usage string
	flags func() *flag.FlagSet
}{
	"serve":       {"serve [flags]", func() *flag.FlagSet { fs, _ := configFlagSet("serve"); return fs }},
	"check":       {"check [flags]", func() *flag.FlagSet { fs, _ := configFlagSet("check"); return fs }},
	"probe":       {"probe [flags] <url>", func() *flag.FlagSet { return newProbeFlags().fset }},
	"epg":         {"epg ids [flags]", nil},
	"epg ids":     {"epg ids [flags]", func() *flag.FlagSet { fs, _, _ := epgFlagSet("epg ids"); return fs }},
	"healthcheck": {"healthcheck [flags]", func() *flag.FlagSet { fs, _ := configFlagSet("healthcheck"); return fs }},
	"tunnel":      {"tunnel list | tunnel stop <name> [flags]", nil},
	"tunnel list": {"tunnel list [flags]", func() *flag.FlagSet { fs, _ := configFlagSet("tunnel list"); return fs }},
	"tunnel stop": {"tunnel stop <name> [flags]", func() *flag.FlagSet { fs, _ := configFlagSet("tunnel stop"); return fs }},
	"version":     {"version", nil},
	"help":        {"help [command]", nil},
}

// isHelpFlag reports whether arg asks for help like the flag package does.
func isHelpFlag(arg string) bool {
	return arg == "-h" || arg == "-help" || arg == "--help"
}

// configFlagSet returns the flags shared by serve, check and the commands
// that resolve the config (spec 5.1). The flag package prints no usage:
// help goes through the help command, so that -h prints to stdout.
func configFlagSet(name string) (*flag.FlagSet, *config.Flags) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {}
	var f config.Flags
	fs.StringVar(&f.ConfigDir, "config-dir", "", "config directory (env "+config.EnvConfigDir+")")
	fs.StringVar(&f.Listen, "listen", "", "listen address [host]:port")
	fs.StringVar(&f.LogLevel, "log-level", "", "debug, info, warn or error")
	fs.StringVar(&f.LogFormat, "log-format", "", "text or json")
	return fs, &f
}

// help prints the help of a command (name is its path, "" for the
// program) to stdout and returns the exit code.
func (c *cli) help(name string) int {
	text := usage
	if t, ok := topics[name]; ok {
		var b strings.Builder
		b.WriteString("usage: wg-stream-proxy " + t.usage + "\n")
		if t.flags != nil {
			fs := t.flags()
			fs.SetOutput(&b)
			b.WriteString("\nflags:\n")
			fs.PrintDefaults()
		}
		text = b.String()
	}
	if _, err := fmt.Fprint(c.stdout, text); err != nil {
		_, _ = fmt.Fprintf(c.stderr, "wg-stream-proxy: help: %v\n", err)
		return 1
	}
	return 0
}

// helpCmd runs the help command: help [command [subcommand]].
func (c *cli) helpCmd(args []string) int {
	if len(args) == 1 && isHelpFlag(args[0]) {
		return c.help("help")
	}
	name := strings.Join(args, " ")
	if _, ok := topics[name]; !ok && name != "" {
		_, _ = fmt.Fprintf(c.stderr, "wg-stream-proxy: help: unknown command %q\n\n%s", name, usage)
		return 2
	}
	return c.help(name)
}

// parseFailed returns the exit code for a flag parse error of the command
// name: -h prints the help to stdout (exit 0); any other error was
// already reported by the flag package, so only a pointer to the help is
// added (exit 2).
func (c *cli) parseFailed(name string, err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return c.help(name)
	}
	_, _ = fmt.Fprintf(c.stderr, "wg-stream-proxy: %s: see \"wg-stream-proxy help %s\"\n", name, name)
	return 2
}
