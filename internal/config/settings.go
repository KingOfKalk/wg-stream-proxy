// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"fmt"
	"log/slog"
	"maps"
	"math"
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
	defaultWatchInterval  = 2 * time.Second
	defaultPlaylistSize   = 8 << 20
	defaultEPGRefresh     = 6 * time.Hour
	// minEPGRefresh keeps a typo ("6s") from fetching a large EPG file
	// over and over.
	minEPGRefresh = time.Minute
)

// MaxPlaylistSizeLimit is the upper bound of [upstream] max_playlist_size
// (spec 3.4), the largest playlist hls.Rewrite accepts.
const MaxPlaylistSizeLimit = 32 << 20

var logLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// overrideSource labels values that came from a flag.
const overrideSource = "(flag)"

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

	v = ""
	if raw.PublicBaseURL != nil {
		v = *raw.PublicBaseURL
	}
	if v != "" && !httpURL(v) {
		fail("public_base_url", src, "must be an absolute http(s) URL")
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

	c.clips = Clips{Error: true}
	if v := raw.Clips.Error; v != nil {
		c.clips.Error = *v
	}

	c.watchInterval = defaultWatchInterval
	if v := raw.Reload.WatchInterval; v != nil {
		// Zero turns watching off, so it is the one value that is not
		// "positive".
		switch d, err := time.ParseDuration(*v); {
		case err != nil:
			fail("reload.watch_interval", src, "invalid duration %q", *v)
		case d < 0:
			fail("reload.watch_interval", src, `must be positive, or "0" to turn watching off`)
		default:
			c.watchInterval = d
		}
	}

	c.upstream.MaxPlaylistSize = defaultPlaylistSize
	if v := up.MaxPlaylistSize; v != nil {
		n, ok := parseSize(*v)
		switch {
		case !ok:
			fail("upstream.max_playlist_size", src, `invalid size %q: use a whole number with unit B, KiB or MiB, e.g. "8MiB"`, *v)
		case n <= 0:
			fail("upstream.max_playlist_size", src, "must be positive")
		case n > MaxPlaylistSizeLimit:
			fail("upstream.max_playlist_size", src, "must be at most %dMiB", MaxPlaylistSizeLimit>>20)
		}
		c.upstream.MaxPlaylistSize = n
	}

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

	if v := raw.Debug.Listen; v != nil && *v != "" {
		c.debugListen = *v
		switch {
		case !validListen(*v):
			fail("debug.listen", src, "invalid address %q: use [host]:port", *v)
		case validListen(c.listen) && samePort(*v, c.listen):
			// Never on the main port (spec 5.3).
			_, port, _ := net.SplitHostPort(c.listen)
			fail("debug.listen", src, "must not use the port of listen (%s)", port)
		}
	}

	if raw.EPG.URL != nil && *raw.EPG.URL != "" {
		if !httpURL(*raw.EPG.URL) {
			fail("epg.url", src, "must be an absolute http(s) URL")
		}
		c.epgURL = *raw.EPG.URL
	}
	c.epgRefresh = duration("epg.refresh", raw.EPG.Refresh, defaultEPGRefresh)
	if c.epgRefresh > 0 && c.epgRefresh < minEPGRefresh {
		fail("epg.refresh", src, "must be at least %s", minEPGRefresh)
	}
	c.epgSources = epgSources(raw.EPG.Source, routes, fail, src)
	if c.epgURL != "" && len(raw.EPG.Source) > 0 {
		fail("epg.url", src, "not allowed together with [[epg.source]]: use url (the player fetches the EPG) or sources (the proxy serves it)")
	}
	return errs
}

// epgSources validates the [[epg.source]] entries (spec 5.3): unique
// valid names, absolute http(s) URLs of at most 4096 bytes, an existing
// route (omitted: direct).
func epgSources(raw []rawEPGSource, routes map[string]bool, fail func(key, origin, format string, args ...any), src string) []EPGSource {
	var out []EPGSource
	seen := map[string]bool{}
	for i, rs := range raw {
		key := fmt.Sprintf("epg.source %d", i+1)
		s := EPGSource{Route: RouteDirect}
		ok := true
		switch {
		case rs.Name == nil || *rs.Name == "":
			fail(key, src, "name: required")
			ok = false
		case !ValidName(*rs.Name):
			fail(key, src, "name: invalid name %q: use [a-z0-9-], 1-%d characters", *rs.Name, maxNameLen)
			ok = false
		case seen[*rs.Name]:
			fail(key, src, "name: duplicate source %q", *rs.Name)
			ok = false
		default:
			s.Name = *rs.Name
			seen[s.Name] = true
			key += fmt.Sprintf(" (%q)", s.Name)
		}
		switch {
		case rs.URL == nil || *rs.URL == "":
			fail(key, src, "url: required")
			ok = false
		case len(*rs.URL) > maxURLLen:
			fail(key, src, "url: longer than %d bytes", maxURLLen)
			ok = false
		case !httpURL(*rs.URL):
			fail(key, src, "url: must be an absolute http(s) URL")
			ok = false
		default:
			s.URL = *rs.URL
		}
		if rs.Tunnel != nil {
			if !routes[*rs.Tunnel] {
				fail(key, src, "tunnel: unknown tunnel %q", *rs.Tunnel)
				ok = false
			}
			s.Route = *rs.Tunnel
		}
		if ok {
			out = append(out, s)
		}
	}
	return out
}

// sizeUnits are the units of a size value, as multipliers.
var sizeUnits = map[string]int64{"B": 1, "KiB": 1 << 10, "MiB": 1 << 20}

// parseSize parses a size: a whole number of decimal digits directly
// followed by the unit B, KiB or MiB ("8MiB"). ok is false for other
// forms and for values that do not fit an int64.
func parseSize(s string) (n int64, ok bool) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	unit, ok := sizeUnits[s[i:]]
	if i == 0 || !ok {
		return 0, false
	}
	v, err := strconv.ParseInt(s[:i], 10, 64)
	if err != nil || v > math.MaxInt64/unit {
		return 0, false
	}
	return v * unit, true
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
	return err == nil && IsHTTPURL(u)
}

// IsHTTPURL reports whether u is an absolute http or https URL with a
// host: what upstream URLs, redirect targets and probed URLs must be.
func IsHTTPURL(u *url.URL) bool {
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// hasLineBreak reports whether s contains CR or LF.
func hasLineBreak(s string) bool {
	return strings.ContainsAny(s, "\r\n")
}

// samePort reports whether two valid listen addresses have the same port
// other than 0 (port 0 picks a free port).
func samePort(a, b string) bool {
	_, pa, _ := net.SplitHostPort(a)
	_, pb, _ := net.SplitHostPort(b)
	na, _ := strconv.ParseUint(pa, 10, 16)
	nb, _ := strconv.ParseUint(pb, 10, 16)
	return na != 0 && na == nb
}
