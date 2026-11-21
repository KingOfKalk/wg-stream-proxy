// SPDX-License-Identifier: AGPL-3.0-or-later

package hls

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf16"
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
		{"comment line cut by the prefix", "", "# " + strings.Repeat("x", 600), Auto, "unknown"},
		{"url with control bytes is not text", "", "ab://\x00\x01\x02", Auto, ""},
		{"one-letter scheme is not a url", "", "G://\x10\x00", Auto, ""},
		{"text that is not a url", "text/plain", "Service unavailable\n", Auto, "unknown"},

		// XML playlists.
		{"dash body", "application/xml", `<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic">`, Auto, "dash"},
		{"dash body with comment", "", "<?xml version=\"1.0\"?>\n<!-- generated -->\n<MPD>", Auto, "dash"},
		{"dash body prefixed root", "", `<mpd:MPD xmlns:mpd="urn:mpeg:dash:schema:mpd:2011">`, Auto, "dash"},
		{"dash body on a raw channel", "video/mp2t", "<MPD\n type=\"static\">", Raw, "dash"},
		{"dash body on a dash channel", "application/dash+xml", "<MPD>", DASH, ""},
		{"xspf body", "text/xml", `<?xml version="1.0" encoding="UTF-8"?><playlist version="1" xmlns="http://xspf.org/ns/0/">`, Auto, "xspf"},
		{"asx body", "", "<ASX version=\"3.0\">\r\n<Entry><Ref href=\"http://radio.example/s\"/></Entry>", Auto, "asx"},
		{"asx body lower case", "", "<asx version=\"3.0\">", Auto, "asx"},
		{"xml with doctype", "", "<!DOCTYPE asx>\n<asx>", Auto, "asx"},
		{"ttml", "application/ttml+xml", ttml, Auto, ""},
		{"ttml on a raw channel", "", "\n" + ttml + "\n<!-- end -->\n", Raw, ""},
		{"ttml cut off", "application/xml", `<?xml version="1.0"?><tt xmlns="http://www.w3.org/ns/ttml">`, Auto, "unknown"},
		{"ttml followed by an MPD", "text/xml", ttml + `<MPD><BaseURL>http://a.example/</BaseURL></MPD>`, Auto, "dash"},
		{"ttml with a comment hiding an MPD", "text/xml", `<tt><!---></tt><MPD><BaseURL>http://a.example/</BaseURL></MPD>--></tt>`, Auto, "dash"},
		{"ttml with CDATA", "text/xml", `<tt><p><![CDATA[x]]></p></tt>`, Auto, "unknown"},
		{"ttml with > in an attribute", "text/xml", `<tt><p a="></tt><MPD/>"/></tt>`, Auto, "dash"},
		{"ttml with a processing instruction", "text/xml", `<tt><?p x?></tt>`, Auto, "unknown"},
		{"other xml root", "application/xml", `<x/>`, Auto, "unknown"},
		{"MPD after another root", "text/xml", `<x/><MPD type="static"><BaseURL>http://a.example/</BaseURL></MPD>`, Raw, "dash"},
		{"MPD after another root and a declaration", "text/xml", "<?xml version=\"1.0\"?>\n<x></x>\n<MPD type=\"static\">", Auto, "dash"},
		{"html page", "text/html", "<!DOCTYPE html>\n<html><body><a href=\"http://x.example/\">x</a></body></html>", Auto, "unknown"},
		{"root name is a prefix only", "", "<MPDX/>", Auto, "unknown"},
		{"unterminated declaration", "", "<?xml version=\"1.0\"", Auto, "unknown"},
		{"unterminated comment", "", "<!-- <MPD>", Auto, "dash"},
		{"not XML after <", "text/plain", "<<<", Auto, "unknown"},
		{"doctype with internal subset", "text/xml", `<?xml version="1.0"?><!DOCTYPE MPD [<!ENTITY a "b">]><MPD type="static">`, Raw, "dash"},
		{"doctype with internal subset, asx", "", `<!DOCTYPE asx [<!ENTITY a ">">]><asx>`, Auto, "unknown"},
		{"root after a long comment", "application/octet-stream", "<!--" + strings.Repeat("x", 1000) + "--><MPD>", Raw, "dash"},
		{"root beyond the prefix", "application/octet-stream", "<!--" + strings.Repeat("x", 1000), Auto, "unknown"},
		{"declared latin-1", "", `<?xml version="1.0" encoding="ISO-8859-1"?><playlist>`, Auto, "xspf"},
		{"utf-16le", "text/xml", "\xff\xfe<\x00M\x00P\x00D\x00>\x00", Auto, "unknown"},
		{"utf-16be", "", "\xfe\xff\x00<\x00M\x00P\x00D", Auto, "unknown"},
		{"utf-32le", "", "\xff\xfe\x00\x00<\x00\x00\x00", Raw, "unknown"},
		{"utf-32be", "", "\x00\x00\xfe\xff\x00\x00\x00<", Auto, "unknown"},
		{"utf-16 m3u", "audio/mpeg", "\xff\xfeh\x00t\x00t\x00p\x00:\x00/\x00/\x00", Raw, "unknown"},
		{"utf-16be without BOM", "text/xml", "\x00<\x00?\x00x\x00m\x00l", Auto, "unknown"},
		{"utf-16le without BOM", "text/xml", "<\x00?\x00x\x00m\x00l\x00", Auto, "unknown"},
		{"utf-16be without BOM after whitespace", "", "\x00\n\x00<\x00M\x00P\x00D", Raw, "unknown"},
		{"utf-32be without BOM", "", "\x00\x00\x00<\x00\x00\x00M", Auto, "unknown"},
		{"utf-32le without BOM", "", "<\x00\x00\x00M\x00\x00\x00", Auto, "unknown"},
		{"fmp4 with a 60-byte ftyp", "video/mp4", "\x00\x00\x00<ftypiso6\x00\x00\x00\x00", Auto, ""},
		{"comment hiding the root from other parsers", "text/xml", `<!---><MPD type="static"><BaseURL>http://a.example/</BaseURL></MPD>--><x/>`, Auto, "dash"},
		{"comment with a tag", "text/xml", `<!-- <MPD> --><x/>`, Auto, "dash"},
		{"processing instruction hiding the root", "text/xml", `<?p a><MPD><BaseURL>http://a.example/</BaseURL></MPD><q ?><x/>`, Auto, "dash"},
		{"declaration with >", "text/xml", `<?xml version="1.0" x=">"?><x/>`, Auto, "unknown"},
		{"doctype subset hiding the root", "text/xml", `<!DOCTYPE x [<!ENTITY a "]><MPD><BaseURL>http://a.example/</BaseURL></MPD><!-- ">]><x/>`, Auto, "dash"},
		{"doctype with internal subset is not read", "text/xml", `<!DOCTYPE x [<!ENTITY a "b">]><x/>`, Auto, "unknown"},
		{"plain prolog", "text/xml", "\ufeff<?xml version=\"1.0\"?>\n<!-- generated -->\n<!DOCTYPE html PUBLIC \"-//W3C//DTD XHTML 1.0//EN\" \"http://www.w3.org/TR/xhtml1.dtd\">\n<html>", Auto, "unknown"},

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
		"<!DOCTYPE MPD [<!ENTITY a \">\">]><MPD>", "\xff\xfe<\x00M\x00", "<!--" + strings.Repeat("x", 600),
		"<!---><MPD></MPD>--><x/>", "<?p a><MPD></MPD><q ?><x/>", "\x00<\x00?\x00x", "<\x00?\x00x\x00",
		"<x/><MPD></MPD>", ttml, "<tt><!---></tt><MPD/>--></tt>",
		"x<MPD>", "\ufeff\ufeff<MPD>", "#c\n<MPD/>", "\u00a0<MPD>", "\x00<mpd:MPD/>", "WEBVTT\n\n", "plain text, no playlist",
		"\x00\x01\x02\xff<MPD=a=\"b\">", "\x00<MPD!", "WEBVTT\n\n" + strings.Repeat("x", 600) + "<MPD>",
		"<asx><title>\xd6</title></asx>", "<playlist></playlist>\x00", "<html>\xe4\x00", "; \xe4\n[playlist]\n", "<\x9a\x01\xff",
		"<asx x=\"\xd6\">\x00", "<?xml \xd6?><!--\x00--><x:playlist\x00", "<!DOCTYPE\x00",
		"\xfe\xff\x9a\x31\x00\x01", "\xff\xfe\x31\x9a\x00\xd8", "\xfe\xff\xfe\xff\x00#", "\xff\xfe\xff\xfe#\x00", "\xff\xfe\x00\x00",
		"\xfe\xff\x00\xa0\x00[", "\xfe\xff\xd8\x3d\xdc\xfb\x00 ", "\xfe\xff\x00", "\xff\xfe\xd6\x00\x31\x00",
	} {
		f.Add("", []byte(s), uint8(Auto))
		f.Add("audio/x-scpls", []byte(s), uint8(Raw))
	}
	formats := map[string]bool{"": true, "pls": true, "m3u": true, "dash": true, "xspf": true, "asx": true, "unknown": true}
	f.Fuzz(func(t *testing.T, contentType string, prefix []byte, configured uint8) {
		typ := Type(configured % 3)
		got := Unsupported(contentType, prefix, typ)
		if !formats[got] {
			t.Fatalf("Unsupported = %q, not a known format", got)
		}
		if typ == HLS && got != "" {
			t.Fatalf("configured hls refused as %q", got)
		}
		// ASCII text after a UTF-16 byte order mark is refused, whatever
		// it says (spec 3.3).
		if typ == HLS || len(prefix) == 0 || 2+2*len(prefix) > SniffLen {
			return
		}
		for _, c := range prefix {
			if c < 0x20 && c != '\t' && c != '\n' && c != '\r' || c > 0x7e {
				return
			}
		}
		for _, be := range []bool{true, false} {
			if got := Unsupported(contentType, []byte(utf16Body(string(prefix), be)), typ); got == "" {
				t.Fatalf("UTF-16 (big-endian %v) of %q streamed", be, prefix)
			}
		}
	})
}

// TestSniff reads a longer start of bodies that begin like XML without a
// root element in the first SniffLen bytes (spec 3.1, 3.3).
func TestSniff(t *testing.T) {
	longComment := "<!--" + strings.Repeat("x", 1000) + "-->"
	tests := []struct {
		name, body string
		want       int // length of the prefix
		typ        Type
		format     string
	}{
		{"media", strings.Repeat("\x47\x40\x00\x10", 500), SniffLen, Raw, ""},
		{"short body", "<MPD/>", 6, DASH, "dash"},
		{"root in the first bytes", "<MPD>" + strings.Repeat(" ", 2000), 2005, DASH, "dash"},
		{"root after a long comment", longComment + "<MPD>" + strings.Repeat(" ", 2000), len(longComment) + 2005, DASH, "dash"},
		{"root after a long comment, short body", longComment + "<MPD/>", len(longComment) + 6, DASH, "dash"},
		{"no root within XMLSniffLen", "<!--" + strings.Repeat("x", 2*XMLSniffLen), XMLSniffLen, Raw, "unknown"},
		{"html after a long comment", longComment + "<html>" + strings.Repeat(" ", 2000), len(longComment) + 2006, Raw, "unknown"},
		{"long comment, long body", longComment + "<MPD>" + strings.Repeat(" ", 2*XMLSniffLen), XMLSniffLen, DASH, "dash"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prefix, rest, err := Sniff(bufio.NewReaderSize(strings.NewReader(tt.body), SniffLen))
			if err != nil {
				t.Fatal(err)
			}
			if len(prefix) != tt.want {
				t.Errorf("prefix of %d bytes, want %d", len(prefix), tt.want)
			}
			if typ := Detect("", prefix, Auto); typ != tt.typ {
				t.Errorf("Detect = %v, want %v", typ, tt.typ)
			}
			if f := Unsupported("", prefix, Auto); f != tt.format {
				t.Errorf("Unsupported = %q, want %q", f, tt.format)
			}
			b, err := io.ReadAll(rest)
			if err != nil || string(b) != tt.body {
				t.Errorf("rest of the body changed: %d bytes, %v", len(b), err)
			}
		})
	}
}

// ttml is a TTML subtitle document, the media XML players fetch as a
// resource.
const ttml = `<?xml version="1.0" encoding="UTF-8"?>
<tt xmlns="http://www.w3.org/ns/ttml" xml:lang="en"><body><div>
<p begin="00:00:00.000" end="00:00:02.000">Hello &amp; welcome</p>
</div></body></tt>`

// TestUnsupportedXMLWindow: XML that does not fit the sniff window is
// refused, also an allowed media XML root.
func TestUnsupportedXMLWindow(t *testing.T) {
	big := `<tt xmlns="http://www.w3.org/ns/ttml"><body><div>` + strings.Repeat(`<p>x</p>`, XMLSniffLen/8) + `</div></body></tt>`
	prefix, _, err := Sniff(bufio.NewReaderSize(strings.NewReader(big), SniffLen))
	if err != nil {
		t.Fatal(err)
	}
	if got := Unsupported("application/ttml+xml", prefix, Auto); got != FormatUnknown {
		t.Errorf("TTML larger than the window: %q, want unknown", got)
	}
	long := `<x>` + strings.Repeat("t", 70000) + `</x><MPD><BaseURL>http://a.example/</BaseURL></MPD>`
	prefix, _, err = Sniff(bufio.NewReaderSize(strings.NewReader(long), SniffLen))
	if err != nil {
		t.Fatal(err)
	}
	if got := Unsupported("text/xml", prefix, Raw); got != FormatUnknown {
		t.Errorf("MPD beyond the window: %q, want unknown", got)
	}
	small := ttml + strings.Repeat(" ", 1000)
	prefix, _, err = Sniff(bufio.NewReaderSize(strings.NewReader(small), SniffLen))
	if err != nil {
		t.Fatal(err)
	}
	if len(prefix) != len(small) {
		t.Errorf("Sniff read %d bytes of a %d-byte XML body, want all", len(prefix), len(small))
	}
	if got := Unsupported("application/ttml+xml", prefix, Auto); got != "" {
		t.Errorf("TTML within the window: %q, want streamed", got)
	}
}

// TestUnsupportedLeadingText: a manifest after leading text that only
// lighter parsers skip (dash.js, Shaka keep it as a text node and still
// take the MPD) is refused, as is any text body that is no format the
// proxy handles; binary media streams.
func TestUnsupportedLeadingText(t *testing.T) {
	const mpd = `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><BaseURL>http://a.example/</BaseURL><Period/></MPD>`
	rnd := rand.New(rand.NewPCG(1, 2))
	random := make([]byte, 4096)
	for i := range random {
		random[i] = byte(rnd.Uint32())
	}
	midFrame := append([]byte("\x3c\x9a\x01"), bytes.Repeat([]byte("\xff\xfb\x90\x64\x00\x0f\xf0\x00\x00\x69"), 300)...)
	long := strings.Repeat("x", 600)
	tests := []struct {
		name, body, want string
	}{
		{"text before an MPD", "x" + mpd, "dash"},
		{"two BOMs before an MPD", "\ufeff\ufeff" + mpd, "dash"},
		{"hash comment before an MPD", "#c\n" + mpd, "dash"},
		{"NBSP before an MPD", "\u00a0" + mpd, "dash"},
		{"NUL before an MPD", "\x00" + mpd, "dash"},
		{"prefixed MPD after text", "x<mpd:MPD xmlns:mpd=\"urn:mpeg:dash:schema:mpd:2011\"/>", "dash"},
		{"long text before an MPD", long + mpd, "unknown"},
		{"long BOMs before an MPD", strings.Repeat("\ufeff", 200) + mpd, "unknown"},
		{"long comment lines before an MPD", strings.Repeat("#c\n", 200) + mpd, "unknown"},
		{"long NBSP before an MPD", strings.Repeat("\u00a0", 300) + mpd, "unknown"},
		{"plain text", "This is not a stream, just some words.\n", "unknown"},
		{"json", `{"title": "x", "url": "https://a.example/x"}`, "unknown"},
		{"webvtt", "WEBVTT\n\n00:00.000 --> 00:02.000\nHello\n", ""},
		{"webvtt with BOM", "\ufeffWEBVTT - title\n\n00:00.000 --> 00:02.000\nHello\n", ""},
		{"ascii key", "test-only-key-01", ""},
		{"short text", "ok", ""},
		{"mpeg-ts", strings.Repeat("\x47\x40\x00\x10"+strings.Repeat("\xff", 184), 4), ""},
		{"fmp4", "\x00\x00\x00\x18ftypiso6\x00\x00\x00\x00iso6dash" + strings.Repeat("\x00", 600), ""},
		{"adts", strings.Repeat("\xff\xf1\x50\x80\x02\x1f\xfc\x21\x00\x49\x90\x02\x19", 60), ""},
		{"mp3 from mid-frame", string(midFrame), ""},
		{"id3", "ID3\x04\x00\x00\x00\x00\x00\x23TSSE\x00\x00\x00\x0f\x00\x00\x03Lavf" + strings.Repeat("\xff\xfb\x90\x64", 200), ""},
		{"ogg", "OggS\x00\x02" + strings.Repeat("\x00\x11\x7e\x9c", 200), ""},
		{"flac", "fLaC\x00\x00\x00\x22\x12\x00\x12\x00" + strings.Repeat("\x00\x8a\xc4\x42", 200), ""},
		{"jpeg", "\xff\xd8\xff\xe0\x00\x10JFIF\x00\x01" + strings.Repeat("\x93\x00\x41\xfe", 200), ""},
		{"png", "\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR" + strings.Repeat("\x00\xf0\x13\x9e", 200), ""},
		{"random bytes (AES-128 ciphertext)", string(random), ""},
		{"long NUL run before an MPD (residual risk, spec 8)", strings.Repeat("\x00", 600) + mpd, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prefix, _, err := Sniff(bufio.NewReaderSize(strings.NewReader(tt.body), SniffLen))
			if err != nil {
				t.Fatal(err)
			}
			if typ := Detect("text/xml", prefix, Auto); typ != Raw {
				t.Fatalf("Detect = %v, want raw", typ)
			}
			if got := Unsupported("text/xml", prefix, Auto); got != tt.want {
				t.Errorf("Unsupported = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestUnsupportedMPDTag: an MPD start tag is found whatever ends its
// name for lighter parsers (round 5 of the review: "<MPD=a" ends the
// name at '=' for tXml and cml-xml), and in WebVTT bodies longer than
// SniffLen.
func TestUnsupportedMPDTag(t *testing.T) {
	const rest = `a="b" xmlns="urn:mpeg:dash:schema:mpd:2011"><BaseURL>http://a.example/</BaseURL></MPD>`
	for _, term := range []string{"=", " ", ">", "/", "\"", "'", "\x00", "\xff", "!", "(", ";", ","} {
		body := "\x00\x01\x02\xff<MPD" + term + rest
		prefix, _, err := Sniff(bufio.NewReaderSize(strings.NewReader(body), SniffLen))
		if err != nil {
			t.Fatal(err)
		}
		if got := Unsupported("text/xml", prefix, Auto); got != FormatDASH {
			t.Errorf("terminator %q: Unsupported = %q, want dash", term, got)
		}
	}
	for _, name := range []string{"<mpdx ", "<mpd-x>", "<mpd.x>", "<mpd_x>", "<mpd1>", "<mpd:x>"} {
		if got := Unsupported("", []byte("\x00\x01\x02\xff"+name), Auto); got != "" {
			t.Errorf("%q is no MPD tag: Unsupported = %q", name, got)
		}
	}
	if got := Unsupported("", []byte("\x00\x01\x02\xff<MPD"), Auto); got != FormatDASH {
		t.Errorf("tag at the end of the prefix: %q, want dash", got)
	}
	vtt := "WEBVTT\n\n" + strings.Repeat("00:00.000 --> 00:02.000\nHello\n\n", 30) + `<MPD><BaseURL>http://a.example/</BaseURL></MPD>`
	prefix, _, err := Sniff(bufio.NewReaderSize(strings.NewReader(vtt), SniffLen))
	if err != nil {
		t.Fatal(err)
	}
	if len(prefix) != len(vtt) {
		t.Errorf("Sniff read %d bytes of a %d-byte WebVTT body, want all", len(prefix), len(vtt))
	}
	if got := Unsupported("text/vtt", prefix, Auto); got != FormatDASH {
		t.Errorf("MPD after 512 bytes of WebVTT: %q, want dash", got)
	}
}

// TestUnsupportedLegacyEncodings checks spec 3.3 for playlists that are
// not UTF-8 text (issue 155): a legacy encoding (ISO-8859-1,
// Windows-1252) or a control byte after the root element does not let
// them stream, on auto and raw channels and resources; media still
// streams. The Content-Type is outside the refusal table.
func TestUnsupportedLegacyEncodings(t *testing.T) {
	const bom = "\xef\xbb\xbf"
	ref := `<entry><ref href="http://direct.example/secret/stream"/></entry>`
	rnd := rand.New(rand.NewPCG(5, 6))
	random := make([]byte, 4096)
	for i := range random {
		random[i] = byte(rnd.Uint32())
	}
	tests := []struct {
		name, body, want string
	}{
		{"asx with a latin-1 title", `<asx version="3.0"><title>Radio \xd6sterreich</title>` + ref + `</asx>`, "asx"},
		{"asx with a windows-1252 byte", "<ASX version=\"3.0\"><Title>News \x96 Live</Title>" + ref + "</ASX>", "asx"},
		{"asx after BOM and whitespace", bom + "\r\n <asx version=\"3.0\"><title>\xd6</title>" + ref + "</asx>", "asx"},
		{"latin-1 byte in a prolog comment", "<!-- \xd6 -->\n<asx>" + ref + "</asx>", "asx"},
		{"latin-1 byte in the root tag", "<asx version=\"3.0\" title=\"\xd6\">" + ref + "</asx>", "unknown"},
		{
			"xspf in latin-1",
			"<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?>\n<playlist version=\"1\" xmlns=\"http://xspf.org/ns/0/\"><title>Caf\xe9</title>" +
				"<trackList><track><location>http://direct.example/secret/stream</location></track></trackList></playlist>",
			"xspf",
		},
		{"html in latin-1", "<!DOCTYPE html>\n<html><head><title>R\xe4dio</title></head><body><a href=\"http://direct.example/s\">x</a></body></html>", "unknown"},
		{"xspf root followed by NUL", "<playlist version=\"1\"><trackList><track><location>http://direct.example/s</location></track></trackList></playlist>\x00", "xspf"},
		{"asx root followed by NULs", "<asx version=\"3.0\">" + ref + "</asx>\x00\x00\x00", "asx"},
		{"control byte inside the playlist", "<playlist><title>a\x01b</title><trackList><track><location>http://direct.example/s</location></track></trackList></playlist>", "xspf"},
		{"other root followed by NUL", "<smil><body><seq><media src=\"http://direct.example/s\"/></seq></body></smil>\x00", "unknown"},
		{"latin-1 xml without a root in the window", "<!--" + strings.Repeat("\xd6", 1000), "unknown"},
		{"pls after a latin-1 comment", "; R\xe4dio\n[playlist]\nFile1=http://direct.example/s\n", "unknown"},
		{"latin-1 plain text", "Sender nicht verf\xfcgbar, bitte sp\xe4ter erneut versuchen\n", "unknown"},

		// encoding/xml fails before the root and a control byte makes
		// the body binary: the start is read lexically.
		{"latin-1 attribute in the root tag and NUL", "<asx version=\"3.0\" x=\"\xd6\">" + ref + "</asx>\x00", "asx"},
		{
			"latin-1 declaration and root attribute, NUL",
			"<?xml version=\"1.0\" encoding=\"ISO-8859-1\" x=\"\xd6\"?>\n<!-- c -->\n<asx version=\"3.0\" x=\"\xd6\">" + ref + "</asx>\x00",
			"asx",
		},
		{"prefixed root with a latin-1 attribute and NUL", "<a:ASX x=\"\xd6\">" + ref + "</a:ASX>\x00", "asx"},
		{"root name at the end of the prefix", "<?xml \x00?> <asx", "asx"},
		{"xspf with a latin-1 attribute and NUL", "<playlist version=\"1\" title=\"Caf\xe9\"><trackList><track><location>http://direct.example/s</location></track></trackList></playlist>\x00", "xspf"},
		{"short asx tag with a latin-1 attribute", "<asx x=\"\xd6\">", "asx"},
		{"declaration and NUL", "<?xml version=\"1.0\"\x00", "unknown"},
		{"declaration in upper case and NUL", "<?XML version=\"1.0\"?>\x00<x>", "unknown"},
		{"comment and NUL", "<!--\x00 -->", "unknown"},
		{"doctype and NUL", "<!doctype asx\x00>", "unknown"},

		// Media streams, also when it starts with '<'.
		{"mpeg-ts", strings.Repeat("\x47\x40\x00\x10"+strings.Repeat("\xff", 184), 4), ""},
		{"fmp4", "\x00\x00\x00\x18ftypiso6\x00\x00\x00\x00iso6dash" + strings.Repeat("\x00", 600), ""},
		{"adts", strings.Repeat("\xff\xf1\x50\x80\x02\x1f\xfc\x21\x00\x49\x90\x02\x19", 60), ""},
		{"mp3", "ID3\x04\x00\x00\x00\x00\x00\x23TSSE\x00\x00\x00\x0f\x00\x00\x03Lavf" + strings.Repeat("\xff\xfb\x90\x64", 200), ""},
		{"mp3 from mid-frame starting with <", "\x3c\x9a\x01" + strings.Repeat("\xff\xfb\x90\x64\x00\x0f\xf0\x00\x00\x69", 300), ""},
		{"ogg", "OggS\x00\x02" + strings.Repeat("\x00\x11\x7e\x9c", 200), ""},
		{"flac", "fLaC\x00\x00\x00\x22\x12\x00\x12\x00" + strings.Repeat("\x00\x8a\xc4\x42", 200), ""},
		{"aes ciphertext", string(random), ""},
		{"aes ciphertext starting with <", "<" + string(random), ""},
		{"aes ciphertext starting with whitespace and <", " \r\n<" + string(random[100:]), ""},
		{"aes key without control bytes", "\xd6\x8f\xa1\x3c\xe1\x92\x7b\x85\x6d\xaa\xb0\xc4\x4e\xc7\x99\x21", ""},
		{"aes key starting with <", "<\xd6\x8f\xa1\x3c\xe1\x92\x7b\x85\x6d\xaa\xb0\xc4\x4e\xc7\x99", ""},
	}
	for _, tt := range tests {
		for _, configured := range []Type{Auto, Raw} {
			t.Run(tt.name+"/"+configured.String(), func(t *testing.T) {
				body := bufio.NewReaderSize(strings.NewReader(tt.body), SniffLen)
				typ, refused, rest, err := Classify("video/x-ms-asf", body, configured, true)
				if err != nil {
					t.Fatal(err)
				}
				if typ != Raw || refused != tt.want {
					t.Errorf("Classify = %v, %q; want raw, %q", typ, refused, tt.want)
				}
				if b, err := io.ReadAll(rest); err != nil || string(b) != tt.body {
					t.Errorf("rest of the body changed: %d bytes, %v", len(b), err)
				}
			})
		}
	}
}

// utf16Body encodes s as UTF-16 with a byte order mark, big-endian
// (FE FF) or little-endian (FF FE).
func utf16Body(s string, bigEndian bool) string {
	b := []byte("\xff\xfe")
	if bigEndian {
		b = []byte("\xfe\xff")
	}
	for _, u := range utf16.Encode([]rune(s)) {
		if bigEndian {
			b = binary.BigEndian.AppendUint16(b, u)
		} else {
			b = binary.LittleEndian.AppendUint16(b, u)
		}
	}
	return string(b)
}

// randomAfterBOM returns bom followed by n random bytes whose first
// UTF-16 code unit is not ASCII: AES-128 ciphertext or MP3 from
// mid-frame that starts with a UTF-16 byte order mark by chance.
func randomAfterBOM(rnd *rand.Rand, bom string, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rnd.Uint32())
	}
	b[0] |= 0x80 // big-endian high byte, little-endian low byte
	b[1] |= 0x80
	return bom + string(b)
}

// TestUnsupportedUTF16BOM checks spec 3.3 for bodies that start with a
// UTF-16 byte order mark (issue 164): UTF-16 playlists are refused on
// auto and raw channels, whatever follows the mark; media and ciphertext
// that start with FE FF or FF FE by chance stream.
func TestUnsupportedUTF16BOM(t *testing.T) {
	ref := `<entry><ref href="http://direct.example/secret/stream"/></entry>`
	rnd := rand.New(rand.NewPCG(7, 8))
	type tc struct{ name, body, want string }
	var tests []tc
	for _, enc := range []struct {
		name string
		be   bool
	}{{"utf-16be", true}, {"utf-16le", false}} {
		text := func(s string) string { return utf16Body(s, enc.be) }
		tests = append(tests,
			tc{enc.name + " asx after whitespace", text(" \r\n<asx version=\"3.0\">" + ref + "</asx>"), "unknown"},
			tc{enc.name + " xml declaration", text(`<?xml version="1.0" encoding="UTF-16"?><playlist version="1"/>`), "unknown"},
			tc{enc.name + " extended m3u", text("#EXTM3U\n#EXTINF:-1,Radio\nhttp://direct.example/secret/stream\n"), "unknown"},
			tc{enc.name + " pls", text("[playlist]\nFile1=http://direct.example/secret/stream\n"), "unknown"},
			tc{enc.name + " url line", text("http://direct.example/secret/stream\n"), "unknown"},
			tc{enc.name + " short url line", text("//a.b/c"), "unknown"},
			tc{enc.name + " two byte order marks", text("\ufeff#EXTM3U\nhttp://direct.example/secret/stream\n"), "unknown"},
			tc{enc.name + " unicode whitespace", text("\u00a0\u3000[playlist]\nFile1=http://direct.example/secret/stream\n"), "unknown"},
			tc{enc.name + " non-ascii first line", text("\u00d61 Radio.mp3\r\nhttp://direct.example/secret/stream\r\n"), "unknown"},
			tc{enc.name + " cjk first line", text("\u4e2d\u6587.mp3\nhttp://direct.example/secret/stream\n"), "unknown"},
			tc{enc.name + " surrogate pair first", text("\U0001f4fb Radio\nhttp://direct.example/secret/stream\n"), "unknown"},
			tc{enc.name + " playlist followed by NUL", text("#EXTM3U\nhttp://direct.example/secret/stream\n\x00\x00"), "unknown"},
			tc{enc.name + " only whitespace", text(" \n "), "unknown"},
			tc{enc.name + " byte order mark only", text(""), "unknown"},
			tc{enc.name + " ciphertext", randomAfterBOM(rnd, text(""), 4096), ""},
		)
	}
	tests = append(tests,
		tc{"utf-16be byte order mark and a cut code unit", "\xfe\xff\x00", "unknown"},
		tc{"utf-32le byte order mark", "\xff\xfe\x00\x00<\x00\x00\x00a\x00\x00\x00", "unknown"},
		tc{"utf-32be byte order mark", "\x00\x00\xfe\xff\x00\x00\x00<\x00\x00\x00a", "unknown"},
		// A control code unit or an unpaired surrogate: not UTF-16 text.
		tc{"ciphertext after FE FF with a control unit", "\xfe\xff\x9a\x31\xc4\x07\x00\x01\x8f\x1a\x3c\xe1\x92\x7b\x05\x6d\xaa\x10\x4e\xc7", ""},
		tc{"ciphertext after FF FE with an unpaired surrogate", "\xff\xfe\x31\x9a\x07\xc4\x01\xdc\x1a\x8f\xe1\x3c\x7b\x92\x6d\x05\x10\xaa\xc7\x4e", ""},
		tc{"id3 after FE FF", "\xfe\xffID3\x04\x00\x00\x00\x00\x00\x23TSSE\x00\x00\x00\x0f\x00\x00\x03Lavf", ""},
		// At most 16 bytes, as an AES-128 key: only the first code unit
		// counts.
		tc{"aes key starting with FE FF", "\xfe\xff\x1a\x8f\x3c\xe1\x92\x7b\x05\x6d\xaa\x10\x4e\xc7\x99\x21", ""},
		tc{"aes key starting with FF FE", "\xff\xfe\x8f\x1a\xe1\x3c\x7b\x92\x6d\x05\x10\xaa\xc7\x4e\x21\x99", ""},
		tc{"aes key starting with FE FF and an ascii unit", "\xfe\xff\x00\x41\x3c\xe1\x92\x7b\x05\x6d\xaa\x10\x4e\xc7\x99\x21", "unknown"},
	)
	for _, tt := range tests {
		for _, configured := range []Type{Auto, Raw} {
			t.Run(tt.name+"/"+configured.String(), func(t *testing.T) {
				body := bufio.NewReaderSize(strings.NewReader(tt.body), SniffLen)
				typ, refused, rest, err := Classify("application/octet-stream", body, configured, true)
				if err != nil {
					t.Fatal(err)
				}
				if typ != Raw || refused != tt.want {
					t.Errorf("Classify = %v, %q; want raw, %q", typ, refused, tt.want)
				}
				if b, err := io.ReadAll(rest); err != nil || string(b) != tt.body {
					t.Errorf("rest of the body changed: %d bytes, %v", len(b), err)
				}
			})
		}
	}
}

// TestUnsupportedUTF16BOMRate: of random SniffLen prefixes that start
// with a UTF-16 byte order mark, only a small share is refused (spec 3.3:
// about 1 in 560; every one of them before issue 164).
func TestUnsupportedUTF16BOMRate(t *testing.T) {
	const n = 20000
	rnd := rand.New(rand.NewPCG(9, 10))
	b := make([]byte, SniffLen)
	refused := 0
	for i := range n {
		for j := range b {
			b[j] = byte(rnd.Uint32())
		}
		copy(b, []string{"\xfe\xff", "\xff\xfe"}[i%2])
		if Unsupported("", b, Raw) != "" {
			refused++
		}
	}
	if refused > n/100 {
		t.Errorf("%d of %d random bodies after a UTF-16 byte order mark refused, want at most 1%%", refused, n)
	}
}
