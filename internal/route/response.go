// SPDX-License-Identifier: AGPL-3.0-or-later

package route

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// MaxPlaylistSize is the largest playlist body in bytes (spec 3.4).
const MaxPlaylistSize = 2 << 20

// ErrTooLarge: a playlist body is larger than MaxPlaylistSize.
var ErrTooLarge = errors.New("route: playlist larger than 2 MiB")

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

// ReadPlaylist reads a playlist body of at most MaxPlaylistSize bytes.
func ReadPlaylist(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxPlaylistSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxPlaylistSize {
		return nil, ErrTooLarge
	}
	return b, nil
}

// RedactURL returns s without query string, fragment and user info, for
// logs and errors (spec 8). Unparsable URLs give "(invalid URL)".
func RedactURL(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return "(invalid URL)"
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}
