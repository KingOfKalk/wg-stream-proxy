// SPDX-License-Identifier: AGPL-3.0-or-later

package hls

import (
	"strings"
	"testing"
)

func TestUnsupported(t *testing.T) {
	const bom = "\xef\xbb\xbf"
	// Media starts: what raw channels and /r/ resources stream.
	var (
		ts      = "\x47\x40\x00\x10\x00\x00\xb0\x0d\x00\x01\xc1\x00\x00" + strings.Repeat("\xff", 40)
		fmp4    = "\x00\x00\x00\x18ftypiso6\x00\x00\x00\x00iso6dash"
		mp3ID3  = "ID3\x04\x00\x00\x00\x00\x00\x23TSSE\x00\x00\x00\x0f\x00\x00\x03Lavf"
		mp3Sync = "\xff\xfb\x90\x64\x00\x0f\xf0\x00\x00\x69\x00\x00\x00\x08"
		adts    = "\xff\xf1\x50\x80\x02\x1f\xfc\x21\x00\x49\x90\x02\x19"
		ogg     = "OggS\x00\x02\x00\x00\x00\x00\x00\x00\x00\x00\x2a\x00\x00\x00"
		key     = "\x8f\x1a\x00\x3c\xe1\x92\x7b\x05\x6d\xaa\x10\x00\x4e\xc7\x99\x21"
	)
	tests := []struct {
		name        string
		contentType string
		prefix      string
		configured  Type
		want        string
	}{
		// Content types.
		{"pls content type", "audio/x-scpls", "", Auto, "pls"},
		{"pls content type, case and parameters", "Audio/X-SCPLS; charset=utf-8", "", Auto, "pls"},
		{"pls scpls", "audio/scpls", "", Auto, "pls"},
		{"pls+xml", "application/pls+xml", "", Auto, "pls"},
		{"m3u x-mpegurl", "audio/x-mpegurl", "", Auto, "m3u"},
		{"m3u mpegurl", "audio/mpegurl", "", Auto, "m3u"},
		{"dash content type", "application/dash+xml", "", Auto, "dash"},
		{"xspf content type", "application/xspf+xml", "", Auto, "xspf"},
		{"asx video", "video/x-ms-asx", "", Auto, "asx"},
		{"asx audio", "audio/x-ms-asx", "", Auto, "asx"},
		{"wvx", "video/x-ms-wvx", "", Auto, "asx"},
		{"wax", "audio/x-ms-wax", "", Auto, "asx"},
		{"content type wins over a media body", "audio/x-scpls", mp3Sync, Auto, "pls"},
		{"raw ignores the content type", "audio/x-mpegurl", mp3Sync, Raw, ""},
		{"raw ignores a pls content type", "audio/x-scpls", "", Raw, ""},
		{"asf media is not asx", "video/x-ms-asf", "\x30\x26\xb2\x75\x8e\x66\xcf\x11", Auto, ""},

		// PLS bodies.
		{"pls body", "", "[playlist]\nNumberOfEntries=1\nFile1=http://radio.example/stream\n", Auto, "pls"},
		{"pls body lower case header", "text/plain", "[Playlist]\r\nFile1=http://radio.example/stream\r\n", Auto, "pls"},
		{"pls body after BOM and whitespace", "", bom + "\r\n [playlist]\n", Auto, "pls"},
		{"pls body on a raw channel", "audio/mpeg", "[playlist]\nFile1=http://radio.example/stream\n", Raw, "pls"},
		{"asf reference body", "video/x-ms-asf", "[Reference]\r\nRef1=http://radio.example/stream?MSWMExt=.asf\r\n", Auto, "asx"},

		// M3U bodies.
		{"extended m3u on a raw channel", "application/vnd.apple.mpegurl", "#EXTM3U\n#EXTINF:-1,Radio\nhttp://radio.example/stream\n", Raw, "m3u"},
		{"plain m3u", "text/plain", "http://radio.example/stream.mp3\n", Auto, "m3u"},
		{"plain m3u https", "", "https://radio.example/a\r\nhttps://radio.example/b\r\n", Auto, "m3u"},
		{"plain m3u with comments and blank lines", "", "# my radio\n\n#EXTINF:-1,Radio\n  HTTP://radio.example/stream\n", Auto, "m3u"},
		{"plain m3u other scheme", "", "rtsp://radio.example/live\n", Auto, "m3u"},
		{"plain m3u scheme-relative", "", "//radio.example/stream\n", Auto, "m3u"},
		{"plain m3u without line end", "", "http://radio.example/stream", Auto, "m3u"},
		{"plain m3u on a raw channel", "audio/mpeg", "http://radio.example/stream\n", Raw, "m3u"},
		{"plain m3u with lone CR", "", "# radio\rhttp://radio.example/a\rhttp://radio.example/b\r", Auto, "m3u"},
		{"plain m3u after BOM", "", bom + "http://radio.example/stream\n", Auto, "m3u"},
		{"relative lines are not detected", "text/plain", "stream.mp3\n", Auto, ""},
		{"comment line cut by the prefix", "", "# " + strings.Repeat("x", 600), Auto, ""},
		{"url with control bytes is not text", "", "ab://\x00\x01\x02", Auto, ""},
		{"one-letter scheme is not a url", "", "G://\x10\x00", Auto, ""},
		{"text that is not a url", "text/plain", "Service unavailable\n", Auto, ""},

		// XML playlists.
		{"dash body", "application/xml", `<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic">`, Auto, "dash"},
		{"dash body with comment", "", "<?xml version=\"1.0\"?>\n<!-- generated -->\n<MPD>", Auto, "dash"},
		{"dash body prefixed root", "", `<mpd:MPD xmlns:mpd="urn:mpeg:dash:schema:mpd:2011">`, Auto, "dash"},
		{"dash body on a raw channel", "video/mp2t", "<MPD\n type=\"static\">", Raw, "dash"},
		{"xspf body", "text/xml", `<?xml version="1.0" encoding="UTF-8"?><playlist version="1" xmlns="http://xspf.org/ns/0/">`, Auto, "xspf"},
		{"asx body", "", "<ASX version=\"3.0\">\r\n<Entry><Ref href=\"http://radio.example/s\"/></Entry>", Auto, "asx"},
		{"asx body lower case", "", "<asx version=\"3.0\">", Auto, "asx"},
		{"xml with doctype", "", "<!DOCTYPE asx>\n<asx>", Auto, "asx"},
		{"other xml root", "application/xml", `<?xml version="1.0"?><tt xmlns="http://www.w3.org/ns/ttml">`, Auto, ""},
		{"html page", "text/html", "<!DOCTYPE html>\n<html><body><a href=\"http://x.example/\">x</a>", Auto, ""},
		{"root name is a prefix only", "", "<MPDX>", Auto, ""},
		{"unterminated declaration", "", "<?xml version=\"1.0\"", Auto, ""},
		{"unterminated comment", "", "<!-- <MPD>", Auto, ""},

		// Media and other resources stream.
		{"mpeg-ts", "video/mp2t", ts, Auto, ""},
		{"mpeg-ts raw", "video/mp2t", ts, Raw, ""},
		{"fmp4", "video/mp4", fmp4, Auto, ""},
		{"mp3 with id3", "audio/mpeg", mp3ID3, Auto, ""},
		{"mp3 frame sync", "audio/mpeg", mp3Sync, Auto, ""},
		{"aac adts", "audio/aac", adts, Auto, ""},
		{"aac adts without content type", "", adts, Auto, ""},
		{"ogg", "application/ogg", ogg, Auto, ""},
		{"icy mp3 stream", "audio/mpeg", strings.Repeat(mp3Sync, 40), Raw, ""},
		{"aes key", "application/octet-stream", key, Auto, ""},
		{"webvtt", "text/vtt", "WEBVTT\n\n00:00.000 --> 00:04.000\nhttp://not.a.playlist.example/\n", Auto, ""},
		{"empty body", "", "", Auto, ""},
		{"only whitespace", "", " \r\n\t", Auto, ""},
		{"octet-stream", "application/octet-stream", "data", Auto, ""},
		{"hls content type is not refused here", "application/vnd.apple.mpegurl", "", Auto, ""},
		{"configured hls is never refused", "audio/x-scpls", "[playlist]\n", HLS, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Unsupported(tt.contentType, []byte(tt.prefix), tt.configured)
			if got != tt.want {
				t.Errorf("Unsupported(%q, %q, %v) = %q, want %q", tt.contentType, tt.prefix, tt.configured, got, tt.want)
			}
		})
	}
}

// TestUnsupportedHLSFirst checks the order of spec 3.1 and 3.3: a body
// Detect calls HLS on an auto channel or resource is rewritten, never
// refused, even with an M3U Content-Type.
func TestUnsupportedHLSFirst(t *testing.T) {
	for _, ct := range []string{"audio/x-mpegurl", "audio/mpegurl", "text/plain", ""} {
		if got := Detect(ct, []byte("#EXTM3U\n#EXTINF:4,\nseg1.ts\n"), Auto); got != HLS {
			t.Errorf("Detect(%q, #EXTM3U) = %v, want HLS", ct, got)
		}
	}
}

func FuzzUnsupported(f *testing.F) {
	for _, s := range []string{
		"[playlist]\nFile1=http://a.example/\n", "http://a.example/\n", "#EXTM3U\n", "# c\n//a.example/x\n",
		"<?xml version=\"1.0\"?><!-- c --><MPD>", "<!DOCTYPE asx><asx>", "\x47\x40\x00\x10", "ID3\x04", "",
	} {
		f.Add("", []byte(s), uint8(Auto))
		f.Add("audio/x-scpls", []byte(s), uint8(Raw))
	}
	formats := map[string]bool{"": true, "pls": true, "m3u": true, "dash": true, "xspf": true, "asx": true}
	f.Fuzz(func(t *testing.T, contentType string, prefix []byte, configured uint8) {
		typ := Type(configured % 3)
		got := Unsupported(contentType, prefix, typ)
		if !formats[got] {
			t.Fatalf("Unsupported = %q, not a known format", got)
		}
		if typ == HLS && got != "" {
			t.Fatalf("configured hls refused as %q", got)
		}
	})
}
