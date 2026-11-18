// SPDX-License-Identifier: AGPL-3.0-or-later

package epg

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
)

// Download fetches the XMLTV file at url through rt and parses its
// channels. rt is the direct route (spec 7.2): the private-range guard
// and the [upstream] timeouts apply, redirects are followed like for any
// upstream. The file may be gzip-compressed or plain, and is read at most
// MaxSize bytes (decompressed).
func Download(ctx context.Context, rt *route.Route, url string) (*Index, error) {
	resp, err := rt.Fetch(ctx, route.Request{URL: url})
	if err != nil {
		return nil, fmt.Errorf("epg: download: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("epg: download %s: status %d", route.LogURL(url), resp.StatusCode)
	}
	return parseBody(resp.Body, MaxSize)
}

// parseBody opens and parses an XMLTV body with the given size limit.
func parseBody(body io.Reader, limit int64) (*Index, error) {
	r, err := Open(body, limit)
	if err != nil {
		return nil, err
	}
	return Parse(r)
}
