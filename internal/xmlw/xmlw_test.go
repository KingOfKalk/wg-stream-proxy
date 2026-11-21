// SPDX-License-Identifier: AGPL-3.0-or-later

package xmlw

import (
	"bytes"
	"encoding/xml"
	"io"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestValidName(t *testing.T) {
	tests := []struct {
		space, local string
		want         bool
	}{
		{"", "MPD", true},
		{"", "a", true},
		{"", "a1._-", true},
		{"dvb", "url", true},
		{"xlink", "href", true},
		{"", "", false},
		{"", "_media", false},
		{"", "1a", false},
		{"", "-a", false},
		{"", ".a", false},
		{"", "a:b", false},
		{"", "a b", false},
		{"", "a\u00e9", false},
		{"", "\u00e9", false},
		{"_x", "media", false},
		{"x", "_media", false},
		{"x", "", false},
		{"1", "a", false},
	}
	for _, tt := range tests {
		if got := ValidName(xml.Name{Space: tt.space, Local: tt.local}); got != tt.want {
			t.Errorf("ValidName(%q, %q) = %v, want %v", tt.space, tt.local, got, tt.want)
		}
	}
}

func TestAppend(t *testing.T) {
	name := xml.Name{Space: "mpd", Local: "BaseURL"}
	attrs := []xml.Attr{
		{Name: xml.Name{Local: "a"}, Value: `&<>"'` + "\t\n\r x"},
		{Name: xml.Name{Space: "dvb", Local: "url"}, Value: ""},
	}
	tests := []struct {
		name string
		got  []byte
		want string
	}{
		{"start tag", AppendStartTag([]byte("x"), name, attrs), `x<mpd:BaseURL a="&amp;&lt;&gt;&#34;&#39;&#x9;&#xA;&#xD; x" dvb:url=""`},
		{"start tag without attributes", AppendStartTag(nil, xml.Name{Local: "tv"}, nil), `<tv`},
		{"attribute", AppendAttr([]byte("<a"), xml.Attr{Name: xml.Name{Local: "b"}, Value: "c&d"}), `<a b="c&amp;d"`},
		{"end tag", AppendEndTag([]byte("x"), name), `x</mpd:BaseURL>`},
		{"element", AppendElement(nil, name, attrs[1:], "h&<>\r\n"), `<mpd:BaseURL dvb:url="">h&amp;&lt;&gt;&#xD;&#xA;</mpd:BaseURL>`},
		{"text", AppendText([]byte("x"), []byte("&<>\r\n\t\"'y")), "x&amp;&lt;&gt;&#xD;\n\t\"'y"},
	}
	for _, tt := range tests {
		if string(tt.got) != tt.want {
			t.Errorf("%s:\n got %s\nwant %s", tt.name, tt.got, tt.want)
		}
	}
}

// TestRoundTrip: encoding/xml reads back the names, values and text
// that were written, whitespace and CR included.
func TestRoundTrip(t *testing.T) {
	values := []string{"", "plain", `&<>"'`, "\t\n\r \r\n", "]]>", "&amp;", "\u00e9\u2028"}
	for _, v := range values {
		attrs := []xml.Attr{{Name: xml.Name{Local: "v"}, Value: v}, {Name: xml.Name{Space: "p", Local: "w"}, Value: v}}
		doc := AppendStartTag(nil, xml.Name{Local: "r"}, []xml.Attr{{Name: xml.Name{Space: "xmlns", Local: "p"}, Value: "urn:p"}})
		doc = append(doc, '>')
		doc = AppendText(doc, []byte(v))
		doc = append(AppendStartTag(doc, xml.Name{Space: "p", Local: "e"}, attrs), "/>"...)
		doc = AppendElement(doc, xml.Name{Local: "t"}, nil, v)
		doc = AppendEndTag(doc, xml.Name{Local: "r"})

		d := xml.NewDecoder(bytes.NewReader(doc))
		d.Strict = true
		var texts []string
		for {
			tok, err := d.RawToken()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("%q: %v\n%s", v, err, doc)
			}
			switch tok := tok.(type) {
			case xml.StartElement:
				if tok.Name.Local == "e" {
					if len(tok.Attr) != 2 || tok.Attr[0] != attrs[0] || tok.Attr[1] != attrs[1] {
						t.Errorf("%q: attributes %q, want %q", v, tok.Attr, attrs)
					}
				}
			case xml.CharData:
				texts = append(texts, string(tok))
			}
		}
		if v == "" && len(texts) == 0 {
			continue // empty text gives no token
		}
		if len(texts) != 2 || texts[0] != v || texts[1] != v {
			t.Errorf("%q: texts %q", v, texts)
		}
	}
}
