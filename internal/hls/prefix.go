// SPDX-License-Identifier: AGPL-3.0-or-later

package hls

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// prefixRoot is the path of prefix-signed resources (spec 4.1).
const prefixRoot = "/p/"

// prefixLabel starts the signed message of a prefix path. It contains a
// ':', which channel ids never do, so no /r/ message (channel + "\n" +
// URL) equals a prefix message and no signature verifies as the other.
const prefixLabel = "prefix:"

// Modes of a prefix path: whether a request below it may carry a query.
const (
	modeNoQuery = "n"
	modeQuery   = "q"
)

// ErrInvalidPrefix: PrefixPath got a channel id or an upstream prefix that
// is not canonical (spec 4.1).
var ErrInvalidPrefix = errors.New("hls: invalid upstream prefix")

// PrefixPath returns the signed prefix path
// /p/{channel}/{sig}/{mode}/{b64prefix}/ for an upstream prefix (spec
// 4.1). A request for this path plus a relative path below it reaches
// prefix plus that path, and only that (ParsePrefix). query allows a query
// on such requests.
//
// prefix must be canonical: "http://" or "https://", a host with an
// optional port, and a path that starts and ends with '/', without user
// info, query, fragment, dot segments or bytes url.URL.String would
// escape; at most MaxURLLen bytes. Otherwise the error wraps
// ErrInvalidPrefix and never contains the prefix.
func (s *Signer) PrefixPath(channel, prefix string, query bool) (string, error) {
	if !validChannel(channel) {
		return "", fmt.Errorf("%w: invalid channel id", ErrInvalidPrefix)
	}
	if _, ok := parsePrefix(prefix); !ok {
		return "", fmt.Errorf("%w: not a canonical http(s) directory URL", ErrInvalidPrefix)
	}
	mode := modeNoQuery
	if query {
		mode = modeQuery
	}
	var b strings.Builder
	b.WriteString(prefixRoot)
	b.WriteString(channel)
	b.WriteByte('/')
	b.WriteString(b64.EncodeToString(s.prefixMAC(channel, mode, prefix)))
	b.WriteByte('/')
	b.WriteString(mode)
	b.WriteByte('/')
	b.WriteString(b64.EncodeToString([]byte(prefix)))
	b.WriteByte('/')
	return b.String(), nil
}

// PrefixResource is a verified request for a prefix-signed resource.
type PrefixResource struct {
	// Channel is the channel id.
	Channel string
	// URL is the upstream URL: the prefix plus the request path below
	// the prefix path, plus the query if Query allows one.
	URL string
	// Prefix is the signed upstream prefix.
	Prefix string
	// Query: the mode allows a query.
	Query bool

	base *url.URL // Prefix, parsed
}

// Below reports whether u is a URL a request below the prefix path could
// reach (spec 4.1): the prefix's scheme, host and port, no user info, a
// path below the prefix directory that ParsePrefix would accept, and a
// query only if the mode allows one. A fragment is ignored (it is never
// sent). Upstream redirects of a prefix-signed request are only followed
// if the target is below the prefix.
func (r PrefixResource) Below(u *url.URL) bool {
	if r.base == nil || u.Scheme != r.base.Scheme || u.Host != r.base.Host || u.User != nil || u.Opaque != "" {
		return false
	}
	path, ok := strings.CutPrefix(u.EscapedPath(), r.base.EscapedPath())
	if !ok || checkPrefixPath(path) != nil {
		return false
	}
	switch {
	case u.RawQuery == "" && !u.ForceQuery:
		return true
	case !r.Query:
		return false
	}
	return validQuery(u.RawQuery)
}

// ParsePrefix verifies a request for a prefix-signed resource (spec 4.1)
// and returns the channel id, the upstream URL and the prefix. u is the
// request URL (r.URL); its escaped path is checked, so every upstream URL
// stays below the prefix: same scheme, host and port, below the prefix
// directory. Every failure wraps ErrInvalidResource; the error never
// contains a URL.
func (s *Signer) ParsePrefix(u *url.URL) (PrefixResource, error) {
	var r PrefixResource
	rest, ok := strings.CutPrefix(u.EscapedPath(), prefixRoot)
	if !ok {
		return r, fmt.Errorf("%w: not under %s", ErrInvalidResource, prefixRoot)
	}
	parts := strings.SplitN(rest, "/", 5)
	if len(parts) != 5 {
		return r, fmt.Errorf("%w: want 5 path segments", ErrInvalidResource)
	}
	channel, sigText, mode, prefixText, path := parts[0], parts[1], parts[2], parts[3], parts[4]
	if !validChannel(channel) {
		return r, fmt.Errorf("%w: invalid channel id", ErrInvalidResource)
	}
	if len(sigText) != b64.EncodedLen(sigLen) {
		return r, fmt.Errorf("%w: bad signature length", ErrInvalidResource)
	}
	sig, err := decodeB64(sigText)
	if err != nil {
		return r, fmt.Errorf("%w: bad signature encoding", ErrInvalidResource)
	}
	if mode != modeNoQuery && mode != modeQuery {
		return r, fmt.Errorf("%w: unknown mode", ErrInvalidResource)
	}
	if prefixText == "" || len(prefixText) > b64.EncodedLen(MaxURLLen) {
		return r, fmt.Errorf("%w: bad prefix length", ErrInvalidResource)
	}
	raw, err := decodeB64(prefixText)
	if err != nil {
		return r, fmt.Errorf("%w: bad prefix encoding", ErrInvalidResource)
	}
	prefix := string(raw)
	if !hmac.Equal(sig, s.prefixMAC(channel, mode, prefix)) {
		return r, fmt.Errorf("%w: signature mismatch", ErrInvalidResource)
	}
	// Signed prefixes are canonical; check again anyway.
	base, ok := parsePrefix(prefix)
	if !ok {
		return r, fmt.Errorf("%w: invalid prefix", ErrInvalidResource)
	}
	if err := checkPrefixPath(path); err != nil {
		return r, fmt.Errorf("%w: %w", ErrInvalidResource, err)
	}
	upstream := prefix + path
	switch {
	case u.RawQuery == "" && !u.ForceQuery:
	case mode != modeQuery:
		return r, fmt.Errorf("%w: query not allowed", ErrInvalidResource)
	case !validQuery(u.RawQuery):
		return r, fmt.Errorf("%w: invalid query", ErrInvalidResource)
	case u.RawQuery != "":
		upstream += "?" + u.RawQuery
	}
	if len(upstream) > MaxURLLen {
		return r, fmt.Errorf("%w: upstream URL too long", ErrInvalidResource)
	}
	// By construction the URL is below the prefix; verify it the way the
	// route parses it.
	up, err := url.Parse(upstream)
	if err != nil || up.Scheme != base.Scheme || up.Host != base.Host || up.User != nil ||
		!strings.HasPrefix(up.EscapedPath(), base.EscapedPath()) {
		return r, fmt.Errorf("%w: outside the prefix", ErrInvalidResource)
	}
	return PrefixResource{Channel: channel, URL: upstream, Prefix: prefix, Query: mode == modeQuery, base: base}, nil
}

// prefixMAC returns the first sigLen bytes of
// HMAC-SHA256(key, "prefix:" + channel + "\n" + mode + "\n" + prefix).
func (s *Signer) prefixMAC(channel, mode, prefix string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(prefixLabel + channel + "\n" + mode + "\n" + prefix))
	return m.Sum(nil)[:sigLen]
}

// parsePrefix parses a canonical upstream prefix (see PrefixPath).
func parsePrefix(prefix string) (*url.URL, bool) {
	if prefix == "" || len(prefix) > MaxURLLen {
		return nil, false
	}
	for i := 0; i < len(prefix); i++ {
		if c := prefix[i]; c <= ' ' || c >= 0x7f {
			return nil, false
		}
	}
	u, err := url.Parse(prefix)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return nil, false
	}
	path := u.EscapedPath()
	if !strings.HasPrefix(path, "/") || !strings.HasSuffix(path, "/") || u.Scheme+"://"+u.Host+path != prefix {
		return nil, false
	}
	for seg := range strings.SplitSeq(path, "/") {
		if seg == "." || seg == ".." {
			return nil, false
		}
	}
	return u, true
}

// checkPrefixPath checks the escaped request path below a prefix path:
// one or more segments of RFC 3986 path characters (unreserved,
// sub-delims, ':', '@' and valid percent escapes). Decoded once, a
// segment must not contain '/', '\', '%' (a double encoding), control
// bytes or non-ASCII bytes (overlong UTF-8 such as %c0%ae is a dot for
// some servers), and its name (the part before a ';', which some servers
// treat as a path parameter) must not be empty or consist of dots and
// spaces only (Windows servers drop trailing dots and spaces, so "..%20"
// is ".."): the upstream server must not be able to read it as a step
// out of the prefix directory.
func checkPrefixPath(path string) error {
	if path == "" {
		return errors.New("empty path")
	}
	for seg := range strings.SplitSeq(path, "/") {
		dec := make([]byte, 0, len(seg))
		for i := 0; i < len(seg); i++ {
			c := seg[i]
			switch {
			case c == '%':
				if i+2 >= len(seg) || !isHex(seg[i+1]) || !isHex(seg[i+2]) {
					return errors.New("bad percent escape")
				}
				c = unhex(seg[i+1])<<4 | unhex(seg[i+2])
				i += 2
				if c < 0x20 || c >= 0x7f || c == '/' || c == '\\' || c == '%' {
					return errors.New("forbidden escaped byte")
				}
			case !isPathChar(c):
				return errors.New("forbidden path byte")
			}
			dec = append(dec, c)
		}
		name, _, _ := strings.Cut(string(dec), ";")
		if strings.TrimRight(name, ". ") == "" {
			return errors.New("empty segment or dots and spaces only")
		}
	}
	return nil
}

// validQuery reports whether a request query may be passed upstream:
// printable ASCII without space and '#'.
func validQuery(q string) bool {
	for i := 0; i < len(q); i++ {
		if byteClass[q[i]]&classQuery == 0 {
			return false
		}
	}
	return true
}

// isPathChar reports whether c may appear unescaped in a path segment
// (RFC 3986 pchar without pct-encoded): unreserved, sub-delims, ':', '@'.
func isPathChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("-._~!$&'()*+,;=:@", c) >= 0
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}
