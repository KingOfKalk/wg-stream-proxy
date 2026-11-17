// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Defaults of config.toml (spec 5.3).
const (
	defaultListen         = ":2349"
	defaultUserAgent      = "Mozilla/5.0 (X11; Linux x86_64) Firefox/132.0"
	defaultConnectTimeout = 10 * time.Second
	defaultHeaderTimeout  = 15 * time.Second
	defaultReadTimeout    = 30 * time.Second
	defaultIdleTimeout    = 60 * time.Minute
)

var logLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// overrideSource labels values that came from a flag or the environment.
const overrideSource = "(flag or environment)"

// applySettings validates config.toml values from file src (with
// overrides applied) and stores them in c. routes holds the valid route
// names for allow_private.
func (c *Config) applySettings(raw rawConfig, src string, ov Overrides, routes map[string]bool) []error {
	var errs []error
	fail := func(key, origin, format string, args ...any) {
		if origin == overrideSource {
			key += " " + overrideSource
		} else {
			key = origin + ": " + key
		}
		errs = append(errs, fmt.Errorf("%s: %s", key, fmt.Sprintf(format, args...)))
	}
	// pick returns the override if set, else the file value, else def.
	pick := func(override string, file *string, def string) (string, string) {
		if override != "" {
			return override, overrideSource
		}
		if file != nil {
			return *file, src
		}
		return def, src
	}

	v, origin := pick(ov.Listen, raw.Listen, defaultListen)
	if !validListen(v) {
		fail("listen", origin, "invalid address %q: use [host]:port", v)
	}
	c.listen = v

	v, origin = pick(ov.LogLevel, raw.LogLevel, "info")
	if lvl, ok := logLevels[v]; ok {
		c.logLevel = lvl
	} else {
		fail("log_level", origin, "invalid value %q: use debug, info, warn or error", v)
	}

	v, origin = pick(ov.LogFormat, raw.LogFormat, string(LogFormatText))
	if f := LogFormat(v); f == LogFormatText || f == LogFormatJSON {
		c.logFormat = f
	} else {
		fail("log_format", origin, "invalid value %q: use text or json", v)
	}

	v, origin = pick(ov.PublicBaseURL, raw.PublicBaseURL, "")
	if v != "" && !httpURL(v) {
		fail("public_base_url", origin, "must be an absolute http(s) URL")
	}
	c.publicBaseURL = v

	up := raw.Upstream
	c.upstream.UserAgent = defaultUserAgent
	if up.UserAgent != nil {
		c.upstream.UserAgent = *up.UserAgent
		if hasLineBreak(*up.UserAgent) {
			fail("upstream.user_agent", src, "must not contain line breaks")
		}
	}
	duration := func(key string, raw *string, def time.Duration) time.Duration {
		if raw == nil {
			return def
		}
		d, err := time.ParseDuration(*raw)
		switch {
		case err != nil:
			fail(key, src, "invalid duration %q", *raw)
		case d <= 0:
			fail(key, src, "must be positive")
		}
		return d
	}
	c.upstream.ConnectTimeout = duration("upstream.connect_timeout", up.ConnectTimeout, defaultConnectTimeout)
	c.upstream.HeaderTimeout = duration("upstream.header_timeout", up.HeaderTimeout, defaultHeaderTimeout)
	c.upstream.ReadTimeout = duration("upstream.read_timeout", up.ReadTimeout, defaultReadTimeout)
	c.idleTimeout = duration("tunnels.idle_timeout", raw.Tunnels.IdleTimeout, defaultIdleTimeout)

	c.allowPrivate = map[string][]netip.Prefix{}
	for _, route := range slices.Sorted(maps.Keys(up.AllowPrivate)) {
		key := "upstream.allow_private." + route
		if !routes[route] {
			fail(key, src, "unknown route %q", route)
			continue
		}
		for _, s := range up.AllowPrivate[route] {
			p, err := netip.ParsePrefix(s)
			switch {
			case err != nil:
				fail(key, src, "invalid prefix %q: use CIDR notation", s)
			case p != p.Masked():
				fail(key, src, "%q has host bits set, use %q", s, p.Masked().String())
			default:
				c.allowPrivate[route] = append(c.allowPrivate[route], p)
			}
		}
	}

	if raw.EPG.URL != nil && *raw.EPG.URL != "" {
		if !httpURL(*raw.EPG.URL) {
			fail("epg.url", src, "must be an absolute http(s) URL")
		}
		c.epgURL = *raw.EPG.URL
	}
	return errs
}

// validListen reports whether s is host:port with a numeric port.
func validListen(s string) bool {
	_, port, err := net.SplitHostPort(s)
	if err != nil {
		return false
	}
	_, err = strconv.ParseUint(port, 10, 16)
	return err == nil
}

// httpURL reports whether s is an absolute http or https URL with a host.
func httpURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// hasLineBreak reports whether s contains CR or LF.
func hasLineBreak(s string) bool {
	return strings.ContainsAny(s, "\r\n")
}
