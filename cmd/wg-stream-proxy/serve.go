// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/hls"
	"github.com/KingOfKalk/wg-stream-proxy/internal/server"
	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel"
)

// parseFlags parses the flags shared by serve and check (spec 5.1) and
// resolves the config dir and overrides.
func (c *cli) parseFlags(cmd string, args []string) (config.Options, int, bool) {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	var f config.Flags
	fs.StringVar(&f.ConfigDir, "config-dir", "", "config directory (env "+config.EnvConfigDir+")")
	fs.StringVar(&f.Listen, "listen", "", "listen address [host]:port (env "+config.EnvListen+")")
	fs.StringVar(&f.LogLevel, "log-level", "", "debug, info, warn or error (env "+config.EnvLogLevel+")")
	fs.StringVar(&f.LogFormat, "log-format", "", "text or json (env "+config.EnvLogFormat+")")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return config.Options{}, 0, false
		}
		return config.Options{}, 2, false
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(c.stderr, "wg-stream-proxy: %s: unexpected arguments: %s\n", cmd, strings.Join(fs.Args(), " "))
		return config.Options{}, 2, false
	}
	opts, err := config.Resolve(f, c.env)
	if err != nil {
		c.printErrors(err)
		return config.Options{}, 1, false
	}
	return opts, 0, true
}

// printErrors prints every line of a (joined) error.
func (c *cli) printErrors(err error) {
	_, _ = fmt.Fprintln(c.stderr, "wg-stream-proxy: invalid config:")
	for _, line := range strings.Split(err.Error(), "\n") {
		if line != "" {
			_, _ = fmt.Fprintln(c.stderr, "  "+line)
		}
	}
}

// newLogger returns the slog logger of spec 5.3 on w.
func newLogger(w io.Writer, format config.LogFormat, level slog.Level) *slog.Logger {
	o := &slog.HandlerOptions{Level: level}
	if format == config.LogFormatJSON {
		return slog.New(slog.NewJSONHandler(w, o))
	}
	return slog.New(slog.NewTextHandler(w, o))
}

// serve runs the proxy until SIGINT or SIGTERM (or c.ctx) ends it.
// Startup errors exit with 1.
func (c *cli) serve(args []string) int {
	opts, code, ok := c.parseFlags("serve", args)
	if !ok {
		return code
	}
	cfg, err := config.Load(opts)
	if err != nil {
		c.printErrors(err)
		return 1
	}
	log := newLogger(c.stderr, cfg.LogFormat(), cfg.LogLevel())

	key, generated, err := hls.LoadSigningKey(c.getenv, c.readFile)
	if err != nil {
		log.Error("signing key", "error", err)
		return 1
	}
	if generated {
		log.Warn("no signing key set: using a random key, resource URLs do not survive restarts",
			"set", hls.EnvSigningKey+" or "+hls.EnvSigningKeyFile)
	}
	signer, err := hls.NewSigner(key)
	if err != nil {
		log.Error("signing key", "error", err)
		return 1
	}

	ln, err := net.Listen("tcp", cfg.Listen())
	if err != nil {
		log.Error("cannot listen", "addr", cfg.Listen(), "error", err)
		return 1
	}
	if c.listening != nil {
		c.listening(ln.Addr())
	}

	ctx, stop := signal.NotifyContext(c.ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal, a second one terminates at once.
	context.AfterFunc(ctx, stop)

	err = server.Run(ctx, server.RunOptions{
		Config:   cfg,
		Signer:   signer,
		Logger:   log,
		Version:  version,
		Listener: ln,
	})
	if err != nil {
		log.Error("server failed", "error", err)
		return 1
	}
	return 0
}

// check validates the config, the tunnel files and the signing key
// variables and lists every error (spec 7.2). Nothing is started.
func (c *cli) check(args []string) int {
	opts, code, ok := c.parseFlags("check", args)
	if !ok {
		return code
	}
	var errs []error
	cfg, err := config.Load(opts)
	log := newLogger(c.stderr, config.LogFormatText, slog.LevelInfo)
	connect := time.Second // Load only validates it; nothing is started
	var tunnels []config.Tunnel
	if err != nil {
		errs = append(errs, err)
		// Still check the tunnel files the config would use.
		tunnels = tunnelFiles(opts.Paths.TunnelsDir)
	} else {
		log = newLogger(c.stderr, cfg.LogFormat(), cfg.LogLevel())
		connect = cfg.Upstream().ConnectTimeout
		tunnels = cfg.Tunnels()
	}
	mgr, err := tunnel.Load(tunnels, tunnel.Options{ConnectTimeout: connect, Logger: log})
	if err != nil {
		errs = append(errs, err)
	} else {
		_ = mgr.Close()
	}
	if _, _, err := hls.LoadSigningKey(c.getenv, c.readFile); err != nil {
		errs = append(errs, fmt.Errorf("signing key: %w", err))
	}
	if len(errs) > 0 {
		c.printErrors(errors.Join(errs...))
		return 1
	}
	enabled := 0
	for _, ch := range cfg.Channels() {
		if ch.Enabled() {
			enabled++
		}
	}
	_, _ = fmt.Fprintf(c.stdout, "config ok: %d channels (%d enabled), %d tunnels\n",
		len(cfg.Channels()), enabled, len(cfg.Tunnels()))
	return 0
}

// tunnelFiles lists the tunnel files of dir with valid names, like
// config.Load does; invalid names are reported by config.Load.
func tunnelFiles(dir string) []config.Tunnel {
	paths, _ := filepath.Glob(filepath.Join(dir, "*.conf"))
	var out []config.Tunnel
	for _, p := range paths {
		name := strings.TrimSuffix(filepath.Base(p), ".conf")
		if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() ||
			name == config.RouteDirect || !config.ValidName(name) {
			continue
		}
		out = append(out, config.Tunnel{Name: name, Path: p})
	}
	return out
}
