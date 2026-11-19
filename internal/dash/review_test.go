// SPDX-License-Identifier: AGPL-3.0-or-later

package dash

import (
	"errors"
	"strings"
	"testing"
)

// The tests in this file cover the findings of the security review of
// #124: parser differentials, events and descriptors with URLs, BaseURL
// attributes, scheme case, non-ASCII references and output size.

const mpdNS = `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" xmlns:dvb="urn:dvb:dash:dash-extensions:2014-1">`

// TestRewriteParserDifferential: players whose XML parsers end comments
// and processing instructions earlier than encoding/xml (dash.js
// cml-xml, Shaka tXml) must not see a BaseURL the rewriter did not see.
func TestRewriteParserDifferential(t *testing.T) {
	ok := []struct{ name, in string }{
		{"comment ending at the first -->", mpdNS + `<!---><BaseURL>https://evil.example/</BaseURL>--><Period/></MPD>`},
		{"comment in an element", mpdNS + `<Period><!-- -><BaseURL>https://evil.example/</BaseURL> --></Period></MPD>`},
		{"declaration with a tag inside", `<?xml version="1.0" x="><BaseURL>https://evil.example/</BaseURL>"?>` + mpdNS + `<Period/></MPD>`},
		{"BOM and declaration", "\ufeff<?xml version='1.0' encoding='utf-8'?>\n" + mpdNS + `<Period/><!-- https://evil.example/ --></MPD>`},
	}
	for _, tt := range ok {
		t.Run(tt.name, func(t *testing.T) {
			out, err := Rewrite([]byte(tt.in), testOptions(t))
			if err != nil {
				t.Fatalf("Rewrite: %v", err)
			}
			if strings.Contains(string(out), "evil") || strings.Contains(string(out), "<!--") {
				t.Errorf("output keeps a comment or the hidden URL:\n%s", out)
			}
			if strings.Contains(tt.in, "<?xml") && !strings.HasPrefix(strings.TrimPrefix(string(out), "\ufeff"), `<?xml version="1.0" encoding="UTF-8"?>`) {
				t.Errorf("declaration not written canonically:\n%s", out)
			}
			checkRewritten(t, out)
		})
	}
	rejected := []struct{ name, in string }{
		{"processing instruction", mpdNS + `<?x a><BaseURL>https://evil.example/</BaseURL><x b=""/?><Period/></MPD>`},
		{"processing instruction before the root", `<?x a><BaseURL>https://evil.example/</BaseURL>?>` + mpdNS + `<Period/></MPD>`},
		{"declaration not first", mpdNS + `<?xml version="1.0"?><Period/></MPD>`},
		{"CDATA outside BaseURL", mpdNS + `<Period><Title><![CDATA[x]]></Title></Period></MPD>`},
		{"CDATA in an attribute-free element", mpdNS + `<ProgramInformation><Title><![CDATA[]]><BaseURL>https://evil.example/</BaseURL>]]></Title></ProgramInformation><Period/></MPD>`},
		{"> in an attribute value", mpdNS + `<Period id="a><BaseURL>https://evil.example/</BaseURL>"/></MPD>`},
		{"> in a single-quoted attribute value", mpdNS + `<Period id='>'/></MPD>`},
	}
	for _, tt := range rejected {
		t.Run(tt.name, func(t *testing.T) {
			out, err := Rewrite([]byte(tt.in), testOptions(t))
			if !errors.Is(err, ErrInvalidManifest) {
				t.Fatalf("Rewrite = %q, %v; want ErrInvalidManifest", out, err)
			}
		})
	}
}

// TestRewriteEvents keeps only event schemes whose events carry no URL
// a player fetches, and removes every other EventStream and
// InbandEventStream.
func TestRewriteEvents(t *testing.T) {
	in := mpdNS + `<Period>` +
		`<EventStream schemeIdUri="urn:mpeg:dash:event:callback:2015" value="1"><Event id="1" messageData="https://evil.example/cb"/></EventStream>` +
		`<EventStream schemeIdUri="urn:mpeg:dash:event:alternativeMPD:replace:2025"><Event><AlternativeMPD uri="https://evil.example/a.mpd"/></Event></EventStream>` +
		`<EventStream schemeIdUri="urn:mpeg:dash:event:2012" value="3"><Event>https://evil.example/</Event></EventStream>` +
		`<EventStream schemeIdUri="URN:SCTE:SCTE35:2014:XML+BIN" timescale="90000"><Event id="7"><Signal/></Event></EventStream>` +
		`<EventStream schemeIdUri="urn:mpeg:dash:event:2012" value="1"/>` +
		`<AdaptationSet>` +
		`<InbandEventStream schemeIdUri="urn:mpeg:dash:event:alternativeMPD:insert:2025" value="1"/>` +
		`<InbandEventStream schemeIdUri="urn:mpeg:dash:event:callback:2015" value="1"/>` +
		`<InbandEventStream schemeIdUri="https://aomedia.org/emsg/ID3" value="0"/>` +
		`<InbandEventStream schemeIdUri="urn:scte:scte35:2013:bin"/>` +
		`</AdaptationSet></Period></MPD>`
	want := mpdNS + `<BaseURL>http://wsp.lan/p/n/{https://cdn.example.net/ch/}/</BaseURL><Period>` +
		`<EventStream schemeIdUri="URN:SCTE:SCTE35:2014:XML+BIN" timescale="90000"><Event id="7"><Signal/></Event></EventStream>` +
		`<EventStream schemeIdUri="urn:mpeg:dash:event:2012" value="1"/>` +
		`<AdaptationSet>` +
		`<InbandEventStream schemeIdUri="https://aomedia.org/emsg/ID3" value="0"/>` +
		`<InbandEventStream schemeIdUri="urn:scte:scte35:2013:bin"/>` +
		`</AdaptationSet></Period></MPD>`
	out, err := Rewrite([]byte(in), testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
	checkRewritten(t, out)
}

// TestRewriteFontDownload rewrites the dvb:url of DVB font download
// descriptors, which players fetch themselves.
func TestRewriteFontDownload(t *testing.T) {
	in := mpdNS + `<Period><BaseURL>dash/</BaseURL><AdaptationSet mimeType="application/mp4">` +
		`<SupplementalProperty schemeIdUri="urn:dvb:dash:fontdownload:2014" value="1" dvb:url="https://fonts.example.com/a/f.woff" dvb:mimeType="application/font-woff" dvb:fontFamily="F"/>` +
		`<EssentialProperty schemeIdUri="URN:DVB:DASH:FONTDOWNLOAD:2014" value="1" dvb:url="fonts/g.woff" dvb:fontFamily="G"/>` +
		`</AdaptationSet></Period></MPD>`
	want := mpdNS + `<BaseURL>http://wsp.lan/p/n/{https://cdn.example.net/ch/}/</BaseURL><Period><BaseURL>http://wsp.lan/p/n/{https://cdn.example.net/ch/dash/}/</BaseURL><AdaptationSet mimeType="application/mp4">` +
		`<SupplementalProperty schemeIdUri="urn:dvb:dash:fontdownload:2014" value="1" dvb:url="http://wsp.lan/p/n/{https://fonts.example.com/a/}/f.woff" dvb:mimeType="application/font-woff" dvb:fontFamily="F"/>` +
		`<EssentialProperty schemeIdUri="URN:DVB:DASH:FONTDOWNLOAD:2014" value="1" dvb:url="http://wsp.lan/p/n/{https://cdn.example.net/ch/dash/fonts/}/g.woff" dvb:fontFamily="G"/>` +
		`</AdaptationSet></Period></MPD>`
	out, err := Rewrite([]byte(in), testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
	for _, bad := range []string{
		`<SupplementalProperty schemeIdUri="urn:dvb:dash:fontdownload:2014" dvb:url="ftp://fonts.example.com/f.woff"/>`,
		`<SupplementalProperty schemeIdUri="urn:dvb:dash:fontdownload:2014" dvb:url="a.woff" x:url="b.woff" xmlns:x="urn:x"/>`,
	} {
		if _, err := Rewrite([]byte(mpdNS+`<Period><AdaptationSet>`+bad+`</AdaptationSet></Period></MPD>`), testOptions(t)); !errors.Is(err, ErrInvalidManifest) {
			t.Errorf("%s: %v, want ErrInvalidManifest", bad, err)
		}
	}
}

// TestRewriteBaseURLAttributes keeps only the BaseURL attributes of the
// schema and DVB; byteRange (Annex E templates) and others are dropped.
func TestRewriteBaseURLAttributes(t *testing.T) {
	in := mpdNS + `<BaseURL serviceLocation="a" byteRange="$base$?r=$first$-$last$" dvb:priority="1" dvb:weight="2" availabilityTimeOffset="1.5" availabilityTimeComplete="false" timeShiftBufferDepth="PT1M" rangeAccess="true" foo="x">https://a.example.com/x/</BaseURL><Period/></MPD>`
	want := mpdNS + `<BaseURL serviceLocation="a" dvb:priority="1" dvb:weight="2" availabilityTimeOffset="1.5" availabilityTimeComplete="false" timeShiftBufferDepth="PT1M" rangeAccess="true">http://wsp.lan/p/n/{https://a.example.com/x/}/</BaseURL><Period/></MPD>`
	out, err := Rewrite([]byte(in), testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
}

// TestRewriteSchemeCase matches the chaining and fallback schemes
// case-insensitively.
func TestRewriteSchemeCase(t *testing.T) {
	out, err := Rewrite([]byte(mpdNS+`<SupplementalProperty schemeIdUri=" URN:MPEG:DASH:CHAINING:2016" value="https://next.example/n.mpd"/><Period/></MPD>`), testOptions(t))
	if err != nil || strings.Contains(string(out), "next.example") {
		t.Errorf("chaining in upper case kept: %v\n%s", err, out)
	}
	if _, err := Rewrite([]byte(mpdNS+`<EssentialProperty schemeIdUri="urn:MPEG:dash:Fallback:2016" value="https://next.example/n.mpd"/><Period/></MPD>`), testOptions(t)); !errors.Is(err, ErrInvalidManifest) {
		t.Errorf("EssentialProperty fallback in mixed case: %v", err)
	}
}

// TestRewriteNonASCII rejects non-ASCII bytes in references, BaseURLs
// and Representation ids: some players trim Unicode whitespace such as
// NBSP, which would turn NBSP + "//host" into a scheme-relative URL.
func TestRewriteNonASCII(t *testing.T) {
	for _, in := range []string{
		mpdNS + "<Period><SegmentTemplate media=\"\u00a0//evil.example/$Number$.m4s\"/></Period></MPD>",
		mpdNS + "<Period><SegmentList><SegmentURL media=\" //evil.example/1.m4s\"/></SegmentList></Period></MPD>",
		mpdNS + "<BaseURL>\u00a0//evil.example/</BaseURL><Period/></MPD>",
		mpdNS + "<Period><SegmentTemplate media=\"$RepresentationID$/$Number$.m4s\"/><Representation id=\"\u00a0/evil.example\"/></Period></MPD>",
	} {
		if out, err := Rewrite([]byte(in), testOptions(t)); !errors.Is(err, ErrInvalidManifest) {
			t.Errorf("%q: Rewrite = %q, %v; want ErrInvalidManifest", in, out, err)
		}
	}
}

// TestRewriteOutputLimit bounds the output: every reference that is not
// plain becomes a whole prefix URL, so a small manifest could otherwise
// grow by orders of magnitude.
func TestRewriteOutputLimit(t *testing.T) {
	var b strings.Builder
	b.WriteString(mpdNS + `<BaseURL>https://a.example.com/` + strings.Repeat("d/", 1500) + `</BaseURL><Period><SegmentList>`)
	for range 1000 {
		b.WriteString(`<SegmentURL media="../a"/>`)
	}
	b.WriteString(`</SegmentList></Period></MPD>`)
	in := []byte(b.String())
	if out, err := Rewrite(in, testOptions(t)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Rewrite: %d bytes from %d, %v; want ErrTooLarge", len(out), len(in), err)
	}
	if got := maxOutput(len(in)); got != 1<<20 {
		t.Errorf("maxOutput(%d) = %d, want the 1 MiB floor", len(in), got)
	}
	for n, want := range map[int]int{0: 1 << 20, 1 << 20: 16 << 20, 8 << 20: 128 << 20, MaxSize: 128 << 20} {
		if got := maxOutput(n); got != want {
			t.Errorf("maxOutput(%d) = %d, want %d", n, got, want)
		}
	}
}
