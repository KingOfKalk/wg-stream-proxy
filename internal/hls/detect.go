// SPDX-License-Identifier: AGPL-3.0-or-later

package hls

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
)

// Type is the stream type of a channel or resource (spec 3.1).
type Type uint8

const (
	// Auto detects the type from the response (upstream.type = "auto").
	Auto Type = iota
	// HLS is a playlist that gets rewritten.
	HLS
	// Raw is piped through unchanged.
	Raw
	// DASH is an MPEG-DASH manifest (MPD) that gets rewritten.
	DASH
)

// SniffLen is the number of leading body bytes Detect needs to recognise a
// playlist. Callers pass at least this many bytes, or the whole body if it
// is shorter.
const SniffLen = 512

const utf8BOM = "\xef\xbb\xbf"

// ParseType parses a configured upstream.type. The empty string means Auto.
func ParseType(s string) (Type, error) {
	switch s {
	case "", "auto":
		return Auto, nil
	case "hls":
		return HLS, nil
	case "raw":
		return Raw, nil
	case "dash":
		return DASH, nil
	}
	return Auto, fmt.Errorf("hls: unknown stream type %q (want auto, hls, dash or raw)", s)
}

// String returns the configuration name of t.
func (t Type) String() string {
	switch t {
	case Auto:
		return "auto"
	case HLS:
		return "hls"
	case Raw:
		return "raw"
	case DASH:
		return "dash"
	}
	return fmt.Sprintf("Type(%d)", uint8(t))
}

// Detect returns HLS, DASH or Raw for a response, following spec 3.1 step
// 2: a configured type other than Auto wins, then the Content-Type, then
// the body: #EXTM3U after an optional UTF-8 BOM and whitespace is HLS, an
// XML root element MPD is DASH. prefix is the start of the body (see
// Sniff). Resources under /r/ and /p/ pass Auto.
func Detect(contentType string, prefix []byte, configured Type) Type {
	if configured != Auto {
		return configured
	}
	mediaType, _, _ := strings.Cut(contentType, ";")
	mediaType = strings.TrimSpace(mediaType)
	switch {
	case strings.EqualFold(mediaType, "application/vnd.apple.mpegurl"),
		strings.EqualFold(mediaType, "application/x-mpegurl"):
		return HLS
	case strings.EqualFold(mediaType, "application/dash+xml"):
		return DASH
	}
	body := bytes.TrimPrefix(prefix, []byte(utf8BOM))
	body = bytes.TrimLeft(body, " \t\r\n\v\f")
	switch {
	case bytes.HasPrefix(body, []byte("#EXTM3U")):
		return HLS
	case len(body) > 0 && body[0] == '<' && isDASHRoot(body):
		return DASH
	}
	return Raw
}

// isDASHRoot reports whether the XML document starting at body has the
// root element MPD (any case, any namespace prefix).
func isDASHRoot(body []byte) bool {
	root, ok := xmlRoot(body)
	return ok && xmlRoots[strings.ToLower(root)] == FormatDASH
}

// Classify decides what the proxy does with a 2xx response (spec 3.1
// step 2, 3.3); serving and the probe command both use it, so they cannot
// differ. typ is Detect's answer by the configured type and the
// Content-Type; if that is Raw and sniff is set, the start of the body
// decides (Sniff, Detect), and a body that is still Raw but a playlist the
// proxy does not rewrite gives its format in refused (Unsupported). Without
// sniff (a HEAD request, whose body is never read) the body is left
// unread.
//
// rest is the body to read from now on: it reads the whole body, the
// sniffed bytes included. err is a read error of Sniff; typ is then Raw.
func Classify(contentType string, body *bufio.Reader, configured Type, sniff bool) (typ Type, refused string, rest *bufio.Reader, err error) {
	typ = Detect(contentType, nil, configured)
	if typ != Raw || !sniff {
		return typ, "", body, nil
	}
	prefix, rest, err := Sniff(body)
	if err != nil {
		return typ, "", rest, err
	}
	typ = Detect(contentType, prefix, configured)
	if typ == Raw {
		refused = Unsupported(contentType, prefix, configured)
	}
	return typ, refused, rest, nil
}

// IsMedia reports whether playlist is an HLS media playlist: it has an
// #EXTINF segment and no #EXT-X-STREAM-INF variant (a master playlist).
// Tags count at the start of a line, after an optional UTF-8 BOM and
// whitespace, with the line ends of Rewrite.
func IsMedia(playlist []byte) bool {
	playlist = bytes.TrimPrefix(playlist, []byte(utf8BOM))
	segment := false
	for rest := playlist; len(rest) > 0; {
		var line []byte
		line, _, rest = nextLine(rest)
		line = bytes.TrimLeft(line, lineSpace)
		switch {
		case bytes.HasPrefix(line, []byte("#EXT-X-STREAM-INF:")):
			return false
		case bytes.HasPrefix(line, []byte("#EXTINF:")):
			segment = true
		}
	}
	return segment
}
