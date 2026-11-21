// SPDX-License-Identifier: AGPL-3.0-or-later

package hls

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
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

// mediaXML are the root elements (lower-case local names) of XML media
// that players fetch as resources and the proxy streams: TTML subtitles.
// Every other XML body that is not a playlist the proxy rewrites is
// refused (FormatUnknown), HTML included.
var mediaXML = map[string]bool{"tt": true}

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
// starts like XML is FormatUnknown unless it is a playlist (its format,
// whatever its encoding or control bytes), one complete TTML document
// within XMLSniffLen bytes, or binary without a root element (media may
// start with '<'); UTF-16 or UTF-32 text is FormatUnknown too (fail
// closed).
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
	if hasMPDTag(prefix) {
		// Anywhere in the prefix: lighter player parsers keep leading
		// text or bytes as a text node and still take the MPD.
		return FormatDASH
	}
	text := textLike(prefix)
	body := stripBOMs(prefix)
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
		// Whatever text says: players read playlists in legacy
		// encodings and stop at a control byte only after the root.
		root, ok := xmlRoot(body)
		if !ok {
			// A printable AES-128 key may start with '<'; it is rarely
			// valid UTF-8.
			if text && (len(prefix) > maxBinaryTextLen || utf8.Valid(prefix)) {
				return FormatUnknown
			}
			// Binary without a root element: media such as AES
			// ciphertext or MP3 from mid-frame may start with '<',
			// but not like an XML prolog or a playlist root.
			return lexicalXML(body)
		}
		if f := xmlRoots[strings.ToLower(root)]; f != "" {
			return f
		}
		// Players look for a playlist root among all top-level elements
		// (dash.js and Shaka take a later MPD), so XML is streamed only
		// if it is provably harmless: whole within the sniff window, one
		// top-level element of a media format, nothing a lighter parser
		// could read differently.
		if text && mediaXML[strings.ToLower(root)] && len(prefix) < XMLSniffLen && singleElement(body) {
			return ""
		}
		return FormatUnknown
	case urlList(body):
		return FormatM3U
	case text && len(prefix) > maxBinaryTextLen && !webVTT(prefix):
		// Text that is no format the proxy handles is never a media
		// stream; a player's parser could find a playlist in it.
		return FormatUnknown
	}
	return ""
}

// maxBinaryTextLen is the longest body that is not classified as text
// even if all its bytes are printable: an AES-128 key (16 bytes) can be.
const maxBinaryTextLen = 16

// stripBOMs removes any number of leading UTF-8 byte order marks.
func stripBOMs(b []byte) []byte {
	for bytes.HasPrefix(b, []byte(utf8BOM)) {
		b = b[len(utf8BOM):]
	}
	return b
}

// textLike reports whether a body prefix is text: after any number of
// UTF-8 byte order marks, at least one byte, no control characters other
// than TAB, LF, CR and FF, and no DEL. Bytes from 0x80 are text in any
// encoding, so playlists in ISO-8859-1, Windows-1252 and other legacy
// encodings are text too. Media and ciphertext are not: a random byte is
// a control character with a probability of 29/256, so 512 random bytes
// avoid them with a probability of about 10^-27.
func textLike(prefix []byte) bool {
	b := stripBOMs(prefix)
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if c < 0x20 && c != '\t' && c != '\n' && c != '\r' && c != '\f' || c == 0x7f {
			return false
		}
	}
	return true
}

// hasMPDTag reports whether b contains "<mpd" or ":mpd" (any case)
// followed by a byte that cannot continue an XML name ([A-Za-z0-9._:-])
// or the end of b: the start of an MPD element, also with a namespace
// prefix. Lighter parsers end a name at any such byte ("<MPD=a" is an
// MPD element for them).
func hasMPDTag(b []byte) bool {
	for i := 0; i+4 <= len(b); i++ {
		if b[i] != '<' && b[i] != ':' || !strings.EqualFold(string(b[i+1:i+4]), "mpd") {
			continue
		}
		if i+4 == len(b) {
			return true
		}
		if c := b[i+4]; !isLetter(c) && (c < '0' || c > '9') && strings.IndexByte("._:-", c) < 0 {
			return true
		}
	}
	return false
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// webVTT reports whether a body prefix is WebVTT: "WEBVTT" after any
// number of UTF-8 byte order marks and whitespace.
func webVTT(prefix []byte) bool {
	return bytes.HasPrefix(bytes.TrimLeft(stripBOMs(prefix), " \t\r\n\v\f"), []byte("WEBVTT"))
}

// hasPrefixFold reports whether b starts with the ASCII string prefix,
// ignoring case.
func hasPrefixFold(b []byte, prefix string) bool {
	return len(b) >= len(prefix) && strings.EqualFold(string(b[:len(prefix)]), prefix)
}

// xmlRoot returns the local name of the root element of an XML document
// that starts at b (after an optional UTF-8 BOM), as encoding/xml reads
// it. ok is false if no root element starts within b, if b is not XML up
// to it, or if the prolog could hide another root element from players'
// lighter parsers, which end comments, processing instructions and
// declarations at other places: the prolog may hold only whitespace, the
// XML declaration, comments and a doctype without internal subset, none
// of them with '<' or '>' inside, and no comment starting with '-'. The
// declared encoding is ignored: element names are ASCII in every format
// concerned.
func xmlRoot(b []byte) (name string, ok bool) {
	d := xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(b, []byte(utf8BOM))))
	d.Strict = false
	d.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	for {
		t, err := d.RawToken()
		if err != nil {
			return "", false
		}
		switch t := t.(type) {
		case xml.StartElement:
			// RawToken returns a start tag only when it is complete.
			return t.Name.Local, true
		case xml.CharData:
			if len(bytes.Trim(t, " \t\r\n")) > 0 {
				return "", false
			}
		case xml.Comment:
			if bytes.HasPrefix(t, []byte("-")) || bytes.ContainsAny(t, "<>") {
				return "", false
			}
		case xml.ProcInst:
			if t.Target != "xml" || bytes.ContainsAny(t.Inst, "<>") {
				return "", false
			}
		case xml.Directive:
			if bytes.ContainsAny(t, "<>[") {
				return "", false
			}
		default:
			return "", false
		}
	}
}

// lexicalXML classifies the start of b, a body starting with '<' that
// xmlRoot cannot read (a legacy byte in an attribute or the declaration),
// without an XML parser: the format of a playlist root (xmlRoots) named
// first after an optional XML declaration, comments and whitespace
// (namespace prefix ignored, the name ends at any byte that cannot
// continue it or at the end of b); otherwise FormatUnknown if b starts
// with "<?xml", "<!--" or "<!DOCTYPE" (any case), else "". Random bytes
// match with a probability of about 10^-9 ("<asx" in any case).
func lexicalXML(b []byte) string {
	rest := b
	for {
		rest = bytes.TrimLeft(rest, " \t\r\n")
		end := -1
		switch {
		case hasPrefixFold(rest, "<?xml"):
			if i := bytes.Index(rest, []byte("?>")); i >= 0 {
				end = i + len("?>")
			}
		case bytes.HasPrefix(rest, []byte("<!--")):
			if i := bytes.Index(rest[len("<!--"):], []byte("-->")); i >= 0 {
				end = len("<!--") + i + len("-->")
			}
		}
		if end < 0 {
			break
		}
		rest = rest[end:]
	}
	if len(rest) > 1 && rest[0] == '<' {
		n := 1
		for n < len(rest) && (isLetter(rest[n]) || rest[n] >= '0' && rest[n] <= '9' || strings.IndexByte("._:-", rest[n]) >= 0) {
			n++
		}
		name := string(rest[1:n])
		if i := strings.LastIndexByte(name, ':'); i >= 0 {
			name = name[i+1:]
		}
		if f := xmlRoots[strings.ToLower(name)]; f != "" {
			return f
		}
	}
	if hasPrefixFold(b, "<?xml") || bytes.HasPrefix(b, []byte("<!--")) || hasPrefixFold(b, "<!doctype") {
		return FormatUnknown
	}
	return ""
}

// wideBOM reports whether b starts with a UTF-16 or UTF-32 byte order
// mark. The proxy reads no playlist in these encodings, and the checks
// of Detect and Unsupported would not see a playlist in them.
func wideBOM(b []byte) bool {
	return bytes.HasPrefix(b, []byte("\xfe\xff")) || bytes.HasPrefix(b, []byte("\xff\xfe")) ||
		bytes.HasPrefix(b, []byte("\x00\x00\xfe\xff")) || wideXML(b)
}

// wideXML reports whether b starts like XML in UTF-16 or UTF-32 without
// a byte order mark: '<' as a 16- or 32-bit code unit followed by an
// ASCII character, after UTF-16 whitespace. Media starts differ: an fMP4
// box size such as 00 00 00 3C is followed by its type, not by zeros.
func wideXML(b []byte) bool {
	be, le := b, b
	for len(be) >= 2 && be[0] == 0 && isXMLSpace(be[1]) {
		be = be[2:]
	}
	for len(le) >= 2 && isXMLSpace(le[0]) && le[1] == 0 {
		le = le[2:]
	}
	switch {
	case len(be) >= 4 && be[0] == 0 && be[1] == '<' && be[2] == 0 && be[3] != 0 && be[3] < 0x80:
		return true // UTF-16BE
	case len(le) >= 4 && le[0] == '<' && le[1] == 0 && le[2] != 0 && le[2] < 0x80 && le[3] == 0:
		return true // UTF-16LE
	case len(b) >= 8 && bytes.HasPrefix(b, []byte("\x00\x00\x00<\x00\x00\x00")) && b[7] != 0:
		return true // UTF-32BE
	case len(b) >= 8 && bytes.HasPrefix(b, []byte("<\x00\x00\x00")) && b[4] != 0 && bytes.Equal(b[5:8], []byte{0, 0, 0}):
		return true // UTF-32LE
	}
	return false
}

func isXMLSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

// xmlStart reports whether the body starting with prefix begins like
// XML: '<' after an optional UTF-8 BOM and whitespace.
func xmlStart(prefix []byte) bool {
	body := bytes.TrimLeft(bytes.TrimPrefix(prefix, []byte(utf8BOM)), " \t\r\n\v\f")
	return len(body) > 0 && body[0] == '<'
}

// XMLSniffLen is how much of a text body Sniff reads when it starts like
// XML or WebVTT.
const XMLSniffLen = 64 << 10

// Sniff returns the start of a body for Detect and Unsupported (spec
// 3.1, 3.3): SniffLen bytes, or the whole body if it is shorter; if it
// is text that starts like XML or WebVTT, up to XMLSniffLen bytes. rest
// reads the whole body, the returned bytes included; the caller reads
// from rest only. A read error other than io.EOF is returned.
func Sniff(body *bufio.Reader) (prefix []byte, rest *bufio.Reader, err error) {
	prefix, err = body.Peek(SniffLen)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, body, err
	}
	if len(prefix) < SniffLen || !xmlStart(prefix) && !webVTT(prefix) || !textLike(prefix) {
		return prefix, body, nil
	}
	// XML is classified by the whole document if it fits (Unsupported);
	// WebVTT, the one text format streamed, is searched for an MPD tag.
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

// singleElement reports whether the XML document b (after an optional
// UTF-8 BOM) is complete and holds exactly one top-level element,
// followed only by whitespace and comments, without anything players'
// lighter parsers end at other places: no processing instruction after
// the declaration, no doctype, no CDATA section, no '<' or '>' inside a
// tag, no comment starting with '-' or holding '<' or '>'. The prolog
// was checked by xmlRoot.
func singleElement(b []byte) bool {
	b = bytes.TrimPrefix(b, []byte(utf8BOM))
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = true
	d.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	depth, roots := 0, 0
	for off := int64(0); ; {
		t, err := d.RawToken()
		if errors.Is(err, io.EOF) {
			return roots == 1 && depth == 0
		}
		if err != nil {
			return false
		}
		raw := b[off:d.InputOffset()]
		off = d.InputOffset()
		switch t := t.(type) {
		case xml.StartElement:
			if depth == 0 {
				roots++
			}
			if roots > 1 || bytes.IndexByte(raw[1:], '<') >= 0 || bytes.IndexByte(raw, '>') != len(raw)-1 {
				return false
			}
			depth++
		case xml.EndElement:
			if len(raw) > 0 && (bytes.IndexByte(raw[1:], '<') >= 0 || bytes.IndexByte(raw, '>') != len(raw)-1) {
				return false
			}
			depth--
		case xml.CharData:
			if bytes.HasPrefix(raw, []byte("<")) {
				return false // CDATA
			}
			if depth == 0 && len(bytes.Trim(t, " \t\r\n")) > 0 {
				return false
			}
		case xml.Comment:
			if bytes.HasPrefix(t, []byte("-")) || bytes.ContainsAny(t, "<>") {
				return false
			}
		case xml.ProcInst:
			if roots > 0 || t.Target != "xml" {
				return false
			}
		default:
			return false
		}
	}
}
