// SPDX-License-Identifier: AGPL-3.0-or-later

package hls

import (
	"bytes"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fuzzURLFor is a short resource URL builder, so rewritten output stays
// well below the URL limit and can be rewritten again.
func fuzzURLFor(dst, abs []byte) []byte {
	return strconv.AppendInt(append(dst, "/r/f/"...), int64(len(abs)), 10)
}

// isRewritable reports whether the rewriter must replace URI s: it parses
// and is relative or http(s). Rewrite rejects unparseable relative and
// http(s) URIs, so after a successful Rewrite an unparseable URI has
// another scheme.
func isRewritable(s []byte) bool {
	if len(s) == 0 {
		return false
	}
	u, err := url.Parse(string(s))
	return err == nil && (u.Scheme == "" || u.Scheme == "http" || u.Scheme == "https")
}

// line is one line of a playlist as the rewriter splits it.
type line struct{ content, eol []byte }

func lines(b []byte) []line {
	var ls []line
	for len(b) > 0 {
		var l line
		l.content, l.eol, b = nextLine(b)
		ls = append(ls, l)
	}
	return ls
}

// checkRewrite verifies the L2 invariants for one input/output pair: same
// lines and line endings, every rewritable URI line and URI attribute now
// points to /r/, and nothing else changed.
func checkRewrite(t *testing.T, in, out []byte) {
	t.Helper()
	inLines, outLines := lines(in), lines(out)
	if len(inLines) != len(outLines) {
		t.Fatalf("line count %d -> %d\nin:  %q\nout: %q", len(inLines), len(outLines), in, out)
	}
	for i := range inLines {
		inC, inEOL := inLines[i].content, inLines[i].eol
		outC, outEOL := outLines[i].content, outLines[i].eol
		if !bytes.Equal(inEOL, outEOL) {
			t.Fatalf("line %d: terminator %q -> %q", i, inEOL, outEOL)
		}
		if i == 0 {
			var inBOM, outBOM bool
			inC, inBOM = bytes.CutPrefix(inC, []byte(utf8BOM))
			outC, outBOM = bytes.CutPrefix(outC, []byte(utf8BOM))
			if inBOM != outBOM {
				t.Fatalf("BOM %v -> %v", inBOM, outBOM)
			}
		}
		trimmed := bytes.Trim(inC, lineSpace)
		switch {
		case len(trimmed) > 0 && trimmed[0] != '#':
			if !isRewritable(trimmed) {
				if !bytes.Equal(inC, outC) {
					t.Fatalf("line %d: non-http URI changed: %q -> %q", i, inC, outC)
				}
				continue
			}
			lead := bytes.Index(inC, trimmed)
			trail := inC[lead+len(trimmed):]
			if !bytes.HasPrefix(outC, inC[:lead]) || !bytes.HasSuffix(outC, trail) ||
				!bytes.HasPrefix(outC[lead:], []byte("/r/")) {
				t.Fatalf("line %d: URI line not rewritten to /r/: %q -> %q", i, inC, outC)
			}
		case bytes.HasPrefix(trimmed, []byte("#EXT")) && !bytes.HasPrefix(trimmed, []byte("#EXTINF:")):
			checkTag(t, i, inC, outC)
			checkNoHTTPURI(t, i, outC)
		default:
			if !bytes.Equal(inC, outC) {
				t.Fatalf("line %d: changed: %q -> %q", i, inC, outC)
			}
		}
	}
}

// checkNoHTTPURI fails if "URI=" or "X-ASSET-LIST=" in a rewritten tag line
// is followed by an http(s) or scheme-relative URL, the way a lenient or
// regex-based player parser would read it. fuzzURLFor values are relative,
// so any such URL was left unrewritten.
func checkNoHTTPURI(t *testing.T, i int, out []byte) {
	t.Helper()
	for _, marker := range []string{"URI=", "X-ASSET-LIST="} {
		checkNoHTTPAfter(t, i, out, marker)
	}
}

func checkNoHTTPAfter(t *testing.T, i int, out []byte, marker string) {
	t.Helper()
	for rest := out; ; {
		j := bytes.Index(rest, []byte(marker))
		if j < 0 {
			return
		}
		rest = rest[j+len(marker):]
		v := rest
		if q, ok := bytes.CutPrefix(v, []byte(`"`)); ok {
			v = bytes.TrimLeft(q, " \t")
		}
		lower := bytes.ToLower(v)
		if bytes.HasPrefix(lower, []byte("http:")) || bytes.HasPrefix(lower, []byte("https:")) || bytes.HasPrefix(v, []byte("//")) {
			t.Fatalf("line %d: unrewritten http URI after %s: %q", i, marker, out)
		}
	}
}

func checkTag(t *testing.T, i int, in, out []byte) {
	t.Helper()
	inVals, outVals := uriAttrValues(in), uriAttrValues(out)
	if len(inVals) != len(outVals) {
		t.Fatalf("line %d: URI attribute count %d -> %d: %q -> %q", i, len(inVals), len(outVals), in, out)
	}
	var inMasked, outMasked []byte
	inDone, outDone := 0, 0
	for j := range inVals {
		iv := in[inVals[j].start:inVals[j].end]
		ov := out[outVals[j].start:outVals[j].end]
		if isRewritable(iv) {
			if !bytes.HasPrefix(ov, []byte("/r/")) {
				t.Fatalf("line %d: URI attribute not rewritten to /r/: %q -> %q", i, iv, ov)
			}
		} else if !bytes.Equal(iv, ov) {
			t.Fatalf("line %d: non-http URI attribute changed: %q -> %q", i, iv, ov)
		}
		inMasked = append(inMasked, in[inDone:inVals[j].start]...)
		outMasked = append(outMasked, out[outDone:outVals[j].start]...)
		inDone, outDone = inVals[j].end, outVals[j].end
	}
	inMasked = append(inMasked, in[inDone:]...)
	outMasked = append(outMasked, out[outDone:]...)
	if !bytes.Equal(inMasked, outMasked) {
		t.Fatalf("line %d: bytes outside URI values changed: %q -> %q", i, in, out)
	}
}

func FuzzRewrite(f *testing.F) {
	for _, name := range []string{"master.m3u8", "media_aes.m3u8"} {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	for _, s := range []string{
		"",
		"\xef\xbb\xbf#EXTM3U\r\n#EXTINF:6,\r\nseg.ts\r\n",
		"#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"k.key\",IV=0x1\nhttp://example.com/a.ts",
		"#EXT-X-FOO: URI = \"skd://x\" , URI=\"data:,x\",URI=\"//cdn.example.org/a\"\n",
		"  ../a.ts\t\r\n#comment URI=\"x\"\n\n",
		"#EXTINF:6.0,URI=\"https://example.com/x\"\nfoo:%zz\n",
		"#EXT-X-KEY:METHOD=AES-128 URI=\"k\"\n#EXT-X-FOO:A=\"x\\\"\",URI=\"https://example.com/k\"\n",
		"#EXT-X-FOO:URI=https://example.com/k,X-ASSET-URI=\"a\"\rseg.ts\r",
		"#EXT-X-DATERANGE:ID=\"a\",X-ASSET-LIST=\"https://ads.example.com/l.json\",X-ASSET-URI=\"../b.m3u8\"\n",
		"#EXT-X-DATERANGE:X-ASSET-LIST=//ads.example.com/l.json\n#EXT-X-FOO:A=\"x\" junk X-ASSET-LIST=\"l.json\"\n",
		"../a/b.ts?x=1\n/abs/./c.ts\n//cdn.example.org/d.ts\nHTTP://h.example/../e.ts?\n%41/x.ts?#f\na b/c.ts\n#EXT-X-MAP:URI=\"../../i.mp4\"\n",
	} {
		f.Add([]byte(s))
	}
	final, err := url.Parse("https://origin.example.com/live/a/index.m3u8?s=1")
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		out, err := Rewrite(in, final, fuzzURLFor)
		if err != nil {
			if !errors.Is(err, ErrInvalidURI) && !errors.Is(err, ErrURLTooLong) && !errors.Is(err, ErrInvalidPlaylist) && !errors.Is(err, ErrPlaylistTooLarge) {
				t.Fatalf("unexpected error: %v", err)
			}
			return
		}
		checkRewrite(t, in, out)
		if _, err := Rewrite(out, final, fuzzURLFor); err != nil {
			t.Fatalf("rewritten output does not parse again: %v\nout: %q", err, out)
		}
	})
}

func FuzzParseResourceURL(f *testing.F) {
	s := mustSigner(f, testKey)
	valid := s.Path("ert-world", "https://cdn.example.com/live/seg-1.ts?token=abc")
	f.Add(valid)
	f.Add(s.Path("a", "http://192.0.2.1/"))
	f.Add(strings.Replace(valid, "ert-world", "ert-worle", 1))
	f.Add("/r/a/b/c/d")
	f.Add("/r/" + strings.Repeat("/", 5))
	f.Add("")
	f.Fuzz(func(t *testing.T, path string) {
		channel, upstream, err := s.Parse(path)
		if err != nil {
			if !errors.Is(err, ErrInvalidResource) {
				t.Fatalf("error %v does not wrap ErrInvalidResource", err)
			}
			if channel != "" || upstream != "" {
				t.Fatalf("Parse returned (%q, %q) with an error", channel, upstream)
			}
			return
		}
		if !validChannel(channel) || upstream == "" || len(upstream) > MaxURLLen {
			t.Fatalf("Parse accepted channel %q, upstream of %d bytes", channel, len(upstream))
		}
		want := s.Path(channel, upstream)
		if path[:strings.LastIndexByte(path, '/')] != want[:strings.LastIndexByte(want, '/')] {
			t.Fatalf("accepted path %q is not the signed path %q", path, want)
		}
	})
}
