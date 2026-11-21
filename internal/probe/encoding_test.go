// SPDX-License-Identifier: AGPL-3.0-or-later

package probe

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func gzipString(s string) string {
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	_, _ = io.WriteString(zw, s)
	_ = zw.Close()
	return b.String()
}

// TestRunContentEncoding: the probe decodes or refuses a Content-Encoding
// the transport did not undo like the proxy does (spec 3.4, 7.2), so it
// reports the classification of the decoded body. The transport undoes
// gzip only if it asked for it: not with an Accept-Encoding of --header,
// and never x-gzip.
func TestRunContentEncoding(t *testing.T) {
	asked := http.Header{"Accept-Encoding": {"gzip"}}
	tests := []struct {
		name   string
		coding string
		header http.Header
		ctype  string
		body   string
		// want
		ok             bool
		steps          []string
		format         string
		refused, class string
		segments       int
		streaming      bool
	}{
		{
			name: "gzip pls", coding: "gzip", header: asked, ctype: "application/octet-stream", body: "[playlist]\nFile1=http://radio.example/s\n",
			steps: []string{"url"}, format: "refused", refused: "pls", class: "unsupported",
		},
		{
			name: "x-gzip pls", coding: "x-gzip", ctype: "application/octet-stream", body: "[playlist]\nFile1=http://radio.example/s\n",
			steps: []string{"url"}, format: "refused", refused: "pls", class: "unsupported",
		},
		{
			name: "gzip hls", coding: "gzip", header: asked, ctype: "application/octet-stream", body: media,
			ok: true, steps: []string{"url", "segment"}, format: "hls-media", segments: 2,
		},
		{
			name: "x-gzip dash", coding: "x-gzip", ctype: "application/octet-stream", body: dashManifest,
			ok: true, steps: []string{"url"}, format: "dash",
		},
		{
			name: "gzip media", coding: "gzip", header: asked, ctype: "video/mp2t", body: "\x47\x00\x00\x10" + strings.Repeat("\x00\xff", 400),
			ok: true, steps: []string{"url"}, format: "raw", streaming: true,
		},
		{
			name: "br", coding: "br", ctype: "application/octet-stream", body: "\x1b\x8f\x00\x00",
			steps: []string{"url"}, class: "other",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := tt.body
			if tt.coding != "br" {
				body = gzipString(body)
			}
			fx := newFixture(t, map[string]file{
				"/live/list":    {ctype: tt.ctype, body: body, header: http.Header{"Content-Encoding": {tt.coding}}},
				"/live/seg1.ts": {ctype: "video/mp2t", body: "segment"},
			})
			rep := Run(t.Context(), fx.url("/live/list"), Options{Route: directRoute(t, true, nil), Header: tt.header, MaxPlaylistSize: 1 << 20})
			if rep.OK != tt.ok || !slices.Equal(roles(rep), tt.steps) {
				t.Fatalf("ok %v, steps %v, want %v %v: %+v", rep.OK, roles(rep), tt.ok, tt.steps, rep.Steps)
			}
			s := rep.Steps[0]
			if s.Format != tt.format || s.Refused != tt.refused || s.Class != tt.class || s.Segments != tt.segments || s.Streaming != tt.streaming {
				t.Errorf("step %+v, want format %q refused %q class %q segments %d streaming %v", s, tt.format, tt.refused, tt.class, tt.segments, tt.streaming)
			}
			if tt.class == "other" && !strings.Contains(s.Error, "unsupported content encoding") {
				t.Errorf("error %q does not name the encoding", s.Error)
			}
		})
	}
}
