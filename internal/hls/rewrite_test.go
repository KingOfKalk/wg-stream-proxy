// SPDX-License-Identifier: AGPL-3.0-or-later

package hls

import (
	"bytes"
	"errors"
	"flag"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files in testdata/")

// proxyURL is a readable stand-in for the server's resource URL builder.
func proxyURL(dst, abs []byte) []byte {
	return append(append(dst, "http://proxy.example:2349/r/"...), abs...)
}

func mustParse(t testing.TB, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", s, err)
	}
	return u
}

func TestRewriteGolden(t *testing.T) {
	tests := []struct {
		file   string
		final  string
		golden string
	}{
		{"master.m3u8", "https://origin.example.com/live/channel/master.m3u8", "master.golden"},
		{"media_aes.m3u8", "https://origin.example.com/live/channel/720p/index.m3u8?session=1", "media_aes.golden"},
		// Relative URIs resolve against the final URL after a redirect, not
		// against the URL the channel is configured with.
		{"master.m3u8", "http://edge-7.example.net:8080/redirected/path/master.m3u8", "master_redirect.golden"},
	}
	for _, tt := range tests {
		t.Run(tt.golden, func(t *testing.T) {
			in, err := os.ReadFile(filepath.Join("testdata", tt.file))
			if err != nil {
				t.Fatal(err)
			}
			got, err := Rewrite(in, mustParse(t, tt.final), proxyURL)
			if err != nil {
				t.Fatalf("Rewrite: %v", err)
			}
			golden := filepath.Join("testdata", tt.golden)
			if *update {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("Rewrite output differs from %s (run with -update to regenerate)\ngot:\n%s", golden, got)
			}
		})
	}
}

func TestRewrite(t *testing.T) {
	const final = "https://origin.example.com/live/a/index.m3u8"
	const r = "http://proxy.example:2349/r/"
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"LF", "#EXTM3U\n#EXTINF:6,\nseg.ts\n", "#EXTM3U\n#EXTINF:6,\n" + r + "https://origin.example.com/live/a/seg.ts\n"},
		{"CRLF", "#EXTM3U\r\n#EXTINF:6,\r\nseg.ts\r\n", "#EXTM3U\r\n#EXTINF:6,\r\n" + r + "https://origin.example.com/live/a/seg.ts\r\n"},
		{"mixed line endings", "#EXTM3U\r\nseg1.ts\nseg2.ts\r\n", "#EXTM3U\r\n" + r + "https://origin.example.com/live/a/seg1.ts\n" + r + "https://origin.example.com/live/a/seg2.ts\r\n"},
		{"no final newline", "#EXTM3U\nseg.ts", "#EXTM3U\n" + r + "https://origin.example.com/live/a/seg.ts"},
		{"lone CR at end", "#EXTM3U\nseg.ts\r", "#EXTM3U\n" + r + "https://origin.example.com/live/a/seg.ts\r"},
		// ffmpeg also ends a line at a lone CR; a URI behind one must not stay
		// unrewritten inside what looks like a tag line.
		{"lone CR ends a line", "#EXTINF:6,\rseg.ts\n", "#EXTINF:6,\r" + r + "https://origin.example.com/live/a/seg.ts\n"},
		{"CR CR is two lines", "#EXTM3U\r\rseg.ts\r\n", "#EXTM3U\r\r" + r + "https://origin.example.com/live/a/seg.ts\r\n"},
		{"LF CR is two lines", "#EXTM3U\n\rseg.ts", "#EXTM3U\n\r" + r + "https://origin.example.com/live/a/seg.ts"},
		{"BOM kept", "\xef\xbb\xbf#EXTM3U\nseg.ts\n", "\xef\xbb\xbf#EXTM3U\n" + r + "https://origin.example.com/live/a/seg.ts\n"},
		{"BOM before URI line", "\xef\xbb\xbfseg.ts\n", "\xef\xbb\xbf" + r + "https://origin.example.com/live/a/seg.ts\n"},
		{"blank and whitespace lines kept", "#EXTM3U\n\n  \t\n\r\n", "#EXTM3U\n\n  \t\n\r\n"},
		{"whitespace around URI kept", "  seg.ts\t\n", "  " + r + "https://origin.example.com/live/a/seg.ts\t\n"},
		{"comments kept", "# URI=\"https://example.com/x\"\n#EXTM3U-ish comment\n", "# URI=\"https://example.com/x\"\n#EXTM3U-ish comment\n"},
		{"lowercase ext tag is a comment", "#ext-x-key:URI=\"k.key\"\n", "#ext-x-key:URI=\"k.key\"\n"},
		{"unknown tag without attributes", "#EXT-X-FOO\n#EXT-X-BAR:\n#EXT-X-BAZ:42\n", "#EXT-X-FOO\n#EXT-X-BAR:\n#EXT-X-BAZ:42\n"},
		{"URI in unknown tag", "#EXT-X-VENDOR:A=1,URI=\"x.json\"\n", "#EXT-X-VENDOR:A=1,URI=\"" + r + "https://origin.example.com/live/a/x.json\"\n"},
		{"two URI attributes", "#EXT-X-FOO:URI=\"a\",URI=\"b\"\n", "#EXT-X-FOO:URI=\"" + r + "https://origin.example.com/live/a/a\",URI=\"" + r + "https://origin.example.com/live/a/b\"\n"},
		{"spaces in attribute list", "#EXT-X-KEY:METHOD=AES-128, URI = \"k.key\" ,IV=0x1\n", "#EXT-X-KEY:METHOD=AES-128, URI = \"" + r + "https://origin.example.com/live/a/k.key\" ,IV=0x1\n"},
		{"-URI attributes rewritten", "#EXT-X-FOO:X-URI=\"https://example.com/a\",SERVER-URI=\"b\"\n", "#EXT-X-FOO:X-URI=\"" + r + "https://example.com/a\",SERVER-URI=\"" + r + "https://origin.example.com/live/a/b\"\n"},
		{"other names ending in URI kept", "#EXT-X-FOO:XURI=\"b\",NOURI=\"data:,x\"\n", "#EXT-X-FOO:XURI=\"b\",NOURI=\"data:,x\"\n"},
		{"whitespace-separated attributes", "#EXT-X-KEY:METHOD=AES-128 URI=\"k.key\"\n", "#EXT-X-KEY:METHOD=AES-128 URI=\"" + r + "https://origin.example.com/live/a/k.key\"\n"},
		{"no separator after quoted value", "#EXT-X-FOO:A=\"x\"URI=\"k\"\n", "#EXT-X-FOO:A=\"x\"URI=\"" + r + "https://origin.example.com/live/a/k\"\n"},
		{"repeated separators", "#EXT-X-FOO:A=1,, ,URI=\"k\"\n", "#EXT-X-FOO:A=1,, ,URI=\"" + r + "https://origin.example.com/live/a/k\"\n"},
		{"lowercase attribute name kept", "#EXT-X-FOO:uri=\"https://example.com/a\"\n", "#EXT-X-FOO:uri=\"https://example.com/a\"\n"},
		{"unquoted relative URI kept", "#EXT-X-FOO:URI=k.key\n", "#EXT-X-FOO:URI=k.key\n"},
		{"URI= inside quoted value kept", "#EXT-X-FOO:NAME=\"see URI=\",X=\"y\"\n", "#EXT-X-FOO:NAME=\"see URI=\",X=\"y\"\n"},
		{"empty URI kept", "#EXT-X-FOO:URI=\"\"\n", "#EXT-X-FOO:URI=\"\"\n"},
		{"quote inside value stops parsing", "#EXT-X-FOO:URI=\"a\",B=\"x,URI=\"c\"\n", "#EXT-X-FOO:URI=\"" + r + "https://origin.example.com/live/a/a\",B=\"x,URI=\"c\"\n"},
		{"malformed list stops parsing", "#EXT-X-FOO:URI=\"a\" junk,URI=\"b\"\n", "#EXT-X-FOO:URI=\"" + r + "https://origin.example.com/live/a/a\" junk,URI=\"b\"\n"},
		{"EXTINF title is not an attribute list", "#EXTINF:6.0,URI=\"https://example.com/x\"\n", "#EXTINF:6.0,URI=\"https://example.com/x\"\n"},
		{"EXTINF negative duration", "#EXTINF:-1 URI=\"https://example.com/x\",Name\n", "#EXTINF:-1 URI=\"https://example.com/x\",Name\n"},
		{"comma inside quoted value", "#EXT-X-FOO:CODECS=\"a,b\",URI=\"c\"\n", "#EXT-X-FOO:CODECS=\"a,b\",URI=\"" + r + "https://origin.example.com/live/a/c\"\n"},
		{"data URI line kept", "data:application/octet-stream;base64,AAAA\n", "data:application/octet-stream;base64,AAAA\n"},
		{"skd URI kept", "#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"skd://abc\"\n", "#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"skd://abc\"\n"},
		{"other scheme with odd bytes kept", "foo:%zz bar\n", "foo:%zz bar\n"},
		{"uppercase scheme rewritten", "HTTP://Example.COM/A.ts\n", r + "http://Example.COM/A.ts\n"},
		{"scheme-relative takes final scheme", "//cdn.example.org/a.ts\n", r + "https://cdn.example.org/a.ts\n"},
		{"absolute path", "/other/a.ts?x=1#frag\n", r + "https://origin.example.com/other/a.ts?x=1#frag\n"},
		{"dot segments resolved", "../../b/./c.ts\n", r + "https://origin.example.com/b/c.ts\n"},
		{"query only", "?v=2\n", r + "https://origin.example.com/live/a/index.m3u8?v=2\n"},
		{"plain http", "http://example.com/a.ts\n", r + "http://example.com/a.ts\n"},
		{"empty input", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Rewrite([]byte(tt.in), mustParse(t, final), proxyURL)
			if err != nil {
				t.Fatalf("Rewrite: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("Rewrite(%q)\n got %q\nwant %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRewriteErrors(t *testing.T) {
	const final = "https://origin.example.com/live/index.m3u8"
	longURL := "https://example.com/" + strings.Repeat("a", MaxURLLen-len("https://example.com/")+1)
	tests := []struct {
		name    string
		in      []byte
		final   *url.URL
		urlFor  func(dst, abs []byte) []byte
		wantErr error
	}{
		{"too large", bytes.Repeat([]byte("#"), MaxPlaylistSize+1), mustParse(t, final), proxyURL, ErrPlaylistTooLarge},
		{"NUL byte", []byte("#EXTM3U\n#EXTINF:6,\x00http://example.com/a.ts\n"), mustParse(t, final), proxyURL, ErrInvalidPlaylist},
		// Lines players may read as an http URI attribute the rewriter does not
		// see: rejected instead of passing an unrewritten URL.
		{"unquoted http URI", []byte("#EXT-X-KEY:METHOD=AES-128,URI=https://example.com/k\n"), mustParse(t, final), proxyURL, ErrInvalidPlaylist},
		{"unquoted scheme-relative URI", []byte("#EXT-X-KEY:METHOD=AES-128,URI=//example.com/k\n"), mustParse(t, final), proxyURL, ErrInvalidPlaylist},
		{"escaped quote before URI", []byte("#EXT-X-KEY:KEYFORMAT=\"a\\\"\",URI=\"https://example.com/k\"\n"), mustParse(t, final), proxyURL, ErrInvalidPlaylist},
		{"http URI after malformed element", []byte("#EXT-X-FOO:URI=\"a\" junk,URI=\"https://example.com/b\"\n"), mustParse(t, final), proxyURL, ErrInvalidPlaylist},
		{"http value of name ending in URI", []byte("#EXT-X-FOO:XURI=\"HTTPS://example.com/b\"\n"), mustParse(t, final), proxyURL, ErrInvalidPlaylist},
		{"quoted URI with leading space", []byte("#EXT-X-FOO:A=\"x\" junk URI=\" http://example.com/b\"\n"), mustParse(t, final), proxyURL, ErrInvalidPlaylist},
		{"invalid port", []byte("#EXTM3U\nhttp://example.com:port/a.ts\n"), mustParse(t, final), proxyURL, ErrInvalidURI},
		{"invalid escape in relative URI", []byte("#EXTM3U\nseg%zz.ts\n"), mustParse(t, final), proxyURL, ErrInvalidURI},
		{"invalid host in attribute", []byte("#EXT-X-KEY:METHOD=AES-128,URI=\"https://exa mple.com/k\"\n"), mustParse(t, final), proxyURL, ErrInvalidURI},
		{"colon in first relative segment", []byte("1http://example.com/a.ts\n"), mustParse(t, final), proxyURL, ErrInvalidURI},
		{"missing scheme name", []byte("://example.com/a.ts\n"), mustParse(t, final), proxyURL, ErrInvalidURI},
		{"upstream URL over limit", []byte(longURL + "\n"), mustParse(t, final), proxyURL, ErrURLTooLong},
		{"nil final URL", []byte("seg.ts\n"), nil, proxyURL, ErrInvalidFinalURL},
		{"relative final URL", []byte("seg.ts\n"), mustParse(t, "/live/index.m3u8"), proxyURL, ErrInvalidFinalURL},
		{"non-http final URL", []byte("seg.ts\n"), mustParse(t, "ftp://example.com/index.m3u8"), proxyURL, ErrInvalidFinalURL},
		{"urlFor returns quote", []byte("#EXT-X-MAP:URI=\"init.mp4\"\n"), mustParse(t, final), func(dst, _ []byte) []byte { return append(dst, `/r/"`...) }, ErrInvalidResourceURL},
		{"urlFor returns newline", []byte("seg.ts\n"), mustParse(t, final), func(dst, _ []byte) []byte { return append(dst, "/r/a\n#EXT-X-ENDLIST"...) }, ErrInvalidResourceURL},
		{"urlFor returns empty", []byte("seg.ts\n"), mustParse(t, final), func(dst, _ []byte) []byte { return dst }, ErrInvalidResourceURL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Rewrite(tt.in, tt.final, tt.urlFor)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Rewrite error = %v, want %v", err, tt.wantErr)
			}
			if got != nil {
				t.Errorf("Rewrite returned output %q with an error", got)
			}
		})
	}
}

func TestRewriteAcceptsLimits(t *testing.T) {
	final := mustParse(t, "https://origin.example.com/live/index.m3u8")
	maxURL := "https://example.com/" + strings.Repeat("a", MaxURLLen-len("https://example.com/"))
	if _, err := Rewrite([]byte(maxURL+"\n"), final, proxyURL); err != nil {
		t.Errorf("URL of exactly MaxURLLen bytes: %v", err)
	}
	big := append([]byte("#EXTM3U\n"), bytes.Repeat([]byte("#"), MaxPlaylistSize-len("#EXTM3U\n"))...)
	if _, err := Rewrite(big, final, proxyURL); err != nil {
		t.Errorf("playlist of exactly MaxPlaylistSize bytes: %v", err)
	}
}

func TestRewriteDoesNotModifyInput(t *testing.T) {
	in := []byte("#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"k.key\"\nseg.ts\n")
	orig := bytes.Clone(in)
	if _, err := Rewrite(in, mustParse(t, "https://origin.example.com/a/b.m3u8"), proxyURL); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(in, orig) {
		t.Errorf("input modified: %q", in)
	}
}

// TestRewriteWithSigner checks the pieces together: every URI the rewriter
// emits is a signed path that verifies to the resolved upstream URL.
func TestRewriteWithSigner(t *testing.T) {
	s := mustSigner(t, testKey)
	in, err := os.ReadFile(filepath.Join("testdata", "media_aes.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	final := mustParse(t, "https://origin.example.com/live/channel/720p/index.m3u8")
	var want []string
	out, err := Rewrite(in, final, func(dst, abs []byte) []byte {
		want = append(want, string(abs))
		return append(dst, "http://proxy.example:2349"+s.Path("ch-1", string(abs))...)
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for line := range bytes.Lines(out) {
		for rest := string(line); ; {
			i := strings.Index(rest, "http://proxy.example:2349/r/")
			if i < 0 {
				break
			}
			rest = rest[i+len("http://proxy.example:2349"):]
			end := strings.IndexAny(rest, "\"\n")
			channel, upstream, err := s.Parse(rest[:end])
			if err != nil || channel != "ch-1" {
				t.Fatalf("Parse(%q) = %q, %v", rest[:end], channel, err)
			}
			got = append(got, upstream)
			rest = rest[end:]
		}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("verified upstream URLs\n got %q\nwant %q", got, want)
	}
	if len(got) != 11 {
		t.Errorf("%d rewritten URIs, want 11", len(got))
	}
}
