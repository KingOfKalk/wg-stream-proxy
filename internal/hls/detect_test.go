// SPDX-License-Identifier: AGPL-3.0-or-later

package hls

import (
	"strings"
	"testing"
)

func TestDetect(t *testing.T) {
	const bom = "\xef\xbb\xbf"
	tests := []struct {
		name        string
		contentType string
		prefix      string
		configured  Type
		want        Type
	}{
		{"configured hls wins over raw body", "audio/mpeg", "ID3", HLS, HLS},
		{"configured raw wins over playlist", "application/vnd.apple.mpegurl", "#EXTM3U\n", Raw, Raw},
		{"apple content type", "application/vnd.apple.mpegurl", "", Auto, HLS},
		{"x-mpegurl content type", "application/x-mpegurl", "", Auto, HLS},
		{"content type case-insensitive", "Application/X-MpegURL", "", Auto, HLS},
		{"content type with parameters", "application/vnd.apple.mpegurl; charset=utf-8", "", Auto, HLS},
		{"content type with spaces", "  application/x-mpegURL ;charset=UTF-8", "", Auto, HLS},
		{"other content type, playlist body", "text/plain", "#EXTM3U\n#EXT-X-VERSION:3\n", Auto, HLS},
		{"no content type, playlist body", "", "#EXTM3U", Auto, HLS},
		{"BOM before EXTM3U", "", bom + "#EXTM3U\n", Auto, HLS},
		{"whitespace before EXTM3U", "", " \t\r\n\n#EXTM3U\n", Auto, HLS},
		{"BOM and whitespace before EXTM3U", "", bom + "\r\n  #EXTM3U\r\n", Auto, HLS},
		{"whitespace before BOM is raw", "", " " + bom + "#EXTM3U\n", Auto, Raw},
		{"lowercase extm3u is raw", "", "#extm3u\n", Auto, Raw},
		{"truncated EXTM3U is raw", "", "#EXTM3", Auto, Raw},
		{"MPEG-TS body is raw", "video/mp2t", "\x47\x40\x00\x10", Auto, Raw},
		{"audio/mpegurl is not in the spec list", "audio/mpegurl", "http://example.com/a.mp3\n", Auto, Raw},
		{"empty body is raw", "", "", Auto, Raw},
		{"configured dash wins over playlist", "application/vnd.apple.mpegurl", "#EXTM3U\n", DASH, DASH},
		{"configured hls wins over manifest", "application/dash+xml", "<MPD>", HLS, HLS},
		{"configured raw wins over manifest", "application/dash+xml", "<MPD>", Raw, Raw},
		{"dash content type", "application/dash+xml", "", Auto, DASH},
		{"dash content type case-insensitive", " Application/DASH+XML ; charset=utf-8", "", Auto, DASH},
		{"MPD root", "application/xml", `<?xml version="1.0" encoding="UTF-8"?>` + "\n<!-- packager -->\n" + `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic">`, Auto, DASH},
		{"MPD root with BOM and prefix", "", bom + "\n<mpd:MPD xmlns:mpd=\"urn:mpeg:dash:schema:mpd:2011\">", Auto, DASH},
		{"MPD root lower case", "text/xml", "<mpd>", Auto, DASH},
		{"other XML root", "text/xml", "<playlist>", Auto, Raw},
		{"hls content type wins over MPD body", "application/x-mpegurl", "<MPD>", Auto, HLS},
		{"dash content type wins over EXTM3U body", "application/dash+xml", "#EXTM3U\n", Auto, DASH},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Detect(tt.contentType, []byte(tt.prefix), tt.configured)
			if got != tt.want {
				t.Errorf("Detect(%q, %q, %v) = %v, want %v", tt.contentType, tt.prefix, tt.configured, got, tt.want)
			}
		})
	}
}

func TestDetectWithinSniffLen(t *testing.T) {
	ws := strings.Repeat(" ", SniffLen-len("#EXTM3U"))
	if got := Detect("", []byte(ws+"#EXTM3U"), Auto); got != HLS {
		t.Errorf("EXTM3U ending at SniffLen: got %v, want HLS", got)
	}
}

func TestParseType(t *testing.T) {
	tests := []struct {
		in      string
		want    Type
		wantErr bool
	}{
		{"", Auto, false},
		{"auto", Auto, false},
		{"hls", HLS, false},
		{"raw", Raw, false},
		{"HLS", Auto, true},
		{"dash", DASH, false},
		{"DASH", Auto, true},
		{"mpd", Auto, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseType(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseType(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("ParseType(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestTypeString(t *testing.T) {
	for typ, want := range map[Type]string{Auto: "auto", HLS: "hls", Raw: "raw", DASH: "dash", Type(42): "Type(42)"} {
		if got := typ.String(); got != want {
			t.Errorf("Type(%d).String() = %q, want %q", uint8(typ), got, want)
		}
	}
}
