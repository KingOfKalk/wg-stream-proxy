// SPDX-License-Identifier: AGPL-3.0-or-later

package dash

import (
	"bytes"
	"errors"
	"flag"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

const (
	testFinal = "https://cdn.example.net/ch/manifest.mpd"
	testTime  = "http://wsp.lan/time"
)

// testPrefixURL is a readable stand-in for the signed prefix URLs of the
// server: the mode and the upstream prefix in braces.
func testPrefixURL(prefix string, query bool) (string, error) {
	mode := "n"
	if query {
		mode = "q"
	}
	return "http://wsp.lan/p/" + mode + "/{" + prefix + "}/", nil
}

func mustParse(t testing.TB, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func testOptions(t testing.TB) Options {
	return Options{Final: mustParse(t, testFinal), PrefixURL: testPrefixURL, TimeURL: testTime}
}

// TestRewriteGolden rewrites made-up manifests of realistic structure and
// compares them with golden files (go test -update writes them).
func TestRewriteGolden(t *testing.T) {
	for _, name := range []string{"live", "vod", "nested"} {
		t.Run(name, func(t *testing.T) {
			in, err := os.ReadFile(filepath.Join("testdata", name+".mpd"))
			if err != nil {
				t.Fatal(err)
			}
			out, err := Rewrite(in, testOptions(t))
			if err != nil {
				t.Fatalf("Rewrite: %v", err)
			}
			golden := filepath.Join("testdata", name+".golden.mpd")
			if *update {
				if err := os.WriteFile(golden, out, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out, want) {
				t.Errorf("output differs from %s:\n%s", golden, out)
			}
			checkRewritten(t, out)
		})
	}
}

// TestRewriteLiveChangesOnlyURLs checks the live golden case line by
// line: only the BaseURL and UTCTiming lines change, plus the added
// MPD-level BaseURL, the comments go and the declaration is written
// again; everything else stays byte for byte.
func TestRewriteLiveChangesOnlyURLs(t *testing.T) {
	in, err := os.ReadFile("testdata/live.mpd")
	if err != nil {
		t.Fatal(err)
	}
	out, err := Rewrite(in, testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.NewReplacer(
		`<?xml version="1.0" encoding="utf-8"?>`, `<?xml version="1.0" encoding="UTF-8"?>`,
		`<!-- Made-up live channel modelled on a broadcaster's origin packager -->`, ``,
		`<!-- 2026-11-19T11:59:00Z - 2026-11-19T12:00:00Z -->`, ``,
		`profiles="urn:mpeg:dash:profile:isoff-live:2011,urn:com:dashif:dash264">`,
		`profiles="urn:mpeg:dash:profile:isoff-live:2011,urn:com:dashif:dash264"><BaseURL>http://wsp.lan/p/n/{https://cdn.example.net/ch/}/</BaseURL>`,
		`<BaseURL>dash/</BaseURL>`,
		`<BaseURL>http://wsp.lan/p/n/{https://cdn.example.net/ch/dash/}/</BaseURL>`,
		`<UTCTiming schemeIdUri="urn:mpeg:dash:utc:http-iso:2014" value="https://time.example.org/?iso"/>`,
		`<UTCTiming schemeIdUri="urn:mpeg:dash:utc:http-iso:2014" value="http://wsp.lan/time"/>`,
	).Replace(string(in))
	if string(out) != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
}

func TestRewrite(t *testing.T) {
	const (
		mpdOpen  = `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static">`
		mpdBase  = `<BaseURL>http://wsp.lan/p/n/{https://cdn.example.net/ch/}/</BaseURL>`
		mpdBaseQ = `<BaseURL>http://wsp.lan/p/q/{https://cdn.example.net/ch/}/</BaseURL>`
	)
	tests := []struct {
		name, in, want string
	}{
		{
			name: "base added for the manifest directory",
			in:   mpdOpen + `<Period><AdaptationSet><SegmentTemplate media="v/$Number$.m4s" initialization="v/init.mp4"/></AdaptationSet></Period></MPD>`,
			want: mpdOpen + mpdBase + `<Period><AdaptationSet><SegmentTemplate media="v/$Number$.m4s" initialization="v/init.mp4"/></AdaptationSet></Period></MPD>`,
		},
		{
			name: "absolute MPD base",
			in:   mpdOpen + "\n  <BaseURL>https://cdn2.example.com/live/</BaseURL>\n<Period/></MPD>",
			want: mpdOpen + "\n  <BaseURL>http://wsp.lan/p/n/{https://cdn2.example.com/live/}/</BaseURL>\n<Period/></MPD>",
		},
		{
			name: "failover bases each with own prefix, attributes kept",
			in: mpdOpen + `<BaseURL serviceLocation="a" dvb:priority="1">https://a.example.com/x/</BaseURL>` +
				`<BaseURL serviceLocation="b">https://b.example.com:8443/y/</BaseURL><Period><BaseURL>dash/</BaseURL></Period></MPD>`,
			want: mpdOpen + `<BaseURL serviceLocation="a" dvb:priority="1">http://wsp.lan/p/n/{https://a.example.com/x/}/</BaseURL>` +
				`<BaseURL serviceLocation="b">http://wsp.lan/p/n/{https://b.example.com:8443/y/}/</BaseURL>` +
				`<Period><BaseURL>http://wsp.lan/p/n/{https://a.example.com/x/dash/}/</BaseURL></Period></MPD>`,
		},
		{
			name: "file base keeps file name and query",
			in:   mpdOpen + `<Period><AdaptationSet><Representation id="v"><BaseURL>../v/video_1080.mp4?tok=a&amp;b=1</BaseURL><SegmentBase indexRange="0-99"/></Representation></AdaptationSet></Period></MPD>`,
			want: mpdOpen + mpdBase + `<Period><AdaptationSet><Representation id="v"><BaseURL>http://wsp.lan/p/q/{https://cdn.example.net/v/}/video_1080.mp4?tok=a&amp;b=1</BaseURL><SegmentBase indexRange="0-99"/></Representation></AdaptationSet></Period></MPD>`,
		},
		{
			name: "empty base is the parent base",
			in:   mpdOpen + `<BaseURL/><Period/></MPD>`,
			want: mpdOpen + `<BaseURL>http://wsp.lan/p/n/{https://cdn.example.net/ch/}/manifest.mpd</BaseURL><Period/></MPD>`,
		},
		{
			name: "base with whitespace, entities, CDATA and a comment",
			in:   mpdOpen + "<BaseURL>\n  https://cdn2.example.com/a&amp;b/<!-- x --><![CDATA[c/]]>\n</BaseURL><Period/></MPD>",
			want: mpdOpen + `<BaseURL>http://wsp.lan/p/n/{https://cdn2.example.com/a&amp;b/c/}/</BaseURL><Period/></MPD>`,
		},
		{
			name: "base names are case-insensitive, namespace prefix kept",
			in:   `<mpd:MPD xmlns:mpd="urn:mpeg:dash:schema:mpd:2011"><mpd:Period><mpd:baseurl>https://cdn2.example.com/p/</mpd:baseurl></mpd:Period></mpd:MPD>`,
			want: `<mpd:MPD xmlns:mpd="urn:mpeg:dash:schema:mpd:2011"><mpd:BaseURL>http://wsp.lan/p/n/{https://cdn.example.net/ch/}/</mpd:BaseURL><mpd:Period><mpd:baseurl>http://wsp.lan/p/n/{https://cdn2.example.com/p/}/</mpd:baseurl></mpd:Period></mpd:MPD>`,
		},
		{
			name: "absolute template with its own prefix and query",
			in:   mpdOpen + `<Period><AdaptationSet><SegmentTemplate media="https://cdn2.example.com/live/$RepresentationID$/s-$Number%05d$.m4s?t=$Time$&amp;x=1" initialization="https://cdn2.example.com/live/$RepresentationID$/init.mp4"/></AdaptationSet></Period></MPD>`,
			want: mpdOpen + mpdBase + `<Period><AdaptationSet><SegmentTemplate media="http://wsp.lan/p/q/{https://cdn2.example.com/live/}/$RepresentationID$/s-$Number%05d$.m4s?t=$Time$&amp;x=1" initialization="http://wsp.lan/p/n/{https://cdn2.example.com/live/}/$RepresentationID$/init.mp4"/></AdaptationSet></Period></MPD>`,
		},
		{
			name: "rooted, dot-segment and scheme-relative references",
			in: mpdOpen + `<Period><BaseURL>dash/</BaseURL><AdaptationSet>` +
				`<SegmentTemplate media="/live/$Number$.m4s" initialization="../init/$RepresentationID$.mp4" index="//cdn3.example.com/idx/$Number$.sidx"/>` +
				`</AdaptationSet></Period></MPD>`,
			want: mpdOpen + mpdBase + `<Period><BaseURL>http://wsp.lan/p/n/{https://cdn.example.net/ch/dash/}/</BaseURL><AdaptationSet>` +
				`<SegmentTemplate media="http://wsp.lan/p/n/{https://cdn.example.net/live/}/$Number$.m4s" initialization="http://wsp.lan/p/n/{https://cdn.example.net/ch/init/}/$RepresentationID$.mp4" index="http://wsp.lan/p/n/{https://cdn3.example.com/idx/}/$Number$.sidx"/>` +
				`</AdaptationSet></Period></MPD>`,
		},
		{
			name: "whitespace around a reference, upper-case scheme",
			in:   mpdOpen + `<Period><SegmentTemplate media=" //cdn3.example.com/s/$Number$.m4s&#10;" initialization="HTTPS://cdn3.example.com/i/init.mp4"/></Period></MPD>`,
			want: mpdOpen + mpdBase + `<Period><SegmentTemplate media="http://wsp.lan/p/n/{https://cdn3.example.com/s/}/$Number$.m4s" initialization="http://wsp.lan/p/n/{https://cdn3.example.com/i/}/init.mp4"/></Period></MPD>`,
		},
		{
			name: "colon in the first segment of a template",
			in:   mpdOpen + `<Period><SegmentTemplate media="$RepresentationID$:$Number$.m4s"/></Period></MPD>`,
			want: mpdOpen + mpdBase + `<Period><SegmentTemplate media="http://wsp.lan/p/n/{https://cdn.example.net/ch/}/$RepresentationID$:$Number$.m4s"/></Period></MPD>`,
		},
		{
			name: "segment list with absolute and relative segment URLs",
			in: mpdOpen + `<Period><AdaptationSet><Representation id="1"><SegmentList duration="2">` +
				`<Initialization sourceURL="https://cdn2.example.com/vod/init.mp4" range="0-800"/>` +
				`<SegmentURL media="https://cdn2.example.com/vod/seg1.m4s"/><SegmentURL media="seg2.m4s" mediaRange="0-9"/>` +
				`</SegmentList></Representation></AdaptationSet></Period></MPD>`,
			want: mpdOpen + mpdBase + `<Period><AdaptationSet><Representation id="1"><SegmentList duration="2">` +
				`<Initialization sourceURL="http://wsp.lan/p/n/{https://cdn2.example.com/vod/}/init.mp4" range="0-800"/>` +
				`<SegmentURL media="http://wsp.lan/p/n/{https://cdn2.example.com/vod/}/seg1.m4s"/><SegmentURL media="seg2.m4s" mediaRange="0-9"/>` +
				`</SegmentList></Representation></AdaptationSet></Period></MPD>`,
		},
		{
			name: "relative template with a query allows queries on the bases",
			in:   mpdOpen + `<Period><BaseURL>dash/</BaseURL><SegmentTemplate media="s-$Number$.m4s?n=$Number$"/></Period></MPD>`,
			want: mpdOpen + mpdBaseQ + `<Period><BaseURL>http://wsp.lan/p/q/{https://cdn.example.net/ch/dash/}/</BaseURL><SegmentTemplate media="s-$Number$.m4s?n=$Number$"/></Period></MPD>`,
		},
		{
			name: "location, patch location, metrics and content steering removed",
			in: mpdOpen + "<Location>https://origin.example.com/m.mpd</Location>\n<PatchLocation ttl=\"60\">https://origin.example.com/p</PatchLocation>\n" +
				`<ContentSteering defaultServiceLocation="a">https://steer.example.com/s</ContentSteering>` +
				`<Metrics metrics="DVBErrors"><Reporting schemeIdUri="urn:dvb:dash:reporting:2014" value="1" dvb:reportingUrl="https://report.example.com/"/></Metrics>` +
				`<SupplementalProperty schemeIdUri="urn:mpeg:dash:chaining:2016" value="https://next.example.com/n.mpd"/>` +
				`<SupplementalProperty schemeIdUri="urn:mpeg:dash:fallback:2016" value="https://fb.example.com/f.mpd"/>` +
				`<SupplementalProperty schemeIdUri="urn:example:keep" value="1"/><Period/></MPD>`,
			want: mpdOpen + mpdBase + "\n\n" + `<SupplementalProperty schemeIdUri="urn:example:keep" value="1"/><Period/></MPD>`,
		},
		{
			name: "UTCTiming: direct kept, others served by the proxy",
			in: mpdOpen + `<Period/><UTCTiming schemeIdUri="urn:mpeg:dash:utc:direct:2014" value="2026-11-19T12:00:00Z"/>` +
				`<UTCTiming schemeIdUri="urn:mpeg:dash:utc:http-xsdate:2014" value="https://time.example.org/xs"></UTCTiming>` +
				`<UTCTiming value="pool.example.org" schemeIdUri="urn:mpeg:dash:utc:ntp:2014" id="t"/>` +
				`<UTCTiming schemeIdUri="urn:mpeg:dash:utc:http-head:2014"/></MPD>`,
			want: mpdOpen + mpdBase + `<Period/><UTCTiming schemeIdUri="urn:mpeg:dash:utc:direct:2014" value="2026-11-19T12:00:00Z"/>` +
				`<UTCTiming schemeIdUri="urn:mpeg:dash:utc:http-iso:2014" value="http://wsp.lan/time"></UTCTiming>` +
				`<UTCTiming value="http://wsp.lan/time" schemeIdUri="urn:mpeg:dash:utc:http-iso:2014" id="t"/>` +
				`<UTCTiming schemeIdUri="urn:mpeg:dash:utc:http-iso:2014" value="http://wsp.lan/time"/></MPD>`,
		},
		{
			name: "resolve-to-zero xlink kept",
			in:   mpdOpen + `<Period xmlns:xlink="http://www.w3.org/1999/xlink" xlink:href="urn:mpeg:dash:resolve-to-zero:2013" xlink:actuate="onLoad"/></MPD>`,
			want: mpdOpen + mpdBase + `<Period xmlns:xlink="http://www.w3.org/1999/xlink" xlink:href="urn:mpeg:dash:resolve-to-zero:2013" xlink:actuate="onLoad"/></MPD>`,
		},
		{
			name: "unknown elements, namespaces and BOM kept, comments dropped, tags and declaration written again, CRLF read as LF",
			in: "\ufeff<?xml version=\"1.0\"?>\r\n<!-- c -->\r\n" + `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" xmlns:scte35="urn:scte:scte35:2014:xml+bin" type='static'>` + "\r\n" +
				`<Period><EventStream schemeIdUri="urn:scte:scte35:2014:xml+bin"><Event id="1"><scte35:Signal><scte35:Binary>/DA=</scte35:Binary></scte35:Signal></Event></EventStream>` +
				`<ProgramInformation moreInformationURL="https://info.example.com/"><Title>T &amp; &lt;x&gt;</Title></ProgramInformation><!-- d --></Period></MPD>` + "\r\n",
			want: "\ufeff<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n\n" + `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" xmlns:scte35="urn:scte:scte35:2014:xml+bin" type="static">` + mpdBase + "\n" +
				`<Period><EventStream schemeIdUri="urn:scte:scte35:2014:xml+bin"><Event id="1"><scte35:Signal><scte35:Binary>/DA=</scte35:Binary></scte35:Signal></Event></EventStream>` +
				`<ProgramInformation moreInformationURL="https://info.example.com/"><Title>T &amp; &lt;x&gt;</Title></ProgramInformation></Period></MPD>` + "\n",
		},
		{
			name: "attributes of rewritten elements re-escaped",
			in:   mpdOpen + `<Period><SegmentTemplate xmlns:x="urn:x" x:a='q"&lt;' media="https://cdn2.example.com/a/$Number$.m4s"/></Period></MPD>`,
			want: mpdOpen + mpdBase + `<Period><SegmentTemplate xmlns:x="urn:x" x:a="q&#34;&lt;" media="http://wsp.lan/p/n/{https://cdn2.example.com/a/}/$Number$.m4s"/></Period></MPD>`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := Rewrite([]byte(tt.in), testOptions(t))
			if err != nil {
				t.Fatalf("Rewrite: %v", err)
			}
			if string(out) != tt.want {
				t.Errorf("got\n%s\nwant\n%s", out, tt.want)
			}
		})
	}
}

func TestRewriteErrors(t *testing.T) {
	const mpdOpen = `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011">`
	tests := []struct {
		name, in string
		want     error
	}{
		{"content protection", mpdOpen + `<Period><AdaptationSet><ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" value="cenc"/></AdaptationSet></Period></MPD>`, ErrDRM},
		{"content protection prefixed, lower case", mpdOpen + `<Period><AdaptationSet><x:contentprotection xmlns:x="urn:x"/></AdaptationSet></Period></MPD>`, ErrDRM},
		{"doctype", `<!DOCTYPE MPD><MPD><Period/></MPD>`, ErrInvalidManifest},
		{"entity", `<!DOCTYPE MPD [<!ENTITY b "https://evil.example/">]><MPD><BaseURL>&b;</BaseURL></MPD>`, ErrInvalidManifest},
		{"unknown entity", mpdOpen + `<BaseURL>&b;</BaseURL></MPD>`, ErrInvalidManifest},
		{"not an MPD", `<playlist><Period/></playlist>`, ErrInvalidManifest},
		{"empty MPD", `<MPD/>`, ErrInvalidManifest},
		{"empty input", ``, ErrInvalidManifest},
		{"HLS", "#EXTM3U\n#EXTINF:2,\nseg.ts\n", ErrInvalidManifest},
		{"second root", mpdOpen + `<Period/></MPD><MPD><BaseURL>https://evil.example/</BaseURL></MPD>`, ErrInvalidManifest},
		{"text after root", mpdOpen + `<Period/></MPD>x`, ErrInvalidManifest},
		{"text before root", `x` + mpdOpen + `<Period/></MPD>`, ErrInvalidManifest},
		{"unclosed", mpdOpen + `<Period>`, ErrInvalidManifest},
		{"mismatched end tag", mpdOpen + `<Period><BaseURL>https://a.example/</Period></BaseURL></MPD>`, ErrInvalidManifest},
		{"duplicate attribute", mpdOpen + `<Period><SegmentTemplate media="a.m4s" media="https://evil.example/a.m4s"/></Period></MPD>`, ErrInvalidManifest},
		{"duplicate attribute in other case", mpdOpen + `<Period><SegmentTemplate media="a.m4s" MEDIA="https://evil.example/a.m4s"/></Period></MPD>`, ErrInvalidManifest},
		{"duplicate attribute other prefix", mpdOpen + `<Period><SegmentTemplate media="a.m4s" x:media="https://evil.example/a.m4s" xmlns:x="urn:x"/></Period></MPD>`, ErrInvalidManifest},
		{"remote period", mpdOpen + `<Period xmlns:xlink="http://www.w3.org/1999/xlink" xlink:href="https://evil.example/p.xml" xlink:actuate="onLoad"/></MPD>`, ErrInvalidManifest},
		{"remote segment list", mpdOpen + `<Period><AdaptationSet><SegmentList href="list.xml"/></AdaptationSet></Period></MPD>`, ErrInvalidManifest},
		{"xml base", mpdOpen + `<Period xml:base="https://evil.example/"/></MPD>`, ErrInvalidManifest},
		{"base with child element", mpdOpen + `<BaseURL>https://a.example/<b/></BaseURL><Period/></MPD>`, ErrInvalidManifest},
		{"base with processing instruction", mpdOpen + `<BaseURL>https://a.example/<?x?></BaseURL><Period/></MPD>`, ErrInvalidManifest},
		{"base with other scheme", mpdOpen + `<BaseURL>ftp://a.example/</BaseURL><Period/></MPD>`, ErrInvalidManifest},
		{"base with user info", mpdOpen + `<BaseURL>https://user:pw@a.example/</BaseURL><Period/></MPD>`, ErrInvalidManifest},
		{"base with space", mpdOpen + `<BaseURL>https://a.example/a b/</BaseURL><Period/></MPD>`, ErrInvalidManifest},
		{"base with tab", mpdOpen + "<BaseURL>/&#9;/evil.example/</BaseURL><Period/></MPD>", ErrInvalidManifest},
		{"base with backslash", mpdOpen + `<BaseURL>\\evil.example\x\</BaseURL><Period/></MPD>`, ErrInvalidManifest},
		{"base does not parse", mpdOpen + `<BaseURL>https://a.example:port/</BaseURL><Period/></MPD>`, ErrInvalidManifest},
		{"base without host", mpdOpen + `<BaseURL>https:///x/</BaseURL><Period/></MPD>`, ErrInvalidManifest},
		{"template with other scheme", mpdOpen + `<Period><SegmentTemplate media="ftp://a.example/$Number$.m4s"/></Period></MPD>`, ErrInvalidManifest},
		{"template with inner space", mpdOpen + `<Period><SegmentTemplate media="/ /evil.example/$Number$.m4s"/></Period></MPD>`, ErrInvalidManifest},
		{"template host from identifier", mpdOpen + `<Period><SegmentTemplate media="https://$RepresentationID$/s.m4s"/></Period></MPD>`, ErrInvalidManifest},
		{"source URL with backslash", mpdOpen + `<Period><SegmentBase><Initialization sourceURL="\\evil.example\i.mp4"/></SegmentBase></Period></MPD>`, ErrInvalidManifest},
		{"representation id with colon", mpdOpen + `<Period><SegmentTemplate media="$RepresentationID$/$Number$.m4s"/><Representation id="https:"/></Period></MPD>`, ErrInvalidManifest},
		{"representation id with leading slash", mpdOpen + `<Period><Representation id="/evil.example"/></Period></MPD>`, ErrInvalidManifest},
		{"representation id with backslash", mpdOpen + `<Period><Representation id="\evil.example"/></Period></MPD>`, ErrInvalidManifest},
		{"representation id with space", mpdOpen + `<Period><Representation id="a b"/></Period></MPD>`, ErrInvalidManifest},
		{"essential chaining", mpdOpen + `<EssentialProperty schemeIdUri="urn:mpeg:dash:chaining:2016" value="https://next.example/"/><Period/></MPD>`, ErrInvalidManifest},
		{"latin-1", `<?xml version="1.0" encoding="ISO-8859-1"?><MPD><Period/></MPD>`, ErrInvalidManifest},
		{"xml 1.1", `<?xml version="1.1"?><MPD><Period/></MPD>`, ErrInvalidManifest},
		{"too deep", mpdOpen + strings.Repeat("<a>", maxDepth) + strings.Repeat("</a>", maxDepth) + `</MPD>`, ErrInvalidManifest},
		{"too large", mpdOpen + strings.Repeat(" ", MaxSize) + `</MPD>`, ErrTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := Rewrite([]byte(tt.in), testOptions(t))
			if !errors.Is(err, tt.want) {
				t.Fatalf("Rewrite = %q, %v; want %v", out, err, tt.want)
			}
			if strings.Contains(err.Error(), "example") {
				t.Errorf("error contains a URL: %v", err)
			}
		})
	}
}

func TestRewriteOptionErrors(t *testing.T) {
	in := []byte(`<MPD><Period/></MPD>`)
	for _, final := range []string{"", "/rel/m.mpd", "ftp://a.example/m.mpd", "https:///m.mpd"} {
		opts := testOptions(t)
		opts.Final = nil
		if final != "" {
			opts.Final = mustParse(t, final)
		}
		if _, err := Rewrite(in, opts); !errors.Is(err, ErrInvalidFinalURL) {
			t.Errorf("final %q: error %v, want ErrInvalidFinalURL", final, err)
		}
	}
	opts := testOptions(t)
	opts.PrefixURL = func(string, bool) (string, error) { return "", errors.New("no") }
	if _, err := Rewrite(in, opts); !errors.Is(err, ErrInvalidManifest) {
		t.Errorf("prefix builder error: %v, want ErrInvalidManifest", err)
	}
}

func TestRewriteWithoutTimeURLRemovesUTCTiming(t *testing.T) {
	opts := testOptions(t)
	opts.TimeURL = ""
	out, err := Rewrite([]byte(`<MPD><Period/><UTCTiming schemeIdUri="urn:mpeg:dash:utc:http-iso:2014" value="https://time.example.org/"/></MPD>`), opts)
	if err != nil {
		t.Fatal(err)
	}
	if want := `<MPD><BaseURL>http://wsp.lan/p/n/{https://cdn.example.net/ch/}/</BaseURL><Period/></MPD>`; string(out) != want {
		t.Errorf("got %s, want %s", out, want)
	}
}

func TestRewriteDoesNotModifyInput(t *testing.T) {
	in, err := os.ReadFile("testdata/live.mpd")
	if err != nil {
		t.Fatal(err)
	}
	orig := bytes.Clone(in)
	if _, err := Rewrite(in, testOptions(t)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(in, orig) {
		t.Error("Rewrite modified its input")
	}
}

// TestRewriteManyAttributes guards against quadratic checks: an element
// with very many attributes is checked in linear time.
func TestRewriteManyAttributes(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<MPD><Period`)
	for i := range 200000 {
		b.WriteString(` a` + strconv.Itoa(i) + `="1"`)
	}
	b.WriteString(`/></MPD>`)
	if _, err := Rewrite([]byte(b.String()), testOptions(t)); err != nil {
		t.Fatal(err)
	}
	dup := strings.Replace(b.String(), ` a199999="1"`, ` a0="2"`, 1)
	if _, err := Rewrite([]byte(dup), testOptions(t)); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("duplicate among many attributes: %v", err)
	}
}

func TestCount(t *testing.T) {
	in, err := os.ReadFile("testdata/nested.mpd")
	if err != nil {
		t.Fatal(err)
	}
	if p, a := Count(in); p != 1 || a != 2 {
		t.Errorf("Count = %d periods, %d adaptation sets; want 1, 2", p, a)
	}
	if p, a := Count([]byte("<MPD><Period/><period><AdaptationSet/><x")); p != 2 || a != 1 {
		t.Errorf("Count of a broken manifest = %d, %d", p, a)
	}
}
