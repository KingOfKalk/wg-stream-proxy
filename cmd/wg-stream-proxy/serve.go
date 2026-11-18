// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
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
	"sync"
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
	_, opts, code, ok := c.parseFlagSet(cmd, args)
	return opts, code, ok
}

// parseFlagSet is parseFlags that also returns the flags, for resolving
// again on a reload.
func (c *cli) parseFlagSet(cmd string, args []string) (config.Flags, config.Options, int, bool) {
	f, opts, _, code, ok := c.parseFlagArgs(cmd, args, 0)
	return f, opts, code, ok
}

// parseFlagArgs is parseFlagSet that also accepts up to maxArgs
// arguments after the flags and returns them.
func (c *cli) parseFlagArgs(cmd string, args []string, maxArgs int) (config.Flags, config.Options, []string, int, bool) {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	var f config.Flags
	fs.StringVar(&f.ConfigDir, "config-dir", "", "config directory (env "+config.EnvConfigDir+")")
	fs.StringVar(&f.Listen, "listen", "", "listen address [host]:port (env "+config.EnvListen+")")
	fs.StringVar(&f.LogLevel, "log-level", "", "debug, info, warn or error (env "+config.EnvLogLevel+")")
	fs.StringVar(&f.LogFormat, "log-format", "", "text or json (env "+config.EnvLogFormat+")")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return f, config.Options{}, nil, 0, false
		}
		return f, config.Options{}, nil, 2, false
	}
	if fs.NArg() > maxArgs {
		_, _ = fmt.Fprintf(c.stderr, "wg-stream-proxy: %s: unexpected arguments: %s\n", cmd, strings.Join(fs.Args(), " "))
		return f, config.Options{}, nil, 2, false
	}
	opts, err := config.Resolve(f, c.env)
	if err != nil {
		c.printErrors(err)
		return f, config.Options{}, nil, 1, false
	}
	return f, opts, fs.Args(), 0, true
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
func newLogger(w io.Writer, format config.LogFormat, level slog.Leveler) *slog.Logger {
	o := &slog.HandlerOptions{Level: level}
	if format == config.LogFormatJSON {
		return slog.New(slog.NewJSONHandler(w, o))
	}
	return slog.New(slog.NewTextHandler(w, o))
}

// serve runs the proxy until SIGINT or SIGTERM (or c.ctx) ends it;
// SIGHUP reloads the configuration (spec 5.6). Startup errors exit with
// 1.
func (c *cli) serve(args []string) int {
	flags, opts, code, ok := c.parseFlagSet("serve", args)
	if !ok {
		return code
	}
	// Early, so a SIGHUP during startup does not end the process.
	hangup := c.hangup
	if hangup == nil {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGHUP)
		defer signal.Stop(ch)
		hangup = ch
	}
	cfg, err := config.Load(opts)
	if err != nil {
		c.printErrors(err)
		return 1
	}
	var level slog.LevelVar
	level.Set(cfg.LogLevel())
	log := newLogger(c.stderr, cfg.LogFormat(), &level)

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
	// The debug endpoints only on their own listener, off by default
	// (spec 7.1).
	var debugLn net.Listener
	if addr := cfg.DebugListen(); addr != "" {
		debugLn, err = net.Listen("tcp", addr)
		if err != nil {
			_ = ln.Close()
			log.Error("cannot listen", "addr", addr, "error", err)
			return 1
		}
		if c.debugListening != nil {
			c.debugListening(debugLn.Addr())
		}
	}

	ctx, stop := signal.NotifyContext(c.ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal, a second one terminates at once.
	context.AfterFunc(ctx, stop)

	reloader := server.NewReloader(c.reloadLoader(flags, opts, log, key, generated))
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-hangup:
				log.Info("SIGHUP: reloading")
				err := reloader.Reload()
				if errors.Is(err, server.ErrNotServing) {
					log.Warn("reload ignored: not serving")
				}
				if c.reloaded != nil {
					c.reloaded(err)
				}
			case <-done:
				return
			}
		}
	})
	defer wg.Wait()
	defer close(done)

	err = server.Run(ctx, server.RunOptions{
		Config:        cfg,
		Signer:        signer,
		Logger:        log,
		Version:       version,
		Listener:      ln,
		DebugListener: debugLn,
		Reloader:      reloader,
		LogLevel:      &level,
	})
	if err != nil {
		log.Error("server failed", "error", err)
		return 1
	}
	return 0
}

// reloadLoader returns the config loader of a reload (spec 5.6): it
// resolves the flags and environment again and loads the files. The
// tunnels dir location and the signing key need a restart: a change is
// logged and the old value kept (the signer is never replaced).
func (c *cli) reloadLoader(flags config.Flags, first config.Options, log *slog.Logger, key []byte, generated bool) func() (*config.Config, error) {
	return func() (*config.Config, error) {
		opts, err := config.Resolve(flags, c.env)
		if err != nil {
			return nil, err
		}
		if opts.Paths.TunnelsDir != first.Paths.TunnelsDir {
			log.Warn("reload: tunnels dir changed, restart required; keeping the old one",
				"tunnels_dir", first.Paths.TunnelsDir, "configured", opts.Paths.TunnelsDir)
			opts.Paths.TunnelsDir = first.Paths.TunnelsDir
		}
		switch next, nextGenerated, err := hls.LoadSigningKey(c.getenv, c.readFile); {
		case err != nil:
			log.Warn("reload: signing key: restart required to change it; keeping the old key", "error", err)
		case nextGenerated != generated || !generated && !bytes.Equal(next, key):
			log.Warn("reload: signing key changed, restart required; keeping the old key")
		}
		return config.Load(opts)
	}
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
