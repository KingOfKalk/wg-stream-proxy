// SPDX-License-Identifier: AGPL-3.0-or-later

package hls

import (
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
