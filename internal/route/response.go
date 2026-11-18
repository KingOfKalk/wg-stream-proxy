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

// logFileName reduces a path segment like hls.resourceName does.
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
