// SPDX-License-Identifier: AGPL-3.0-or-later

package hls

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
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
	return resourcePrefix + channel + "/" + s.sign(channel, upstream) + "/" +
		b64.EncodeToString([]byte(upstream)) + "/" + resourceName(upstream)
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

func (s *Signer) sign(channel, upstream string) string {
	return b64.EncodeToString(s.mac(channel, upstream))
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

// resourceName returns the {name} segment for an upstream URL (spec 4): the
// last path segment reduced to [A-Za-z0-9._-], at most maxNameLen bytes
// with the extension kept. Empty or dots-only results (which path
// cleaning would remove) become "data".
func resourceName(upstream string) string {
	u, err := url.Parse(upstream)
	if err != nil {
		return fallbackName
	}
	seg := u.Path[strings.LastIndexByte(u.Path, '/')+1:]
	name := strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			return r
		}
		return -1
	}, seg)
	if len(name) > maxNameLen {
		ext := ""
		if i := strings.LastIndexByte(name, '.'); i >= 0 && len(name)-i < maxNameLen {
			ext = name[i:]
		}
		name = name[:maxNameLen-len(ext)] + ext
	}
	if strings.Trim(name, ".") == "" {
		return fallbackName
	}
	return name
}

func isB64URLByte(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_'
}

func isChannelByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-'
}
