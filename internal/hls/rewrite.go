// SPDX-License-Identifier: AGPL-3.0-or-later

package hls

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
)

const (
	// MaxPlaylistSize is the largest playlist body Rewrite accepts (spec 3.4).
	MaxPlaylistSize = 2 << 20
	// MaxURLLen is the longest upstream URL in bytes (spec 3.4).
	MaxURLLen = 4096
)

var (
	// ErrPlaylistTooLarge: the playlist exceeds MaxPlaylistSize.
	ErrPlaylistTooLarge = errors.New("hls: playlist too large")
	// ErrInvalidFinalURL: the final URL is not an absolute http(s) URL.
	ErrInvalidFinalURL = errors.New("hls: final URL is not an absolute http(s) URL")
	// ErrInvalidURI: an http(s) or relative URI in the playlist does not
	// parse. Leaving it unchanged could let the player fetch it directly,
	// bypassing the channel's route, so the playlist is rejected.
	ErrInvalidURI = errors.New("hls: invalid URI in playlist")
	// ErrURLTooLong: a resolved upstream URL exceeds MaxURLLen.
	ErrURLTooLong = errors.New("hls: upstream URL too long")
	// ErrInvalidPlaylist: players could read the playlist differently from
	// the rewriter and fetch an unrewritten URL directly: it contains a NUL
	// byte (a line end for ffmpeg), or a tag has "URI=" with an http(s) URL
	// outside a recognised URI attribute (unquoted value, escaped quote,
	// broken attribute list).
	ErrInvalidPlaylist = errors.New("hls: playlist rejected")
	// ErrInvalidResourceURL: urlFor returned an empty string or one with
	// a quote or line break, which would corrupt the playlist.
	ErrInvalidResourceURL = errors.New("hls: invalid resource URL")
)

const lineSpace = " \t\v\f"

// Rewrite rewrites an HLS playlist (spec 3.2). Every URI line (non-blank,
// not starting with '#') and every quoted URI attribute (named URI or ending
// in -URI) of an #EXT tag other than #EXTINF is replaced with urlFor(abs),
// where abs is the URI resolved against final, the playlist's URL after
// redirects. Only http, https and relative URIs are replaced; URIs with
// other schemes and all other bytes (line endings LF, CRLF or lone CR, BOM,
// blank lines, comments, other tags and attributes, whitespace around URI
// lines) stay unchanged.
//
// Fail closed: input larger than MaxPlaylistSize (the caller limits the
// read as well), an http(s) or relative URI that does not parse or resolves
// to a URL longer than MaxURLLen, and playlists players could read
// differently (ErrInvalidPlaylist) are rejected as a whole. playlist is not
// modified.
func Rewrite(playlist []byte, final *url.URL, urlFor func(abs *url.URL) string) ([]byte, error) {
	if len(playlist) > MaxPlaylistSize {
		return nil, ErrPlaylistTooLarge
	}
	if final == nil || !isHTTPScheme(final.Scheme) || final.Host == "" {
		return nil, ErrInvalidFinalURL
	}
	if bytes.IndexByte(playlist, 0) >= 0 {
		return nil, fmt.Errorf("%w: NUL byte", ErrInvalidPlaylist)
	}
	rw := rewriter{final: final, urlFor: urlFor, out: make([]byte, 0, len(playlist)+len(playlist)/2)}
	first := true
	for rest := playlist; len(rest) > 0; {
		var content, eol []byte
		content, eol, rest = nextLine(rest)
		if first {
			first = false
			if bytes.HasPrefix(content, []byte(utf8BOM)) {
				rw.out = append(rw.out, utf8BOM...)
				content = content[len(utf8BOM):]
			}
		}
		if err := rw.line(content); err != nil {
			return nil, err
		}
		rw.out = append(rw.out, eol...)
	}
	return rw.out, nil
}

type rewriter struct {
	final  *url.URL
	urlFor func(*url.URL) string
	out    []byte
}

// nextLine splits off the first line of b. Lines end at LF, CRLF or a lone
// CR (ffmpeg reads a lone CR as a line end too); eol is empty only for the
// last line without terminator.
func nextLine(b []byte) (content, eol, rest []byte) {
	i := bytes.IndexAny(b, "\r\n")
	switch {
	case i < 0:
		return b, nil, nil
	case b[i] == '\r' && i+1 < len(b) && b[i+1] == '\n':
		return b[:i], b[i : i+2], b[i+2:]
	}
	return b[:i], b[i : i+1], b[i+1:]
}

// line appends the rewritten content of one line, without terminator.
func (rw *rewriter) line(content []byte) error {
	trimmed := bytes.TrimLeft(content, lineSpace)
	lead := len(content) - len(trimmed)
	trimmed = bytes.TrimRight(trimmed, lineSpace)
	switch {
	case len(trimmed) == 0:
		rw.out = append(rw.out, content...)
		return nil
	case trimmed[0] == '#':
		if bytes.HasPrefix(trimmed, []byte("#EXT")) {
			return rw.tag(content)
		}
		rw.out = append(rw.out, content...)
		return nil
	}
	rw.out = append(rw.out, content[:lead]...)
	if err := rw.uri(trimmed); err != nil {
		return err
	}
	rw.out = append(rw.out, content[lead+len(trimmed):]...)
	return nil
}

// tag appends a tag line with the values of its URI attributes rewritten.
// #EXTINF has no attribute list and is copied as is.
func (rw *rewriter) tag(line []byte) error {
	if bytes.HasPrefix(bytes.TrimLeft(line, lineSpace), []byte("#EXTINF:")) {
		rw.out = append(rw.out, line...)
		return nil
	}
	spans := uriAttrValues(line)
	if strayHTTPURI(line, spans) {
		return fmt.Errorf("%w: URI= with an http(s) URL outside a recognised attribute", ErrInvalidPlaylist)
	}
	done := 0
	for _, v := range spans {
		rw.out = append(rw.out, line[done:v.start]...)
		if err := rw.uri(line[v.start:v.end]); err != nil {
			return err
		}
		done = v.end
	}
	rw.out = append(rw.out, line[done:]...)
	return nil
}

// span is a byte range [start, end) in a line.
type span struct{ start, end int }

// uriAttrValues returns the positions of the quoted values of all URI
// attributes (named URI or ending in -URI, such as SERVER-URI or
// X-ASSET-URI) in a tag line "#EXT...:NAME=VALUE,NAME="VALUE",...".
// Like ffmpeg, it accepts runs of commas and whitespace between attributes
// and whitespace around '='. Parsing stops at the first element that is not
// an attribute, so tags whose value is not an attribute list
// (#EXT-X-BYTERANGE, ...) yield nothing.
func uriAttrValues(line []byte) []span {
	colon := bytes.IndexByte(line, ':')
	if colon < 0 {
		return nil
	}
	var spans []span
	i, n := colon+1, len(line)
	skip := func(seps string) {
		for i < n && strings.IndexByte(seps, line[i]) >= 0 {
			i++
		}
	}
	for {
		skip(" \t,")
		if i >= n {
			return spans
		}
		nameStart := i
		for i < n && isAttrNameByte(line[i]) {
			i++
		}
		name := line[nameStart:i]
		skip(" \t")
		if len(name) == 0 || i >= n || line[i] != '=' {
			return spans
		}
		i++
		skip(" \t")
		if i < n && line[i] == '"' {
			end := bytes.IndexByte(line[i+1:], '"')
			if end < 0 {
				return spans
			}
			valStart, valEnd := i+1, i+1+end
			if string(name) == "URI" || bytes.HasSuffix(name, []byte("-URI")) {
				spans = append(spans, span{valStart, valEnd})
			}
			i = valEnd + 1
		} else {
			for i < n && line[i] != ',' && line[i] != ' ' && line[i] != '\t' {
				i++
			}
		}
	}
}

// strayHTTPURI reports whether a tag line contains "URI=" followed by an
// http(s) or scheme-relative URL that is not one of the rewritten values in
// spans. Players parse attribute lists differently (ffmpeg accepts unquoted
// values and backslash escapes, others match URI="..." anywhere in the
// line); such a URL would stay unrewritten and the player could fetch it
// directly.
func strayHTTPURI(line []byte, spans []span) bool {
	for i := 0; ; {
		j := bytes.Index(line[i:], []byte("URI="))
		if j < 0 {
			return false
		}
		start := i + j + len("URI=")
		i = start
		var val []byte
		if start < len(line) && line[start] == '"' {
			start++
			end := bytes.IndexByte(line[start:], '"')
			if end < 0 {
				end = len(line) - start
			}
			val = bytes.TrimLeft(line[start:start+end], lineSpace)
		} else {
			end := bytes.IndexAny(line[start:], ", \t")
			if end < 0 {
				end = len(line) - start
			}
			val = line[start : start+end]
		}
		if slices.ContainsFunc(spans, func(s span) bool { return s.start == start }) {
			continue
		}
		if isHTTPScheme(uriScheme(val)) || bytes.HasPrefix(val, []byte("//")) {
			return true
		}
	}
}

func isAttrNameByte(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-'
}

// uri appends the replacement for one URI, or the URI itself if it is
// empty or has a scheme other than http and https.
func (rw *rewriter) uri(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	if scheme := uriScheme(raw); scheme != "" && !isHTTPScheme(scheme) {
		rw.out = append(rw.out, raw...)
		return nil
	}
	ref, err := url.Parse(string(raw))
	if err != nil {
		// The parse error is dropped: it quotes the URI, which may carry
		// tokens that must not reach the logs (spec 8).
		return ErrInvalidURI
	}
	abs := rw.final.ResolveReference(ref)
	if len(abs.String()) > MaxURLLen {
		return ErrURLTooLong
	}
	repl := rw.urlFor(abs)
	if repl == "" || strings.ContainsAny(repl, "\"\r\n") {
		return ErrInvalidResourceURL
	}
	rw.out = append(rw.out, repl...)
	return nil
}

// uriScheme returns the RFC 3986 scheme of a URI reference, or "" for a
// relative reference.
func uriScheme(raw []byte) string {
	for i, c := range raw {
		switch {
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
		case i > 0 && (c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'):
		case i > 0 && c == ':':
			return string(raw[:i])
		default:
			return ""
		}
	}
	return ""
}

func isHTTPScheme(s string) bool {
	return strings.EqualFold(s, "http") || strings.EqualFold(s, "https")
}
