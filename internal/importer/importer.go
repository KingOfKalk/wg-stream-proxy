// SPDX-License-Identifier: AGPL-3.0-or-later

// Package importer converts an M3U channel list into a channel file for
// channels.d (spec 7.2 import). It is pure: it reads bytes and returns the
// TOML text and warnings; the caller does all I/O.
package importer

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/http/httpguts"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/epg"
)

// maxURLLen is the maximum upstream URL length (spec 3.4).
const maxURLLen = 4096

// Options control the output.
type Options struct {
	// Tunnel is written as the file-level tunnel if set: a tunnel name or
	// "direct". Without it the route follows spec 5.2 (a tunnel named
	// like the channel file stem).
	Tunnel string
	// EPG, if set, turns on --match-epg: a tvg_id is kept only if the EPG
	// has it; otherwise the best suggestion is written as a comment.
	EPG *epg.Index
}

// Warning is a line that was dropped or only partly converted.
type Warning struct {
	Line int // 1-based line number in the M3U file
	Msg  string
}

// String returns "line N: message".
func (w Warning) String() string { return fmt.Sprintf("line %d: %s", w.Line, w.Msg) }

// Result is the outcome of an import.
type Result struct {
	TOML     []byte // channel file; nil on error
	Warnings []Warning
}

// ErrNoChannels is returned if the input contains no importable channel.
var ErrNoChannels = errors.New("no channels found")

// Import converts an M3U file into a channel file (spec 7.2). Warnings
// are returned sorted by line, also together with an error.
func Import(data []byte, opts Options) (Result, error) {
	if opts.Tunnel != "" && opts.Tunnel != config.RouteDirect && !config.ValidName(opts.Tunnel) {
		return Result{}, fmt.Errorf("invalid tunnel name %q: use [a-z0-9-], 1-48 characters", opts.Tunnel)
	}
	doc, warns := parse(data)
	if len(doc.channels) == 0 {
		return Result{Warnings: warns}, ErrNoChannels
	}
	assignIDs(doc.channels)
	if opts.EPG != nil {
		warns = append(warns, matchEPG(doc.channels, opts.EPG)...)
		slices.SortStableFunc(warns, func(a, b Warning) int { return cmp.Compare(a.Line, b.Line) })
	}
	return Result{TOML: render(doc, opts), Warnings: warns}, nil
}

// attr is one NAME=VALUE attribute of an #EXTM3U or #EXTINF line.
type attr struct{ key, value string }

// directive is a #<DIRECTIVE>:key=value line.
type directive struct {
	line             int
	name, key, value string
}

// channel is one imported channel. Empty strings mean "not set".
type channel struct {
	line    int // line of the #EXTINF
	name    string
	url     string
	id      string // set only if the slug of name cannot be used
	group   string
	enabled bool
	tvgID   string
	tvgHint string // comment line suggesting a tvg_id (--match-epg)
	logo    string
	headers map[string]string // canonical names
	attrs   map[string]string
	extra   map[string]map[string]string
}

// document is a parsed M3U file.
type document struct {
	epgURLs  []string // from the #EXTM3U header, deduplicated
	channels []channel
}

// entry is a channel whose URL line has not been seen yet.
type entry struct {
	channel
	drop bool // invalid #EXTINF: consume its lines, output nothing
	dirs []directive
}

type parser struct {
	doc   document
	warns []Warning
	cur   *entry      // after #EXTINF, until its URL
	dirs  []directive // directive lines before the next #EXTINF
}

func (p *parser) warn(line int, format string, args ...any) {
	p.warns = append(p.warns, Warning{Line: line, Msg: fmt.Sprintf(format, args...)})
}

var bom = []byte("\xef\xbb\xbf")

// parse reads all channels of an M3U file.
func parse(data []byte) (document, []Warning) {
	p := &parser{}
	for i, line := range splitLines(bytes.TrimPrefix(data, bom)) {
		p.line(i+1, line)
	}
	if p.cur != nil {
		p.missingURL()
	}
	for _, d := range p.dirs {
		p.warn(d.line, "#%s line without a following channel dropped", d.name)
	}
	slices.SortStableFunc(p.warns, func(a, b Warning) int { return cmp.Compare(a.Line, b.Line) })
	return p.doc, p.warns
}

// splitLines splits at LF, CRLF and lone CR.
func splitLines(data []byte) []string {
	var lines []string
	start := 0
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '\n':
			lines = append(lines, string(data[start:i]))
			start = i + 1
		case '\r':
			lines = append(lines, string(data[start:i]))
			if i+1 < len(data) && data[i+1] == '\n' {
				i++
			}
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, string(data[start:]))
	}
	return lines
}

func (p *parser) line(n int, s string) {
	if !utf8.ValidString(s) {
		p.warn(n, "invalid UTF-8 replaced with U+FFFD")
		s = strings.ToValidUTF8(s, "�")
	}
	if strings.ContainsFunc(s, func(r rune) bool { return r != '\t' && unicode.IsControl(r) }) {
		p.warn(n, "line with control characters dropped")
		return
	}
	s = strings.TrimSpace(s)
	switch {
	case s == "":
	case s == "#EXTM3U" || strings.HasPrefix(s, "#EXTM3U ") || strings.HasPrefix(s, "#EXTM3U\t"):
		p.header(n, s[len("#EXTM3U"):])
	case strings.HasPrefix(s, "#EXTINF:"):
		p.extinf(n, s)
	case s[0] == '#' && hasHTTPPrefix(s[1:]):
		// Commented-out URL: the channel is imported disabled.
		p.url(n, s[1:], false)
	case s[0] == '#':
		p.directive(n, s)
	default:
		p.url(n, s, true)
	}
}

func hasHTTPPrefix(s string) bool {
	s = strings.ToLower(s)
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// header handles the attributes of #EXTM3U. The EPG URL belongs in
// config.toml and is written as a comment.
func (p *parser) header(n int, s string) {
	attrs, _, ok := parseAttrs(s)
	if !ok {
		p.warn(n, "malformed #EXTM3U attributes dropped")
	}
	for _, a := range attrs {
		switch strings.ToLower(a.key) {
		case "url-tvg", "x-tvg-url":
			if a.value != "" && !slices.Contains(p.doc.epgURLs, a.value) {
				p.doc.epgURLs = append(p.doc.epgURLs, a.value)
			}
		default:
			p.warn(n, "#EXTM3U attribute %q dropped", a.key)
		}
	}
}

func (p *parser) extinf(n int, s string) {
	if p.cur != nil {
		p.missingURL()
	}
	e := &entry{
		channel: channel{
			line: n, enabled: true,
			headers: map[string]string{}, attrs: map[string]string{}, extra: map[string]map[string]string{},
		},
		dirs: p.dirs,
	}
	p.dirs = nil
	p.cur = e

	name, attrs, ok := parseEXTINF(s)
	if !ok {
		p.warn(n, "malformed #EXTINF attributes: the rest of the list is dropped, the name starts after the next comma")
	}
	e.name = name
	if name == "" {
		p.warn(n, "#EXTINF without a channel name: channel dropped")
		e.drop = true
	}
	for _, a := range attrs {
		switch strings.ToLower(a.key) {
		case "tvg-id":
			e.tvgID = a.value
		case "tvg-logo":
			e.logo = a.value
		case "group-title":
			e.group = a.value
		case "tvg-name":
			// Generated from name when the channel list is served.
		default:
			if !validAttrName(a.key) {
				p.warn(n, "invalid attribute name %q dropped", a.key)
				continue
			}
			e.attrs[a.key] = a.value
		}
	}
}

func (p *parser) missingURL() {
	if !p.cur.drop {
		p.warn(p.cur.line, "#EXTINF without a URL line: channel dropped")
	}
	p.cur = nil
}

func (p *parser) directive(n int, s string) {
	name, key, value, ok := parseDirective(s)
	switch {
	case !ok && (len(s) == 1 || s[1] < 'A' || s[1] > 'Z'):
		p.warn(n, "comment dropped")
		return
	case !ok:
		p.warn(n, "line dropped: not #DIRECTIVE:key=value")
		return
	case name == "EXTM3U" || name == "EXTINF" || name == "EXTGRP" || strings.HasPrefix(name, "EXT-X-"):
		p.warn(n, "#%s line dropped: not allowed in playlist.extra", name)
		return
	}
	d := directive{line: n, name: name, key: key, value: value}
	if p.cur != nil {
		p.cur.dirs = append(p.cur.dirs, d)
	} else {
		p.dirs = append(p.dirs, d)
	}
}

// parseDirective splits #<DIRECTIVE>:key=value.
func parseDirective(s string) (name, key, value string, ok bool) {
	name, kv, found := strings.Cut(s[1:], ":")
	if !found || !validDirective(name) {
		return "", "", "", false
	}
	key, value, found = strings.Cut(kv, "=")
	if !found || key == "" {
		return "", "", "", false
	}
	return name, key, value, true
}

// url completes the current channel with a URL line, optionally followed
// by |Header=Value pairs.
func (p *parser) url(n int, s string, enabled bool) {
	e := p.cur
	p.cur = nil
	if e == nil {
		p.dirs = nil
		if enabled {
			p.warn(n, "URL without #EXTINF dropped")
		} else {
			p.warn(n, "commented-out URL without #EXTINF dropped")
		}
		return
	}
	if e.drop {
		return
	}
	raw, pipe, hasPipe := strings.Cut(s, "|")
	raw = strings.TrimSpace(raw)
	switch {
	case len(raw) > maxURLLen:
		p.warn(n, "URL longer than %d bytes: channel dropped", maxURLLen)
		return
	case !httpURL(raw):
		p.warn(n, "not an absolute http(s) URL: channel dropped")
		return
	}
	e.url = raw
	e.enabled = enabled

	for _, d := range e.dirs {
		switch {
		case d.name == "KODIPROP" && d.key == "inputstream.adaptive.stream_headers":
			p.headerList(&e.channel, d.line, d.value)
		case d.name == "EXTVLCOPT" && d.key == "http-user-agent":
			p.setHeader(&e.channel, d.line, "User-Agent", d.value)
		case d.name == "EXTVLCOPT" && d.key == "http-referrer":
			p.setHeader(&e.channel, d.line, "Referer", d.value)
		default:
			if e.extra[d.name] == nil {
				e.extra[d.name] = map[string]string{}
			}
			e.extra[d.name][d.key] = d.value
		}
	}
	if hasPipe {
		p.headerList(&e.channel, n, pipe)
	}
	p.doc.channels = append(p.doc.channels, e.channel)
}

// httpURL reports whether s is an absolute http(s) URL, like config does.
func httpURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// headerList adds Name=Value pairs separated by "&"; values are
// percent-decoded.
func (p *parser) headerList(ch *channel, n int, s string) {
	for part := range strings.SplitSeq(s, "&") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		name, value, ok := strings.Cut(part, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			p.warn(n, "header without Name=Value dropped")
			continue
		}
		// Values that do not decode, or not to UTF-8 (TOML strings are
		// UTF-8), are kept as written.
		if v, err := url.PathUnescape(value); err == nil && utf8.ValidString(v) {
			value = v
		}
		p.setHeader(ch, n, name, value)
	}
}

// setHeader sets a header; a later value replaces an earlier one.
func (p *parser) setHeader(ch *channel, n int, name, value string) {
	value = strings.TrimSpace(value)
	switch {
	case !httpguts.ValidHeaderFieldName(name):
		p.warn(n, "invalid header name %q dropped", name)
	case !httpguts.ValidHeaderFieldValue(value):
		p.warn(n, "invalid value for header %q dropped", http.CanonicalHeaderKey(name))
	default:
		ch.headers[http.CanonicalHeaderKey(name)] = value
	}
}

// parseEXTINF returns the name and attributes of an #EXTINF line. The
// duration is ignored. ok is false if the attribute list is malformed;
// the attributes before the error are returned and the name is the text
// after the next comma.
func parseEXTINF(line string) (name string, attrs []attr, ok bool) {
	s := strings.TrimPrefix(line, "#EXTINF:")
	i := strings.IndexAny(s, " \t,")
	if i < 0 {
		return "", nil, true
	}
	attrs, name, ok = parseAttrs(s[i:])
	return name, attrs, ok
}

// parseAttrs reads NAME="VALUE" or NAME=VALUE attributes separated by
// whitespace up to the first comma outside quotes; the rest is the name.
func parseAttrs(s string) (attrs []attr, name string, ok bool) {
	isSpace := func(c byte) bool { return c == ' ' || c == '\t' }
	pos := 0
	for {
		for pos < len(s) && isSpace(s[pos]) {
			pos++
		}
		if pos == len(s) {
			return attrs, "", true
		}
		if s[pos] == ',' {
			return attrs, strings.TrimSpace(s[pos+1:]), true
		}
		start := pos
		for pos < len(s) && !isSpace(s[pos]) && strings.IndexByte(`=,"`, s[pos]) < 0 {
			pos++
		}
		if pos == start || pos == len(s) || s[pos] != '=' {
			return attrs, nameAfterComma(s[start:]), false
		}
		key := s[start:pos]
		pos++
		var value string
		if pos < len(s) && s[pos] == '"' {
			end := strings.IndexByte(s[pos+1:], '"')
			if end < 0 {
				return attrs, nameAfterComma(s[start:]), false
			}
			value = s[pos+1 : pos+1+end]
			pos += end + 2
		} else {
			vs := pos
			for pos < len(s) && !isSpace(s[pos]) && s[pos] != ',' {
				pos++
			}
			value = s[vs:pos]
		}
		if pos < len(s) && !isSpace(s[pos]) && s[pos] != ',' {
			return attrs, nameAfterComma(s[start:]), false
		}
		attrs = append(attrs, attr{key, value})
	}
}

func nameAfterComma(s string) string {
	_, name, _ := strings.Cut(s, ",")
	return strings.TrimSpace(name)
}

// validAttrName mirrors the playlist.attrs name rule of spec 5.4.
func validAttrName(s string) bool {
	return s != "" && !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || r == '"' || r == '=' || r == ','
	})
}

// validDirective reports whether s matches [A-Z][A-Z0-9-]* (spec 5.4).
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
