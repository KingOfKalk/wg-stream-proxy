// SPDX-License-Identifier: AGPL-3.0-or-later

package route

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// ErrTooLarge: a playlist body is larger than [upstream] max_playlist_size.
var ErrTooLarge = errors.New("route: playlist too large")

var (
	// ErrContentEncoding: the response has a Content-Encoding the proxy
	// does not decode (spec 3.4).
	ErrContentEncoding = errors.New("route: unsupported content encoding")
	// ErrEncodedPartial: a 206 response with a Content-Encoding, whose
	// Content-Range counts encoded bytes (spec 3.4).
	ErrEncodedPartial = errors.New("route: partial response with a content encoding")
)

// maxCodingText bounds the Content-Encoding value quoted in an error.
const maxCodingText = 64

// DecodeBody undoes a Content-Encoding of resp that the transport did
// not undo (spec 3.4). Go's transport decodes gzip only if it asked for
// it, which it does not for a request with Range or an Accept-Encoding
// of upstream.headers, and never decodes x-gzip; some servers send
// pre-compressed objects whatever the request said. It must run before
// the body is classified or relayed:
//   - no coding, or only identity: resp is unchanged;
//   - exactly one gzip or x-gzip: the body is decoded while it is read
//     and resp looks like a transparently decompressed response
//     (Uncompressed, no Content-Encoding and Content-Length, so
//     CopyResponseHeader drops Content-Length and ETag);
//   - a 206 with any coding fails with ErrEncodedPartial: its
//     Content-Range counts encoded bytes, so the caller fetches the
//     resource again without Range. The same holds for a 206 the
//     transport decoded;
//   - any other coding, or several, fails with ErrContentEncoding: the
//     proxy cannot tell whether the body is a playlist.
//
// The errors are *Error values; the caller closes resp.Body in every
// case.
func DecodeBody(resp *http.Response) error {
	var codings []string
	for _, v := range resp.Header.Values("Content-Encoding") {
		for c := range strings.SplitSeq(v, ",") {
			if c = strings.Trim(c, " \t"); c != "" && !strings.EqualFold(c, "identity") {
				codings = append(codings, c)
			}
		}
	}
	if len(codings) == 0 && !resp.Uncompressed {
		return nil
	}
	if resp.StatusCode == http.StatusPartialContent {
		return encodingError(resp, ErrEncodedPartial)
	}
	if len(codings) == 0 {
		return nil
	}
	if len(codings) > 1 || !strings.EqualFold(codings[0], "gzip") && !strings.EqualFold(codings[0], "x-gzip") {
		text := strings.Join(codings, ", ")
		if len(text) > maxCodingText {
			text = text[:maxCodingText] + "…"
		}
		return encodingError(resp, fmt.Errorf("%w %q", ErrContentEncoding, text))
	}
	resp.Body = &gzipBody{body: resp.Body}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.Uncompressed = true
	return nil
}

// encodingError wraps err with the logged form of the response's URL.
func encodingError(resp *http.Response, err error) error {
	u := "(invalid URL)"
	if resp.Request != nil && resp.Request.URL != nil {
		u = LogURL(resp.Request.URL.String())
	}
	return &Error{URL: u, Err: err}
}

// gzipBody decodes a gzip body. The gzip header is read with the first
// Read, so a body that is closed unread (HEAD, spec 3.4) is never read.
// Read errors of the underlying body pass unchanged (read_timeout).
type gzipBody struct {
	body io.ReadCloser
	zr   *gzip.Reader
	err  error
}

func (b *gzipBody) Read(p []byte) (int, error) {
	if b.zr == nil && b.err == nil {
		b.zr, b.err = gzip.NewReader(b.body)
		if b.err != nil && b.err != io.EOF {
			b.err = fmt.Errorf("route: gzip body: %w", b.err)
		}
	}
	if b.err != nil {
		return 0, b.err
	}
	n, err := b.zr.Read(p)
	if err != nil && err != io.EOF {
		err = fmt.Errorf("route: gzip body: %w", err)
	}
	return n, err
}

// Close closes the underlying body.
func (b *gzipBody) Close() error { return b.body.Close() }

// copiedHeaders are always copied from the upstream response (spec 3.4).
var copiedHeaders = []string{"Content-Type", "Content-Range", "Accept-Ranges", "Cache-Control", "Last-Modified"}

// CopyResponseHeader copies the response headers of spec 3.4 from resp to
// dst: Content-Type, Content-Range, Accept-Ranges, Cache-Control,
// Last-Modified and icy-*, plus Content-Length and ETag if the body is
// passed unchanged (not rewritten, not transparently decompressed).
func CopyResponseHeader(dst http.Header, resp *http.Response, rewritten bool) {
	for _, k := range copiedHeaders {
		if v := resp.Header.Values(k); len(v) > 0 {
			dst[k] = slices.Clone(v)
		}
	}
	for k, v := range resp.Header {
		if len(k) > 4 && strings.EqualFold(k[:4], "icy-") {
			dst[http.CanonicalHeaderKey(k)] = slices.Clone(v)
		}
	}
	if rewritten || resp.Uncompressed {
		return
	}
	for _, k := range []string{"Content-Length", "Etag"} {
		if v := resp.Header.Values(k); len(v) > 0 {
			dst[k] = slices.Clone(v)
		}
	}
}

// ClientStatus maps an upstream status to the client status (spec 3.4):
// 2xx and 4xx pass through, everything else (5xx, and redirects that
// were not followed) is 502.
func ClientStatus(code int) int {
	if code >= 200 && code < 300 || code >= 400 && code < 500 {
		return code
	}
	return http.StatusBadGateway
}

// ReadPlaylist reads a playlist body of at most limit bytes
// ([upstream] max_playlist_size, spec 3.4).
func ReadPlaylist(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%w: more than %d bytes (upstream.max_playlist_size)", ErrTooLarge, limit)
	}
	return b, nil
}

const (
	logNameMax      = 64
	logNameFallback = "data"
)

// LogURL returns the form of an upstream URL that may appear in logs and
// error values (spec 8): scheme, host (with port; IP literals unchanged)
// and file name, with the rest of the path replaced by an ellipsis:
// "https://cdn.example.com/…/7.m3u8". Query string, fragment, user info
// and every path segment but the last are dropped, because they can carry
// tokens. The file name is the last path segment reduced like the
// resource {name} (spec 4): [A-Za-z0-9._-], at most 64 bytes with the
// extension kept, "data" if nothing is left. A URL without path (or "/")
// has no file name; a file in the root shows without the ellipsis. URLs
// without scheme or host give "(invalid URL)".
func LogURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "(invalid URL)"
	}
	out := u.Scheme + "://" + u.Host
	if u.Path == "" || u.Path == "/" {
		return out
	}
	i := strings.LastIndexByte(u.Path, '/')
	name := logFileName(u.Path[i+1:])
	if i == 0 {
		return out + "/" + name
	}
	return out + "/…/" + name
}

// logFileName reduces a path segment like hls.appendResourceName does.
func logFileName(seg string) string {
	name := strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			return r
		}
		return -1
	}, seg)
	if len(name) > logNameMax {
		ext := ""
		if i := strings.LastIndexByte(name, '.'); i >= 0 && len(name)-i < logNameMax {
			ext = name[i:]
		}
		name = name[:logNameMax-len(ext)] + ext
	}
	if strings.Trim(name, ".") == "" {
		return logNameFallback
	}
	return name
}
