// SPDX-License-Identifier: AGPL-3.0-or-later

// Package config loads and validates the wg-stream-proxy configuration:
// config.toml, the channel files in channels.d and the tunnel names from
// the tunnels dir (spec 5). Tunnel file contents are parsed elsewhere.
//
// A loaded Config is immutable: all fields are unexported and accessors
// return copies, so it can be shared between goroutines without locking.
package config

import (
	"log/slog"
	"maps"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"time"
)

// RouteDirect is the route name for channels that use the host network.
const RouteDirect = "direct"

// LogFormat is the log output format.
type LogFormat string

// Log formats (spec 5.3).
const (
	LogFormatText LogFormat = "text"
	LogFormatJSON LogFormat = "json"
)

// UpstreamType selects how a channel's upstream response is handled
// (spec 3.1).
type UpstreamType string

// Upstream types (spec 5.4).
const (
	TypeAuto UpstreamType = "auto"
	TypeHLS  UpstreamType = "hls"
	TypeRaw  UpstreamType = "raw"
)

// Upstream holds the [upstream] settings of config.toml (spec 3.4, 5.3).
type Upstream struct {
	UserAgent      string
	ConnectTimeout time.Duration
	HeaderTimeout  time.Duration
	ReadTimeout    time.Duration
	// MaxPlaylistSize is the largest playlist body in bytes.
	MaxPlaylistSize int64
}

// Clips holds the [clips] settings of config.toml (spec 5.3).
type Clips struct {
	// Error: GET /c/ of an HLS channel answers a failure with an error
	// clip instead of an HTTP error (spec 3.4).
	Error bool
}

// Tunnel is a tunnel file found in the tunnels dir. Its content is not
// parsed by this package.
type Tunnel struct {
	Name string // file stem
	Path string // path of the .conf file
}

// Config is a validated configuration. The zero value is not useful; use
// Load.
type Config struct {
	paths         Paths
	listen        string
	debugListen   string
	logLevel      slog.Level
	logFormat     LogFormat
	publicBaseURL string
	upstream      Upstream
	allowPrivate  map[string][]netip.Prefix
	idleTimeout   time.Duration
	watchInterval time.Duration
	clips         Clips
	epgURL        string
	tunnels       []Tunnel
	channels      []Channel
	byID          map[string]int
}

// Paths returns the config and tunnels directories.
func (c *Config) Paths() Paths { return c.paths }

// LogosDir returns the directory of local logos (spec 5.1).
func (c *Config) LogosDir() string { return logosDir(c.paths.ConfigDir) }

// Listen returns the listen address.
func (c *Config) Listen() string { return c.listen }

// DebugListen returns [debug] listen, the address of the debug
// endpoints, or "" when they are off.
func (c *Config) DebugListen() string { return c.debugListen }

// LogLevel returns the log level.
func (c *Config) LogLevel() slog.Level { return c.logLevel }

// LogFormat returns the log format.
func (c *Config) LogFormat() LogFormat { return c.logFormat }

// PublicBaseURL returns the configured base URL for absolute URLs, or "".
func (c *Config) PublicBaseURL() string { return c.publicBaseURL }

// Upstream returns the [upstream] settings.
func (c *Config) Upstream() Upstream { return c.upstream }

// AllowPrivate returns the private-range guard exceptions per route name
// (spec 3.5). Routes without exceptions are absent.
func (c *Config) AllowPrivate() map[string][]netip.Prefix {
	m := make(map[string][]netip.Prefix, len(c.allowPrivate))
	for k, v := range c.allowPrivate {
		m[k] = slices.Clone(v)
	}
	return m
}

// Clips returns the [clips] settings.
func (c *Config) Clips() Clips { return c.clips }

// IdleTimeout returns [tunnels] idle_timeout.
func (c *Config) IdleTimeout() time.Duration { return c.idleTimeout }

// WatchInterval returns [reload] watch_interval, how often the config
// files are checked for changes (spec 5.6); 0 means watching is off.
func (c *Config) WatchInterval() time.Duration { return c.watchInterval }

// EPGURL returns [epg] url, or "".
func (c *Config) EPGURL() string { return c.epgURL }

// Tunnels returns the tunnel files, sorted by name.
func (c *Config) Tunnels() []Tunnel { return slices.Clone(c.tunnels) }

// Channels returns all channels, including disabled ones, ordered by
// channel file name and then by position in the file.
func (c *Config) Channels() []Channel { return slices.Clone(c.channels) }

// Channel returns the channel with the given id.
func (c *Config) Channel(id string) (Channel, bool) {
	i, ok := c.byID[id]
	if !ok {
		return Channel{}, false
	}
	return c.channels[i], true
}

// LogoKind tells how a channel logo is rendered (spec 5.4).
type LogoKind int

// Logo kinds.
const (
	LogoNone  LogoKind = iota // no logo
	LogoURL                   // http(s) URL, passed through
	LogoLocal                 // file in the logos dir, served as /logos/{file}
	LogoRaw                   // anything else, written as-is
)

// String returns the lowercase kind name.
func (k LogoKind) String() string {
	switch k {
	case LogoNone:
		return "none"
	case LogoURL:
		return "url"
	case LogoLocal:
		return "local"
	case LogoRaw:
		return "raw"
	}
	return "LogoKind(" + strconv.Itoa(int(k)) + ")"
}

// Logo is a channel logo.
type Logo struct {
	Kind  LogoKind
	Value string // URL, file name or raw value
}

// Attr is an extra #EXTINF attribute (spec 5.4).
type Attr struct {
	Key   string
	Value string
}

// Extra is one playlist.extra line: #<Directive>:<Key>=<Value> (spec 5.4).
type Extra struct {
	Directive string
	Key       string
	Value     string
}

// Channel is a validated channel with all layers merged.
type Channel struct {
	file    string
	id      string
	name    string
	url     string
	route   string
	group   string
	enabled bool
	typ     UpstreamType
	header  http.Header
	tvgID   string
	logo    Logo
	attrs   []Attr
	extra   []Extra
}

// File returns the channel file the channel comes from.
func (ch Channel) File() string { return ch.file }

// ID returns the channel id.
func (ch Channel) ID() string { return ch.id }

// Name returns the display name.
func (ch Channel) Name() string { return ch.name }

// URL returns the upstream URL.
func (ch Channel) URL() string { return ch.url }

// Route returns the tunnel name, or RouteDirect.
func (ch Channel) Route() string { return ch.route }

// Group returns the group, possibly empty.
func (ch Channel) Group() string { return ch.group }

// Enabled reports whether the channel is listed and served.
func (ch Channel) Enabled() bool { return ch.enabled }

// Type returns the upstream type.
func (ch Channel) Type() UpstreamType { return ch.typ }

// Header returns the merged upstream request headers with canonical names.
// User-Agent is always present if [upstream] user_agent is set; it is
// empty if a layer removed it (spec 5.4), and then none is sent.
func (ch Channel) Header() http.Header { return ch.header.Clone() }

// TVGID returns the EPG id, possibly empty.
func (ch Channel) TVGID() string { return ch.tvgID }

// Logo returns the logo.
func (ch Channel) Logo() Logo { return ch.logo }

// Attrs returns the extra #EXTINF attributes sorted by key.
func (ch Channel) Attrs() []Attr { return slices.Clone(ch.attrs) }

// Extra returns the extra lines sorted by directive, then key.
func (ch Channel) Extra() []Extra { return slices.Clone(ch.extra) }

// Equal reports whether ch and o have the same merged model: every
// field a reload could change in what the proxy serves or lists (id,
// name, URL, route, group, enabled, type, headers, tvg id, logo, attrs,
// extra). The channel file does not count. Maps are compared by content
// and attrs and extra are sorted, so the order in the files is
// irrelevant.
func (ch Channel) Equal(o Channel) bool {
	return ch.id == o.id && ch.name == o.name && ch.url == o.url &&
		ch.route == o.route && ch.group == o.group && ch.enabled == o.enabled &&
		ch.typ == o.typ && ch.tvgID == o.tvgID && ch.logo == o.logo &&
		maps.EqualFunc(ch.header, o.header, slices.Equal[[]string]) &&
		slices.Equal(ch.attrs, o.attrs) && slices.Equal(ch.extra, o.extra)
}

// sortedAttrs converts an attribute map to a slice sorted by key.
func sortedAttrs(m map[string]string) []Attr {
	out := make([]Attr, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		out = append(out, Attr{Key: k, Value: m[k]})
	}
	return out
}

// sortedExtra converts directive -> key -> value maps to a sorted slice.
func sortedExtra(m map[string]map[string]string) []Extra {
	var out []Extra
	for _, d := range slices.Sorted(maps.Keys(m)) {
		for _, a := range sortedAttrs(m[d]) {
			out = append(out, Extra{Directive: d, Key: a.Key, Value: a.Value})
		}
	}
	return out
}
