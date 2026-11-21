// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"math/rand/v2"
	"net/http"
	"strings"
	"testing"
)

// encodingTOML has an auto channel on a path without a playlist
// extension, so a client Range is forwarded (spec 3.4).
const encodingTOML = `
tunnel = "direct"

[[channel]]
name = "Auto"
url = "{UP}/list"
`

// identity asks the proxy for an unencoded response, so the test client
// neither asks for gzip nor decodes it: the test sees the bytes the proxy
// sends. h, if set, is added.
func identity(h http.Header) http.Header {
	out := http.Header{"Accept-Encoding": {"identity"}}
	for k, v := range h {
		out[k] = v
	}
	return out
}

// encodingPaths are the paths of the upstream /list: the auto channel, a
// resource and a prefix-signed resource of it.
func encodingPaths(t *testing.T, e *directEnv) []string {
	t.Helper()
	p, err := e.signer.PrefixPath("auto", e.upURL+"/", false)
	if err != nil {
		t.Fatal(err)
	}
	return []string{"/c/auto", e.signer.Path("auto", e.upURL+"/list"), p + "list"}
}

// TestContentEncoding checks spec 3.4: a response with a Content-Encoding
// the transport did not undo (a request with Range, or x-gzip) is decoded
// before the detection, so a compressed playlist is refused or rewritten
// like the plain one, and nothing reaches the player encoded without its
// Content-Encoding.
func TestContentEncoding(t *testing.T) {
	const secret = "token-a1b2c3"
	stream := "http://radio.example/" + secret + "/stream.mp3"
	media := binaryBody('G', 4096)
	tests := []struct {
		name, ctype, body string
		// want: "refused", "hls", "dash" or "media"
		want string
	}{
		{name: "pls", ctype: "application/octet-stream", body: "[playlist]\nNumberOfEntries=1\nFile1=" + stream + "\n", want: "refused"},
		{name: "pls by type", ctype: "audio/x-scpls", body: "[playlist]\nFile1=" + stream + "\n", want: "refused"},
		{name: "hls", ctype: "application/octet-stream", body: "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\n" + stream + "\n", want: "hls"},
		{name: "hls by type", ctype: "application/vnd.apple.mpegurl", body: "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\n" + stream + "\n", want: "hls"},
		{name: "dash", ctype: "application/octet-stream", want: "dash"},
		{name: "media", ctype: "video/mp2t", body: media, want: "media"},
	}
	for _, tt := range tests {
		for _, coding := range []string{"gzip", "x-gzip"} {
			for _, rng := range []string{"", "bytes=0-"} {
				name := tt.name + "/" + coding
				if rng != "" {
					name += "/range"
				}
				t.Run(name, func(t *testing.T) {
					e := newDirectEnv(t, allowLoopback, encodingTOML)
					body := tt.body
					if tt.want == "dash" {
						body = dashManifest(e.upURL)
					}
					// The upstream ignores Range and always sends the
					// whole, compressed body (a pre-compressed object).
					e.up.add("/list", file{ctype: tt.ctype, status: http.StatusOK, body: gzipString(t, body), header: http.Header{
						"Content-Encoding": {coding}, "Etag": {`"v1"`},
					}})
					var h http.Header
					if rng != "" {
						h = http.Header{"Range": {rng}}
					}
					for _, p := range encodingPaths(t, e) {
						resp, got := e.get(t, p, identity(h))
						if ce := resp.Header.Get("Content-Encoding"); ce != "" {
							t.Errorf("GET %s: Content-Encoding %q, want none", p, ce)
						}
						if strings.HasPrefix(got, "\x1f\x8b") {
							t.Errorf("GET %s: compressed body reached the client", p)
						}
						switch tt.want {
						case "refused":
							if resp.StatusCode != http.StatusBadGateway || strings.Contains(got, secret) {
								t.Errorf("GET %s: status %d, body %q, want 502 without the playlist", p, resp.StatusCode, got)
							}
						case "hls":
							if resp.StatusCode != http.StatusOK || !strings.Contains(got, e.proxy.URL+"/r/auto/") || strings.Contains(got, "radio.example") {
								t.Errorf("GET %s: status %d, body %q, want the rewritten playlist", p, resp.StatusCode, got)
							}
						case "dash":
							if resp.StatusCode != http.StatusOK || !strings.Contains(got, e.proxy.URL+"/p/auto/") || resp.Header.Get("Content-Type") != manifestContentType {
								t.Errorf("GET %s: status %d, Content-Type %q, want the rewritten manifest", p, resp.StatusCode, resp.Header.Get("Content-Type"))
							}
						case "media":
							if resp.StatusCode != http.StatusOK || got != tt.body {
								t.Errorf("GET %s: status %d, %d bytes, want 200 and the decoded body", p, resp.StatusCode, len(got))
							}
							for _, k := range []string{"Etag", "Content-Length"} {
								if v := resp.Header.Get(k); v != "" {
									t.Errorf("GET %s: %s %q for a decoded body, want none", p, k, v)
								}
							}
						}
					}
					logs := e.logs.String()
					if strings.Contains(logs, secret) {
						t.Errorf("log leaks the upstream path:\n%s", logs)
					}
					if n := strings.Count(logs, `msg="unsupported playlist format refused"`); tt.want == "refused" && n != 3 {
						t.Errorf("%d refusal lines, want 3:\n%s", n, logs)
					}
				})
			}
		}
	}
}

// TestContentEncodingHead: a HEAD answer carries the headers of the
// decoded body.
func TestContentEncodingHead(t *testing.T) {
	e := newDirectEnv(t, allowLoopback, encodingTOML)
	e.up.add("/list", file{ctype: "video/mp2t", status: http.StatusOK, body: gzipString(t, "media"), header: http.Header{
		"Content-Encoding": {"gzip"}, "Etag": {`"v1"`},
	}})
	resp, body := do(t, e.client, http.MethodHead, e.proxy.URL+"/c/auto", identity(http.Header{"Range": {"bytes=0-"}}))
	if resp.StatusCode != http.StatusOK || body != "" {
		t.Errorf("HEAD: status %d, body %q", resp.StatusCode, body)
	}
	for _, k := range []string{"Content-Encoding", "Etag", "Content-Length"} {
		if v := resp.Header.Get(k); v != "" {
			t.Errorf("HEAD: %s %q, want none", k, v)
		}
	}
}

// TestContentEncodingRefused: a coding the proxy does not decode fails
// with 502 (fail closed: it cannot see whether the body is a playlist),
// also for a relayed 4xx and a HEAD request.
func TestContentEncodingRefused(t *testing.T) {
	for _, coding := range []string{"br", "deflate", "zstd", "gzip, gzip"} {
		t.Run(coding, func(t *testing.T) {
			e := newDirectEnv(t, allowLoopback, encodingTOML)
			e.up.add("/list", file{ctype: "application/octet-stream", status: http.StatusOK, body: "\x1b\x8f\x00\x00encoded", header: http.Header{"Content-Encoding": {coding}}})
			for _, p := range encodingPaths(t, e) {
				for _, h := range []http.Header{nil, {"Range": {"bytes=0-"}}} {
					resp, got := e.get(t, p, identity(h))
					if resp.StatusCode != http.StatusBadGateway || strings.Contains(got, "encoded") {
						t.Errorf("GET %s %v: status %d, body %q, want 502", p, h, resp.StatusCode, got)
					}
				}
			}
			resp, _ := do(t, e.client, http.MethodHead, e.proxy.URL+"/c/auto", identity(nil))
			if resp.StatusCode != http.StatusBadGateway {
				t.Errorf("HEAD: status %d, want 502", resp.StatusCode)
			}
			e.up.add("/list", file{ctype: "text/html", status: http.StatusNotFound, body: "\x1b\x8f\x00\x00encoded", header: http.Header{"Content-Encoding": {coding}}})
			if resp, got := e.get(t, "/c/auto", identity(nil)); resp.StatusCode != http.StatusBadGateway || strings.Contains(got, "encoded") {
				t.Errorf("404: status %d, body %q, want 502", resp.StatusCode, got)
			}
			logs := e.logs.String()
			if !strings.Contains(logs, "unsupported content encoding") || !strings.Contains(logs, "reason=other") {
				t.Errorf("log lacks the refusal:\n%s", logs)
			}
		})
	}

	t.Run("identity", func(t *testing.T) {
		e := newDirectEnv(t, allowLoopback, encodingTOML)
		media := binaryBody('G', 1024)
		e.up.add("/list", file{ctype: "video/mp2t", status: http.StatusOK, body: media, header: http.Header{"Content-Encoding": {"identity"}}})
		for _, p := range encodingPaths(t, e) {
			if resp, got := e.get(t, p, identity(http.Header{"Range": {"bytes=0-"}})); resp.StatusCode != http.StatusOK || got != media {
				t.Errorf("GET %s: status %d, %d bytes, want 200 and the body", p, resp.StatusCode, len(got))
			}
		}
	})
}

// TestContentEncodingPartial: a 206 with a coding counts encoded bytes in
// its Content-Range; it is fetched again without Range (spec 3.4), and a
// second 206 gives 502.
func TestContentEncodingPartial(t *testing.T) {
	const pl = "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\nhttp://cdn.example/seg1.ts\n"
	media := noise(8192)
	tests := []struct {
		name, ctype, body, rng string
		always206              bool
		// want
		status int
		check  func(string) bool
	}{
		{
			name: "hls", ctype: "application/octet-stream", body: pl, rng: "bytes=0-", status: http.StatusOK,
			check: func(b string) bool { return strings.Contains(b, "/r/auto/") && !strings.Contains(b, "cdn.example") },
		},
		{
			name: "media from the start", ctype: "video/mp2t", body: media, rng: "bytes=0-", status: http.StatusOK,
			check: func(b string) bool { return b == media },
		},
		{
			name: "media mid-stream", ctype: "video/mp2t", body: media, rng: "bytes=100-199", status: http.StatusOK,
			check: func(b string) bool { return b == media },
		},
		{
			name: "always 206", ctype: "video/mp2t", body: media, rng: "bytes=0-", always206: true, status: http.StatusBadGateway,
			check: func(b string) bool { return !strings.HasPrefix(b, "\x1f\x8b") && !strings.Contains(b, media[:16]) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newDirectEnv(t, allowLoopback, encodingTOML)
			enc := gzipString(t, tt.body)
			f := file{ctype: tt.ctype, body: enc, header: http.Header{"Content-Encoding": {"gzip"}}}
			if tt.always206 {
				f.status = http.StatusPartialContent
				f.header["Content-Range"] = []string{"bytes 0-9/100"}
			}
			// Without status: http.ServeContent answers Range with 206 and
			// a Content-Range of the encoded bytes.
			e.up.add("/list", f)
			for i, p := range encodingPaths(t, e) {
				resp, got := e.get(t, p, identity(http.Header{"Range": {tt.rng}}))
				if resp.StatusCode != tt.status || !tt.check(got) {
					t.Errorf("GET %s: status %d, %d bytes %.40q, want %d", p, resp.StatusCode, len(got), got, tt.status)
				}
				if cr := resp.Header.Get("Content-Range"); cr != "" {
					t.Errorf("GET %s: Content-Range %q, want none", p, cr)
				}
				reqs := e.up.requestsFor("/list")
				if len(reqs) != 2*(i+1) || reqs[2*i].Range != tt.rng || reqs[2*i+1].Range != "" {
					t.Errorf("GET %s: upstream requests %+v, want one with Range, then one without", p, reqs)
				}
			}
		})
	}
}

// noise is n bytes of a fixed pseudo-random stream: binary, and gzip
// does not shrink it, so ranges of the encoded bytes exist.
func noise(n int) string {
	b := make([]byte, n)
	r := rand.New(rand.NewChaCha8([32]byte{1}))
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return string(b)
}
