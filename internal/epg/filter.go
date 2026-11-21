// SPDX-License-Identifier: AGPL-3.0-or-later

package epg

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/xmlw"
	"golang.org/x/net/html/charset"
)

// maxDepth bounds the element nesting of an XMLTV file; real files nest
// 4 levels (tv, programme, credits, actor).
const maxDepth = 32

// ErrInvalid: the source is no well-formed XMLTV document the filter
// accepts (spec 6.1).
var ErrInvalid = errors.New("epg: invalid XMLTV")

func invalid(reason string) error { return fmt.Errorf("%w: %s", ErrInvalid, reason) }

// Copy is the filtered copy of one EPG source: for each kept channel id
// its <channel> element and its programmes, written again from the
// parsed tokens. It is immutable once Filter returns it.
type Copy struct {
	chans map[string]*entry
	// Read is the number of XML bytes read (after decompression).
	Read int64
	// Size is the number of bytes kept.
	Size int64
	// Programmes is the number of programmes kept.
	Programmes int
}

// entry holds one channel id of a Copy.
type entry struct {
	channel    []byte // the first <channel> element with the id, nil if none
	programmes []byte // its programmes in source order
	count      int    // number of programmes
}

// Channels returns the number of kept channels: ids with a <channel>
// element.
func (c *Copy) Channels() int {
	n := 0
	for _, e := range c.chans {
		if e.channel != nil {
			n++
		}
	}
	return n
}

// Has reports whether the source has a <channel> element with id.
func (c *Copy) Has(id string) bool {
	e := c.chans[id]
	return e != nil && e.channel != nil
}

// MaxToken bounds one token of a source (a tag with its attributes, a
// text, comment, CDATA section or declaration): the parser holds a whole
// token in memory, so without it a gzip of a few hundred KiB could make
// it hold the whole 200 MiB at once. Real tokens are a few KiB.
const MaxToken = 1 << 20

// errTokenTooLarge: a token is larger than MaxToken.
var errTokenTooLarge = fmt.Errorf("%w: a tag, text, comment or declaration larger than 1 MiB", ErrInvalid)

// countReader counts the bytes read through it, and fails once more than
// MaxToken bytes were read since the last token (since is reset after
// each one; the decoder reads ahead at most its buffer of 4 KiB).
type countReader struct {
	r     io.Reader
	n     int64
	since int64
}

func (c *countReader) Read(p []byte) (int, error) {
	if c.since > MaxToken {
		return 0, errTokenTooLarge
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	c.since += int64(n)
	return n, err
}

// Filter reads an XMLTV document (decompressed, see Open) as a stream and
// keeps the <channel> elements whose id is in ids and the <programme>
// elements of those ids that have not ended at now (spec 6.1): a
// programme ends at its stop attribute; one without a stop of the form
// YYYYMMDDhhmm[ss] [+-hhmm] (no zone: UTC) is kept. Only the first
// <channel> element of an id counts. Nothing but these elements and their
// content is kept, and everything kept is written again from the parsed
// tokens: element and attribute names of the form [A-Za-z][A-Za-z0-9._-]*
// without namespace prefix (other elements are dropped with their content,
// other attributes and repeated ones dropped), values and text escaped,
// comments, processing instructions and declarations dropped. The input
// is never held in memory, only what is kept, at most MaxSize bytes.
//
// The whole document must be well-formed XML 1.0 (strict, entities only
// the predefined ones; a declared encoding other than UTF-8 is
// converted) with the single root element tv and a nesting depth of at
// most 32, and no token (tag, text, comment, declaration) larger than
// MaxToken; otherwise Filter fails with ErrInvalid, and with ErrTooLarge
// when the input or the result is larger than MaxSize.
func Filter(r io.Reader, ids map[string]bool, now time.Time) (*Copy, error) {
	return filterLimit(r, ids, now, MaxSize)
}

// filterLimit is Filter with the limit of the kept bytes.
func filterLimit(r io.Reader, ids map[string]bool, now time.Time, limit int64) (*Copy, error) {
	cr := &countReader{r: r}
	f := &filter{
		d:     xml.NewDecoder(cr),
		cr:    cr,
		ids:   ids,
		now:   now,
		limit: limit,
		copy:  &Copy{chans: map[string]*entry{}},
	}
	f.d.Strict = true
	f.d.CharsetReader = charset.NewReaderLabel
	if err := f.run(); err != nil {
		return nil, err
	}
	f.copy.Read = cr.n
	return f.copy, nil
}

// filter is the state of one Filter run.
type filter struct {
	d     *xml.Decoder
	cr    *countReader
	ids   map[string]bool
	now   time.Time
	limit int64 // of the kept bytes
	copy  *Copy
	stack []xml.Name // open elements

	out     *[]byte // where the kept element goes, nil while none is kept
	cur     *entry  // entry of the kept element
	prog    bool    // the kept element is a programme
	skip    int     // depth of a dropped element inside the kept one, 0 if none
	pending bool    // a start tag waits for its '>' or "/>"
	// elemStart is the length of *out when the kept element started.
	elemStart int
}

func (f *filter) run() error {
	rootDone := false
	for first := true; ; first = false {
		t, err := f.d.RawToken()
		f.cr.since = 0
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return syntaxError(err)
		}
		switch t := t.(type) {
		case xml.StartElement:
			depth := len(f.stack) + 1
			switch {
			case rootDone:
				return invalid("content after the root element")
			case depth > maxDepth:
				return invalid("elements nested too deep")
			case depth == 1 && (t.Name.Space != "" || t.Name.Local != "tv"):
				return invalid("root element is not tv")
			}
			f.stack = append(f.stack, t.Name)
			if err := f.start(t, depth); err != nil {
				return err
			}
		case xml.EndElement:
			depth := len(f.stack)
			if depth == 0 || f.stack[depth-1] != t.Name {
				return invalid("mismatched end tag")
			}
			f.stack = f.stack[:depth-1]
			rootDone = depth == 1
			if err := f.end(t, depth); err != nil {
				return err
			}
		case xml.CharData:
			if len(f.stack) == 0 {
				text := []byte(t)
				if first {
					text = bytes.TrimPrefix(text, []byte("\ufeff"))
				}
				if len(bytes.Trim(text, " \t\r\n")) > 0 {
					return invalid("text outside the root element")
				}
				continue
			}
			if f.out != nil && f.skip == 0 && len(t) > 0 { // an empty CDATA section is no content
				f.closeStart()
				*f.out = xmlw.AppendText(*f.out, t)
				if err := f.checkSize(); err != nil {
					return err
				}
			}
		}
		// Comments, processing instructions and declarations (a DOCTYPE)
		// are dropped; the decoder has checked their syntax and the XML
		// declaration.
	}
	if !rootDone {
		return invalid("no complete tv element")
	}
	return nil
}

// syntaxError maps a decoder error: ErrTooLarge passes, syntax errors
// keep only their line (the message may quote the input).
func syntaxError(err error) error {
	if errors.Is(err, ErrTooLarge) {
		return ErrTooLarge
	}
	var se *xml.SyntaxError
	if errors.As(err, &se) {
		return fmt.Errorf("%w: XML syntax error in line %d", ErrInvalid, se.Line)
	}
	if strings.HasPrefix(err.Error(), "xml: ") {
		// Version and encoding errors of the XML declaration.
		return invalid("unsupported XML version or encoding")
	}
	return err
}

// start handles a start tag at depth (1 is the root).
func (f *filter) start(t xml.StartElement, depth int) error {
	switch {
	case depth == 2:
		f.keep(t)
		if f.out == nil {
			return nil
		}
		f.elemStart = len(*f.out)
	case f.out == nil || f.skip > 0:
		return nil
	case !plainName(t.Name):
		f.skip = depth
		return nil
	}
	f.closeStart()
	*f.out = appendStartTag(*f.out, t)
	f.pending = true
	return f.checkSize()
}

// keep decides whether a child element of tv is kept and sets out.
func (f *filter) keep(t xml.StartElement) {
	if t.Name.Space != "" {
		return
	}
	switch t.Name.Local {
	case "channel":
		id, _ := attr(t, "id")
		if !f.ids[id] {
			return
		}
		e := f.entry(id)
		if e.channel != nil {
			return // only the first element of an id counts
		}
		e.channel = []byte{}
		f.out, f.cur, f.prog = &e.channel, e, false
	case "programme":
		id, _ := attr(t, "channel")
		if !f.ids[id] {
			return
		}
		if stop, ok := attr(t, "stop"); ok {
			if end, ok := ParseTime(stop); ok && !end.After(f.now) {
				return // ended
			}
		}
		e := f.entry(id)
		f.out, f.cur, f.prog = &e.programmes, e, true
	}
}

// entry returns the entry of id, adding it.
func (f *filter) entry(id string) *entry {
	e := f.copy.chans[id]
	if e == nil {
		e = &entry{}
		f.copy.chans[id] = e
	}
	return e
}

// end handles an end tag at depth (the depth of its start tag).
func (f *filter) end(t xml.EndElement, depth int) error {
	if f.out == nil {
		return nil
	}
	switch {
	case f.skip == depth:
		f.skip = 0
		return nil
	case f.skip > 0:
		return nil
	}
	if f.pending {
		*f.out = append(*f.out, "/>"...)
		f.pending = false
	} else {
		*f.out = xmlw.AppendEndTag(*f.out, t.Name)
	}
	if depth == 2 {
		*f.out = append(*f.out, '\n')
		f.copy.Size += int64(len(*f.out) - f.elemStart)
		if f.prog {
			f.cur.count++
			f.copy.Programmes++
		}
		f.out, f.cur = nil, nil
	}
	return f.checkSize()
}

// closeStart ends a pending start tag with '>'.
func (f *filter) closeStart() {
	if f.pending {
		*f.out = append(*f.out, '>')
		f.pending = false
	}
}

// checkSize fails once the kept bytes, the element being written
// included, exceed MaxSize: escaping can make an element longer than its
// input.
func (f *filter) checkSize() error {
	n := f.copy.Size
	if f.out != nil {
		n += int64(len(*f.out) - f.elemStart)
	}
	if n > f.limit {
		return ErrTooLarge
	}
	return nil
}

// attr returns the value of the first attribute named name (no prefix).
func attr(t xml.StartElement, name string) (string, bool) {
	for _, a := range t.Attr {
		if a.Name.Space == "" && a.Name.Local == name {
			return a.Value, true
		}
	}
	return "", false
}

// plainName reports whether n has no prefix and a local name of the form
// [A-Za-z][A-Za-z0-9._-]* (xmlw.ValidName). XML allows more, but players'
// lighter parsers read other names differently.
func plainName(n xml.Name) bool { return n.Space == "" && xmlw.ValidName(n) }

// appendStartTag appends a start tag without its closing '>': the name
// and the attributes with plain names, the first of each name only,
// values escaped in double quotes.
func appendStartTag(dst []byte, t xml.StartElement) []byte {
	dst = xmlw.AppendStartTag(dst, t.Name, nil)
	for i, a := range t.Attr {
		if !plainName(a.Name) || repeated(t.Attr[:i], a.Name) {
			continue
		}
		dst = xmlw.AppendAttr(dst, a)
	}
	return dst
}

// repeated reports whether attrs has an attribute named n.
func repeated(attrs []xml.Attr, n xml.Name) bool {
	for _, a := range attrs {
		if a.Name == n {
			return true
		}
	}
	return false
}

// ParseTime parses an XMLTV time of the form YYYYMMDDhhmm or
// YYYYMMDDhhmmss, optionally followed by a zone offset +hhmm or -hhmm
// (with or without a space before it). Without a zone the time is UTC
// (XMLTV DTD). Other forms (shorter dates, zone names) give ok false.
func ParseTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	if n != 12 && n != 14 {
		return time.Time{}, false
	}
	digits, zone := s[:n], strings.TrimLeft(s[n:], " ")
	if n == 12 {
		digits += "00"
	}
	loc := time.UTC
	if zone != "" {
		if len(zone) != 5 || zone[0] != '+' && zone[0] != '-' {
			return time.Time{}, false
		}
		hh, ok1 := twoDigits(zone[1:3])
		mm, ok2 := twoDigits(zone[3:5])
		if !ok1 || !ok2 || hh > 23 || mm > 59 {
			return time.Time{}, false
		}
		off := hh*3600 + mm*60
		if zone[0] == '-' {
			off = -off
		}
		loc = time.FixedZone("", off)
	}
	t, err := time.ParseInLocation("20060102150405", digits, loc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func twoDigits(s string) (int, bool) {
	if s[0] < '0' || s[0] > '9' || s[1] < '0' || s[1] > '9' {
		return 0, false
	}
	return int(s[0]-'0')*10 + int(s[1]-'0'), true
}
