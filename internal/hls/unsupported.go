// SPDX-License-Identifier: AGPL-3.0-or-later

package hls

import (
	"bytes"
	"strings"
)

// Playlist formats the proxy does not rewrite (spec 3.3), as returned by
// Unsupported.
const (
	FormatPLS  = "pls"
	FormatM3U  = "m3u"
	FormatDASH = "dash"
	FormatXSPF = "xspf"
	FormatASX  = "asx"
)

// unsupportedTypes maps the Content-Types of playlists the proxy does not
// rewrite to their format. video/x-ms-asf is left out: it is also the
// type of ASF media.
var unsupportedTypes = map[string]string{
	"audio/x-scpls":        FormatPLS,
	"audio/scpls":          FormatPLS,
	"application/pls+xml":  FormatPLS,
	"audio/x-mpegurl":      FormatM3U,
	"audio/mpegurl":        FormatM3U,
	"application/dash+xml": FormatDASH,
	"application/xspf+xml": FormatXSPF,
	"video/x-ms-asx":       FormatASX,
	"audio/x-ms-asx":       FormatASX,
	"video/x-ms-wvx":       FormatASX,
	"audio/x-ms-wax":       FormatASX,
}

// xmlRoots maps the lower-case local name of the root element of XML
// playlists to their format.
var xmlRoots = map[string]string{
	"mpd":      FormatDASH,
	"playlist": FormatXSPF,
	"asx":      FormatASX,
}

// Unsupported returns the format of a playlist the proxy does not rewrite
// (spec 3.3), or "" for anything else. The caller passes responses Detect
// classified as Raw: piping such a playlist would hand the player
// upstream URLs to fetch directly, past the channel's tunnel.
//
// A configured HLS or DASH type is never refused: the rewriter handles
// the body. On an auto channel, Detect classifies DASH manifests (format
// "dash") as DASH, so only raw channels refuse them.
// A configured Raw type ignores the Content-Type, like Detect, but not the
// body. prefix is the start of the body (see SniffLen).
func Unsupported(contentType string, prefix []byte, configured Type) string {
	if configured == HLS || configured == DASH {
		return ""
	}
	if configured != Raw {
		mediaType, _, _ := strings.Cut(contentType, ";")
		if f, ok := unsupportedTypes[strings.ToLower(strings.TrimSpace(mediaType))]; ok {
			return f
		}
	}
	body := bytes.TrimPrefix(prefix, []byte(utf8BOM))
	body = bytes.TrimLeft(body, " \t\r\n\v\f")
	switch {
	case hasPrefixFold(body, "[playlist]"):
		return FormatPLS
	case hasPrefixFold(body, "[reference]"):
		// The ASF reference file, the INI-style form of ASX.
		return FormatASX
	case bytes.HasPrefix(body, []byte("#EXTM3U")):
		// Only on a raw channel; Detect calls it HLS otherwise.
		return FormatM3U
	case len(body) > 0 && body[0] == '<':
		return xmlRoots[strings.ToLower(xmlRoot(body))]
	case urlList(body):
		return FormatM3U
	}
	return ""
}

// hasPrefixFold reports whether b starts with the ASCII string prefix,
// ignoring case.
func hasPrefixFold(b []byte, prefix string) bool {
	return len(b) >= len(prefix) && strings.EqualFold(string(b[:len(prefix)]), prefix)
}

// xmlRoot returns the local name of the root element of an XML document
// that starts at b, skipping the XML declaration, processing
// instructions, comments, a doctype and whitespace; "" if the root
// element does not start within b.
func xmlRoot(b []byte) string {
	for {
		b = bytes.TrimLeft(b, " \t\r\n")
		var end string
		switch {
		case bytes.HasPrefix(b, []byte("<?")):
			end = "?>"
		case bytes.HasPrefix(b, []byte("<!--")):
			end = "-->"
		case bytes.HasPrefix(b, []byte("<!")):
			end = ">"
		case bytes.HasPrefix(b, []byte("<")):
			name := b[1:]
			i := bytes.IndexAny(name, " \t\r\n/>")
			if i < 0 {
				return "" // the name may go on beyond the prefix
			}
			name = name[:i]
			if i := bytes.LastIndexByte(name, ':'); i >= 0 {
				name = name[i+1:]
			}
			return string(name)
		default:
			return ""
		}
		i := bytes.Index(b, []byte(end))
		if i < 0 {
			return ""
		}
		b = b[i+len(end):]
	}
}

// urlList reports whether the first line of b that is neither blank nor a
// # comment is an absolute or scheme-relative URL: a plain M3U playlist.
// Relative lines are not detected; the player resolves them against the
// proxy URL, not the upstream.
func urlList(b []byte) bool {
	for len(b) > 0 {
		// Lines end at LF, CRLF or a lone CR, as in HLS (spec 3.2).
		end := bytes.IndexAny(b, "\r\n")
		line := b
		if end >= 0 {
			line = b[:end]
		}
		line = bytes.Trim(line, " \t")
		if len(line) > 0 && line[0] != '#' {
			return isURLLine(line)
		}
		if end < 0 {
			return false // a comment cut off by the prefix
		}
		b = b[end+1:]
	}
	return false
}

// isURLLine reports whether line is "scheme://..." with a scheme of at
// least two characters, or "//...", without control characters up to its
// end (or the end of the prefix).
func isURLLine(line []byte) bool {
	i := 0
	for i < len(line) && isSchemeChar(line[i], i == 0) {
		i++
	}
	switch {
	case i >= 2 && bytes.HasPrefix(line[i:], []byte("://")):
	case i == 0 && bytes.HasPrefix(line, []byte("//")):
	default:
		return false
	}
	for _, c := range line {
		if c < 0x20 && c != '\t' || c == 0x7f {
			return false
		}
	}
	return true
}

// isSchemeChar reports whether c may appear in a URI scheme (RFC 3986):
// a letter first, then letters, digits, "+", "-" and ".".
func isSchemeChar(c byte, first bool) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		return true
	case first:
		return false
	case c >= '0' && c <= '9', c == '+', c == '-', c == '.':
		return true
	}
	return false
}
