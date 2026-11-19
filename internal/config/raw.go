// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// rawConfig is the TOML structure of config.toml (spec 5.3). Pointers
// distinguish unset keys from empty values.
type rawConfig struct {
	Listen        *string     `toml:"listen"`
	LogLevel      *string     `toml:"log_level"`
	LogFormat     *string     `toml:"log_format"`
	PublicBaseURL *string     `toml:"public_base_url"`
	Upstream      rawSettings `toml:"upstream"`
	Tunnels       struct {
		IdleTimeout *string `toml:"idle_timeout"`
	} `toml:"tunnels"`
	Clips struct {
		Error    *bool   `toml:"error"`
		Inflight *bool   `toml:"inflight"`
		Grace    *string `toml:"grace"`
	} `toml:"clips"`
	Reload struct {
		WatchInterval *string `toml:"watch_interval"`
	} `toml:"reload"`
	EPG struct {
		URL *string `toml:"url"`
	} `toml:"epg"`
	Debug struct {
		Listen *string `toml:"listen"`
	} `toml:"debug"`
}

type rawSettings struct {
	UserAgent       *string             `toml:"user_agent"`
	ConnectTimeout  *string             `toml:"connect_timeout"`
	HeaderTimeout   *string             `toml:"header_timeout"`
	ReadTimeout     *string             `toml:"read_timeout"`
	MaxPlaylistSize *string             `toml:"max_playlist_size"`
	AllowPrivate    map[string][]string `toml:"allow_private"`
}

// rawChannelFile is the TOML structure of a channel file (spec 5.4).
type rawChannelFile struct {
	Group    *string      `toml:"group"`
	Tunnel   *string      `toml:"tunnel"`
	Defaults rawLayer     `toml:"defaults"`
	Channel  []rawChannel `toml:"channel"`
}

// rawGlobalFile is the TOML structure of channels.d/_defaults.toml
// (spec 5.4). Group, Tunnel and Channel are not allowed there; they are
// decoded only to report them with a clear message.
type rawGlobalFile struct {
	Group    any      `toml:"group"`
	Tunnel   any      `toml:"tunnel"`
	Channel  any      `toml:"channel"`
	Defaults rawLayer `toml:"defaults"`
}

// rawLayer holds the keys shared by [defaults.*] and a channel.
type rawLayer struct {
	Upstream rawUpstream `toml:"upstream"`
	Playlist rawPlaylist `toml:"playlist"`
}

type rawChannel struct {
	Name     *string     `toml:"name"`
	URL      *string     `toml:"url"`
	ID       *string     `toml:"id"`
	Tunnel   *string     `toml:"tunnel"`
	Group    *string     `toml:"group"`
	Enabled  *bool       `toml:"enabled"`
	Upstream rawUpstream `toml:"upstream"`
	Playlist rawPlaylist `toml:"playlist"`
}

type rawUpstream struct {
	Type    *string           `toml:"type"`
	Headers map[string]string `toml:"headers"`
}

type rawPlaylist struct {
	TVGID *string                      `toml:"tvg_id"`
	Logo  *string                      `toml:"logo"`
	Attrs map[string]string            `toml:"attrs"`
	Extra map[string]map[string]string `toml:"extra"`
}

// goField matches the Go struct field part of go-toml type errors.
var goField = regexp.MustCompile(`into struct field \S+ of type `)

// decodeTOML decodes data into v and rejects unknown keys. Errors carry
// path, line and column.
func decodeTOML(path string, data []byte, v any) []error {
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil {
		return nil
	}
	var strict *toml.StrictMissingError
	if errors.As(err, &strict) {
		out := make([]error, 0, len(strict.Errors))
		for i := range strict.Errors {
			e := &strict.Errors[i]
			row, col := e.Position()
			out = append(out, fmt.Errorf("%s:%d:%d: unknown key %q", path, row, col, strings.Join(e.Key(), ".")))
		}
		return out
	}
	var de *toml.DecodeError
	if errors.As(err, &de) {
		row, col := de.Position()
		// Type errors name Go struct fields; keep only the TOML side.
		msg := goField.ReplaceAllString(strings.TrimPrefix(de.Error(), "toml: "), "into ")
		if k := de.Key(); len(k) > 0 {
			msg = fmt.Sprintf("key %q: %s", strings.Join(k, "."), msg)
		}
		if slices.Contains(de.Key(), "extra") && strings.Contains(msg, "table") {
			msg += " (dotted keys in playlist.extra must be quoted)"
		}
		return []error{fmt.Errorf("%s:%d:%d: %s", path, row, col, msg)}
	}
	return []error{fmt.Errorf("%s: %w", path, err)}
}
