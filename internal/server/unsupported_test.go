// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"math/rand/v2"
	"net/http"
	"strings"
	"testing"
)

// unsupportedTOML has an auto, a raw and an hls channel on one path, and
// an auto channel for the /r/ resources.
const unsupportedTOML = `
tunnel = "direct"

[[channel]]
name = "Auto"
url = "{UP}/list"

[[channel]]
name = "Raw"
url = "{UP}/list"
[channel.upstream]
type = "raw"

[[channel]]
name = "HLS"
url = "{UP}/list"
[channel.upstream]
type = "hls"
`

// TestUnsupportedPlaylists checks spec 3.3: a playlist the proxy does not
// rewrite is refused with 502 on auto and raw channels and /r/
// resources, so the player never gets its upstream URLs; media streams.
func TestUnsupportedPlaylists(t *testing.T) {
	// secret is a path segment of every URL in the playlists; it must
	// never reach the player or the log.
	const secret = "token-a1b2c3"
	stream := "http://radio.example/" + secret + "/stream.mp3"
	tests := []struct {
		name   string
		ctype  string
		body   string
		format string // "": streamed unchanged
		// channels lists the channels to check; default auto, raw and a
		// resource.
		channels []string
	}{
		{name: "pls", ctype: "audio/x-scpls", body: "[playlist]\nNumberOfEntries=1\nFile1=" + stream + "\n", format: "pls"},
		{name: "plain m3u", ctype: "audio/x-mpegurl", body: stream + "\n", format: "m3u"},
		{
			// Auto channels and resources rewrite it (spec 3.3).
			name: "dash on a raw channel", ctype: "application/dash+xml",
			body: `<?xml version="1.0"?><MPD><BaseURL>` + stream + `</BaseURL></MPD>`, format: "dash", channels: []string{"raw"},
		},
		{
			// An auto channel rewrites it; a raw channel must not pipe it.
			name: "extended m3u on a raw channel", ctype: "audio/x-mpegurl",
			body: "#EXTM3U\n#EXTINF:-1,Radio\n" + stream + "\n", format: "m3u", channels: []string{"raw"},
		},
		{name: "mp3", ctype: "audio/mpeg", body: "ID3\x04\x00\x00\x00\x00\x00\x00" + strings.Repeat("\xff\xfb\x90\x64", 200)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newDirectEnv(t, allowLoopback, unsupportedTOML)
			e.up.add("/list", file{ctype: tt.ctype, body: tt.body})
			var paths []string
			for _, ch := range tt.channels {
				paths = append(paths, "/c/"+ch)
			}
			if tt.channels == nil {
				// Both channels and a resource of a rewritten playlist.
				paths = []string{"/c/auto", "/c/raw", e.signer.Path("auto", e.upURL+"/list")}
			}
			for _, p := range paths {
				resp, body := e.get(t, p, nil)
				if tt.format == "" {
					if resp.StatusCode != http.StatusOK || body != tt.body {
						t.Errorf("GET %s: status %d, %d bytes, want 200 and the body", p, resp.StatusCode, len(body))
					}
					continue
				}
				if resp.StatusCode != http.StatusBadGateway {
					t.Errorf("GET %s: status %d, want 502", p, resp.StatusCode)
				}
				if strings.Contains(body, secret) || strings.Contains(body, "radio.example") {
					t.Errorf("GET %s: body leaks the upstream URL: %q", p, body)
				}
			}
			logs := e.logs.String()
			if strings.Contains(logs, secret) {
				t.Errorf("log leaks the upstream path:\n%s", logs)
			}
			n := strings.Count(logs, `msg="unsupported playlist format refused"`)
			if tt.format == "" {
				if n != 0 {
					t.Errorf("media refused:\n%s", logs)
				}
				return
			}
			if n != len(paths) {
				t.Errorf("%d warn lines, want %d:\n%s", n, len(paths), logs)
			}
			for _, want := range []string{"level=WARN", "route=direct", "format=" + tt.format, "url=" + e.upURL + "/list"} {
				if !strings.Contains(logs, want) {
					t.Errorf("log lacks %q:\n%s", want, logs)
				}
			}
		})
	}

	t.Run("hls channels rewrite", func(t *testing.T) {
		// The rewriter turns every URI into a resource URL or fails
		// closed (spec 3.2); nothing is refused before it.
		e := newDirectEnv(t, allowLoopback, unsupportedTOML)
		e.up.add("/list", file{ctype: "audio/x-mpegurl", body: stream + "\n"})
		resp, body := e.get(t, "/c/hls.m3u8", nil)
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(body, e.proxy.URL+"/r/hls/") || strings.Contains(body, "radio.example") {
			t.Errorf("hls: status %d, body %q", resp.StatusCode, body)
		}
	})

	t.Run("auto rewrites extended m3u", func(t *testing.T) {
		e := newDirectEnv(t, allowLoopback, unsupportedTOML)
		e.up.add("/list", file{ctype: "audio/x-mpegurl", body: "#EXTM3U\n#EXTINF:-1,Radio\n" + stream + "\n"})
		resp, body := e.get(t, "/c/auto", nil)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, e.proxy.URL+"/r/auto/") || strings.Contains(body, "radio.example") {
			t.Errorf("auto: status %d, body %q", resp.StatusCode, body)
		}
	})

	t.Run("HEAD and error statuses carry no URLs", func(t *testing.T) {
		e := newDirectEnv(t, allowLoopback, unsupportedTOML)
		e.up.add("/list", file{ctype: "audio/x-scpls", body: "[playlist]\nFile1=" + stream + "\n"})
		resp, body := do(t, e.client, http.MethodHead, e.proxy.URL+"/c/auto", nil)
		if resp.StatusCode != http.StatusOK || body != "" {
			t.Errorf("HEAD: status %d, body %q", resp.StatusCode, body)
		}
		e.up.add("/list", file{ctype: "audio/x-scpls", status: http.StatusNotFound, body: "not found"})
		if resp, body := e.get(t, "/c/auto", nil); resp.StatusCode != http.StatusNotFound || body != "not found" {
			t.Errorf("404: status %d, body %q", resp.StatusCode, body)
		}
	})
}

// TestSniffGap checks spec 3.1 and 3.3 for XML playlists whose root
// element is not in the first 512 bytes, or that a short look misreads:
// they are detected or refused, never piped with their URLs.
func TestSniffGap(t *testing.T) {
	const secret = "token-a1b2c3"
	stream := "http://radio.example/" + secret + "/stream.mp3"
	longComment := "<!--" + strings.Repeat("x", 1000) + "-->"
	tests := []struct {
		name, ctype, body string
		// want per path: /c/auto, /c/raw, /r/
		want [3]int
	}{
		{
			name: "doctype with internal subset", ctype: "text/xml",
			body: `<?xml version="1.0"?><!DOCTYPE MPD [<!ENTITY a "b">]><MPD><BaseURL>` + stream + `</BaseURL><Period/></MPD>`,
			// auto and /r/ detect DASH, whose rewriter rejects the DOCTYPE.
			want: [3]int{502, 502, 502},
		},
		{
			name: "MPD after a long comment", ctype: "application/octet-stream",
			body: longComment + `<MPD><BaseURL>` + stream + `</BaseURL><Period/></MPD>`,
			want: [3]int{200, 502, 200},
		},
		{
			name: "MPD hidden in a comment from encoding/xml", ctype: "text/xml",
			body: `<!---><MPD><BaseURL>` + stream + `</BaseURL><Period/></MPD>--><x/>`,
			want: [3]int{502, 502, 502},
		},
		{
			name: "UTF-16BE MPD without BOM", ctype: "text/xml",
			body: utf16be(`<?xml version="1.0"?><MPD><BaseURL>` + stream + `</BaseURL></MPD>`),
			want: [3]int{502, 502, 502},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newDirectEnv(t, allowLoopback, unsupportedTOML)
			e.up.add("/list", file{ctype: tt.ctype, body: tt.body})
			for i, p := range []string{"/c/auto", "/c/raw", e.signer.Path("auto", e.upURL+"/list")} {
				resp, body := e.get(t, p, nil)
				if resp.StatusCode != tt.want[i] {
					t.Errorf("GET %s: status %d, want %d", p, resp.StatusCode, tt.want[i])
				}
				if strings.Contains(body, secret) || strings.Contains(body, "radio.example") || strings.Contains(body, utf16le("radio")) || strings.Contains(body, utf16be("radio")) {
					t.Errorf("GET %s: body leaks the upstream URL", p)
				}
			}
		})
	}
}

// utf16be encodes ASCII s as UTF-16BE.
func utf16be(s string) string {
	b := make([]byte, 0, 2*len(s))
	for i := 0; i < len(s); i++ {
		b = append(b, 0, s[i])
	}
	return string(b)
}

// utf16le encodes ASCII s as UTF-16LE.
func utf16le(s string) string {
	b := make([]byte, 0, 2*len(s))
	for i := 0; i < len(s); i++ {
		b = append(b, s[i], 0)
	}
	return string(b)
}

// TestSniffStreams: TTML subtitles and media that only look like XML in
// part still stream unchanged.
func TestSniffStreams(t *testing.T) {
	ttml := `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<tt xmlns="http://www.w3.org/ns/ttml" xml:lang="en"><body><div><p begin="0s" end="2s">Hello</p></div></body></tt>` + "\n"
	rnd := rand.New(rand.NewPCG(3, 4))
	random := make([]byte, 4096)
	for i := range random {
		random[i] = byte(rnd.Uint32())
	}
	for _, tt := range []struct{ name, ctype, body string }{
		{"ttml", "application/ttml+xml", ttml},
		{"mp3 from mid-frame starting with <", "audio/mpeg", "\x3c\x9a\x01" + strings.Repeat("\xff\xfb\x90\x64\x00\x0f\xf0\x00\x00\x69", 300)},
		{"random bytes", "application/octet-stream", string(random)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newDirectEnv(t, allowLoopback, unsupportedTOML)
			e.up.add("/list", file{ctype: tt.ctype, body: tt.body})
			for _, p := range []string{"/c/auto", "/c/raw", e.signer.Path("auto", e.upURL+"/list")} {
				if resp, body := e.get(t, p, nil); resp.StatusCode != http.StatusOK || body != tt.body {
					t.Errorf("GET %s: status %d, %d bytes; want 200 and the body", p, resp.StatusCode, len(body))
				}
			}
		})
	}
	// A TTML document larger than the sniff window is refused.
	big := `<tt xmlns="http://www.w3.org/ns/ttml"><body>` + strings.Repeat(`<p>x</p>`, 9000) + `</body></tt>`
	e := newDirectEnv(t, allowLoopback, unsupportedTOML)
	e.up.add("/list", file{ctype: "application/ttml+xml", body: big})
	if resp, _ := e.get(t, "/c/auto", nil); resp.StatusCode != http.StatusBadGateway {
		t.Errorf("large TTML: status %d, want 502", resp.StatusCode)
	}
}
