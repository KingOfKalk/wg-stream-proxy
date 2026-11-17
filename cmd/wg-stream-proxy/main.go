// SPDX-License-Identifier: AGPL-3.0-or-later

// Command wg-stream-proxy routes IPTV streams through userspace WireGuard
// tunnels and serves them as one M3U8 playlist.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

const sourceURL = "https://github.com/KingOfKalk/wg-stream-proxy"

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: wg-stream-proxy [command] [args]

commands:
  serve        run the proxy (default)
  check        validate the config
  import       convert an M3U file to channel files
  epg          list or match EPG ids
  healthcheck  query /healthz on the listen address
  version      print version and source URL
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches the subcommand in args and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	// Write errors on stderr are ignored: there is nowhere left to report them.
	switch cmd {
	case "version":
		return runVersion(args, stdout, stderr)
	case "serve", "check", "import", "epg", "healthcheck":
		_, _ = fmt.Fprintf(stderr, "wg-stream-proxy: %s: not implemented yet\n", cmd)
		return 1
	default:
		_, _ = fmt.Fprintf(stderr, "wg-stream-proxy: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
}

func runVersion(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		_, _ = fmt.Fprintln(stderr, "wg-stream-proxy: version: unexpected arguments")
		return 2
	}
	if _, err := fmt.Fprintf(stdout, "wg-stream-proxy %s\nsource: %s\n", version, sourceURL); err != nil {
		_, _ = fmt.Fprintf(stderr, "wg-stream-proxy: version: %v\n", err)
		return 1
	}
	return 0
}
