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
		{name: "pls as text", ctype: "text/plain", body: "[playlist]\r\nFile1=" + stream + "\r\n", format: "pls"},
		{name: "plain m3u", ctype: "audio/x-mpegurl", body: stream + "\n", format: "m3u"},
		{name: "plain m3u as octet-stream", ctype: "application/octet-stream", body: "# radio\n" + stream + "\n", format: "m3u"},
		{
			// Auto channels and resources rewrite it (spec 3.3).
			name: "dash on a raw channel", ctype: "application/dash+xml",
			body: `<?xml version="1.0"?><MPD><BaseURL>` + stream + `</BaseURL></MPD>`, format: "dash", channels: []string{"raw"},
		},
		{name: "xspf", ctype: "application/xspf+xml", body: `<playlist xmlns="http://xspf.org/ns/0/"><trackList><track><location>` + stream + `</location></track></trackList></playlist>`, format: "xspf"},
		{name: "asx", ctype: "video/x-ms-asf", body: `<ASX version="3.0"><Entry><Ref href="` + stream + `"/></Entry></ASX>`, format: "asx"},
		{
			// An auto channel rewrites it; a raw channel must not pipe it.
			name: "extended m3u on a raw channel", ctype: "audio/x-mpegurl",
			body: "#EXTM3U\n#EXTINF:-1,Radio\n" + stream + "\n", format: "m3u", channels: []string{"raw"},
		},
		{name: "mp3", ctype: "audio/mpeg", body: "ID3\x04\x00\x00\x00\x00\x00\x00" + strings.Repeat("\xff\xfb\x90\x64", 200)},
		{name: "mpeg-ts", ctype: "video/mp2t", body: strings.Repeat("\x47\x40\x00\x10"+strings.Repeat("\xff", 184), 4)},
		{name: "aac", ctype: "audio/aac", body: strings.Repeat("\xff\xf1\x50\x80\x02\x1f\xfc", 100)},
		{name: "ogg", ctype: "audio/ogg", body: "OggS\x00\x02" + strings.Repeat("\x00", 600)},
		{name: "fmp4", ctype: "video/mp4", body: "\x00\x00\x00\x18ftypiso6\x00\x00\x00\x00iso6dash" + strings.Repeat("\x00", 600)},
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
			name: "XSPF after a long comment", ctype: "application/octet-stream",
			body: longComment + `<playlist><trackList><track><location>` + stream + `</location></track></trackList></playlist>`,
			want: [3]int{502, 502, 502},
		},
		{
			name: "no root within 64 KiB", ctype: "text/xml",
			body: "<!--" + strings.Repeat("x", 70000) + `--><playlist><location>` + stream + `</location></playlist>`,
			want: [3]int{502, 502, 502},
		},
		{
			name: "MPD hidden in a comment from encoding/xml", ctype: "text/xml",
			body: `<!---><MPD><BaseURL>` + stream + `</BaseURL><Period/></MPD>--><x/>`,
			want: [3]int{502, 502, 502},
		},
		{
			name: "MPD hidden in a processing instruction", ctype: "text/xml",
			body: `<?p a><MPD><BaseURL>` + stream + `</BaseURL><Period/></MPD><q ?><x/>`,
			want: [3]int{502, 502, 502},
		},
		{
			name: "MPD hidden in a doctype", ctype: "text/xml",
			body: `<!DOCTYPE x [<!ENTITY a "]><MPD><BaseURL>` + stream + `</BaseURL><Period/></MPD><!-- ">]><x/>`,
			want: [3]int{502, 502, 502},
		},
		{
			name: "MPD after another root", ctype: "text/xml",
			body: `<x/><MPD><BaseURL>` + stream + `</BaseURL><Period/></MPD>`,
			want: [3]int{502, 502, 502},
		},
		{
			name: "MPD after another root and a declaration", ctype: "text/xml",
			body: "<?xml version=\"1.0\"?>\n<x></x>\n<MPD><BaseURL>" + stream + "</BaseURL><Period/></MPD>",
			want: [3]int{502, 502, 502},
		},
		{
			name: "MPD beyond the sniff window", ctype: "text/xml",
			body: "<x>" + strings.Repeat("t", 70000) + "</x><MPD><BaseURL>" + stream + "</BaseURL><Period/></MPD>",
			want: [3]int{502, 502, 502},
		},
		{
			name: "HTML page", ctype: "text/html",
			body: `<!DOCTYPE html><html><body><a href="` + stream + `">x</a></body></html>`,
			want: [3]int{502, 502, 502},
		},
		{
			name: "UTF-16BE MPD without BOM", ctype: "text/xml",
			body: utf16be(`<?xml version="1.0"?><MPD><BaseURL>` + stream + `</BaseURL></MPD>`),
			want: [3]int{502, 502, 502},
		},
		{
			name: "UTF-16 MPD", ctype: "text/xml",
			body: "\xff\xfe" + utf16le(`<MPD><BaseURL>`+stream+`</BaseURL></MPD>`),
			want: [3]int{502, 502, 502},
		},
	}
	mpd := `<MPD><BaseURL>` + stream + `</BaseURL><Period/></MPD>`
	for _, lead := range []struct{ name, short, long string }{
		{"text", "x", strings.Repeat("x", 600)},
		{"a second BOM", "\ufeff\ufeff", strings.Repeat("\ufeff", 200)},
		{"a comment line", "#c\n", strings.Repeat("#c\n", 200)},
		{"NBSP", "\u00a0", strings.Repeat("\u00a0", 300)},
		{"NUL", "\x00", ""},
	} {
		tests = append(tests, struct {
			name, ctype, body string
			want              [3]int
		}{"MPD after " + lead.name, "text/xml", lead.short + mpd, [3]int{502, 502, 502}})
		if lead.long != "" {
			tests = append(tests, struct {
				name, ctype, body string
				want              [3]int
			}{"MPD after long " + lead.name, "text/xml", lead.long + mpd, [3]int{502, 502, 502}})
		}
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
		{"webvtt", "text/vtt", "WEBVTT\n\n00:00.000 --> 00:02.000\nHello\n"},
		{"mp3 from mid-frame starting with <", "audio/mpeg", "\x3c\x9a\x01" + strings.Repeat("\xff\xfb\x90\x64\x00\x0f\xf0\x00\x00\x69", 300)},
		{"random bytes", "application/octet-stream", string(random)},
		{"fmp4 with a 60-byte ftyp", "video/mp4", "\x00\x00\x00<ftypiso6\x00\x00\x00\x00iso6dash" + strings.Repeat("\x00", 2000)},
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
