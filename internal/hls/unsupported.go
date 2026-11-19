// SPDX-License-Identifier: AGPL-3.0-or-later

package hls

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"errors"
	"io"
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
	// FormatUnknown is a body the proxy cannot classify safely: it starts
	// like XML but its root element is not found within XMLSniffLen
	// bytes (see Sniff), or it is UTF-16 or UTF-32 text (byte order
	// mark). Refused, fail closed.
	FormatUnknown = "unknown"
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
// body. prefix is the start of the body as Sniff returns it; a body that
// starts like XML without a root element in prefix, or with a UTF-16 or
// UTF-32 byte order mark, is FormatUnknown (fail closed).
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
	if wideBOM(prefix) {
		return FormatUnknown
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
		root, ok := xmlRoot(body)
		if !ok {
			return FormatUnknown
		}
		return xmlRoots[strings.ToLower(root)]
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
// that starts at b (after an optional UTF-8 BOM), as encoding/xml reads
// it: after the XML declaration, processing instructions, comments and a
// doctype with its internal subset. ok is false if no root element
// starts within b or b is not XML up to it. The declared encoding is
// ignored: element names are ASCII in every format concerned.
func xmlRoot(b []byte) (name string, ok bool) {
	d := xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(b, []byte(utf8BOM))))
	d.Strict = false
	d.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	for {
		t, err := d.RawToken()
		if err != nil {
			return "", false
		}
		if se, ok := t.(xml.StartElement); ok {
			// RawToken returns a start tag only when it is complete.
			return se.Name.Local, true
		}
	}
}

// wideBOM reports whether b starts with a UTF-16 or UTF-32 byte order
// mark. The proxy reads no playlist in these encodings, and the checks
// of Detect and Unsupported would not see a playlist in them.
func wideBOM(b []byte) bool {
	return bytes.HasPrefix(b, []byte("\xfe\xff")) || bytes.HasPrefix(b, []byte("\xff\xfe")) ||
		bytes.HasPrefix(b, []byte("\x00\x00\xfe\xff"))
}

// xmlStart reports whether the body starting with prefix begins like
// XML: '<' after an optional UTF-8 BOM and whitespace.
func xmlStart(prefix []byte) bool {
	body := bytes.TrimLeft(bytes.TrimPrefix(prefix, []byte(utf8BOM)), " \t\r\n\v\f")
	return len(body) > 0 && body[0] == '<'
}

// XMLSniffLen is how much of a body Sniff reads when it starts like XML
// but its root element does not start within the first SniffLen bytes
// (a long comment or doctype).
const XMLSniffLen = 64 << 10

// Sniff returns the start of a body for Detect and Unsupported (spec
// 3.1, 3.3): SniffLen bytes, or the whole body if it is shorter; if it
// starts like XML and its root element does not start within them, up to
// XMLSniffLen bytes. rest reads the whole body, the returned bytes
// included; the caller reads from rest only. A read error other than
// io.EOF is returned.
func Sniff(body *bufio.Reader) (prefix []byte, rest *bufio.Reader, err error) {
	prefix, err = body.Peek(SniffLen)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, body, err
	}
	if len(prefix) < SniffLen || !xmlStart(prefix) {
		return prefix, body, nil
	}
	if _, ok := xmlRoot(prefix); ok {
		return prefix, body, nil
	}
	rest = bufio.NewReaderSize(body, XMLSniffLen)
	prefix, err = rest.Peek(XMLSniffLen)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, rest, err
	}
	return prefix, rest, nil
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
