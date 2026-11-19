// SPDX-License-Identifier: AGPL-3.0-or-later

package epg

import (
	"bytes"
	"encoding/xml"
	"io"
	"testing"
)

// FuzzParse checks that Open and Parse never panic, and that a parsed
// index is consistent: sorted unique ids, no empty ids or names, and
// matching any name never fails.
func FuzzParse(f *testing.F) {
	plain, err := readFixture()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(plain)
	f.Add([]byte(`<tv><channel id="a"><display-name>A</display-name></channel><programme/></tv>`))
	f.Add([]byte("<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><tv><channel id=\"\xe9\"/></tv>"))
	f.Add([]byte{0x1f, 0x8b, 8, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := Open(bytes.NewReader(data), 1<<20)
		if err != nil {
			return
		}
		x, err := Parse(io.LimitReader(r, 1<<20))
		if err != nil {
			return
		}
		prev := ""
		for _, c := range x.Channels() {
			if c.ID == "" || c.ID <= prev {
				t.Fatalf("ids not sorted and unique: %q after %q", c.ID, prev)
			}
			prev = c.ID
			for _, n := range c.Names {
				if n == "" {
					t.Fatalf("empty name in %q", c.ID)
				}
				x.Match(n, 3)
			}
		}
	})
}

// FuzzFilter checks the filter of the served guide (spec 7.2): it never
// panics, and what it keeps is a well-formed XMLTV document that has
// only the root tv, <channel> and <programme> elements of the wanted
// ids, plain names, no comments, processing instructions, declarations
// or CDATA after the XML declaration, and that filters to itself again.
func FuzzFilter(f *testing.F) {
	plain, err := readFixture()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(plain)
	f.Add([]byte(source("gr", "a", "b.example")))
	f.Add([]byte(`<?xml version="1.0" encoding="ISO-8859-1"?><!DOCTYPE tv SYSTEM "xmltv.dtd"><tv><channel id="a" x:y="1" id="b"><!-- c --><display-name><![CDATA[<b>&]]></display-name><x:z/><icon src="a&amp;b"></icon></channel>` +
		"<programme channel=\"a\" stop=\"20261119103000 +0100\"><title>\xe9</title></programme><programme channel=\"a\"><?pi x?></programme></tv>"))
	f.Add([]byte(`<tv><channel id="a"><display-name a="1" a="2">x</display-name></channel><foo><channel id="a"/></foo></tv>`))
	f.Fuzz(func(t *testing.T, data []byte) {
		ids := map[string]bool{"a": true, "b.example": true}
		c, err := filterLimit(bytes.NewReader(data), ids, filterNow, 1<<20)
		if err != nil {
			return
		}
		wants := []Want{{ID: "a"}, {ID: "b.example"}}
		doc, _, _ := Merge([]string{"s"}, map[string]*Copy{"s": c}, wants)
		checkGuide(t, doc, ids)
		again, err := filterLimit(bytes.NewReader(doc), ids, filterNow, 1<<21)
		if err != nil {
			t.Fatalf("guide does not filter again: %v\n%s", err, doc)
		}
		doc2, _, _ := Merge([]string{"s"}, map[string]*Copy{"s": again}, wants)
		if !bytes.Equal(doc, doc2) {
			t.Fatalf("guide changes when filtered again:\n%s\n---\n%s", doc, doc2)
		}
	})
}

// checkGuide checks a served guide with a strict parser.
func checkGuide(t *testing.T, doc []byte, ids map[string]bool) {
	t.Helper()
	if !bytes.HasPrefix(doc, []byte(xmlHeader)) {
		t.Fatalf("no header:\n%s", doc)
	}
	d := xml.NewDecoder(bytes.NewReader(doc[len(`<?xml version="1.0" encoding="UTF-8"?>`):]))
	d.Strict = true
	depth := 0
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("guide is not well-formed: %v\n%s", err, doc)
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			depth++
			if !plainName(tok.Name) {
				t.Fatalf("name %v", tok.Name)
			}
			for _, a := range tok.Attr {
				if !plainName(a.Name) {
					t.Fatalf("attribute name %v", a.Name)
				}
			}
			switch {
			case depth == 1 && tok.Name.Local != "tv":
				t.Fatalf("root %s", tok.Name.Local)
			case depth == 2 && tok.Name.Local == "channel":
				if id, _ := attr(tok, "id"); !ids[id] {
					t.Fatalf("channel %q", id)
				}
			case depth == 2 && tok.Name.Local == "programme":
				if id, _ := attr(tok, "channel"); !ids[id] {
					t.Fatalf("programme of %q", id)
				}
				if stop, ok := attr(tok, "stop"); ok {
					if end, ok := ParseTime(stop); ok && !end.After(filterNow) {
						t.Fatalf("ended programme %q", stop)
					}
				}
			case depth == 2:
				t.Fatalf("element %s in tv", tok.Name.Local)
			}
		case xml.EndElement:
			depth--
		case xml.Comment, xml.ProcInst, xml.Directive:
			t.Fatalf("token %T in the guide", tok)
		}
	}
	if bytes.Contains(doc, []byte("<![CDATA[")) || bytes.Contains(doc, []byte("<!--")) {
		t.Fatalf("CDATA or comment:\n%s", doc)
	}
}
