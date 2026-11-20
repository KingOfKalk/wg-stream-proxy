// SPDX-License-Identifier: AGPL-3.0-or-later

package hls

// Temporary differential test (issue 139): Classify against the
// classification of server.respond and probe.detect on origin/main. Not
// committed.

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"testing"
	"testing/iotest"
)

// oldServer is proxy.go respond on origin/main, up to the decision.
func oldServer(ctype string, body *bufio.Reader, configured Type, head bool) (Type, string, *bufio.Reader, error) {
	typ := Detect(ctype, nil, configured)
	if typ == Raw && !head {
		prefix, rest, err := Sniff(body)
		body = rest
		if err != nil {
			return typ, "", body, err
		}
		typ = Detect(ctype, prefix, configured)
		if typ == Raw {
			if format := Unsupported(ctype, prefix, configured); format != "" {
				return typ, format, body, nil
			}
		}
	}
	return typ, "", body, nil
}

// oldProbe is probe.detect on origin/main.
func oldProbe(contentType string, body *bufio.Reader) (typ Type, refused string, rest *bufio.Reader, err error) {
	typ = Detect(contentType, nil, Auto)
	if typ != Raw {
		return typ, "", body, nil
	}
	prefix, rest, err := Sniff(body)
	if err != nil {
		return typ, "", rest, err
	}
	typ = Detect(contentType, prefix, Auto)
	if typ == Raw {
		refused = Unsupported(contentType, prefix, Auto)
	}
	return typ, refused, rest, nil
}

func FuzzZZClassifyDiff(f *testing.F) {
	for _, s := range []string{"#EXTM3U\n", "<MPD/>", "[playlist]\n", "\x47\x40", "<!--" + string(bytes.Repeat([]byte("x"), 700)) + "--><asx>", ttml, ""} {
		f.Add("", []byte(s), uint8(0), false, false)
		f.Add("audio/x-scpls", []byte(s), uint8(2), true, true)
	}
	errX := errors.New("x")
	f.Fuzz(func(t *testing.T, ct string, data []byte, conf uint8, head, failing bool) {
		configured := Type(conf % 4)
		mk := func() *bufio.Reader {
			var r io.Reader = bytes.NewReader(data)
			if failing {
				r = io.MultiReader(r, iotest.ErrReader(errX))
			}
			return bufio.NewReaderSize(r, SniffLen)
		}
		cmp := func(what string, t1 Type, f1 string, r1 *bufio.Reader, e1 error, t2 Type, f2 string, r2 *bufio.Reader, e2 error) {
			b1, re1 := io.ReadAll(r1)
			b2, re2 := io.ReadAll(r2)
			if t1 != t2 || f1 != f2 || !errors.Is(e1, e2) || !bytes.Equal(b1, b2) || !errors.Is(re1, re2) {
				t.Fatalf("%s differs: %v %q %v %d %v / %v %q %v %d %v", what, t1, f1, e1, len(b1), re1, t2, f2, e2, len(b2), re2)
			}
		}
		t1, f1, r1, e1 := oldServer(ct, mk(), configured, head)
		t2, f2, r2, e2 := Classify(ct, mk(), configured, !head)
		cmp("server", t1, f1, r1, e1, t2, f2, r2, e2)
		t1, f1, r1, e1 = oldProbe(ct, mk())
		t2, f2, r2, e2 = Classify(ct, mk(), Auto, true)
		cmp("probe", t1, f1, r1, e1, t2, f2, r2, e2)
	})
}
