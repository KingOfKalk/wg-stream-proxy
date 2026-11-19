// SPDX-License-Identifier: AGPL-3.0-or-later

package dash

import (
	"errors"
	"testing"
)

// TestRewriteNames: lighter XML parsers (dash.js, Shaka) skip leading
// non-letters in attribute names and let a later attribute win, so
// "_media" or ":media" would override "media" for them. Every element and
// attribute name must be a plain XML name.
func TestRewriteNames(t *testing.T) {
	for _, in := range []string{
		mpdNS + `<Period><SegmentTemplate media="seg-$Number$.m4s" _media="https://evil.example/$Number$.m4s"/></Period></MPD>`,
		mpdNS + `<Period><SegmentTemplate media="seg-$Number$.m4s" :media="https://evil.example/$Number$.m4s"/></Period></MPD>`,
		mpdNS + `<Period><SegmentTemplate media="$RepresentationID$/$Number$.m4s"/><Representation id="v" _id="//evil.example"/></Period></MPD>`,
		mpdNS + `<Period><EventStream schemeIdUri="urn:scte:scte35:2013:xml" _schemeIdUri="urn:mpeg:dash:event:callback:2015" value="1"/></Period></MPD>`,
		mpdNS + `<Period><AdaptationSet><SupplementalProperty schemeIdUri="urn:dvb:dash:fontdownload:2014" dvb:url="f.woff" _url="https://fonts.evil.example/f.woff"/></AdaptationSet></Period></MPD>`,
		mpdNS + `<Period><SegmentTemplate media="a.m4s" x:-media="b" xmlns:x="urn:x"/></Period></MPD>`,
		mpdNS + `<Period><SegmentTemplate media="a.m4s" _x:media="b"/></Period></MPD>`,
		mpdNS + `<Period><_x/></Period></MPD>`,
		mpdNS + `<Period><SegmentTemplate media="a.m4s" 1media="b"/></Period></MPD>`,
	} {
		if out, err := Rewrite([]byte(in), testOptions(t)); !errors.Is(err, ErrInvalidManifest) {
			t.Errorf("%s:\nRewrite = %s, %v; want ErrInvalidManifest", in, out, err)
		}
	}
}

// TestRewriteCanonical: every tag is written again from what
// encoding/xml parsed: double quotes, one space between attributes,
// escaped values and text, no bytes of the input tags.
func TestRewriteCanonical(t *testing.T) {
	in := "<MPD  xmlns='urn:mpeg:dash:schema:mpd:2011'\n\ttype = \"static\" ><Period id='a&quot;b'\r\n><ProgramInformation><Title>A &amp; B &#x3C;C&#62; ' \"</Title></ProgramInformation>" +
		"<AdaptationSet><S t='1' /><Role schemeIdUri=\"urn:mpeg:dash:role:2011\" value='main'></Role></AdaptationSet></Period ></MPD >\n"
	want := `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static"><BaseURL>http://wsp.lan/p/n/{https://cdn.example.net/ch/}/</BaseURL><Period id="a&#34;b">` +
		`<ProgramInformation><Title>A &amp; B &lt;C&gt; ' "</Title></ProgramInformation>` +
		`<AdaptationSet><S t="1"/><Role schemeIdUri="urn:mpeg:dash:role:2011" value="main"></Role></AdaptationSet></Period></MPD>` + "\n"
	out, err := Rewrite([]byte(in), testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != want {
		t.Errorf("got\n%q\nwant\n%q", out, want)
	}
}

// TestRewriteBaseURLAfterReferences: in schema order, descriptors such as
// a font download property come before the BaseURL of an AdaptationSet,
// and the BaseURL of a level may follow its child elements. References
// resolve against the bases of their level wherever the BaseURL stands.
func TestRewriteBaseURLAfterReferences(t *testing.T) {
	in := mpdNS + `<Period><AdaptationSet>` +
		`<SupplementalProperty schemeIdUri="urn:dvb:dash:fontdownload:2014" dvb:url="fonts/f.woff"/>` +
		`<SegmentTemplate media="../x/$Number$.m4s"/>` +
		`<BaseURL>https://cdn2.example.com/a/b/</BaseURL>` +
		`</AdaptationSet></Period><BaseURL>https://cdn3.example.com/root/</BaseURL></MPD>`
	want := mpdNS + `<Period><AdaptationSet>` +
		`<SupplementalProperty schemeIdUri="urn:dvb:dash:fontdownload:2014" dvb:url="http://wsp.lan/p/n/{https://cdn2.example.com/a/b/fonts/}/f.woff"/>` +
		`<SegmentTemplate media="http://wsp.lan/p/n/{https://cdn2.example.com/a/x/}/$Number$.m4s"/>` +
		`<BaseURL>http://wsp.lan/p/n/{https://cdn2.example.com/a/b/}/</BaseURL>` +
		`</AdaptationSet></Period><BaseURL>http://wsp.lan/p/n/{https://cdn3.example.com/root/}/</BaseURL></MPD>`
	out, err := Rewrite([]byte(in), testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
	checkRewritten(t, out)
}
