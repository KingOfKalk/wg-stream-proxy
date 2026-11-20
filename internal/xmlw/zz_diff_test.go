// SPDX-License-Identifier: AGPL-3.0-or-later

package xmlw

// Temporary differential test (issue 139): the writers of internal/dash
// and internal/epg on origin/main against this package. Not committed.

import (
	"bytes"
	"encoding/xml"
	"testing"
)

// ---- internal/dash/rewrite.go on origin/main ----

func dashValidName(n xml.Name) bool {
	return (n.Space == "" || dashValidNCName(n.Space)) && dashValidNCName(n.Local)
}

func dashValidNCName(s string) bool {
	if s == "" || !dashIsLetter(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if c := s[i]; !dashIsLetter(c) && (c < '0' || c > '9') && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func dashIsLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func dashAppendStartTag(dst []byte, name xml.Name, attrs []xml.Attr, selfClose bool) []byte {
	dst = dashAppendName(append(dst, '<'), name)
	for _, a := range attrs {
		dst = dashAppendName(append(dst, ' '), a.Name)
		dst = append(dashAppendEscaped(append(dst, `="`...), a.Value), '"')
	}
	if selfClose {
		return append(dst, "/>"...)
	}
	return append(dst, '>')
}

func dashAppendElement(dst []byte, name xml.Name, attrs []xml.Attr, text string) []byte {
	dst = dashAppendEscaped(dashAppendStartTag(dst, name, attrs, false), text)
	return append(dashAppendName(append(dst, "</"...), name), '>')
}

func dashAppendName(dst []byte, n xml.Name) []byte {
	if n.Space != "" {
		dst = append(append(dst, n.Space...), ':')
	}
	return append(dst, n.Local...)
}

func dashAppendEscaped(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '&':
			dst = append(dst, "&amp;"...)
		case '<':
			dst = append(dst, "&lt;"...)
		case '>':
			dst = append(dst, "&gt;"...)
		case '"':
			dst = append(dst, "&#34;"...)
		case '\'':
			dst = append(dst, "&#39;"...)
		case '\t':
			dst = append(dst, "&#x9;"...)
		case '\n':
			dst = append(dst, "&#xA;"...)
		case '\r':
			dst = append(dst, "&#xD;"...)
		default:
			dst = append(dst, c)
		}
	}
	return dst
}

func dashAppendText(dst []byte, s []byte) []byte {
	for _, c := range s {
		switch c {
		case '&':
			dst = append(dst, "&amp;"...)
		case '<':
			dst = append(dst, "&lt;"...)
		case '>':
			dst = append(dst, "&gt;"...)
		case '\r':
			dst = append(dst, "&#xD;"...)
		default:
			dst = append(dst, c)
		}
	}
	return dst
}

// ---- internal/epg/filter.go on origin/main ----

func epgPlainName(n xml.Name) bool {
	if n.Space != "" || n.Local == "" || !dashIsLetter(n.Local[0]) {
		return false
	}
	for i := 1; i < len(n.Local); i++ {
		if c := n.Local[i]; !dashIsLetter(c) && (c < '0' || c > '9') && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func epgAppendStartTag(dst []byte, t xml.StartElement) []byte {
	dst = epgAppendName(append(dst, '<'), t.Name)
	for i, a := range t.Attr {
		if !epgPlainName(a.Name) || epgRepeated(t.Attr[:i], a.Name) {
			continue
		}
		dst = epgAppendName(append(dst, ' '), a.Name)
		dst = append(dashAppendEscaped(append(dst, `="`...), a.Value), '"')
	}
	return dst
}

func epgRepeated(attrs []xml.Attr, n xml.Name) bool {
	for _, a := range attrs {
		if a.Name == n {
			return true
		}
	}
	return false
}

func epgAppendName(dst []byte, n xml.Name) []byte { return append(dst, n.Local...) }

// ---- the new callers, as written in dash and epg on the branch ----

func newEpgPlainName(n xml.Name) bool { return n.Space == "" && ValidName(n) }

func newEpgAppendStartTag(dst []byte, t xml.StartElement) []byte {
	dst = AppendStartTag(dst, t.Name, nil)
	for i, a := range t.Attr {
		if !newEpgPlainName(a.Name) || epgRepeated(t.Attr[:i], a.Name) {
			continue
		}
		dst = AppendAttr(dst, a)
	}
	return dst
}

func newDashStartTag(dst []byte, name xml.Name, attrs []xml.Attr, selfClose bool) []byte {
	dst = AppendStartTag(dst, name, attrs)
	if selfClose {
		return append(dst, "/>"...)
	}
	return append(dst, '>')
}

func FuzzZZDiff(f *testing.F) {
	f.Add("mpd", "BaseURL", "", "a", "dvb", "url", "x&<>\"'\t\n\r", []byte("t&<>\r\n"), true)
	f.Add("", "channel", "", "id", "", "id", "a", []byte(""), false)
	f.Add("_x", "1a", "x", "_m", "", "", "é", []byte("é"), false)
	f.Fuzz(func(t *testing.T, space, local, s1, l1, s2, l2, value string, text []byte, selfClose bool) {
		name := xml.Name{Space: space, Local: local}
		attrs := []xml.Attr{
			{Name: xml.Name{Space: s1, Local: l1}, Value: value},
			{Name: xml.Name{Space: s2, Local: l2}, Value: value + "x"},
			{Name: xml.Name{Space: s1, Local: l1}, Value: "dup"},
		}
		for _, n := range []xml.Name{name, attrs[0].Name, attrs[1].Name} {
			if dashValidName(n) != ValidName(n) {
				t.Fatalf("ValidName(%q) differs", n)
			}
			if epgPlainName(n) != newEpgPlainName(n) {
				t.Fatalf("plainName(%q) differs", n)
			}
		}
		pre := []byte("pre")
		if a, b := dashAppendStartTag(pre, name, attrs, selfClose), newDashStartTag(pre, name, attrs, selfClose); !bytes.Equal(a, b) {
			t.Fatalf("dash start tag:\n%q\n%q", a, b)
		}
		if a, b := dashAppendElement(pre, name, attrs, value), AppendElement(pre, name, attrs, value); !bytes.Equal(a, b) {
			t.Fatalf("dash element:\n%q\n%q", a, b)
		}
		if a, b := append(dashAppendName(append(pre, "</"...), name), '>'), AppendEndTag(pre, name); !bytes.Equal(a, b) {
			t.Fatalf("dash end tag:\n%q\n%q", a, b)
		}
		if a, b := dashAppendText(pre, text), AppendText(pre, text); !bytes.Equal(a, b) {
			t.Fatalf("text:\n%q\n%q", a, b)
		}
		// epg writes only names without a prefix (plainName, or the
		// unprefixed channel and programme elements).
		if space == "" {
			se := xml.StartElement{Name: name, Attr: attrs}
			if a, b := epgAppendStartTag(pre, se), newEpgAppendStartTag(pre, se); !bytes.Equal(a, b) {
				t.Fatalf("epg start tag:\n%q\n%q", a, b)
			}
			if a, b := append(epgAppendName(append(pre, "</"...), name), '>'), AppendEndTag(pre, name); !bytes.Equal(a, b) {
				t.Fatalf("epg end tag:\n%q\n%q", a, b)
			}
		}
	})
}
