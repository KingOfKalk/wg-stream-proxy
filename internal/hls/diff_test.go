// SPDX-License-Identifier: AGPL-3.0-or-later

package hls

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"net/url"
	"strings"
	"testing"
)

// rewriteBoth rewrites in with the current implementation and the
// PathBuilder, and with the reference implementation and referencePath,
// and fails if output or error differ.
func rewriteBoth(t testing.TB, in []byte, final *url.URL) {
	t.Helper()
	const base = "http://proxy.example:2349"
	pb := mustSigner(t, testKey).PathBuilder("ch-1")
	got, gotErr := Rewrite(in, final, func(dst, abs []byte) []byte {
		return pb.AppendPath(append(dst, base...), abs)
	})
	want, wantErr := referenceRewrite(in, final, func(abs *url.URL) string {
		return base + referencePath(testKey, "ch-1", abs.String())
	})
	if fmt.Sprint(gotErr) != fmt.Sprint(wantErr) {
		t.Fatalf("error %v, reference %v\nin: %q", gotErr, wantErr, in)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("output differs from the reference\nin:  %q\ngot: %q\nwant: %q", in, got, want)
	}
}

func TestRewriteMatchesReferenceLarge(t *testing.T) {
	rewriteBoth(t, largeMediaPlaylist(largeSegments), mustParse(t, largeFinal))
}

// Pieces of URIs with the cases the fast paths must leave to net/url:
// escapes, dot segments, other schemes, hosts, user info, fragments,
// spaces, non-ASCII bytes and colons.
var (
	diffHeads = []string{
		"", "a/", "a/b/", "./", "../", "../../x/", "a/./", "a/../", ".../", "/", "/abs/", "/abs/../",
		"//cdn.example.org/", "//cdn.example.org", "//u@cdn.example.org/", "///", "//[::1]:80/",
		"http://Example.COM/", "HTTPS://h.example:8080/p/", "http://h.example:/", "http://h.example:x/",
		"http://[::1]/", "http://[fe80::1%25eth0]/", "http://u:p@h.example/", "http://h.example/../a/",
		"http://", "http:", "http:/", "http:///", "https://h.example//", "http://h%41.example/",
		"http://h.example:99999999/", "http://h.example:0/", "http://h.example:80:80/", "http://-h..example/",
		"%41/", "%2F/", "a%2fb/", "%zz/", "a b/", "\xc3\xa9/", "a:b/", ":/", "a;b/", "~a/", "a@b/",
		"mailto:", "skd://k/", "data:", "1http://h/", "h.example?/", "a#/",
	}
	diffFiles = []string{
		"seg.ts", "a", ".", "..", "...", "x", "-_.~", "a$&+,;=@b", "a:b", "%41.ts", "a%2F", "a b",
		"\xc3\xa9.ts", "a!b", "(x)", "a*'b", "a[1]", "", "a\tb", "a\x7fb", "a\\b", "a|b", "a^b", "a`b",
		"a{b}", "a<b>", "1731931200123456789.ts", strings.Repeat("n", 70) + ".ts",
	}
	diffQueries = []string{
		"", "?", "??", "?a=1&b=2", "?a=%41", "?%zz", "?x y", "?\xc3\xa9", "?a#f", "#frag", "#%zz",
		"?a/b/../c", "?a?b", "?a\"b", "?a\x7fb",
	}
	diffFinals = []string{
		"https://origin.example.com/live/a/index.m3u8?s=1",
		"http://Origin.Example.com:8080/a/../b/./c/master.m3u8",
		"https://origin.example.com",
		"https://origin.example.com/%41b/c%2Fd/x.m3u8",
		"https://u:p@[2001:db8::1]:443/live/a b/x.m3u8#frag",
		"HTTP://origin.example.com/x",
	}
)

// randomPlaylist builds a playlist of URI lines and URI attributes from
// the pieces above, mixed with other lines.
func randomPlaylist(r *rand.Rand) []byte {
	var b bytes.Buffer
	pick := func(s []string) string { return s[r.IntN(len(s))] }
	for range 1 + r.IntN(6) {
		uri := pick(diffHeads) + pick(diffFiles) + pick(diffQueries)
		switch r.IntN(6) {
		case 0:
			fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"%s\"\n", uri)
		case 1:
			fmt.Fprintf(&b, "#EXTINF:6.000,\n#EXT-X-PROGRAM-DATE-TIME:2026-11-18T12:00:00.000Z\n")
			fallthrough
		default:
			fmt.Fprintf(&b, " %s\r\n", uri)
		}
	}
	return b.Bytes()
}

func TestRewriteMatchesReferenceRandom(t *testing.T) {
	r := rand.New(rand.NewPCG(76, 1))
	finals := make([]*url.URL, len(diffFinals))
	for i, s := range diffFinals {
		finals[i] = mustParse(t, s)
	}
	for range 20000 {
		rewriteBoth(t, randomPlaylist(r), finals[r.IntN(len(finals))])
	}
	// Every combination once, against the first final URL.
	for _, h := range diffHeads {
		for _, f := range diffFiles {
			for _, q := range diffQueries {
				rewriteBoth(t, []byte(h+f+q+"\n"), finals[0])
			}
		}
	}
}

func TestPathBuilderMatchesReference(t *testing.T) {
	pb := mustSigner(t, testKey).PathBuilder("ert-world")
	var dst []byte
	for _, h := range diffHeads {
		for _, f := range diffFiles {
			for _, q := range diffQueries {
				upstream := h + f + q
				want := referencePath(testKey, "ert-world", upstream)
				dst = pb.AppendPath(dst[:0], []byte(upstream))
				if string(dst) != want {
					t.Fatalf("AppendPath(%q) = %q, want %q", upstream, dst, want)
				}
			}
		}
	}
	if got, want := mustSigner(t, testKey).Path("ert-world", "https://cdn.example.com/a.ts"),
		referencePath(testKey, "ert-world", "https://cdn.example.com/a.ts"); got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}

func FuzzResourceName(f *testing.F) {
	for _, s := range []string{
		"https://cdn.example.com/live/seg-1.ts?token=abc",
		"http://h.example:8080/a/%41.ts",
		"http://[::1]/a.ts",
		"http://u@h/a.ts",
		"https://h/a.ts#f",
		"http://h/" + strings.Repeat("a", 80) + ".ts",
		"http://h/...",
		"http://h.example:99999999/a.ts",
		"",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, upstream string) {
		if got, want := string(appendResourceName(nil, []byte(upstream))), referenceResourceName(upstream); got != want {
			t.Fatalf("resource name of %q = %q, want %q", upstream, got, want)
		}
	})
}
