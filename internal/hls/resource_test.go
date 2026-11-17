// SPDX-License-Identifier: AGPL-3.0-or-later

package hls

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
)

var (
	testKey  = []byte("test-only-signing-key-0123456789abcdef")
	otherKey = []byte("another-test-only-key-0123456789abcdef")
)

func mustSigner(t testing.TB, key []byte) *Signer {
	t.Helper()
	s, err := NewSigner(key)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s
}

func TestNewSignerKeyLength(t *testing.T) {
	tests := []struct {
		name    string
		key     []byte
		wantErr bool
	}{
		{"nil", nil, true},
		{"31 bytes", bytes.Repeat([]byte("k"), 31), true},
		{"32 bytes", bytes.Repeat([]byte("k"), 32), false},
		{"64 bytes", bytes.Repeat([]byte("k"), 64), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewSigner(tt.key)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NewSigner error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, ErrShortKey) {
				t.Errorf("error = %v, want ErrShortKey", err)
			}
		})
	}
}

func TestSignerPathFormat(t *testing.T) {
	s := mustSigner(t, testKey)
	const channel = "ert-world"
	const upstream = "https://cdn.example.com/live/seg-1.ts?token=abc"

	got := s.Path(channel, upstream)

	mac := hmac.New(sha256.New, testKey)
	mac.Write([]byte(channel + "\n" + upstream))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
	b64 := base64.RawURLEncoding.EncodeToString([]byte(upstream))
	want := "/r/" + channel + "/" + sig + "/" + b64 + "/seg-1.ts"
	if got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}

func TestSignerPathDoesNotDependOnCallerKeyBuffer(t *testing.T) {
	key := bytes.Clone(testKey)
	s := mustSigner(t, key)
	before := s.Path("a", "https://example.com/a.ts")
	key[0] ^= 0xff
	if after := s.Path("a", "https://example.com/a.ts"); after != before {
		t.Errorf("signature changed after the caller modified its key buffer")
	}
}

func TestResourceName(t *testing.T) {
	long := strings.Repeat("a", 100)
	tests := []struct {
		upstream string
		want     string
	}{
		{"https://example.com/live/seg-1.ts", "seg-1.ts"},
		{"https://example.com/live/index.m3u8?token=abc#x", "index.m3u8"},
		{"https://example.com/live/seg%201%20(hd).ts", "seg1hd.ts"},
		{"https://example.com/live/Ελληνικά_1.m4s", "_1.m4s"},
		{"https://example.com/live/", "data"},
		{"https://example.com", "data"},
		{"https://example.com/live/%E2%9C%93", "data"},
		{"https://example.com/live/..", "data"},
		{"https://example.com/live/.%20.", "data"},
		{"https://example.com/live/.hidden", ".hidden"},
		{"https://example.com/" + long + ".ts", strings.Repeat("a", 61) + ".ts"},
		{"https://example.com/" + long, strings.Repeat("a", 64)},
		{"https://example.com/x." + long, "x." + strings.Repeat("a", 62)},
		{"https://example.com/" + strings.Repeat("a", 64) + ".", strings.Repeat("a", 63) + "."},
		{"https://example.com/" + strings.Repeat("b", 64), strings.Repeat("b", 64)},
		{"https://example.com/a.b.c.tar.gz", "a.b.c.tar.gz"},
		{"::not a url", "data"},
	}
	for _, tt := range tests {
		t.Run(tt.upstream, func(t *testing.T) {
			if got := resourceName(tt.upstream); got != tt.want {
				t.Errorf("resourceName(%q) = %q, want %q", tt.upstream, got, tt.want)
			}
			if got := resourceName(tt.upstream); len(got) > 64 {
				t.Errorf("name %q longer than 64 bytes", got)
			}
		})
	}
}

func TestSignerParseRoundTrip(t *testing.T) {
	s := mustSigner(t, testKey)
	for _, tt := range []struct{ channel, upstream string }{
		{"ert-world", "https://cdn.example.com/live/seg-1.ts?token=abc&x=%2F"},
		{"a", "http://192.0.2.1:8080/"},
		{strings.Repeat("c", 48), "https://example.com/" + strings.Repeat("p", MaxURLLen-len("https://example.com/"))},
	} {
		path := s.Path(tt.channel, tt.upstream)
		channel, upstream, err := s.Parse(path)
		if err != nil {
			t.Fatalf("Parse(%q): %v", path, err)
		}
		if channel != tt.channel || upstream != tt.upstream {
			t.Errorf("Parse = (%q, %q), want (%q, %q)", channel, upstream, tt.channel, tt.upstream)
		}
	}
}

// pathParts returns the four segments after "/r/" of a valid path.
func pathParts(t *testing.T, path string) (channel, sig, b64, name string) {
	t.Helper()
	parts := strings.Split(strings.TrimPrefix(path, "/r/"), "/")
	if len(parts) != 4 {
		t.Fatalf("path %q has %d segments", path, len(parts))
	}
	return parts[0], parts[1], parts[2], parts[3]
}

func TestSignerParseRejects(t *testing.T) {
	s := mustSigner(t, testKey)
	const upstream = "https://cdn.example.com/live/seg-1.ts"
	valid := s.Path("ch-1", upstream)
	channel, sig, b64, name := pathParts(t, valid)
	join := func(parts ...string) string { return "/r/" + strings.Join(parts, "/") }

	flipped := []byte(sig)
	flipped[3] ^= 'A' ^ 'B'
	if flipped[3] == sig[3] {
		t.Fatal("signature not modified")
	}
	// A different, validly encoded upstream URL under the original signature.
	tamperedURL := base64.RawURLEncoding.EncodeToString([]byte(upstream + "?x=1"))
	// Same bytes, non-canonical encoding: the last character carries
	// non-zero padding bits.
	nonCanonicalSig := sig[:len(sig)-1] + string(sig[len(sig)-1]+1)
	tooLongURL := "https://example.com/" + strings.Repeat("p", MaxURLLen)
	tooLongSigned := mustSigner(t, testKey).Path("ch-1", tooLongURL)
	_, tlSig, tlB64, _ := pathParts(t, tooLongSigned)

	tests := []struct {
		name string
		path string
	}{
		{"tampered signature", join(channel, string(flipped), b64, name)},
		{"tampered URL", join(channel, sig, tamperedURL, name)},
		{"tampered channel", join("ch-2", sig, b64, name)},
		{"wrong key", mustSigner(t, otherKey).Path("ch-1", upstream)},
		{"non-canonical signature encoding", join(channel, nonCanonicalSig, b64, name)},
		{"signature with padding", join(channel, sig+"==", b64, name)},
		{"short signature", join(channel, sig[:21], b64, name)},
		{"empty signature", join(channel, "", b64, name)},
		{"standard base64 alphabet char", join(channel, sig[:5]+"+"+sig[6:], b64, name)},
		{"URL with padding", join(channel, sig, b64+"=", name)},
		// encoding/base64 skips CR and LF even in strict mode.
		{"newline in URL", join(channel, sig, b64[:10]+"\n"+b64[10:], name)},
		{"carriage return in URL", join(channel, sig, b64[:10]+"\r"+b64[10:], name)},
		{"newline in signature", join(channel, sig[:10]+"\n"+sig[11:], b64, name)},
		{"empty URL", join(channel, s.sign(channel, ""), "", name)},
		{"URL over limit", join("ch-1", tlSig, tlB64, "x")},
		{"invalid channel chars", join("Ch-1", sig, b64, name)},
		{"channel too long", join(strings.Repeat("c", 49), sig, b64, name)},
		{"empty channel", join("", sig, b64, name)},
		{"missing name", join(channel, sig, b64)},
		{"extra segment", join(channel, sig, b64, name, "x")},
		{"missing prefix", strings.TrimPrefix(valid, "/r")},
		{"other prefix", "/c" + strings.TrimPrefix(valid, "/r")},
		{"no leading slash", strings.TrimPrefix(valid, "/")},
		{"percent-encoded signature byte", join(channel, "%41"+sig[1:], b64, name)},
		{"empty", ""},
		{"prefix only", "/r/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch, up, err := s.Parse(tt.path)
			if !errors.Is(err, ErrInvalidResource) {
				t.Fatalf("Parse(%q) error = %v, want ErrInvalidResource", tt.path, err)
			}
			if ch != "" || up != "" {
				t.Errorf("Parse returned (%q, %q) with an error", ch, up)
			}
		})
	}
}

func TestSignerParseIgnoresName(t *testing.T) {
	s := mustSigner(t, testKey)
	channel, sig, b64, _ := pathParts(t, s.Path("ch-1", "https://example.com/a.ts"))
	for _, name := range []string{"", "other.m3u8", "x%20y"} {
		path := "/r/" + channel + "/" + sig + "/" + b64 + "/" + name
		if _, _, err := s.Parse(path); err != nil {
			t.Errorf("Parse(%q): %v", path, err)
		}
	}
}

func TestSignerDoesNotPrintKey(t *testing.T) {
	s := mustSigner(t, testKey)
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		if out := fmt.Sprintf(format, s); out != "hls.Signer{key:redacted}" {
			t.Errorf("Sprintf(%q, signer) = %q, want the key redacted", format, out)
		}
	}
}
