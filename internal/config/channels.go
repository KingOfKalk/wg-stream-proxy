// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"golang.org/x/net/http/httpguts"
)

// maxURLLen is the maximum upstream URL length (spec 3.4).
const maxURLLen = 4096

// reservedAttrs are #EXTINF attributes generated from dedicated fields
// (spec 5.4); compared case-insensitively.
var reservedAttrs = map[string]bool{
	"tvg-id": true, "tvg-name": true, "tvg-logo": true, "group-title": true,
}

// layer is a validated [defaults.*] or channel layer.
type layer struct {
	typ     *UpstreamType
	headers map[string]string // canonical names
	tvgID   *string
	epgSrc  *string
	logo    *string
	attrs   map[string]string
	extra   map[string]map[string]string
}

// channelLoader builds channels from channel files.
type channelLoader struct {
	configDir string
	tunnels   map[string]bool
	userAgent string
	// epgSources holds the names of the [[epg.source]] entries.
	epgSources map[string]bool
	global     layer // channels.d/_defaults.toml
	errs       []error
	ids        map[string]string // id -> location of first use
	channels   []Channel
}

func (l *channelLoader) fail(prefix, format string, args ...any) {
	l.errs = append(l.errs, fmt.Errorf("%s: %s", prefix, fmt.Sprintf(format, args...)))
}

// loadGlobal validates channels.d/_defaults.toml and keeps it as the
// global layer (spec 5.4). It must be loaded before the channel files.
// The file has no channels, group or route: a global tunnel would route
// every channel file without a tunnel of its own (fail closed, spec 5.2).
func (l *channelLoader) loadGlobal(path string, data []byte) {
	var f rawGlobalFile
	if errs := decodeTOML(path, data, &f); errs != nil {
		l.errs = append(l.errs, errs...)
		return
	}
	if f.Tunnel != nil {
		l.fail(path, "tunnel: not allowed in %s: set it in each channel file or channel (no global route, fail closed)", globalDefaultsFile)
	}
	if f.Group != nil {
		l.fail(path, "group: not allowed in %s: set it in each channel file or channel", globalDefaultsFile)
	}
	if f.Channel != nil {
		l.fail(path, "[[channel]]: not allowed in %s: put channels into channel files", globalDefaultsFile)
	}
	if f.Defaults.Playlist.TVGID != nil {
		l.fail(path, "defaults.playlist.tvg_id: not allowed in %s: the EPG id is per channel", globalDefaultsFile)
		f.Defaults.Playlist.TVGID = nil
	}
	l.global = l.layer(path+": defaults.", f.Defaults.Upstream, f.Defaults.Playlist)
}

// route resolves an explicitly written tunnel value.
func (l *channelLoader) route(name string) (string, bool) {
	if name == RouteDirect || l.tunnels[name] {
		return name, true
	}
	return "", false
}

// loadFile validates one channel file and appends its channels.
func (l *channelLoader) loadFile(path string, data []byte) {
	var f rawChannelFile
	if errs := decodeTOML(path, data, &f); errs != nil {
		l.errs = append(l.errs, errs...)
		return
	}
	stem := strings.TrimSuffix(filepath.Base(path), ".toml")

	fileRoute, fileRouteOK := "", true
	if f.Tunnel != nil {
		fileRoute, fileRouteOK = l.route(*f.Tunnel)
		if !fileRouteOK {
			l.fail(path, "tunnel: unknown tunnel %q", *f.Tunnel)
		}
	}
	if f.Group != nil && hasLineBreak(*f.Group) {
		l.fail(path, "group: must not contain line breaks")
	}
	defaults := l.layer(path+": defaults.", f.Defaults.Upstream, f.Defaults.Playlist)

	for i, rc := range f.Channel {
		before := len(l.errs)
		where := fmt.Sprintf("%s channel %d", path, i+1)
		label := fmt.Sprintf("%s: channel %d", path, i+1)
		name := ""
		if rc.Name != nil {
			name = *rc.Name
		}
		if strings.TrimSpace(name) != "" {
			label += fmt.Sprintf(" (%q)", name)
		}

		ch := Channel{file: path, name: name, enabled: true, typ: TypeAuto}
		switch {
		case strings.TrimSpace(name) == "":
			l.fail(label, "name: required")
		case hasLineBreak(name):
			l.fail(label, "name: must not contain line breaks")
		}

		switch {
		case rc.URL == nil || *rc.URL == "":
			l.fail(label, "url: required")
		case len(*rc.URL) > maxURLLen:
			l.fail(label, "url: longer than %d bytes", maxURLLen)
		case !httpURL(*rc.URL):
			l.fail(label, "url: must be an absolute http(s) URL")
		default:
			ch.url = *rc.URL
		}

		switch {
		case rc.ID != nil:
			ch.id = *rc.ID
			if !ValidName(ch.id) {
				l.fail(label, "id: invalid id %q: use [a-z0-9-], 1-%d characters", ch.id, maxNameLen)
				ch.id = ""
			}
		case strings.TrimSpace(name) != "":
			ch.id = Slug(name)
			if ch.id == "" {
				l.fail(label, "id: name gives an empty slug, set id")
			}
		}
		if ch.id != "" {
			if prev, dup := l.ids[ch.id]; dup {
				l.fail(label, "id: duplicate channel id %q, also used by %s", ch.id, prev)
			} else {
				l.ids[ch.id] = where
			}
		}

		// Route, first match wins (spec 5.2); never direct by default.
		switch {
		case rc.Tunnel != nil:
			r, ok := l.route(*rc.Tunnel)
			if !ok {
				l.fail(label, "tunnel: unknown tunnel %q", *rc.Tunnel)
			}
			ch.route = r
		case f.Tunnel != nil:
			ch.route = fileRoute // an invalid file tunnel is reported once above
		case l.tunnels[stem]:
			ch.route = stem
		default:
			l.fail(label, "no route: set tunnel on the channel or at the top of the file, or add a tunnel file %q", stem+".conf")
		}

		// Group: channel, then file, then file stem.
		switch {
		case rc.Group != nil:
			ch.group = *rc.Group
			if hasLineBreak(ch.group) {
				l.fail(label, "group: must not contain line breaks")
			}
		case f.Group != nil:
			ch.group = *f.Group
		default:
			ch.group = stem
		}

		if rc.Enabled != nil {
			ch.enabled = *rc.Enabled
		}

		own := l.layer(label+": ", rc.Upstream, rc.Playlist)
		l.merge(&ch, l.global, defaults, own)

		if len(l.errs) == before && fileRouteOK {
			l.channels = append(l.channels, ch)
		}
	}
}

// checkEPGSources reports channels with the same tvg_id that name
// different EPG sources (spec 5.4): the served guide has one <channel>
// element per id, so it comes from one source.
func (l *channelLoader) checkEPGSources() {
	type first struct{ src, file, name string }
	byID := map[string]first{}
	for _, ch := range l.channels {
		if ch.tvgID == "" || ch.epgSrc == "" {
			continue
		}
		f, ok := byID[ch.tvgID]
		switch {
		case !ok:
			byID[ch.tvgID] = first{ch.epgSrc, ch.file, ch.name}
		case f.src != ch.epgSrc:
			l.fail(fmt.Sprintf("%s: channel %q", ch.file, ch.name),
				"playlist.epg_source: %q differs from %q of channel %q in %s with the same tvg_id %q",
				ch.epgSrc, f.src, f.name, f.file, ch.tvgID)
		}
	}
}

// merge applies config.toml, then layers (global defaults, file defaults,
// channel) in this order; the more specific layer wins, maps merge key by
// key (spec 5.4). An empty map value removes the key set by a lower
// layer: the header is not sent, the attr or extra line not written.
func (l *channelLoader) merge(ch *Channel, layers ...layer) {
	headers := map[string]string{}
	if l.userAgent != "" {
		headers["User-Agent"] = l.userAgent
	}
	attrs := map[string]string{}
	extra := map[string]map[string]string{}
	logo := ""
	for _, ly := range layers {
		if ly.typ != nil {
			ch.typ = *ly.typ
		}
		if ly.tvgID != nil {
			ch.tvgID = *ly.tvgID
		}
		if ly.epgSrc != nil {
			ch.epgSrc = *ly.epgSrc
		}
		if ly.logo != nil {
			logo = *ly.logo
		}
		maps.Copy(headers, ly.headers)
		maps.Copy(attrs, ly.attrs)
		for d, kv := range ly.extra {
			if extra[d] == nil {
				extra[d] = map[string]string{}
			}
			maps.Copy(extra[d], kv)
		}
	}
	// Opt-out (spec 5.4): drop removed keys. A removed User-Agent stays
	// as an empty value while [upstream] user_agent is set, so the
	// upstream request does not fall back to it and sends none.
	maps.DeleteFunc(headers, func(k, v string) bool {
		return v == "" && (k != "User-Agent" || l.userAgent == "")
	})
	maps.DeleteFunc(attrs, func(_, v string) bool { return v == "" })
	for _, kv := range extra {
		maps.DeleteFunc(kv, func(_, v string) bool { return v == "" })
	}
	ch.header = http.Header{}
	for k, v := range headers {
		ch.header[k] = []string{v}
	}
	ch.attrs = sortedAttrs(attrs)
	ch.extra = sortedExtra(extra)
	ch.logo = resolveLogo(l.configDir, logo)
}

// layer validates the upstream and playlist keys of one layer. prefix
// ends with ": " or "." and is prepended to key names in errors.
func (l *channelLoader) layer(prefix string, up rawUpstream, pl rawPlaylist) layer {
	fail := func(key, format string, args ...any) {
		l.errs = append(l.errs, fmt.Errorf("%s%s: %s", prefix, key, fmt.Sprintf(format, args...)))
	}
	var ly layer

	if up.Type != nil {
		t := UpstreamType(*up.Type)
		if t == TypeAuto || t == TypeHLS || t == TypeDASH || t == TypeRaw {
			ly.typ = &t
		} else {
			fail("upstream.type", "invalid value %q: use auto, hls, dash or raw", *up.Type)
		}
	}

	ly.headers = map[string]string{}
	for _, k := range slices.Sorted(maps.Keys(up.Headers)) {
		v := up.Headers[k]
		ck := http.CanonicalHeaderKey(k)
		switch {
		case !ValidHeaderName(k):
			fail("upstream.headers", "invalid header name %q", k)
		case !ValidHeaderValue(v):
			fail("upstream.headers", "invalid value for header %q", ck)
		case ck == "Host":
			fail("upstream.headers", "%q is not allowed: net/http sends the host of the URL, a Host header would have no effect", ck)
		case ck == "Accept-Encoding":
			fail("upstream.headers", "%q is not allowed: it turns off the transparent decompression of the transport", ck)
		default:
			if _, dup := ly.headers[ck]; dup {
				fail("upstream.headers", "duplicate header %q (names are case-insensitive)", ck)
			}
			ly.headers[ck] = v
		}
	}

	if pl.TVGID != nil && hasLineBreak(*pl.TVGID) {
		fail("playlist.tvg_id", "must not contain line breaks")
	}
	if pl.Logo != nil && hasLineBreak(*pl.Logo) {
		fail("playlist.logo", "must not contain line breaks")
	}
	ly.tvgID, ly.logo = pl.TVGID, pl.Logo
	if v := pl.EPGSource; v != nil {
		// Empty: no source of its own, the first one that has the
		// tvg_id (also over a lower layer's value).
		if *v != "" && !l.epgSources[*v] {
			fail("playlist.epg_source", "unknown EPG source %q (want the name of an [[epg.source]] in config.toml)", *v)
		} else {
			ly.epgSrc = v
		}
	}

	ly.attrs = map[string]string{}
	for _, k := range slices.Sorted(maps.Keys(pl.Attrs)) {
		v := pl.Attrs[k]
		switch {
		case !validAttrName(k):
			fail("playlist.attrs", "invalid attribute name %q", k)
		case reservedAttrs[strings.ToLower(k)]:
			fail("playlist.attrs", "%q is reserved, use the dedicated field", k)
		case hasLineBreak(v):
			fail("playlist.attrs", "value of %q must not contain line breaks", k)
		default:
			ly.attrs[k] = v
		}
	}

	ly.extra = map[string]map[string]string{}
	for _, d := range slices.Sorted(maps.Keys(pl.Extra)) {
		switch {
		case !validDirective(d):
			fail("playlist.extra", "invalid directive %q: use [A-Z][A-Z0-9-]*", d)
			continue
		case d == "EXTM3U" || d == "EXTINF" || d == "EXTGRP" || strings.HasPrefix(d, "EXT-X-"):
			fail("playlist.extra", "directive %q is forbidden", d)
			continue
		}
		kv := map[string]string{}
		for _, k := range slices.Sorted(maps.Keys(pl.Extra[d])) {
			v := pl.Extra[d][k]
			switch {
			case k == "":
				fail("playlist.extra."+d, "empty key")
			case hasLineBreak(k):
				fail("playlist.extra."+d, "key %q must not contain line breaks", k)
			case hasLineBreak(v):
				fail("playlist.extra."+d, "value of %q must not contain line breaks", k)
			default:
				kv[k] = v
			}
		}
		ly.extra[d] = kv
	}
	return ly
}

// resolveLogo classifies a logo value (spec 5.4).
func resolveLogo(configDir, v string) Logo {
	lower := strings.ToLower(v)
	switch {
	case v == "":
		return Logo{}
	case strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://"):
		return Logo{Kind: LogoURL, Value: v}
	case v != "." && v != ".." && !strings.ContainsAny(v, `/\`):
		if fi, err := os.Stat(filepath.Join(logosDir(configDir), v)); err == nil && fi.Mode().IsRegular() {
			return Logo{Kind: LogoLocal, Value: v}
		}
	}
	return Logo{Kind: LogoRaw, Value: v}
}

// ValidHeaderName reports whether s is an HTTP token (RFC 9110 5.6.2).
func ValidHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isAlnum(c) && strings.IndexByte("!#$%&'*+-.^_`|~", c) < 0 {
			return false
		}
	}
	return true
}

// ValidHeaderValue reports whether s is a valid HTTP header field value:
// no control characters except tab (httpguts.ValidHeaderFieldValue), so
// net/http can send it. The empty value is valid (the opt-out, spec 5.4).
func ValidHeaderValue(s string) bool {
	return httpguts.ValidHeaderFieldValue(s)
}

// validAttrName reports whether s can be written as name="value" in an
// #EXTINF line: not empty, no whitespace, control characters, '"', '='
// or ','.
func validAttrName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == '"' || r == '=' || r == ',' {
			return false
		}
	}
	return true
}

// validDirective reports whether s matches [A-Z][A-Z0-9-]*.
func validDirective(s string) bool {
	if s == "" || s[0] < 'A' || s[0] > 'Z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
