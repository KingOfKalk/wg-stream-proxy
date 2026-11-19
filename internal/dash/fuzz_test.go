// SPDX-License-Identifier: AGPL-3.0-or-later

package dash

import (
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const proxyPrefix = "http://wsp.lan/p/"

// checkRewritten verifies the invariants of a rewritten manifest: it
// passes checkManifest again; the MPD has a BaseURL; every BaseURL and
// every segment reference that is not plain points to a prefix URL; no
// removed element is left; UTCTiming is direct or the proxy's.
func checkRewritten(t *testing.T, out []byte) {
	t.Helper()
	if _, err := checkManifest(out); err != nil {
		t.Fatalf("output fails the check: %v\n%s", err, out)
	}
	z := newTokenizer(out)
	depth, rootBase := 0, false
	var base *strings.Builder
	for {
		tok, raw, _, err := z.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("output: %v", err)
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			depth++
			ln := strings.ToLower(tok.Name.Local)
			scheme, _ := attr(tok.Attr, "schemeiduri")
			switch {
			case removedElements[ln], ln == "supplementalproperty" && chainingSchemes[lowerTrim(scheme)]:
				t.Fatalf("output has %s:\n%s", tok.Name.Local, out)
			case ln == "baseurl":
				base = &strings.Builder{}
				if depth == 2 {
					rootBase = true
				}
			case (ln == "eventstream" || ln == "inbandeventstream") && !keptEvent(tok.Attr):
				t.Fatalf("output has %s %q", tok.Name.Local, scheme)
			case ln == "utctiming" && !directTiming[lowerTrim(scheme)]:
				if v, _ := attr(tok.Attr, "value"); scheme != schemeHTTPISO || v != testTime {
					t.Fatalf("UTCTiming %q %q not served by the proxy", scheme, v)
				}
			}
			for _, a := range tok.Attr {
				an := strings.ToLower(a.Name.Local)
				if ln == "baseurl" && !baseURLAttrs[an] && a.Name.Space != "xmlns" && an != "xmlns" {
					t.Fatalf("BaseURL keeps attribute %s", a.Name.Local)
				}
				if !isRefAttr(ln, tok.Attr, an) {
					continue
				}
				v, err := cleanURL(a.Value)
				if err != nil {
					t.Fatal(err)
				}
				if (!plain(v) || fontDownload(ln, tok.Attr)) && !strings.HasPrefix(v, proxyPrefix) {
					t.Fatalf("%s %s=%q is neither plain nor a prefix URL", tok.Name.Local, a.Name.Local, v)
				}
			}
		case xml.Comment:
			t.Fatalf("output has a comment")
		case xml.ProcInst:
			if string(raw) != xmlDecl {
				t.Fatalf("output has processing instruction %q", raw)
			}
		case xml.CharData:
			if base != nil {
				base.Write(tok)
			}
		case xml.EndElement:
			depth--
			if base != nil {
				if v := strings.TrimSpace(base.String()); !strings.HasPrefix(v, proxyPrefix) {
					t.Fatalf("BaseURL %q is not a prefix URL", v)
				}
				base = nil
			}
		}
	}
	if !rootBase {
		t.Fatalf("output has no MPD-level BaseURL:\n%s", out)
	}
}

// FuzzRewrite is the L2 fuzz target of the MPD rewriter: no panic; a
// rewritten manifest keeps the invariants of checkRewritten and rewrites
// again; errors are the documented ones.
func FuzzRewrite(f *testing.F) {
	files, _ := filepath.Glob("testdata/*.mpd")
	for _, name := range files {
		if strings.HasSuffix(name, ".golden.mpd") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	for _, s := range []string{
		`<MPD><Period><SegmentTemplate media="https://a.example/$Number$.m4s?x"/></Period></MPD>`,
		`<MPD><BaseURL>../a/</BaseURL><Period><BaseURL/><AdaptationSet><SegmentList><SegmentURL media="//b.example/s/1.m4s"/></SegmentList></AdaptationSet></Period></MPD>`,
		`<MPD><Location>https://a.example/m.mpd</Location><UTCTiming schemeIdUri="urn:mpeg:dash:utc:ntp:2014" value="ntp.example"/><Period/></MPD>`,
		`<mpd:MPD xmlns:mpd="urn:x"><mpd:Period><mpd:BaseURL>x/</mpd:BaseURL></mpd:Period></mpd:MPD>`,
		`<?xml version="1.0"?><MPD><!---><BaseURL>https://e.example/</BaseURL>--><Period/></MPD>`,
		`<MPD><Period><EventStream schemeIdUri="urn:mpeg:dash:event:callback:2015"><Event messageData="https://e.example/"/></EventStream>` +
			`<AdaptationSet><InbandEventStream schemeIdUri="urn:scte:scte35:2013:bin"/><SupplementalProperty schemeIdUri="urn:dvb:dash:fontdownload:2014" dvb:url="f.woff"/></AdaptationSet></Period></MPD>`,
		`<MPD><BaseURL byteRange="$base$" serviceLocation="a">a/</BaseURL><Period/></MPD>`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		opts := testOptions(t)
		out, err := Rewrite(in, opts)
		if err != nil {
			if !errors.Is(err, ErrInvalidManifest) && !errors.Is(err, ErrDRM) && !errors.Is(err, ErrTooLarge) {
				t.Fatalf("unexpected error %v", err)
			}
			return
		}
		checkRewritten(t, out)
		if _, err := Rewrite(out, opts); err != nil {
			t.Fatalf("output does not rewrite again: %v\n%s", err, out)
		}
	})
}
