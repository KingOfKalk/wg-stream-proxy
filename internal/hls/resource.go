// SPDX-License-Identifier: AGPL-3.0-or-later

package hls

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"net/url"
	"strings"
)

// MinKeyLen is the minimum signing key length in bytes (spec 4).
const MinKeyLen = 32

const (
	resourcePrefix = "/r/"
	sigLen         = 16 // truncated HMAC-SHA256 bytes
	maxChannelLen  = 48
	maxNameLen     = 64
	fallbackName   = "data"
)

var (
	// ErrShortKey: the signing key is shorter than MinKeyLen.
	ErrShortKey = fmt.Errorf("hls: signing key shorter than %d bytes", MinKeyLen)
	// ErrInvalidResource: a /r/ path is malformed or its signature does not
	// match. The server answers 404.
	ErrInvalidResource = errors.New("hls: invalid resource URL")
)

var b64 = base64.RawURLEncoding.Strict()

// Signer builds and verifies signed resource paths
// /r/{channel}/{sig}/{b64url}/{name} (spec 4). It is safe for concurrent
// use. Its key is never printed.
type Signer struct {
	key []byte
}

// NewSigner returns a Signer for key, which must be at least MinKeyLen
// bytes. The key is copied.
func NewSigner(key []byte) (*Signer, error) {
	if len(key) < MinKeyLen {
		return nil, ErrShortKey
	}
	return &Signer{key: append([]byte(nil), key...)}, nil
}

// String hides the key from fmt and log/slog.
func (s *Signer) String() string { return "hls.Signer{key:redacted}" }

// GoString hides the key from %#v.
func (s *Signer) GoString() string { return s.String() }

// Path returns the signed resource path for an absolute upstream URL of a
// channel. channel must be a valid channel id and upstream at most
// MaxURLLen bytes (Rewrite enforces the limit); otherwise Parse rejects the
// result.
func (s *Signer) Path(channel, upstream string) string {
	return string(s.PathBuilder(channel).AppendPath(nil, []byte(upstream)))
}

// PathBuilder returns a builder of signed resource paths for channel. It
// keeps one HMAC state for all paths, so a playlist rewrite does not set
// up the HMAC for every URI.
func (s *Signer) PathBuilder(channel string) *PathBuilder {
	return &PathBuilder{channel: channel, mac: hmac.New(sha256.New, s.key)}
}

// PathBuilder builds the signed resource paths of one channel, like
// Signer.Path. It is not safe for concurrent use.
type PathBuilder struct {
	channel string
	mac     hash.Hash
	msg     []byte // channel + "\n" + upstream
	sum     []byte
}

// String hides the HMAC state, which is derived from the key, from fmt and
// log/slog.
func (b *PathBuilder) String() string { return "hls.PathBuilder{key:redacted}" }

// GoString hides the HMAC state from %#v.
func (b *PathBuilder) GoString() string { return b.String() }

// AppendPath appends the signed resource path for upstream (Signer.Path)
// to dst and returns the extended slice.
func (b *PathBuilder) AppendPath(dst, upstream []byte) []byte {
	b.msg = append(append(append(b.msg[:0], b.channel...), '\n'), upstream...)
	b.mac.Reset()
	b.mac.Write(b.msg)
	b.sum = b.mac.Sum(b.sum[:0])
	dst = append(append(append(dst, resourcePrefix...), b.channel...), '/')
	dst = append(b64.AppendEncode(dst, b.sum[:sigLen]), '/')
	dst = append(b64.AppendEncode(dst, upstream), '/')
	return appendResourceName(dst, upstream)
}

// Parse verifies an escaped request path (r.URL.EscapedPath()) and returns
// the channel id and the upstream URL. The name segment is ignored. Every
// failure wraps ErrInvalidResource; the error never contains the URL.
func (s *Signer) Parse(path string) (channel, upstream string, err error) {
	rest, ok := strings.CutPrefix(path, resourcePrefix)
	if !ok {
		return "", "", fmt.Errorf("%w: not under %s", ErrInvalidResource, resourcePrefix)
	}
	parts := strings.SplitN(rest, "/", 4)
	if len(parts) != 4 || strings.Contains(parts[3], "/") {
		return "", "", fmt.Errorf("%w: want 4 path segments", ErrInvalidResource)
	}
	channel, sigText, urlText := parts[0], parts[1], parts[2]
	if !validChannel(channel) {
		return "", "", fmt.Errorf("%w: invalid channel id", ErrInvalidResource)
	}
	if len(sigText) != b64.EncodedLen(sigLen) {
		return "", "", fmt.Errorf("%w: bad signature length", ErrInvalidResource)
	}
	sig, err := decodeB64(sigText)
	if err != nil {
		return "", "", fmt.Errorf("%w: bad signature encoding", ErrInvalidResource)
	}
	if urlText == "" || len(urlText) > b64.EncodedLen(MaxURLLen) {
		return "", "", fmt.Errorf("%w: bad URL length", ErrInvalidResource)
	}
	raw, err := decodeB64(urlText)
	if err != nil {
		return "", "", fmt.Errorf("%w: bad URL encoding", ErrInvalidResource)
	}
	if !hmac.Equal(sig, s.mac(channel, string(raw))) {
		return "", "", fmt.Errorf("%w: signature mismatch", ErrInvalidResource)
	}
	return channel, string(raw), nil
}

// decodeB64 decodes strict unpadded base64url. encoding/base64 skips CR and
// LF even in strict mode, which would give a resource more than one valid
// path, so any byte outside the alphabet is rejected first.
func decodeB64(s string) ([]byte, error) {
	for i := 0; i < len(s); i++ {
		if !isB64URLByte(s[i]) {
			return nil, errors.New("invalid base64url byte")
		}
	}
	return b64.DecodeString(s)
}

// mac returns the first sigLen bytes of
// HMAC-SHA256(key, channel + "\n" + upstream).
func (s *Signer) mac(channel, upstream string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(channel + "\n" + upstream))
	return m.Sum(nil)[:sigLen]
}

// validChannel reports whether id matches the channel id format of spec
// 5.2: [a-z0-9-], 1-48 bytes.
func validChannel(id string) bool {
	if id == "" || len(id) > maxChannelLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		if !isChannelByte(id[i]) {
			return false
		}
	}
	return true
}

// appendResourceName appends the {name} segment for an upstream URL (spec
// 4): the last path segment reduced to [A-Za-z0-9._-], at most maxNameLen
// bytes with the extension kept. Empty or dots-only results (which path
// cleaning would remove) become "data", as does a URL that does not parse.
func appendResourceName(dst, upstream []byte) []byte {
	path, ok := simplePath(upstream)
	if !ok {
		u, err := url.Parse(string(upstream))
		if err != nil {
			return append(dst, fallbackName...)
		}
		path = []byte(u.Path)
	}
	start := len(dst)
	for _, c := range path[bytes.LastIndexByte(path, '/')+1:] {
		// Bytes, not runes: the kept characters are ASCII, and ASCII
		// bytes never occur inside a multi-byte UTF-8 sequence.
		if byteClass[c]&className != 0 {
			dst = append(dst, c)
		}
	}
	if name := dst[start:]; len(name) > maxNameLen {
		ext := 0
		if i := bytes.LastIndexByte(name, '.'); i >= 0 && len(name)-i < maxNameLen {
			ext = len(name) - i
		}
		copy(name[maxNameLen-ext:], name[len(name)-ext:])
		dst = dst[:start+maxNameLen]
	}
	if len(bytes.Trim(dst[start:], ".")) == 0 {
		return append(dst[:start], fallbackName...)
	}
	return dst
}

// simplePath returns the path of an upstream URL whose url.Parse result is
// known without parsing: "http://" or "https://", a host of
// [A-Za-z0-9.-] with an optional numeric port, a path of ASCII bytes
// without '%', an optional query, no fragment and no control bytes. Such
// a URL parses, and its Path is the path bytes as they are. ok is false
// for every other URL.
func simplePath(u []byte) (path []byte, ok bool) {
	rest, ok := bytes.CutPrefix(u, []byte("https://"))
	if !ok {
		if rest, ok = bytes.CutPrefix(u, []byte("http://")); !ok {
			return nil, false
		}
	}
	for _, c := range u {
		if byteClass[c]&classURL == 0 {
			return nil, false
		}
	}
	host, port := 0, -1
	for ; host < len(rest) && rest[host] != '/'; host++ {
		c := rest[host]
		switch {
		case c >= '0' && c <= '9':
		case port < 0 && (c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '.' || c == '-'):
		case port < 0 && c == ':' && host > 0:
			port = host
		default:
			return nil, false
		}
	}
	if host == 0 || port == host-1 || host == len(rest) {
		return nil, false
	}
	path = rest[host:]
	if i := bytes.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	if bytes.IndexByte(path, '%') >= 0 {
		return nil, false
	}
	return path, true
}

func isB64URLByte(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_'
}

func isChannelByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-'
}
