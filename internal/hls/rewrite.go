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
	// MaxPlaylistSize is the largest playlist body Rewrite accepts: the
	// upper bound of [upstream] max_playlist_size (spec 3.4, 5.3).
	MaxPlaylistSize = 32 << 20
	// MaxURLLen is the longest upstream URL in bytes (spec 3.4).
	MaxURLLen = 4096
)

var (
	// ErrPlaylistTooLarge: the playlist exceeds MaxPlaylistSize, or the
	// rewritten playlist maxOutput.
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
	// ErrInvalidResourceURL: appendURL appended nothing or a quote or line
	// break, which would corrupt the playlist.
	ErrInvalidResourceURL = errors.New("hls: invalid resource URL")
)

const lineSpace = " \t\v\f"

// Rewrite rewrites an HLS playlist (spec 3.2). Every URI line (non-blank,
// not starting with '#') and every quoted URI attribute (named URI or ending
// in -URI) of an #EXT tag other than #EXTINF is replaced with the URL
// appendURL(dst, abs) appends to dst, where abs is the URI resolved against
// final, the playlist's URL after redirects, in the form of
// url.URL.String. abs is only valid during the call. Only http, https and
// relative URIs are replaced; URIs with other schemes and all other bytes
// (line endings LF, CRLF or lone CR, BOM, blank lines, comments, other tags
// and attributes, whitespace around URI lines) stay unchanged.
//
// The output is at most maxOutput(len(playlist)) bytes, else
// ErrPlaylistTooLarge.
//
// Fail closed: input larger than MaxPlaylistSize (the caller limits the
// read as well), an http(s) or relative URI that does not parse or resolves
// to a URL longer than MaxURLLen, and playlists players could read
// differently (ErrInvalidPlaylist) are rejected as a whole. playlist is not
// modified.
func Rewrite(playlist []byte, final *url.URL, appendURL func(dst, abs []byte) []byte) ([]byte, error) {
	if len(playlist) > MaxPlaylistSize {
		return nil, ErrPlaylistTooLarge
	}
	if final == nil || !isHTTPScheme(final.Scheme) || final.Host == "" {
		return nil, ErrInvalidFinalURL
	}
	if bytes.IndexByte(playlist, 0) >= 0 {
		return nil, fmt.Errorf("%w: NUL byte", ErrInvalidPlaylist)
	}
	rw := rewriter{final: final, appendURL: appendURL, out: make([]byte, 0, outputCap(len(playlist)))}
	limit := maxOutput(len(playlist))
	first := true
	for rest := playlist; len(rest) > 0; {
		if len(rw.out) > limit {
			return nil, fmt.Errorf("%w: rewritten playlist larger than %d bytes", ErrPlaylistTooLarge, limit)
		}
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
	if len(rw.out) > limit {
		return nil, fmt.Errorf("%w: rewritten playlist larger than %d bytes", ErrPlaylistTooLarge, limit)
	}
	return rw.out, nil
}

// maxOutput is the largest rewritten playlist for an input of n bytes:
// 16 times the input, at least 1 MiB and at most 64 MiB. A short URI can
// become a long resource URL, so without a limit a small playlist could
// grow by orders of magnitude.
func maxOutput(n int) int {
	return min(max(16*n, 1<<20), 64<<20)
}

// outputCap is the initial capacity of the output for a playlist of n
// bytes. Signed resource URLs are longer than the URIs they replace;
// playlists of long DVR windows grow to about twice their size.
func outputCap(n int) int {
	return 2*n + 4096
}

type rewriter struct {
	final     *url.URL
	appendURL func(dst, abs []byte) []byte
	out       []byte
	abs       []byte    // resolved URL of the current URI
	dirs      []dirBase // resolved directories, see resolve
}

// nextLine splits off the first line of b. Lines end at LF, CRLF or a lone
// CR (ffmpeg reads a lone CR as a line end too); eol is empty only for the
// last line without terminator.
func nextLine(b []byte) (content, eol, rest []byte) {
	// Two IndexByte calls are much faster than one IndexAny.
	i := bytes.IndexByte(b, '\n')
	head := b
	if i >= 0 {
		head = b[:i]
	}
	if cr := bytes.IndexByte(head, '\r'); cr >= 0 {
		i = cr
	}
	switch {
	case i < 0:
		return b, nil, nil
	case b[i] == '\r' && i+1 < len(b) && b[i+1] == '\n':
		return b[:i], b[i : i+2], b[i+2:]
	}
	return b[:i], b[i : i+1], b[i+1:]
}

// isLineSpace reports whether c is in lineSpace.
func isLineSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\v' || c == '\f'
}

// line appends the rewritten content of one line, without terminator.
func (rw *rewriter) line(content []byte) error {
	lead, end := 0, len(content)
	for lead < end && isLineSpace(content[lead]) {
		lead++
	}
	for end > lead && isLineSpace(content[end-1]) {
		end--
	}
	trimmed := content[lead:end]
	switch {
	case len(trimmed) == 0:
		rw.out = append(rw.out, content...)
		return nil
	case trimmed[0] == '#':
		if bytes.HasPrefix(trimmed, []byte("#EXT")) {
			return rw.tag(content, trimmed)
		}
		rw.out = append(rw.out, content...)
		return nil
	}
	rw.out = append(rw.out, content[:lead]...)
	if err := rw.uri(trimmed); err != nil {
		return err
	}
	rw.out = append(rw.out, content[end:]...)
	return nil
}

// tag appends a tag line with the values of its URI attributes rewritten;
// trimmed is the line without surrounding whitespace. #EXTINF has no
// attribute list and is copied as is.
func (rw *rewriter) tag(line, trimmed []byte) error {
	if bytes.HasPrefix(trimmed, []byte("#EXTINF:")) {
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
	if err := rw.resolve(raw); err != nil {
		return err
	}
	if len(rw.abs) > MaxURLLen {
		return ErrURLTooLong
	}
	start := len(rw.out)
	out := rw.appendURL(rw.out, rw.abs)
	if len(out) <= start {
		return ErrInvalidResourceURL
	}
	if repl := out[start:]; bytes.IndexByte(repl, '"') >= 0 || bytes.IndexByte(repl, '\r') >= 0 || bytes.IndexByte(repl, '\n') >= 0 {
		return ErrInvalidResourceURL
	}
	rw.out = out
	return nil
}

// maxDirs bounds the resolved directories one Rewrite keeps.
const maxDirs = 16

// dirBase is a URI directory and the resolved URL it stands for: dir +
// name resolves to base + name for every plain name (see splitURI).
type dirBase struct {
	dir, base string
	ok        bool // false: resolving dir + name needs net/url
}

// resolve sets rw.abs to raw resolved against the final URL, as
// rw.final.ResolveReference(url.Parse(raw)).String() would.
//
// Playlists repeat the same directory in thousands of URIs, so the
// directory is resolved once through net/url and the rest is appended:
// splitURI cuts raw into a directory and a tail (file name and query)
// that net/url copies as it is. Every other URI goes through net/url.
func (rw *rewriter) resolve(raw []byte) error {
	if dir, tail, ok := splitURI(raw); ok {
		if base, ok := rw.dirBase(dir); ok {
			rw.abs = append(append(rw.abs[:0], base...), tail...)
			return nil
		}
	}
	ref, err := url.Parse(string(raw))
	if err != nil {
		// The parse error is dropped: it quotes the URI, which may carry
		// tokens that must not reach the logs (spec 8).
		return ErrInvalidURI
	}
	rw.abs = append(rw.abs[:0], rw.final.ResolveReference(ref).String()...)
	return nil
}

// dirBase returns the resolved base of URI directory dir. It resolves
// dir + "x" with net/url and keeps the result without its final "x", if
// it ends in "/x".
func (rw *rewriter) dirBase(dir []byte) (string, bool) {
	for _, d := range rw.dirs {
		if d.dir == string(dir) {
			return d.base, d.ok
		}
	}
	d := dirBase{dir: string(dir)}
	if ref, err := url.Parse(d.dir + "x"); err == nil {
		s := rw.final.ResolveReference(ref).String()
		if strings.HasSuffix(s, "/x") {
			d.base, d.ok = s[:len(s)-1], true
		}
	}
	if len(rw.dirs) < maxDirs {
		rw.dirs = append(rw.dirs, d)
	}
	return d.base, d.ok
}

// splitURI splits an http(s) or relative URI into dir, up to and including
// the last '/' before the query, and tail, the file name and query after
// it, if net/url resolves and prints the tail as it is: the file name is
// not empty, not "." or "..", and has only bytes that need no escaping
// in a path and cannot start a scheme ([A-Za-z0-9-._~$&+,;=@]); the
// query has only printable ASCII; there is no fragment; dir does not end
// in "//" (the file name would be the host).
func splitURI(raw []byte) (dir, tail []byte, ok bool) {
	end := len(raw)
	if q := bytes.IndexByte(raw, '?'); q >= 0 {
		end = q
	}
	slash := bytes.LastIndexByte(raw[:end], '/')
	file := raw[slash+1 : end]
	if len(file) == 0 || string(file) == "." || string(file) == ".." ||
		slash > 0 && raw[slash-1] == '/' {
		return nil, nil, false
	}
	for _, c := range file {
		if !isPlainNameByte(c) {
			return nil, nil, false
		}
	}
	for _, c := range raw[end:] {
		if byteClass[c]&classQuery == 0 {
			return nil, nil, false
		}
	}
	if bytes.IndexByte(raw[:slash+1], '#') >= 0 {
		return nil, nil, false
	}
	return raw[:slash+1], raw[slash+1:], true
}

// isPlainNameByte reports whether c may appear in a file name splitURI
// accepts: unreserved or a path sub-delimiter net/url does not escape,
// but not ':'.
func isPlainNameByte(c byte) bool { return byteClass[c]&classPlain != 0 }

// Byte classes of the fast paths, as flags in byteClass.
const (
	classPlain = 1 << iota // [A-Za-z0-9-._~$&+,;=@]: isPlainNameByte
	className              // [A-Za-z0-9._-]: kept in a resource {name}
	classURL               // printable ASCII or space, not '#': simplePath
	classQuery             // printable ASCII, not '#': splitURI query
)

var byteClass = func() (t [256]uint8) {
	for c := ' '; c < 0x7f; c++ {
		if c != '#' {
			t[c] |= classURL
			if c != ' ' {
				t[c] |= classQuery
			}
		}
	}
	for c := range t {
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			t[c] |= classPlain | className
		}
	}
	for _, c := range []byte("-._~$&+,;=@") {
		t[c] |= classPlain
	}
	for _, c := range []byte("._-") {
		t[c] |= className
	}
	return t
}()

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
