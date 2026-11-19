// SPDX-License-Identifier: AGPL-3.0-or-later

package epg

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// filterNow is the refresh time of the filter tests: 2026-11-19 10:30 UTC.
var filterNow = time.Date(2026, 11, 19, 10, 30, 0, 0, time.UTC)

// filterDoc filters doc for ids at filterNow and merges the result as
// the only source.
func filterDoc(t *testing.T, doc string, ids ...string) (string, *Copy) {
	t.Helper()
	set := map[string]bool{}
	var wants []Want
	for _, id := range ids {
		set[id] = true
		wants = append(wants, Want{ID: id})
	}
	c, err := Filter(strings.NewReader(doc), set, filterNow)
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	out, _, _ := Merge([]string{"s"}, map[string]*Copy{"s": c}, wants)
	return string(out), c
}

// guide wraps kept elements like Merge does.
func guide(elems ...string) string {
	return xmlHeader + strings.Join(elems, "\n") + "\n</tv>\n"
}

func TestFilter(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		ids  []string
		want string
	}{
		{
			name: "keeps wanted channels and their programmes",
			doc: `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE tv SYSTEM "xmltv.dtd">
<tv generator-info-name="x">
<channel id="a.example"><display-name lang="el">Άλφα</display-name></channel>
<channel id="b.example"><display-name>B</display-name></channel>
<programme start="20261119100000 +0000" stop="20261119110000 +0000" channel="a.example"><title>A1</title></programme>
<programme start="20261119100000 +0000" stop="20261119110000 +0000" channel="b.example"><title>B1</title></programme>
<programme start="20261119110000 +0000" stop="20261119120000 +0000" channel="a.example"><title>A2</title></programme>
</tv>`,
			ids: []string{"a.example"},
			want: guide(`<channel id="a.example"><display-name lang="el">Άλφα</display-name></channel>`,
				`<programme start="20261119100000 +0000" stop="20261119110000 +0000" channel="a.example"><title>A1</title></programme>`,
				`<programme start="20261119110000 +0000" stop="20261119120000 +0000" channel="a.example"><title>A2</title></programme>`),
		},
		{
			name: "ended programmes and time zones",
			doc: `<tv><channel id="a"/>
<programme start="20261119090000 +0000" stop="20261119103000 +0000" channel="a"><title>ends now</title></programme>
<programme start="20261119100000 +0200" stop="20261119120000 +0200" channel="a"><title>ended 10:00 UTC</title></programme>
<programme start="20261119100000 -0100" stop="20261119110000 -0100" channel="a"><title>ends 12:00 UTC</title></programme>
<programme start="20261119100000" stop="20261119103100" channel="a"><title>no zone is UTC</title></programme>
<programme start="20261119100000 +0000" stop="202611191030+0000" channel="a"><title>minutes, no space, ended</title></programme>
<programme start="20261119100000 +0000" channel="a"><title>no stop</title></programme>
<programme start="20261119100000 +0000" stop="20261119 BST" channel="a"><title>unknown form</title></programme>
</tv>`,
			ids: []string{"a"},
			want: guide(`<channel id="a"/>`,
				`<programme start="20261119100000 -0100" stop="20261119110000 -0100" channel="a"><title>ends 12:00 UTC</title></programme>`,
				`<programme start="20261119100000" stop="20261119103100" channel="a"><title>no zone is UTC</title></programme>`,
				`<programme start="20261119100000 +0000" channel="a"><title>no stop</title></programme>`,
				`<programme start="20261119100000 +0000" stop="20261119 BST" channel="a"><title>unknown form</title></programme>`),
		},
		{
			name: "missing ids and duplicate channels",
			doc: `<tv><channel id="a"><display-name>first</display-name></channel>
<channel id="a"><display-name>second</display-name></channel>
<programme channel="zz" stop="20261120000000"/></tv>`,
			ids:  []string{"a", "zz", "nope"},
			want: guide(`<channel id="a"><display-name>first</display-name></channel>`),
		},
		{
			name: "written again from tokens",
			doc: "\ufeff<?xml version='1.0'?><?xml-stylesheet href=\"x.xsl\"?><!-- c --><tv xmlns:x=\"urn:x\">" +
				"<channel id='a' x:evil='1' id='b' _bad='1'  lang = \"el\" ><!-- note --><display-name><![CDATA[A & <B>]]></display-name>" +
				"<x:url>https://evil.example/</x:url><icon src=\"https://logos.example.org/a.png?x=1&amp;y=&quot;2&quot;\"></icon>" +
				"<?pi data?><_x>dropped</_x><url>\r\nhttps://www.example.org/</url></channel></tv>",
			ids: []string{"a"},
			want: guide(`<channel id="a" lang="el"><display-name>A &amp; &lt;B&gt;</display-name>` +
				`<icon src="https://logos.example.org/a.png?x=1&amp;y=&#34;2&#34;"/>` +
				"<url>\nhttps://www.example.org/</url></channel>"),
		},
		{
			name: "other top-level elements and namespaced channels dropped",
			doc:  `<tv><x:channel xmlns:x="urn:x" id="a"/><foo id="a"><channel id="a"/></foo><channel id="a"/></tv>`,
			ids:  []string{"a"},
			want: guide(`<channel id="a"/>`),
		},
		{
			name: "attribute values escaped",
			doc:  "<tv><channel id=\"a\"><display-name note=\"tab&#9;nl&#10;cr&#13;&lt;&gt;&apos;\">x</display-name></channel></tv>",
			ids:  []string{"a"},
			want: guide(`<channel id="a"><display-name note="tab&#x9;nl&#xA;cr&#xD;&lt;&gt;&#39;">x</display-name></channel>`),
		},
		{
			name: "other encoding",
			doc:  "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><tv><channel id=\"a\"><display-name>caf\xe9</display-name></channel></tv>",
			ids:  []string{"a"},
			want: guide(`<channel id="a"><display-name>café</display-name></channel>`),
		},
		{
			name: "nothing wanted",
			doc:  `<tv><channel id="a"/></tv>`,
			want: xmlHeader + "</tv>\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := filterDoc(t, tt.doc, tt.ids...)
			if got != tt.want {
				t.Errorf("got\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

func TestFilterCounts(t *testing.T) {
	doc := `<tv><channel id="a"/><channel id="b"/><channel id="c"/>
<programme channel="a"/><programme channel="a"/><programme channel="b"/><programme channel="c"/></tv>`
	c, err := Filter(strings.NewReader(doc), map[string]bool{"a": true, "b": true, "d": true}, filterNow)
	if err != nil {
		t.Fatal(err)
	}
	if c.Read != int64(len(doc)) || c.Channels() != 2 || c.Programmes != 3 || !c.Has("a") || c.Has("c") || c.Has("d") {
		t.Errorf("read %d channels %d programmes %d", c.Read, c.Channels(), c.Programmes)
	}
	want := len(`<channel id="a"/>`+"\n") * 2
	want += len(`<programme channel="a"/>`+"\n") * 3
	if c.Size != int64(want) {
		t.Errorf("size %d, want %d", c.Size, want)
	}
}

func TestFilterInvalid(t *testing.T) {
	deep := "<tv><channel id=\"a\">" + strings.Repeat("<x>", 31) + strings.Repeat("</x>", 31) + "</channel></tv>"
	tests := []struct {
		name, doc, want string
	}{
		{"empty", "", "no complete tv element"},
		{"whitespace", " \n", "no complete tv element"},
		{"other root", `<rss><channel id="a"/></rss>`, "root element is not tv"},
		{"prefixed root", `<x:tv xmlns:x="urn:x"><channel id="a"/></x:tv>`, "root element is not tv"},
		{"cut", `<tv><channel id="a">`, "no complete tv element"},
		{"cut in a tag", `<tv><channel id="a`, "XML syntax error in line 1"},
		{"mismatched end tag", `<tv><channel id="a"><title></b></channel></tv>`, "mismatched end tag"},
		{"mismatched end tag of a dropped element", `<tv><foo></bar></tv>`, "mismatched end tag"},
		{"text before the root", `hello<tv/>`, "text outside the root element"},
		{"text after the root", `<tv/>x`, "text outside the root element"},
		{"second root", `<tv/><tv><channel id="a"/></tv>`, "content after the root element"},
		{"too deep", deep, "elements nested too deep"},
		{"undefined entity", `<tv><channel id="a"><display-name>&nbsp;</display-name></channel></tv>`, "XML syntax error"},
		{"entity from the doctype", `<!DOCTYPE tv [<!ENTITY e "<channel id='a'/>">]><tv>&e;</tv>`, "XML syntax error"},
		{"invalid UTF-8", "<tv><channel id=\"a\"><display-name>\xff</display-name></channel></tv>", "XML syntax error"},
		{"XML 1.1", `<?xml version="1.1"?><tv/>`, "unsupported XML version or encoding"},
		{"unknown encoding", `<?xml version="1.0" encoding="x-nope"?><tv/>`, "unsupported XML version or encoding"},
		{"duplicate quote", `<tv><channel id="a"" /></tv>`, "XML syntax error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Filter(strings.NewReader(tt.doc), map[string]bool{"a": true}, filterNow)
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %v, want ErrInvalid with %q", err, tt.want)
			}
		})
	}
	if _, err := Filter(strings.NewReader(deep), map[string]bool{}, filterNow); err == nil {
		t.Error("depth is checked also for elements that are not kept")
	}
	ok := "<tv><channel id=\"a\">" + strings.Repeat("<x>", 30) + strings.Repeat("</x>", 30) + "</channel></tv>"
	if _, err := Filter(strings.NewReader(ok), map[string]bool{"a": true}, filterNow); err != nil {
		t.Errorf("depth 32: %v", err)
	}
}

// TestFilterErrorHidesInput: errors never quote the source (logs).
func TestFilterErrorHidesInput(t *testing.T) {
	for _, doc := range []string{
		`<tv><channel id="a"></secret-token></channel></tv>`,
		`<tv><secret-token`,
		`<tv><channel id="a">&secret-token;</channel></tv>`,
		`<secret-token/>`,
	} {
		_, err := Filter(strings.NewReader(doc), map[string]bool{"a": true}, filterNow)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Errorf("%q: error %v", doc, err)
		}
	}
}

func TestFilterLimits(t *testing.T) {
	// The kept bytes: escaping makes the output longer than the input.
	doc := `<tv><channel id="a"><display-name>` + strings.Repeat(`&amp;`, 100) + `</display-name></channel></tv>`
	if _, err := filterLimit(strings.NewReader(doc), map[string]bool{"a": true}, filterNow, 100); !errors.Is(err, ErrTooLarge) {
		t.Errorf("text: error %v, want ErrTooLarge", err)
	}
	doc = `<tv><channel id="a" n="` + strings.Repeat(`'`, 100) + `"/></tv>`
	if _, err := filterLimit(strings.NewReader(doc), map[string]bool{"a": true}, filterNow, 300); !errors.Is(err, ErrTooLarge) {
		t.Errorf("attribute: error %v, want ErrTooLarge", err)
	}
	if _, err := filterLimit(strings.NewReader(doc), map[string]bool{"b": true}, filterNow, 300); err != nil {
		t.Errorf("not kept: %v", err)
	}
	// The input, decompressed (Open).
	big := gz(t, []byte("<tv>"+strings.Repeat(" ", 2000)+"</tv>"))
	r, err := Open(strings.NewReader(string(big)), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Filter(r, nil, filterNow); !errors.Is(err, ErrTooLarge) {
		t.Errorf("input: error %v, want ErrTooLarge", err)
	}
}

func TestParseTime(t *testing.T) {
	utc := func(s string) time.Time {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			panic(err)
		}
		return t
	}
	tests := []struct {
		in   string
		want time.Time
		ok   bool
	}{
		{"20261119103000 +0000", utc("2026-11-19T10:30:00Z"), true},
		{"20261119103000 +0230", utc("2026-11-19T08:00:00Z"), true},
		{"20261119103000 -0100", utc("2026-11-19T11:30:00Z"), true},
		{"20261119103000+0100", utc("2026-11-19T09:30:00Z"), true},
		{"20261119103000", utc("2026-11-19T10:30:00Z"), true},
		{" 202611191030 ", utc("2026-11-19T10:30:00Z"), true},
		{"202611191030 +0100", utc("2026-11-19T09:30:00Z"), true},
		{"20261119", time.Time{}, false},
		{"20261119103000 BST", time.Time{}, false},
		{"20261119103000 +01", time.Time{}, false},
		{"20261119103000 +2400", time.Time{}, false},
		{"20261119103000 +0160", time.Time{}, false},
		{"20261119103000 +01a0", time.Time{}, false},
		{"20261319103000", time.Time{}, false},
		{"2026111910300", time.Time{}, false},
		{"", time.Time{}, false},
	}
	for _, tt := range tests {
		got, ok := ParseTime(tt.in)
		if ok != tt.ok || !got.Equal(tt.want) {
			t.Errorf("ParseTime(%q) = %v, %v; want %v, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}
