// SPDX-License-Identifier: AGPL-3.0-or-later

// Command wg-stream-proxy routes each IPTV and internet radio channel
// through its own userspace WireGuard tunnel (or direct) and serves them as
// one M3U playlist for Kodi, VLC and other players. It proxies HLS (TS,
// fMP4, AES-128) and raw/ICY audio streams; no root, no TUN device, fails
// closed.
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/server"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: wg-stream-proxy [command] [args]

commands:
  serve        run the proxy (default)
  check        validate the config
  import       convert an M3U file to one channel file
  epg          list or match EPG ids
  healthcheck  query /healthz on the listen address
  tunnel       list tunnels or stop one (needs [debug] listen)
  version      print version and source URL
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// cli holds the process environment of a command; tests replace it.
type cli struct {
	stdout, stderr io.Writer
	env            config.Env
	readFile       func(string) ([]byte, error)
	// ctx ends the serve command, like SIGINT and SIGTERM do.
	ctx context.Context
	// listening, if set, is called with the listen address before
	// serving starts.
	listening func(net.Addr)
	// debugListening, if set, is called with the address of the debug
	// listener before serving starts.
	debugListening func(net.Addr)
	// healthTimeout bounds the healthcheck request; 0 means the default.
	healthTimeout time.Duration
	// tunnelTimeout bounds the requests of the tunnel command; 0 means
	// the defaults.
	tunnelTimeout time.Duration
	// hangup, if set, replaces SIGHUP for serve: each value reloads.
	hangup chan os.Signal
	// reloaded, if set, is called with the result of each reload.
	reloaded func(error)
	// epgAllowPrivate adds private-range guard exceptions per route name
	// for the EPG download (tests allow the loopback range).
	epgAllowPrivate map[string][]netip.Prefix
}

// run dispatches the subcommand in args and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	c := &cli{
		stdout:   stdout,
		stderr:   stderr,
		env:      config.OSEnv(),
		readFile: os.ReadFile,
		ctx:      context.Background(),
	}
	return c.run(args)
}

func (c *cli) run(args []string) int {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	// Write errors on stderr are ignored: there is nowhere left to report them.
	switch cmd {
	case "version":
		return runVersion(args, c.stdout, c.stderr)
	case "serve":
		return c.serve(args)
	case "check":
		return c.check(args)
	case "healthcheck":
		return c.healthcheck(args)
	case "tunnel":
		return c.tunnelCmd(args)
	case "import":
		return c.importM3U(args)
	case "epg":
		return c.epgCmd(args)
	default:
		_, _ = fmt.Fprintf(c.stderr, "wg-stream-proxy: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
}

func runVersion(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		_, _ = fmt.Fprintln(stderr, "wg-stream-proxy: version: unexpected arguments")
		return 2
	}
	if _, err := fmt.Fprintf(stdout, "wg-stream-proxy %s\nsource: %s\n", version, server.SourceURL); err != nil {
		_, _ = fmt.Fprintf(stderr, "wg-stream-proxy: version: %v\n", err)
		return 1
	}
	return 0
}

// getenv returns an environment variable; empty means unset.
func (c *cli) getenv(key string) string {
	v, _ := c.env.LookupEnv(key)
	return v
}
