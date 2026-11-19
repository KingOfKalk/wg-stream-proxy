// SPDX-License-Identifier: AGPL-3.0-or-later

package hls

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"testing"
)

const (
	testPrefix  = "https://cdn.example.com:8443/live/ch1/dash/"
	testChannel = "news-1"
)

// prefixRequest parses a request target of a prefix path like the HTTP
// server does.
func prefixRequest(t testing.TB, target string) *url.URL {
	t.Helper()
	u, err := url.ParseRequestURI(target)
	if err != nil {
		t.Fatalf("ParseRequestURI(%q): %v", target, err)
	}
	return u
}

func mustPrefixPath(t testing.TB, s *Signer, channel, prefix string, query bool) string {
	t.Helper()
	p, err := s.PrefixPath(channel, prefix, query)
	if err != nil {
		t.Fatalf("PrefixPath(%q, %q, %v): %v", channel, prefix, query, err)
	}
	return p
}

func TestPrefixPathFormat(t *testing.T) {
	s := mustSigner(t, testKey)
	for _, tt := range []struct {
		query bool
		mode  string
	}{{false, "n"}, {true, "q"}} {
		got := mustPrefixPath(t, s, testChannel, testPrefix, tt.query)
		mac := hmac.New(sha256.New, testKey)
		mac.Write([]byte("prefix:" + testChannel + "\n" + tt.mode + "\n" + testPrefix))
		sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
		want := "/p/" + testChannel + "/" + sig + "/" + tt.mode + "/" + base64.RawURLEncoding.EncodeToString([]byte(testPrefix)) + "/"
		if got != want {
			t.Errorf("PrefixPath(query=%v) = %q, want %q", tt.query, got, want)
		}
	}
}

// TestPrefixSignatureIsNotAResourceSignature checks the domain
// separation: no /r/ signature verifies as a prefix signature, even for
// the same URL.
func TestPrefixSignatureIsNotAResourceSignature(t *testing.T) {
	s := mustSigner(t, testKey)
	r := s.Path(testChannel, testPrefix)
	parts := strings.Split(r, "/")
	forged := "/p/" + testChannel + "/" + parts[3] + "/n/" + parts[4] + "/seg.m4s"
	if _, _, err := s.ParsePrefix(prefixRequest(t, forged)); !errors.Is(err, ErrInvalidResource) {
		t.Errorf("a /r/ signature verified as prefix signature: %v", err)
	}
	p := mustPrefixPath(t, s, testChannel, testPrefix, false)
	pp := strings.Split(p, "/")
	forgedR := "/r/" + testChannel + "/" + pp[3] + "/" + pp[5] + "/x"
	if _, _, err := s.Parse(forgedR); !errors.Is(err, ErrInvalidResource) {
		t.Errorf("a prefix signature verified as /r/ signature: %v", err)
	}
}

func TestPrefixPathRejectsInvalidPrefix(t *testing.T) {
	s := mustSigner(t, testKey)
	for _, prefix := range []string{
		"",
		"https://cdn.example.com/live",          // no trailing slash
		"https://cdn.example.com",               // no path
		"/live/",                                // relative
		"//cdn.example.com/live/",               // scheme-relative
		"ftp://cdn.example.com/live/",           // other scheme
		"HTTPS://cdn.example.com/live/",         // not canonical
		"https://user:pw@cdn.example.com/live/", // user info
		"https://cdn.example.com/live/?a=1",     // query
		"https://cdn.example.com/live/?",        // empty query
		"https://cdn.example.com/live/#f",       // fragment
		"https:///live/",                        // no host
		"https://cdn.example.com/./live/",       // dot segment
		"https://cdn.example.com/a/../live/",    // dot segment
		"https://cdn.example.com/live/..",       // dot segment, no slash
		"https://cdn.example.com/li ve/",        // not canonical
		"https://cdn.example.com/live/\n",       // control byte
		"https://cdn.example.com/" + strings.Repeat("a", MaxURLLen) + "/", // too long
	} {
		if _, err := s.PrefixPath(testChannel, prefix, false); !errors.Is(err, ErrInvalidPrefix) {
			t.Errorf("PrefixPath(%q) error = %v, want ErrInvalidPrefix", prefix, err)
		}
	}
	for _, channel := range []string{"", "Upper", "a/b", strings.Repeat("a", 49)} {
		if _, err := s.PrefixPath(channel, testPrefix, false); !errors.Is(err, ErrInvalidPrefix) {
			t.Errorf("PrefixPath(channel %q) error = %v, want ErrInvalidPrefix", channel, err)
		}
	}
	for _, prefix := range []string{
		"http://cdn.example.com/",
		"https://cdn.example.com/a%20b/",
		"http://[2001:db8::1]:8080/live/",
		"https://192.0.2.1/x/",
		"https://cdn.example.com/a/b=c;d/$e/",
	} {
		if _, err := s.PrefixPath(testChannel, prefix, false); err != nil {
			t.Errorf("PrefixPath(%q): %v", prefix, err)
		}
	}
}

func TestParsePrefix(t *testing.T) {
	s := mustSigner(t, testKey)
	n := mustPrefixPath(t, s, testChannel, testPrefix, false)
	q := mustPrefixPath(t, s, testChannel, testPrefix, true)
	tests := []struct {
		name, target, want string
	}{
		{"segment", n + "News-video=1067600-1077053775557.dash", testPrefix + "News-video=1067600-1077053775557.dash"},
		{"subdirectory", n + "thumbnails/News-thumbnail-1.jpeg", testPrefix + "thumbnails/News-thumbnail-1.jpeg"},
		{"escapes kept", n + "a%20b/c%3Fd.m4s", testPrefix + "a%20b/c%3Fd.m4s"},
		{"sub-delims", n + "seg;v=1,x!$&'()*+:@~_.m4s", testPrefix + "seg;v=1,x!$&'()*+:@~_.m4s"},
		{"dots in names", n + "a..b/.hidden/x...m4s", testPrefix + "a..b/.hidden/x...m4s"},
		{"query allowed", q + "seg-1.m4s?token=abc&n=1", testPrefix + "seg-1.m4s?token=abc&n=1"},
		{"no query with q", q + "seg-1.m4s", testPrefix + "seg-1.m4s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch, up, err := s.ParsePrefix(prefixRequest(t, tt.target))
			if err != nil {
				t.Fatalf("ParsePrefix(%q): %v", tt.target, err)
			}
			if ch != testChannel || up != tt.want {
				t.Errorf("ParsePrefix(%q) = %q, %q; want %q, %q", tt.target, ch, up, testChannel, tt.want)
			}
		})
	}
}

// TestParsePrefixEscapes is the escape test of the prefix-signed URLs: no
// request below a prefix path may reach an upstream URL outside the
// signed prefix, another channel, or the upstream at all with a broken
// signature.
func TestParsePrefixEscapes(t *testing.T) {
	s := mustSigner(t, testKey)
	n := mustPrefixPath(t, s, testChannel, testPrefix, false)
	q := mustPrefixPath(t, s, testChannel, testPrefix, true)
	parts := strings.Split(n, "/") // "", "p", channel, sig, mode, b64, ""
	other := func(i int, v string) string {
		p := append([]string(nil), parts...)
		p[i] = v
		return strings.Join(p, "/")
	}
	b64 := base64.RawURLEncoding.EncodeToString
	otherKeyPath := mustPrefixPath(t, mustSigner(t, otherKey), testChannel, testPrefix, false)
	tests := []struct{ name, target string }{
		{"dot dot", n + "../x.m4s"},
		{"dot dot deep", n + "a/../../x.m4s"},
		{"dot dot last", n + "a/.."},
		{"dot", n + "./x.m4s"},
		{"encoded dot dot", n + "%2e%2e/x.m4s"},
		{"encoded dot dot upper", n + "%2E%2E/x.m4s"},
		{"half encoded dot dot", n + ".%2e/x.m4s"},
		{"encoded dot", n + "%2e/x.m4s"},
		{"dot dot path parameter", n + "..;x/y.m4s"},
		{"encoded dot dot path parameter", n + "%2e%2e;/y.m4s"},
		{"encoded slash", n + "a%2Fb.m4s"},
		{"encoded slash lower", n + "a%2fb.m4s"},
		{"encoded dot dot slash", n + "..%2Fx.m4s"},
		{"encoded backslash", n + "a%5Cb.m4s"},
		{"backslash", n + "a\\b.m4s"},
		{"double slash", n + "a//b.m4s"},
		{"leading double slash", n + "/evil.example/x.m4s"},
		{"trailing slash", n + "a/"},
		{"empty path", n},
		{"no path", strings.TrimSuffix(n, "/")},
		{"double encoded dot dot", n + "%252e%252e/x.m4s"},
		{"encoded NUL", n + "a%00.m4s"},
		{"encoded newline", n + "a%0A.m4s"},
		{"encoded DEL", n + "a%7f.m4s"},
		{"bad escape", n + "a%2.m4s"},
		{"query without q", n + "seg.m4s?a=1"},
		{"empty query without q", n + "seg.m4s?"},
		{"query with space", q + "seg.m4s?a=1%20&b= c"},
		{"query with control", q + "seg.m4s?a=\x01"},
		{"wrong signature", other(3, b64(make([]byte, 16))) + "x.m4s"},
		{"short signature", other(3, "AAAA") + "x.m4s"},
		{"padded signature", other(3, parts[3]+"==") + "x.m4s"},
		{"wrong key", otherKeyPath + "x.m4s"},
		{"other channel", other(2, "other-channel") + "x.m4s"},
		{"invalid channel", other(2, "Ert") + "x.m4s"},
		{"mode switched to q", other(4, "q") + "x.m4s?a=1"},
		{"unknown mode", other(4, "x") + "x.m4s"},
		{"other host", other(5, b64([]byte("https://evil.example.com:8443/live/ch1/dash/"))) + "x.m4s"},
		{"other scheme", other(5, b64([]byte("http://cdn.example.com:8443/live/ch1/dash/"))) + "x.m4s"},
		{"other port", other(5, b64([]byte("https://cdn.example.com:443/live/ch1/dash/"))) + "x.m4s"},
		{"parent prefix", other(5, b64([]byte("https://cdn.example.com:8443/live/"))) + "x.m4s"},
		{"padded prefix", other(5, parts[5]+"=") + "x.m4s"},
		{"prefix with line break", other(5, parts[5][:4]+"\n"+parts[5][4:]) + "x.m4s"},
		{"not under /p/", strings.Replace(n, "/p/", "/r/", 1) + "x.m4s"},
		{"too long", n + strings.Repeat("a", MaxURLLen)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.ParseRequestURI(tt.target)
			if err != nil {
				return // the HTTP server rejects it before any handler
			}
			ch, up, err := s.ParsePrefix(u)
			if !errors.Is(err, ErrInvalidResource) {
				t.Fatalf("ParsePrefix(%q) = %q, %q, %v; want ErrInvalidResource", tt.target, ch, up, err)
			}
			if strings.Contains(err.Error(), "cdn.example") || strings.Contains(err.Error(), "x.m4s") {
				t.Errorf("error contains the URL: %v", err)
			}
		})
	}
}

func TestParsePrefixRoundTripStaysBelowPrefix(t *testing.T) {
	s := mustSigner(t, testKey)
	for _, prefix := range []string{testPrefix, "http://cdn.example.com/", "http://[2001:db8::1]:8080/a%2Fb/"} {
		p := mustPrefixPath(t, s, testChannel, prefix, true)
		ch, up, err := s.ParsePrefix(prefixRequest(t, p+"x/y.m4s?z=1"))
		if err != nil {
			t.Fatalf("ParsePrefix: %v", err)
		}
		if ch != testChannel || up != prefix+"x/y.m4s?z=1" {
			t.Errorf("got %q %q", ch, up)
		}
	}
}

// FuzzParsePrefix checks the prefix path validation (spec 4.1) with any
// request target below a valid prefix path: an accepted request reaches
// an upstream URL below the signed prefix (same scheme, host and port,
// below the prefix directory after decoding and cleaning the path, no
// empty or dot segments) and carries a query only if the mode allows it.
func FuzzParsePrefix(f *testing.F) {
	for _, s := range []string{
		"seg-1.m4s", "a/b/c.m4s", "../x", "%2e%2e/x", "a%2Fb", "a//b", "seg.m4s?a=1", "..;/x",
		"x/%2E./y", "News-video=1067600-1077053775557.dash", "a%25%32%65", "?", "a/.",
	} {
		f.Add(s, false)
		f.Add(s, true)
	}
	s := mustSigner(f, testKey)
	f.Fuzz(func(t *testing.T, rest string, query bool) {
		p := mustPrefixPath(t, s, testChannel, testPrefix, query)
		u, err := url.ParseRequestURI(p + rest)
		if err != nil {
			return
		}
		ch, up, err := s.ParsePrefix(u)
		if err != nil {
			if !errors.Is(err, ErrInvalidResource) {
				t.Fatalf("error %v does not wrap ErrInvalidResource", err)
			}
			return
		}
		if ch != testChannel {
			t.Fatalf("channel %q", ch)
		}
		if !strings.HasPrefix(up, testPrefix) || len(up) > MaxURLLen {
			t.Fatalf("upstream %q not below the prefix", up)
		}
		pu, err := url.Parse(up)
		if err != nil {
			t.Fatalf("upstream %q does not parse: %v", up, err)
		}
		if pu.Scheme != "https" || pu.Host != "cdn.example.com:8443" || pu.User != nil || pu.Fragment != "" {
			t.Fatalf("upstream %q leaves the prefix's scheme, host or port", up)
		}
		below := strings.TrimPrefix(pu.Path, "/live/ch1/dash/")
		if below == pu.Path || below == "" {
			t.Fatalf("upstream %q: decoded path %q not below the prefix", up, pu.Path)
		}
		for seg := range strings.SplitSeq(below, "/") {
			name, _, _ := strings.Cut(seg, ";")
			if name == "" || name == "." || name == ".." {
				t.Fatalf("upstream %q: segment %q in decoded path %q", up, seg, pu.Path)
			}
		}
		if strings.ContainsAny(pu.Path, "\\%") {
			t.Fatalf("upstream %q: backslash or double encoding", up)
		}
		if !query && strings.Contains(up, "?") {
			t.Fatalf("upstream %q has a query without mode q", up)
		}
	})
}
