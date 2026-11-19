// SPDX-License-Identifier: AGPL-3.0-or-later

package dash

import (
	"encoding/xml"
	"errors"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The output of the rewriter must read the same for encoding/xml and for
// the lighter XML parsers of web players (dash.js cml-xml, Shaka tXml).
// lenientValues mimics them: attribute names start at the next ASCII
// letter (other characters are skipped), a later attribute overwrites an
// earlier one, a comment ends at the first "-->" after "<!-", processing
// instructions and declarations end at the first '>'.

// urlAttr reports whether an attribute (lower-case local names) carries
// a URL or decides about one.
func urlAttr(ln, an string) bool {
	switch {
	case containsString(refAttrs[ln], an), an == "href":
		return true
	case ln == "representation":
		return an == "id"
	case ln == "eventstream", ln == "inbandeventstream", ln == "utctiming",
		ln == "supplementalproperty", ln == "essentialproperty":
		return an == "schemeiduri" || an == "value" || an == "url"
	}
	return false
}

func localLower(name string) string {
	if i := strings.LastIndexByte(name, ':'); i >= 0 {
		name = name[i+1:]
	}
	return strings.ToLower(name)
}

// entries returns the URL-bearing values of one element, sorted.
func entries(ln string, attrs map[string]string) []string {
	var out []string
	for an, v := range attrs {
		if urlAttr(ln, an) {
			out = append(out, ln+"."+an+"="+v)
		}
	}
	sort.Strings(out)
	return out
}

// goValues reads the URL-bearing values of a document with encoding/xml.
func goValues(t *testing.T, doc []byte) []string {
	t.Helper()
	var out []string
	d := xml.NewDecoder(strings.NewReader(string(doc)))
	d.Strict = true
	var base *strings.Builder
	for {
		tok, err := d.RawToken()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("output does not parse: %v", err)
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			ln := strings.ToLower(tok.Name.Local)
			attrs := map[string]string{}
			for _, a := range tok.Attr {
				attrs[strings.ToLower(a.Name.Local)] = a.Value
			}
			out = append(out, entries(ln, attrs)...)
			if ln == "baseurl" {
				base = &strings.Builder{}
			}
		case xml.CharData:
			if base != nil {
				base.Write(tok)
			}
		case xml.EndElement:
			if base != nil {
				out = append(out, "baseurl="+base.String())
				base = nil
			}
		}
	}
}

// lenientValues reads the URL-bearing values of a document like a
// lenient player parser.
func lenientValues(s string) []string {
	var out []string
	i := 0
	for {
		j := strings.IndexByte(s[i:], '<')
		if j < 0 {
			return out
		}
		i += j
		rest := s[i:]
		var end int
		switch {
		case strings.HasPrefix(rest, "<!-"):
			end = strings.Index(rest[2:], "-->")
			if end < 0 {
				return out
			}
			i += 2 + end + 3
			continue
		case strings.HasPrefix(rest, "<![CDATA["):
			end = strings.Index(rest, "]]>")
			if end < 0 {
				return out
			}
			i += end + 3
			continue
		case strings.HasPrefix(rest, "<?"), strings.HasPrefix(rest, "<!"), strings.HasPrefix(rest, "</"):
			end = strings.IndexByte(rest, '>')
			if end < 0 {
				return out
			}
			i += end + 1
			continue
		}
		k := 1
		for k < len(rest) && !strings.ContainsRune(" \t\r\n/>", rune(rest[k])) {
			k++
		}
		ln := localLower(rest[1:k])
		attrs := map[string]string{}
		for k < len(rest) && rest[k] != '>' {
			c := rest[k]
			if !isLetter(c) {
				k++
				continue
			}
			n := k
			for k < len(rest) && (isLetterDigit(rest[k]) || strings.IndexByte(":._-", rest[k]) >= 0) {
				k++
			}
			name := rest[n:k]
			for k < len(rest) && strings.IndexByte(" \t\r\n", rest[k]) >= 0 {
				k++
			}
			if k >= len(rest) || rest[k] != '=' {
				attrs[localLower(name)] = ""
				continue
			}
			k++
			for k < len(rest) && strings.IndexByte(" \t\r\n", rest[k]) >= 0 {
				k++
			}
			if k < len(rest) && (rest[k] == '"' || rest[k] == '\'') {
				q := rest[k]
				e := strings.IndexByte(rest[k+1:], q)
				if e < 0 {
					return out
				}
				attrs[localLower(name)] = decodeEntities(rest[k+1 : k+1+e])
				k += e + 2
				continue
			}
			v := k
			for k < len(rest) && strings.IndexByte(" \t\r\n>", rest[k]) < 0 {
				k++
			}
			attrs[localLower(name)] = decodeEntities(rest[v:k])
		}
		out = append(out, entries(ln, attrs)...)
		i += k
		if ln == "baseurl" && !strings.HasSuffix(rest[:k], "/") {
			text := rest[k+1:]
			if e := strings.IndexByte(text, '<'); e >= 0 {
				text = text[:e]
			}
			out = append(out, "baseurl="+decodeEntities(text))
		}
	}
}

func isLetterDigit(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// decodeEntities decodes the predefined and numeric character
// references.
func decodeEntities(s string) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '&')
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		s = s[i:]
		e := strings.IndexByte(s, ';')
		if e < 0 {
			b.WriteString(s)
			return b.String()
		}
		ref := s[1:e]
		switch {
		case ref == "amp":
			b.WriteByte('&')
		case ref == "lt":
			b.WriteByte('<')
		case ref == "gt":
			b.WriteByte('>')
		case ref == "quot":
			b.WriteByte('"')
		case ref == "apos":
			b.WriteByte('\'')
		case strings.HasPrefix(ref, "#x"):
			n, _ := strconv.ParseUint(ref[2:], 16, 32)
			b.WriteRune(rune(n))
		case strings.HasPrefix(ref, "#"):
			n, _ := strconv.ParseUint(ref[1:], 10, 32)
			b.WriteRune(rune(n))
		default:
			b.WriteString(s[:e+1])
		}
		s = s[e+1:]
	}
}

// checkLenient fails t if a lenient parser reads other URL-bearing
// values from out than encoding/xml.
func checkLenient(t *testing.T, out []byte) {
	t.Helper()
	g, l := goValues(t, out), lenientValues(string(out))
	if !slices.Equal(g, l) {
		t.Fatalf("lenient parse differs:\nencoding/xml: %q\nlenient:      %q\n%s", g, l, out)
	}
}

// TestLenientParser checks the lenient parser on the inputs of the
// review: it reads the hidden attributes and elements.
func TestLenientParser(t *testing.T) {
	for in, want := range map[string][]string{
		`<SegmentTemplate media="a" _media="b"/>`:                {"segmenttemplate.media=b"},
		`<!---><BaseURL>https://e.example/</BaseURL>-->`:         {"baseurl=https://e.example/"},
		`<?x a><BaseURL>x&amp;y</BaseURL><x b=""/?>`:             {"baseurl=x&y"},
		`<Representation id='v' :id="w&#x2F;"></Representation>`: {"representation.id=w/"},
	} {
		if got := lenientValues(in); !slices.Equal(got, want) {
			t.Errorf("lenientValues(%q) = %q, want %q", in, got, want)
		}
	}
}
