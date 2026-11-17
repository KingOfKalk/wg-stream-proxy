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
	}
	return Auto, fmt.Errorf("hls: unknown stream type %q (want auto, hls or raw)", s)
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
	}
	return fmt.Sprintf("Type(%d)", uint8(t))
}

// Detect returns HLS or Raw for a response, following spec 3.1 step 2:
// a configured type other than Auto wins, then the Content-Type, then
// whether the body starts with #EXTM3U after an optional UTF-8 BOM and
// whitespace. prefix is the start of the body (see SniffLen). Resources
// under /r/ pass Auto.
func Detect(contentType string, prefix []byte, configured Type) Type {
	if configured == HLS || configured == Raw {
		return configured
	}
	mediaType, _, _ := strings.Cut(contentType, ";")
	mediaType = strings.TrimSpace(mediaType)
	if strings.EqualFold(mediaType, "application/vnd.apple.mpegurl") ||
		strings.EqualFold(mediaType, "application/x-mpegurl") {
		return HLS
	}
	body := bytes.TrimPrefix(prefix, []byte(utf8BOM))
	body = bytes.TrimLeft(body, " \t\r\n\v\f")
	if bytes.HasPrefix(body, []byte("#EXTM3U")) {
		return HLS
	}
	return Raw
}
