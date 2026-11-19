// SPDX-License-Identifier: AGPL-3.0-or-later

// Package dash rewrites MPEG-DASH manifests (MPD) for the proxy (spec
// 3.6): every URL a player would fetch from the manifest goes through a
// prefix-signed proxy URL (spec 4.1), DRM manifests are refused, and
// everything else is written again from what encoding/xml parsed, without
// comments. It does no I/O; fetching, size limits on reads and signing
// belong to the caller.
package dash

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
)

const (
	// MaxSize is the largest manifest Rewrite accepts: the upper bound of
	// [upstream] max_playlist_size (spec 3.4, 5.3).
	MaxSize = 32 << 20
	// maxDepth bounds the element nesting; MPDs nest about 6 levels.
	maxDepth = 32
)

var (
	// ErrTooLarge: the manifest exceeds MaxSize.
	ErrTooLarge = errors.New("dash: manifest too large")
	// ErrInvalidFinalURL: the final URL is not an absolute http(s) URL.
	ErrInvalidFinalURL = errors.New("dash: final URL is not an absolute http(s) URL")
	// ErrDRM: the manifest has a ContentProtection element. DRM is out of
	// scope (spec 1.2); the manifest is refused, never passed through.
	ErrDRM = errors.New("dash: manifest with ContentProtection (DRM) refused")
	// ErrInvalidManifest: the manifest is not a well-formed MPD, or it
	// has a URL the rewriter cannot route through the proxy, or players
	// could read it differently from the rewriter. Passing it through
	// could let the player fetch an upstream URL directly, so it is
	// rejected as a whole. Its messages never contain URLs.
	ErrInvalidManifest = errors.New("dash: manifest rejected")
)

// Options configure Rewrite.
type Options struct {
	// Final is the manifest's URL after redirects: relative references
	// resolve against it. Required.
	Final *url.URL
	// PrefixURL returns the absolute proxy URL, ending in '/', of an
	// upstream prefix: a canonical "scheme://host[:port]/dir/" (spec
	// 4.1). query allows a query on requests below it. Required.
	PrefixURL func(prefix string, query bool) (string, error)
	// TimeURL is the proxy URL that answers the time in ISO 8601 (spec
	// 7.1, /time). UTCTiming elements other than direct ones point to
	// it; empty removes them.
	TimeURL string
}

// Scheme URIs the rewriter knows (ISO/IEC 23009-1, DASH-IF IOP).
const (
	schemeHTTPISO = "urn:mpeg:dash:utc:http-iso:2014"
	resolveToZero = "urn:mpeg:dash:resolve-to-zero:2013"
	// schemeFontDownload is the DVB font download descriptor; players
	// fetch its dvb:url themselves.
	schemeFontDownload = "urn:dvb:dash:fontdownload:2014"
	// xmlDecl is the declaration written for a manifest that had one.
	xmlDecl = `<?xml version="1.0" encoding="UTF-8"?>`
)

// Scheme URIs are compared case-insensitively: the maps below hold
// lower-case keys, see scheme.

// keptEvents are the EventStream and InbandEventStream schemes that are
// kept (lower-case); a scheme ending in ':' is a prefix. Their events
// carry no URL a player fetches. Every other event stream is removed:
// callback events (urn:mpeg:dash:event:callback:2015) make the player
// call a URL, alternative MPD events load another manifest.
var keptEvents = []string{
	"urn:scte:scte35:",             // SCTE-35 splice signals
	"https://aomedia.org/emsg/id3", // ID3 timed metadata
	"https://developer.apple.com/streaming/emsg-id3",
}

// mpdEvents is the MPD event scheme; only its value 1 (the manifest
// expires, reload it from the proxy URL) is kept. Values 2 and 3 carry an
// MPD patch or a whole manifest.
const mpdEvents = "urn:mpeg:dash:event:2012"

// baseURLAttrs are the BaseURL attributes that are kept (lower-case
// local names): the schema's, without byteRange (Annex E byte range
// templates build URLs), and DVB's priority and weight. Namespace
// declarations are kept too.
var baseURLAttrs = map[string]bool{
	"servicelocation":          true,
	"availabilitytimeoffset":   true,
	"availabilitytimecomplete": true,
	"timeshiftbufferdepth":     true,
	"rangeaccess":              true,
	"priority":                 true,
	"weight":                   true,
}

// directTiming are UTCTiming schemes whose value is the time itself: the
// player contacts nobody.
var directTiming = map[string]bool{
	"urn:mpeg:dash:utc:direct:2014": true,
	"urn:mpeg:dash:utc:direct:2012": true,
}

// chainingSchemes are properties whose value is the URL of another
// manifest the player loads (MPD chaining and fallback).
var chainingSchemes = map[string]bool{
	"urn:mpeg:dash:chaining:2016": true,
	"urn:mpeg:dash:fallback:2016": true,
}

// removedElements are removed with their content (lower-case local
// names): manifest reload and patch URLs (the player reloads the proxy
// URL it has), content steering servers and metrics reporting URLs.
var removedElements = map[string]bool{
	"location":        true,
	"patchlocation":   true,
	"contentsteering": true,
	"metrics":         true,
}

// refAttrs maps elements (lower-case local names) to their attributes
// with segment references (lower-case local names).
var refAttrs = map[string][]string{
	"segmenttemplate":     {"media", "initialization", "index", "bitstreamswitching"},
	"segmenturl":          {"media", "index"},
	"initialization":      {"sourceurl"},
	"representationindex": {"sourceurl"},
	"bitstreamswitching":  {"sourceurl"},
}

// Rewrite rewrites a DASH manifest (spec 3.6):
//
//   - every BaseURL, on any level, is resolved (relative ones against
//     the first BaseURL of the level above, or Final) and replaced with
//     the prefix URL of its directory plus its file name and query; an
//     MPD without its own BaseURL gets one for Final's directory, as the
//     first child of the MPD element;
//   - segment references (SegmentTemplate media, initialization, index,
//     bitstreamSwitching; SegmentURL media, index; sourceURL) stay as
//     they are if they are plain relative references, which the player
//     resolves against the rewritten BaseURLs. Absolute, scheme-relative,
//     rooted and dot-segment references become the prefix URL of their
//     fixed directory plus the rest, template identifiers kept;
//   - the dvb:url of DVB font download descriptors becomes a prefix URL
//     like a reference that is not plain;
//   - Location, PatchLocation, ContentSteering, Metrics,
//     SupplementalProperty elements of MPD chaining or fallback, and
//     EventStream and InbandEventStream elements of schemes other than
//     those in keptEvents are removed; BaseURL attributes other than
//     baseURLAttrs are dropped; UTCTiming elements other than direct
//     ones point to TimeURL;
//   - every tag and text is written again from the tokens encoding/xml
//     read (names as read, attribute values escaped in double quotes,
//     text escaped), comments are dropped and the XML declaration is
//     written as xmlDecl: players' lighter parsers disagree with
//     encoding/xml on where comments and processing instructions end and
//     on attribute names, so the output holds only what was parsed.
//
// The output is at most maxOutput(len(mpd)) bytes, else ErrTooLarge.
//
// Prefix URLs allow a query if the URL they stand for has one, and the
// BaseURL ones also if a plain segment reference has one.
//
// Fail closed: a ContentProtection element gives ErrDRM; input larger
// than MaxSize gives ErrTooLarge; anything players could read
// differently, fetch past the proxy, or that does not resolve to an
// http(s) URL gives ErrInvalidManifest (see checkManifest). mpd is not
// modified.
func Rewrite(mpd []byte, opts Options) ([]byte, error) {
	if len(mpd) > MaxSize {
		return nil, ErrTooLarge
	}
	f := opts.Final
	if f == nil || f.Scheme != "http" && f.Scheme != "https" || f.Host == "" || opts.PrefixURL == nil {
		return nil, ErrInvalidFinalURL
	}
	sc, err := checkManifest(mpd)
	if err != nil {
		return nil, err
	}
	rw := &rewriter{opts: opts, scan: sc, prefixes: map[string]string{}, out: make([]byte, 0, len(mpd)+len(mpd)/4+1024)}
	if err := rw.run(mpd); err != nil {
		return nil, err
	}
	return rw.out, nil
}

// invalid returns an ErrInvalidManifest error with a fixed reason.
func invalid(reason string) error {
	return fmt.Errorf("%w: %s", ErrInvalidManifest, reason)
}

// tokenizer returns the raw tokens of a document with their input
// bytes. Self-closing tags give a StartElement with all their bytes and
// an EndElement without bytes.
type tokenizer struct {
	d   *xml.Decoder
	in  []byte
	off int
}

func newTokenizer(in []byte) *tokenizer {
	d := xml.NewDecoder(bytes.NewReader(in))
	d.Strict = true
	return &tokenizer{d: d, in: in}
}

// next returns the next token, its input bytes and its start offset;
// io.EOF at the end. Syntax errors wrap ErrInvalidManifest without the
// decoder's message, which may quote the input.
func (z *tokenizer) next() (xml.Token, []byte, int, error) {
	t, err := z.d.RawToken()
	if err == io.EOF {
		return nil, nil, z.off, io.EOF
	}
	if err != nil {
		var se *xml.SyntaxError
		if errors.As(err, &se) {
			return nil, nil, z.off, fmt.Errorf("%w: XML syntax error in line %d", ErrInvalidManifest, se.Line)
		}
		return nil, nil, z.off, invalid("not a well-formed UTF-8 XML 1.0 document")
	}
	start, end := z.off, int(z.d.InputOffset())
	z.off = end
	return t, z.in[start:end], start, nil
}

// scan is what Rewrite needs to know about the whole manifest before it
// writes the first byte.
type scan struct {
	// bases holds the BaseURL texts of each element that has BaseURL
	// children, by the start offset of the element: references resolve
	// against the bases of their level wherever its BaseURL elements
	// stand (in schema order, descriptors come first).
	bases map[int][]string
	// root is the start offset of the MPD element.
	root int
	// query: a plain segment reference has a query, so the BaseURL
	// prefixes must allow one.
	query bool
}

// checkManifest checks a whole manifest before anything is rewritten. It
// fails with ErrDRM for a ContentProtection element anywhere, and with
// ErrInvalidManifest for:
//   - XML that is not well-formed (also mismatched end tags, which the
//     raw tokenizer leaves to the caller), not UTF-8 or not XML 1.0, a
//     DOCTYPE or other declaration (no entities), text or elements
//     outside the single root element, nesting deeper than maxDepth;
//   - a root element other than MPD, or an empty one;
//   - duplicate attributes, also an interpreted attribute twice with
//     different case or namespace prefix; xml:base;
//   - remote elements (any href attribute, xlink:href in practice) other
//     than resolve-to-zero;
//   - a processing instruction other than the XML declaration as the
//     first token (after a BOM), a CDATA section outside a BaseURL, '<'
//     or '>' inside a start tag (in a quoted value): parsers that end
//     these at the first '>' would read elements the rewriter did not;
//   - a BaseURL with child elements;
//   - a segment reference, font URL or BaseURL with whitespace (other
//     than around it), control bytes, non-ASCII bytes or a backslash;
//   - a Representation id with whitespace, control or non-ASCII bytes,
//     ':' or '\', or starting with '/': $RepresentationID$ must not turn
//     a relative reference into an absolute one;
//   - an EssentialProperty of MPD chaining or fallback;
//   - an element or attribute name (prefix or local part) that is not
//     [A-Za-z][A-Za-z0-9._-]*: parsers that skip leading non-letters in
//     names would read "_media" as a second media attribute.
//
// Scheme URIs compare case-insensitively.
//
// Element and attribute names match by local name, case-insensitively,
// whatever their namespace: some players compare names that way.
func checkManifest(in []byte) (scan, error) {
	sc := scan{bases: map[int][]string{}}
	z := newTokenizer(in)
	var stack []xml.Name
	var starts []int // start offsets of the open elements
	var baseText []byte
	rootDone := false
	inBase := 0 // depth of the open BaseURL element, 0 if none
	for n := 0; ; n++ {
		t, raw, start, err := z.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return sc, err
		}
		if n == 0 && start == 0 && bytes.Equal(raw, []byte("\ufeff")) {
			n-- // the BOM does not count: the declaration may follow it
			continue
		}
		switch t := t.(type) {
		case xml.StartElement:
			depth := len(stack) + 1
			ln := strings.ToLower(t.Name.Local)
			switch {
			case bytes.IndexByte(raw[1:], '<') >= 0 || bytes.IndexByte(raw, '>') != len(raw)-1:
				// Legal in quoted values, but parsers that end a tag
				// at the first '>' would read another element there.
				return sc, invalid("'<' or '>' inside a tag")
			case rootDone:
				return sc, invalid("content after the root element")
			case inBase > 0:
				return sc, invalid("element inside BaseURL")
			case depth > maxDepth:
				return sc, invalid("elements nested too deep")
			case depth == 1 && ln != "mpd":
				return sc, invalid("root element is not MPD")
			case depth == 1 && selfClosing(raw):
				return sc, invalid("empty MPD element")
			case ln == "contentprotection":
				return sc, ErrDRM
			case !validName(t.Name):
				return sc, invalid("element name that is not a plain XML name")
			}
			if err := checkAttrs(ln, t.Attr, &sc); err != nil {
				return sc, err
			}
			if ln == "baseurl" {
				inBase = depth
				baseText = baseText[:0]
			}
			if depth == 1 {
				sc.root = start
			}
			stack = append(stack, t.Name)
			starts = append(starts, start)
		case xml.EndElement:
			if len(stack) == 0 || stack[len(stack)-1] != t.Name {
				return sc, invalid("mismatched end tag")
			}
			if inBase == len(stack) {
				inBase = 0
				parent := starts[len(starts)-2]
				sc.bases[parent] = append(sc.bases[parent], string(baseText))
			}
			stack = stack[:len(stack)-1]
			starts = starts[:len(starts)-1]
			rootDone = len(stack) == 0
		case xml.CharData:
			if inBase == 0 && bytes.HasPrefix(raw, []byte("<![CDATA[")) {
				return sc, invalid("CDATA section outside BaseURL")
			}
			if inBase > 0 {
				baseText = append(baseText, t...)
			}
			text := []byte(t)
			if start == 0 {
				text = bytes.TrimPrefix(text, []byte("\ufeff"))
			}
			if len(stack) == 0 && len(bytes.Trim(text, " \t\r\n")) > 0 {
				return sc, invalid("text outside the root element")
			}
		case xml.ProcInst:
			// Parsers end processing instructions at different places
			// (some at the first '>'); only the declaration is allowed,
			// and it is written again (see run).
			if n != 0 || t.Target != "xml" {
				return sc, invalid("processing instruction other than the XML declaration")
			}
		case xml.Directive:
			return sc, invalid("DOCTYPE or other declaration")
		}
	}
	if !rootDone {
		return sc, invalid("no complete MPD element")
	}
	return sc, nil
}

// interpreted returns the attributes (lower-case local names) the
// rewriter reads on an element.
func interpreted(ln string) []string {
	switch ln {
	case "utctiming":
		return []string{"schemeiduri", "value"}
	case "supplementalproperty", "essentialproperty":
		return []string{"schemeiduri", "url"}
	case "eventstream", "inbandeventstream":
		return []string{"schemeiduri", "value"}
	case "representation":
		return []string{"id"}
	}
	return refAttrs[ln]
}

// checkAttrs checks the attributes of an element (see checkManifest) and
// notes plain segment references with a query in sc.
func checkAttrs(ln string, attrs []xml.Attr, sc *scan) error {
	if hasDuplicate(attrs) {
		return invalid("duplicate attribute")
	}
	interp := interpreted(ln)
	var seen []string // interpreted attributes so far, lower-case
	for _, a := range attrs {
		if !validName(a.Name) {
			return invalid("attribute name that is not a plain XML name")
		}
		an := strings.ToLower(a.Name.Local)
		if an == "href" || containsString(interp, an) {
			if containsString(seen, an) {
				return invalid("duplicate attribute")
			}
			seen = append(seen, an)
		}
		switch {
		case a.Name.Space == "xml" && an == "base":
			return invalid("xml:base")
		case an == "href" && strings.TrimSpace(a.Value) != resolveToZero:
			return invalid("remote element (xlink:href)")
		case ln == "representation" && an == "id" && !validRepresentationID(a.Value):
			return invalid("Representation id that could change a URL")
		case ln == "essentialproperty" && an == "schemeiduri" && chainingSchemes[lowerTrim(a.Value)]:
			return invalid("EssentialProperty of MPD chaining")
		}
		if isRefAttr(ln, attrs, an) {
			v, err := cleanURL(a.Value)
			if err != nil {
				return err
			}
			if plain(v) && strings.IndexByte(v, '?') >= 0 {
				sc.query = true
			}
		}
	}
	return nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// validName reports whether a raw name has a prefix (if any) and a local
// part of the form [A-Za-z][A-Za-z0-9._-]*. XML allows more, but parsers
// that skip leading non-letters in names would read "_media" or ":media"
// as media.
func validName(n xml.Name) bool {
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

// lowerTrim returns a scheme URI for comparison: trimmed and lower-case.
func lowerTrim(v string) string { return strings.ToLower(strings.TrimSpace(v)) }

// fontDownload reports whether a property element is a DVB font
// download descriptor.
func fontDownload(ln string, attrs []xml.Attr) bool {
	if ln != "supplementalproperty" && ln != "essentialproperty" {
		return false
	}
	v, _ := attr(attrs, "schemeiduri")
	return lowerTrim(v) == schemeFontDownload
}

// isRefAttr reports whether attribute an (lower-case local name) of an
// element holds a URL the player fetches: a segment reference or the
// dvb:url of a font download descriptor.
func isRefAttr(ln string, attrs []xml.Attr, an string) bool {
	return containsString(refAttrs[ln], an) || an == "url" && fontDownload(ln, attrs)
}

// keptEvent reports whether an EventStream or InbandEventStream is kept
// (see keptEvents).
func keptEvent(attrs []xml.Attr) bool {
	s, _ := attr(attrs, "schemeiduri")
	s = lowerTrim(s)
	if s == mpdEvents {
		v, _ := attr(attrs, "value")
		return strings.TrimSpace(v) == "1"
	}
	for _, k := range keptEvents {
		if s == k || strings.HasSuffix(k, ":") && strings.HasPrefix(s, k) {
			return true
		}
	}
	return false
}

// validRepresentationID reports whether $RepresentationID$ can be
// replaced by id without changing the scheme or authority of a relative
// reference. Non-ASCII bytes are refused: some players trim Unicode
// whitespace such as NBSP.
func validRepresentationID(id string) bool {
	if strings.HasPrefix(id, "/") {
		return false
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; c <= ' ' || c >= 0x7f || c == ':' || c == '\\' {
			return false
		}
	}
	return true
}

// cleanURL trims XML whitespace around a URL value and rejects values
// with whitespace, control bytes, backslashes or non-ASCII bytes inside:
// URL parsers differ on those (browsers drop tabs and newlines and read
// '\' as '/'; JavaScript's trim also drops NBSP and other Unicode
// whitespace).
func cleanURL(v string) (string, error) {
	v = strings.Trim(v, " \t\r\n")
	for i := 0; i < len(v); i++ {
		if c := v[i]; c <= ' ' || c >= 0x7f || c == '\\' {
			return "", invalid("URL with whitespace, control, non-ASCII byte or backslash")
		}
	}
	return v, nil
}

// plain reports whether a URL value is a relative reference the player
// can resolve against a rewritten base and stay below it: not starting
// with '/', no ':' in its first segment (no scheme, also not after
// template substitution), no "." or ".." segments.
func plain(v string) bool {
	if strings.HasPrefix(v, "/") {
		return false
	}
	path := v
	if i := strings.IndexAny(v, "?#"); i >= 0 {
		path = v[:i]
	}
	first, _, _ := strings.Cut(path, "/")
	if strings.IndexByte(first, ':') >= 0 {
		return false
	}
	for seg := range strings.SplitSeq(path, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// frame is an open element during the rewrite.
type frame struct {
	name xml.Name
	// inherited are the upstream bases of the parent; own the resolved
	// BaseURL children (all of them, known from checkManifest).
	inherited, own []*url.URL
	// ownURLs are the rewritten values of the BaseURL children, in
	// order; next is the index of the next BaseURL child.
	ownURLs []string
	next    int
}

// bases returns the bases the children of f resolve against.
func (f *frame) bases() []*url.URL {
	if len(f.own) > 0 {
		return f.own
	}
	return f.inherited
}

// baseURL is the BaseURL element being read.
type baseURL struct {
	name  xml.Name
	attrs []xml.Attr
}

type rewriter struct {
	opts     Options
	scan     scan
	out      []byte
	stack    []*frame
	base     *baseURL          // open BaseURL element, nil if none
	skip     int               // depth inside a removed element
	prefixes map[string]string // mode + prefix -> prefix URL
}

// run rewrites a manifest that passed checkManifest.
func (rw *rewriter) run(in []byte) error {
	z := newTokenizer(in)
	limit := maxOutput(len(in))
	for {
		if len(rw.out) > limit {
			return fmt.Errorf("%w: rewritten manifest larger than %d bytes", ErrTooLarge, limit)
		}
		t, raw, start, err := z.next()
		if err == io.EOF {
			if len(rw.out) > limit {
				return fmt.Errorf("%w: rewritten manifest larger than %d bytes", ErrTooLarge, limit)
			}
			return nil
		}
		if err != nil {
			return err
		}
		if rw.skip > 0 {
			switch t.(type) {
			case xml.StartElement:
				rw.skip++
			case xml.EndElement:
				rw.skip--
			}
			continue
		}
		// Every token is written again from what encoding/xml parsed
		// (names checked, values escaped, double quotes), so the output
		// holds nothing a lighter parser could read differently.
		switch t := t.(type) {
		case xml.StartElement:
			if err := rw.start(t, raw, start); err != nil {
				return err
			}
		case xml.EndElement:
			if rw.base != nil {
				rw.endBase()
				continue
			}
			rw.stack = rw.stack[:len(rw.stack)-1]
			if len(raw) > 0 { // else written as part of "<x/>"
				rw.out = append(appendName(append(rw.out, "</"...), t.Name), '>')
			}
		case xml.CharData:
			if rw.base != nil {
				continue // the BaseURL is written from checkManifest's text
			}
			rw.out = appendText(rw.out, t)
		case xml.ProcInst:
			// The declaration (checkManifest allows no other processing
			// instruction), written again: its original bytes could
			// hide a tag from parsers that end it at the first '>'.
			rw.out = append(rw.out, xmlDecl...)
		}
		// Comments are dropped: parsers disagree on where they end
		// ("<!--->" ends at once for some), so a comment could hide an
		// element from the rewriter but not from the player.
	}
}

// maxOutput is the largest rewritten manifest for an input of n bytes:
// 16 times the input, at least 1 MiB and at most 64 MiB. Every reference
// that is not plain becomes a whole prefix URL, so without a limit a
// small manifest could grow by orders of magnitude.
func maxOutput(n int) int {
	return min(max(16*n, 1<<20), 64<<20)
}

// start handles a start tag.
func (rw *rewriter) start(t xml.StartElement, raw []byte, start int) error {
	ln := strings.ToLower(t.Name.Local)
	if rw.removed(ln, t.Attr) {
		rw.skip = 1
		return nil
	}
	if ln == "baseurl" {
		rw.base = &baseURL{name: t.Name, attrs: t.Attr}
		return nil
	}
	f := &frame{name: t.Name}
	root := len(rw.stack) == 0
	if root {
		f.inherited = []*url.URL{rw.opts.Final}
	} else {
		f.inherited = rw.stack[len(rw.stack)-1].bases()
	}
	// The BaseURL children are resolved first: the attributes of this
	// element and of all its descendants resolve against them.
	for _, text := range rw.scan.bases[start] {
		u, p, err := rw.baseURL(f.inherited[0], text)
		if err != nil {
			return err
		}
		f.own = append(f.own, u)
		f.ownURLs = append(f.ownURLs, p)
	}
	attrs, _, err := rw.rewriteAttrs(ln, t.Attr, f.inherited[0])
	if err != nil {
		return err
	}
	rw.out = appendStartTag(rw.out, t.Name, attrs, selfClosing(raw))
	rw.stack = append(rw.stack, f)
	if root && len(f.own) == 0 {
		// Without a BaseURL, players resolve against the proxy URL of
		// the manifest; give them its upstream directory.
		u, err := resolve(rw.opts.Final, "")
		if err != nil {
			return err
		}
		prefix, _ := splitURL(u)
		p, err := rw.prefixURL(prefix, rw.scan.query)
		if err != nil {
			return err
		}
		dir, err := url.Parse(prefix)
		if err != nil {
			return invalid("manifest URL")
		}
		f.own = []*url.URL{dir}
		rw.out = appendElement(rw.out, xml.Name{Space: t.Name.Space, Local: "BaseURL"}, nil, p)
	}
	return nil
}

// removed reports whether an element is dropped with its content.
func (rw *rewriter) removed(ln string, attrs []xml.Attr) bool {
	switch ln {
	case "supplementalproperty":
		v, _ := attr(attrs, "schemeiduri")
		return chainingSchemes[lowerTrim(v)]
	case "utctiming":
		v, _ := attr(attrs, "schemeiduri")
		return rw.opts.TimeURL == "" && !directTiming[lowerTrim(v)]
	case "eventstream", "inbandeventstream":
		return !keptEvent(attrs)
	}
	return removedElements[ln]
}

// attr returns the value of the attribute with lower-case local name an.
func attr(attrs []xml.Attr, an string) (string, bool) {
	for _, a := range attrs {
		if strings.EqualFold(a.Name.Local, an) {
			return a.Value, true
		}
	}
	return "", false
}

// rewriteAttrs returns the attributes of an element with its segment
// references or UTCTiming rewritten; changed is false if nothing changed.
// References resolve against base.
func (rw *rewriter) rewriteAttrs(ln string, attrs []xml.Attr, base *url.URL) ([]xml.Attr, bool, error) {
	if ln == "utctiming" {
		if v, _ := attr(attrs, "schemeiduri"); directTiming[lowerTrim(v)] {
			return attrs, false, nil
		}
		out := make([]xml.Attr, 0, len(attrs)+2)
		hasScheme, hasValue := false, false
		for _, a := range attrs {
			switch strings.ToLower(a.Name.Local) {
			case "schemeiduri":
				a.Value, hasScheme = schemeHTTPISO, true
			case "value":
				a.Value, hasValue = rw.opts.TimeURL, true
			}
			out = append(out, a)
		}
		if !hasScheme {
			out = append(out, xml.Attr{Name: xml.Name{Local: "schemeIdUri"}, Value: schemeHTTPISO})
		}
		if !hasValue {
			out = append(out, xml.Attr{Name: xml.Name{Local: "value"}, Value: rw.opts.TimeURL})
		}
		return out, true, nil
	}
	font := fontDownload(ln, attrs)
	if refAttrs[ln] == nil && !font {
		return attrs, false, nil
	}
	var out []xml.Attr
	for i, a := range attrs {
		if !isRefAttr(ln, attrs, strings.ToLower(a.Name.Local)) {
			continue
		}
		v, err := cleanURL(a.Value)
		if err != nil {
			return nil, false, err
		}
		// A font URL may be relative to the BaseURL or not (DVB A168
		// leaves it open to players): always make it absolute.
		if plain(v) && !font {
			continue
		}
		nv, err := rw.reference(v, base)
		if err != nil {
			return nil, false, err
		}
		if out == nil {
			out = append([]xml.Attr(nil), attrs...)
		}
		out[i].Value = nv
	}
	if out == nil {
		return attrs, false, nil
	}
	return out, true, nil
}

// reference rewrites a segment reference that is not plain: its fixed
// directory (up to the last '/' before any '$', '?' or '#') resolved
// against base becomes a prefix URL, the rest is kept as it is, template
// identifiers included. The query mode follows the rest.
func (rw *rewriter) reference(v string, base *url.URL) (string, error) {
	if s := uriScheme(v); s != "" && s != "http" && s != "https" {
		return "", invalid("reference with a scheme other than http(s)")
	}
	limit := len(v)
	if i := strings.IndexAny(v, "$?#"); i >= 0 {
		limit = i
	}
	slash := strings.LastIndexByte(v[:limit], '/')
	head, tail := v[:slash+1], v[slash+1:]
	if i := authorityStart(v); i >= 0 {
		// The directory must include the whole authority.
		if j := strings.IndexByte(v[i:], '/'); j < 0 || i+j > slash {
			return "", invalid("reference without a fixed host and path")
		}
	}

	u, err := resolve(base, head+"x")
	if err != nil {
		return "", err
	}
	prefix, file := splitURL(u)
	if file != "x" {
		return "", invalid("reference directory")
	}
	p, err := rw.prefixURL(prefix, strings.IndexByte(tail, '?') >= 0)
	if err != nil {
		return "", err
	}
	return p + tail, nil
}

// authorityStart returns the index after "//" if v is an absolute or
// scheme-relative URL with an authority, else -1.
func authorityStart(v string) int {
	if s := uriScheme(v); s != "" {
		if strings.HasPrefix(v[len(s)+1:], "//") {
			return len(s) + 3
		}
		return -1
	}
	if strings.HasPrefix(v, "//") {
		return 2
	}
	return -1
}

// endBase writes the BaseURL element that just ended, with the value its
// parent resolved at its start.
func (rw *rewriter) endBase() {
	b := rw.base
	rw.base = nil
	parent := rw.stack[len(rw.stack)-1]
	p := parent.ownURLs[parent.next]
	parent.next++
	rw.out = appendElement(rw.out, b.name, keptBaseURLAttrs(b.attrs), p)
}

// baseURL resolves the text of a BaseURL against base and returns the
// upstream URL and its rewritten value: the prefix URL of its directory
// plus its file name and query.
func (rw *rewriter) baseURL(base *url.URL, text string) (*url.URL, string, error) {
	v, err := cleanURL(text)
	if err != nil {
		return nil, "", err
	}
	u, err := resolve(base, v)
	if err != nil {
		return nil, "", err
	}
	prefix, file := splitURL(u)
	p, err := rw.prefixURL(prefix, rw.scan.query || strings.IndexByte(file, '?') >= 0)
	if err != nil {
		return nil, "", err
	}
	return u, p + file, nil
}

// keptBaseURLAttrs returns the attributes of a BaseURL without those
// not in baseURLAttrs; namespace declarations stay.
func keptBaseURLAttrs(attrs []xml.Attr) []xml.Attr {
	var out []xml.Attr
	for _, a := range attrs {
		if a.Name.Space == "xmlns" || a.Name.Space == "" && a.Name.Local == "xmlns" || baseURLAttrs[strings.ToLower(a.Name.Local)] {
			out = append(out, a)
		}
	}
	return out
}

// prefixURL returns the proxy URL of an upstream prefix, cached per
// manifest.
func (rw *rewriter) prefixURL(prefix string, query bool) (string, error) {
	key := "n" + prefix
	if query {
		key = "q" + prefix
	}
	if p, ok := rw.prefixes[key]; ok {
		return p, nil
	}
	for i := 0; i < len(prefix); i++ {
		// A host can decode to anything (%80); prefixes are ASCII.
		if c := prefix[i]; c <= ' ' || c >= 0x7f {
			return "", invalid("upstream prefix not printable ASCII")
		}
	}
	p, err := rw.opts.PrefixURL(prefix, query)
	if err != nil {
		// The error may quote the prefix; it is not passed on.
		return "", invalid("upstream prefix not signable")
	}
	rw.prefixes[key] = p
	return p, nil
}

// resolve resolves ref against base. The result must be an http(s) URL
// with a host and without user info.
func resolve(base *url.URL, ref string) (*url.URL, error) {
	r, err := url.Parse(ref)
	if err != nil {
		// The parse error quotes the URL; it is not passed on.
		return nil, invalid("URL does not parse")
	}
	u := base.ResolveReference(r)
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" || u.User != nil || u.Opaque != "" {
		return nil, invalid("URL is not http(s) with a host and without user info")
	}
	return u, nil
}

// splitURL splits an http(s) URL into its directory prefix,
// "scheme://host[:port]/dir/", and the rest: the file name and the query.
// The fragment is dropped.
func splitURL(u *url.URL) (prefix, rest string) {
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	i := strings.LastIndexByte(p, '/')
	prefix, rest = u.Scheme+"://"+u.Host+p[:i+1], p[i+1:]
	if u.RawQuery != "" || u.ForceQuery {
		rest += "?" + u.RawQuery
	}
	return prefix, rest
}

// uriScheme returns the RFC 3986 scheme of a URI reference, lower-case,
// or "" for a relative reference.
func uriScheme(v string) string {
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
		case i > 0 && (c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'):
		case i > 0 && c == ':':
			return strings.ToLower(v[:i])
		default:
			return ""
		}
	}
	return ""
}

// selfClosing reports whether the raw bytes of a start tag end in "/>".
func selfClosing(raw []byte) bool {
	return bytes.HasSuffix(raw, []byte("/>"))
}

// appendStartTag appends a start tag; attribute values are escaped.
func appendStartTag(dst []byte, name xml.Name, attrs []xml.Attr, selfClose bool) []byte {
	dst = appendName(append(dst, '<'), name)
	for _, a := range attrs {
		dst = appendName(append(dst, ' '), a.Name)
		dst = append(appendEscaped(append(dst, `="`...), a.Value), '"')
	}
	if selfClose {
		return append(dst, "/>"...)
	}
	return append(dst, '>')
}

// appendElement appends an element with text content.
func appendElement(dst []byte, name xml.Name, attrs []xml.Attr, text string) []byte {
	dst = appendEscaped(appendStartTag(dst, name, attrs, false), text)
	return append(appendName(append(dst, "</"...), name), '>')
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

// hasDuplicate reports whether two attributes have the same name (prefix
// and local name), in linear time.
func hasDuplicate(attrs []xml.Attr) bool {
	if len(attrs) <= 8 {
		for i, a := range attrs {
			for _, b := range attrs[:i] {
				if a.Name == b.Name {
					return true
				}
			}
		}
		return false
	}
	names := make(map[xml.Name]struct{}, len(attrs))
	for _, a := range attrs {
		if _, ok := names[a.Name]; ok {
			return true
		}
		names[a.Name] = struct{}{}
	}
	return false
}

// Count returns the number of Period and AdaptationSet elements of a
// manifest (local names, case-insensitive), for the probe command (spec
// 7.2). A manifest that does not parse counts what was read before.
func Count(mpd []byte) (periods, adaptationSets int) {
	z := newTokenizer(mpd)
	for {
		t, _, _, err := z.next()
		if err != nil {
			return periods, adaptationSets
		}
		if se, ok := t.(xml.StartElement); ok {
			switch strings.ToLower(se.Name.Local) {
			case "period":
				periods++
			case "adaptationset":
				adaptationSets++
			}
		}
	}
}

// appendText appends s escaped as XML text: '&', '<', '>' and CR (which
// encoding/xml does not return, it normalizes line ends to LF).
func appendText(dst []byte, s []byte) []byte {
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
