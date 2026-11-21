// SPDX-License-Identifier: AGPL-3.0-or-later

// Package xmlw writes XML again from the tokens encoding/xml read, for
// the DASH rewriter (spec 3.6) and the XMLTV filter (spec 6.1): untrusted
// documents whose output players read with lighter parsers than
// encoding/xml. Names are written as read (prefix:local) and must have
// passed ValidName; attribute values are escaped in double quotes, text
// is escaped. Which elements and attributes are written is the caller's
// policy.
package xmlw

import "encoding/xml"

// ValidName reports whether a raw name has a prefix (if any) and a local
// part of the form [A-Za-z][A-Za-z0-9._-]*. XML allows more, but parsers
// that skip leading non-letters in names would read "_media" or ":media"
// as media.
func ValidName(n xml.Name) bool {
	return (n.Space == "" || validNCName(n.Space)) && validNCName(n.Local)
}

func validNCName(s string) bool {
	if s == "" || !isLetter(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if c := s[i]; !isLetter(c) && (c < '0' || c > '9') && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// AppendStartTag appends a start tag without its closing '>' or "/>":
// '<', the name and the attributes (see AppendAttr).
func AppendStartTag(dst []byte, name xml.Name, attrs []xml.Attr) []byte {
	dst = appendName(append(dst, '<'), name)
	for _, a := range attrs {
		dst = AppendAttr(dst, a)
	}
	return dst
}

// AppendAttr appends an attribute: a space, the name and the escaped
// value in double quotes.
func AppendAttr(dst []byte, a xml.Attr) []byte {
	dst = appendName(append(dst, ' '), a.Name)
	return append(appendEscaped(append(dst, `="`...), a.Value), '"')
}

// AppendEndTag appends an end tag.
func AppendEndTag(dst []byte, name xml.Name) []byte {
	return append(appendName(append(dst, "</"...), name), '>')
}

// AppendElement appends an element with text content.
func AppendElement(dst []byte, name xml.Name, attrs []xml.Attr, text string) []byte {
	dst = appendEscaped(append(AppendStartTag(dst, name, attrs), '>'), text)
	return AppendEndTag(dst, name)
}

// appendName appends a raw (prefix:local) name.
func appendName(dst []byte, n xml.Name) []byte {
	if n.Space != "" {
		dst = append(append(dst, n.Space...), ':')
	}
	return append(dst, n.Local...)
}

// appendEscaped appends s escaped for XML text and attribute values,
// like xml.EscapeText.
func appendEscaped(dst []byte, s string) []byte {
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

// AppendText appends s escaped as XML text: '&', '<', '>' and CR (which
// encoding/xml does not return, it normalizes line ends to LF).
func AppendText(dst []byte, s []byte) []byte {
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
