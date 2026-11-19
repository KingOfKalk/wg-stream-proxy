// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/importer"
)

// description is the canonical one-line description of the program.
const description = "Route each IPTV and internet radio channel through its own userspace WireGuard tunnel (or direct) and serve them as one M3U playlist for Kodi, VLC and other players."

// commands lists the top-level commands with their one-line summary, in
// the order of the help output.
var commands = []struct{ name, summary string }{
	{"serve", "run the proxy (default)"},
	{"check", "validate the config"},
	{"probe", "test a URL through a route"},
	{"import", "convert an M3U file to one channel file"},
	{"epg", "list or match EPG ids"},
	{"healthcheck", "query /healthz on the listen address"},
	{"tunnel", "list tunnels or stop one (needs [debug] listen)"},
	{"version", "print version and source URL"},
	{"help", "show help for the program or a command"},
}

// topic is the help of one command or subcommand.
type topic struct {
	usage string // after "wg-stream-proxy "
	desc  string
	// flags builds the flag set of the command; nil if it has none.
	flags func() *flag.FlagSet
}

// topics maps a command path ("epg ids") to its help.
var topics = map[string]topic{
	"serve": {
		usage: "serve [flags]",
		desc:  "Run the proxy until SIGINT or SIGTERM; SIGHUP reloads the config. Without a command, serve runs.",
		flags: func() *flag.FlagSet { fs, _ := configFlagSet("serve"); return fs },
	},
	"check": {
		usage: "check [flags]",
		desc:  "Validate config.toml, the channel files, the tunnel files and the signing key variables. Lists every error and starts nothing.",
		flags: func() *flag.FlagSet { fs, _ := configFlagSet("check"); return fs },
	},
	"probe": {
		usage: "probe [flags] <url>",
		desc: "Test a URL through a route without adding a channel: fetch it like the proxy would (private-range guard, [upstream] timeouts and limits) and report the final URL (host and file name only), status, content type, the detected format (HLS master or media, raw or ICY, or a refused playlist format) and where the time went (tunnel start, DNS, connect, TLS, first byte). For an HLS playlist the first variant and the first segment are probed too. " +
			"Exit code 0 if the proxy would serve the URL, 1 if not, 2 for usage errors. The URL may come before or after the flags.\n\n" +
			"If the proxy runs and [debug] listen is set, the running proxy makes the requests through its running tunnel (a changed channel, tunnel or config file counts only after its reload). Without a debug listener, or if nothing listens there, probe runs standalone and starts the tunnel of the route itself, which can stall streams of a running proxy with the same tunnel key; --standalone forces this. The output says which mode ran.",
		flags: func() *flag.FlagSet { return newProbeFlags().fset },
	},
	"import": {
		usage: "import <file.m3u> [flags]",
		desc:  "Convert an M3U file to a channel file, on stdout or in a new file that is never overwritten. Flags may come before or after the file.",
		flags: func() *flag.FlagSet { return newImportFlags().fset },
	},
	"epg": {
		usage: "epg ids | epg match [flags]",
		desc: "List or match EPG ids from an EPG source of config.toml: the [[epg.source]] named by --source, by default the first one, or [epg] url. A source on a tunnel starts that tunnel in this process.\n\n" +
			"commands:\n" +
			"  epg ids    list the id and display names of every EPG channel\n" +
			"  epg match  suggest a tvg_id per channel as TOML, write nothing\n\n" +
			"Run \"wg-stream-proxy help epg <command>\" for the flags.",
	},
	"epg ids": {
		usage: "epg ids [flags]",
		desc:  "List the id and display names of every EPG channel of the EPG source, sorted by id.",
		flags: func() *flag.FlagSet { fs, _, _ := epgFlagSet("epg ids"); return fs },
	},
	"epg match": {
		usage: "epg match [flags]",
		desc:  "Suggest a tvg_id per channel from the EPG as TOML snippets. Writes nothing.",
		flags: func() *flag.FlagSet { fs, _, _ := epgFlagSet("epg match"); return fs },
	},
	"healthcheck": {
		usage: "healthcheck [flags]",
		desc:  "Query /healthz on the listen address. Prints nothing and exits 0 if it answers 200, else prints one line on stderr and exits 1.",
		flags: func() *flag.FlagSet { fs, _ := configFlagSet("healthcheck"); return fs },
	},
	"tunnel": {
		usage: "tunnel list | tunnel stop <name> [flags]",
		desc: "List the tunnels or stop one through the debug endpoints of the running proxy (needs [debug] listen).\n\n" +
			"commands:\n" +
			"  tunnel list         list the tunnels and their state\n" +
			"  tunnel stop <name>  cool down a tunnel and wait until it is stopped\n\n" +
			"Run \"wg-stream-proxy help tunnel <command>\" for the flags.",
	},
	"tunnel list": {
		usage: "tunnel list [flags]",
		desc:  "List the tunnels and their state (needs [debug] listen).",
		flags: func() *flag.FlagSet { fs, _ := configFlagSet("tunnel list"); return fs },
	},
	"tunnel stop": {
		usage: "tunnel stop <name> [flags]",
		desc:  "Cool down a tunnel and wait until it is stopped (needs [debug] listen). The name may come before or after the flags.",
		flags: func() *flag.FlagSet { fs, _ := configFlagSet("tunnel stop"); return fs },
	},
	"version": {
		usage: "version",
		desc:  "Print the version and the source URL.",
	},
	"help": {
		usage: "help [command]",
		desc:  "Show the commands of the program, or the description and flags of a command such as \"epg ids\". \"<command> -h\" does the same.",
	},
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
	fs.StringVar(&f.Listen, "listen", "", "listen address [host]:port (env "+config.EnvListen+")")
	fs.StringVar(&f.LogLevel, "log-level", "", "debug, info, warn or error (env "+config.EnvLogLevel+")")
	fs.StringVar(&f.LogFormat, "log-format", "", "text or json (env "+config.EnvLogFormat+")")
	return fs, &f
}

// importFlags are the flags of import.
type importFlags struct {
	fset      *flag.FlagSet
	opts      importer.Options
	out       string
	configDir string
	matchEPG  bool
}

func newImportFlags() *importFlags {
	f := &importFlags{fset: flag.NewFlagSet("import", flag.ContinueOnError)}
	f.fset.Usage = func() {}
	f.fset.StringVar(&f.opts.Tunnel, "tunnel", "", "tunnel name or direct, written at the top of the channel file")
	f.fset.StringVar(&f.out, "out", "", "write to this new file instead of stdout (never overwritten)")
	f.fset.BoolVar(&f.matchEPG, "match-epg", false, "keep tvg_id only if the EPG (the first [[epg.source]] or [epg] url in config.toml) has it, else suggest the best match in a comment")
	f.fset.StringVar(&f.configDir, "config-dir", "", "config directory for --match-epg (env "+config.EnvConfigDir+")")
	return f
}

// topHelp returns the help of the program: the description, the usage
// and the list of commands.
func topHelp() string {
	var b strings.Builder
	b.WriteString(description + "\n\nusage: wg-stream-proxy [command] [flags]\n\ncommands:\n")
	for _, c := range commands {
		fmt.Fprintf(&b, "  %-12s %s\n", c.name, c.summary)
	}
	b.WriteString("\nRun \"wg-stream-proxy help <command>\" for the description and flags of a command.\n")
	return b.String()
}

// topicHelp returns the help of a command: its usage, description and
// flags.
func topicHelp(name string) string {
	t := topics[name]
	var b strings.Builder
	b.WriteString("usage: wg-stream-proxy " + t.usage + "\n\n" + t.desc + "\n")
	if t.flags != nil {
		fs := t.flags()
		fs.SetOutput(&b)
		b.WriteString("\nflags:\n")
		fs.PrintDefaults()
	}
	return b.String()
}

// help prints the help of a command (name is its path, "" for the
// program) to stdout and returns the exit code.
func (c *cli) help(name string) int {
	text := topHelp()
	if name != "" {
		text = topicHelp(name)
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
	if name == "" {
		return c.help("")
	}
	if _, ok := topics[name]; !ok {
		_, _ = fmt.Fprintf(c.stderr, "wg-stream-proxy: help: unknown command %q\n\n%s", name, topHelp())
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
