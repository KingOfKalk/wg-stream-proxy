// SPDX-License-Identifier: AGPL-3.0-or-later

package route

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestClientStatus(t *testing.T) {
	tests := []struct{ in, want int }{
		{200, 200},
		{204, 204},
		{206, 206},
		{299, 299},
		{400, 400},
		{403, 403},
		{404, 404},
		{451, 451},
		{499, 499},
		{500, 502},
		{502, 502},
		{503, 502},
		{504, 502},
		{599, 502},
		{100, 502},
		{301, 502},
		{304, 502},
		{0, 502},
		{600, 502},
	}
	for _, tt := range tests {
		if got := ClientStatus(tt.in); got != tt.want {
			t.Errorf("ClientStatus(%d) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestCopyResponseHeader(t *testing.T) {
	src := http.Header{
		"Content-Type":   {"video/mp2t"},
		"Content-Range":  {"bytes 0-99/1000"},
		"Accept-Ranges":  {"bytes"},
		"Cache-Control":  {"max-age=10"},
		"Last-Modified":  {"Mon, 02 Jan 2006 15:04:05 GMT"},
		"Icy-Name":       {"Test Radio"},
		"Icy-Br":         {"128"},
		"Icy-Metaint":    {"16000"},
		"Content-Length": {"100"},
		"Etag":           {`"v1"`},
		"Set-Cookie":     {"session=1"},
		"Server":         {"fixture"},
		"Location":       {"http://cdn.example.com/"},
		"X-Other":        {"1"},
	}
	always := []string{"Content-Type", "Content-Range", "Accept-Ranges", "Cache-Control", "Last-Modified", "Icy-Name", "Icy-Br", "Icy-Metaint"}
	tests := []struct {
		name         string
		rewritten    bool
		uncompressed bool
		wantLength   bool
	}{
		{"unchanged body", false, false, true},
		{"rewritten", true, false, false},
		{"decompressed", false, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dst := http.Header{"X-Existing": {"keep"}}
			CopyResponseHeader(dst, &http.Response{Header: src, Uncompressed: tt.uncompressed}, tt.rewritten)
			want := slices.Clone(always)
			if tt.wantLength {
				want = append(want, "Content-Length", "Etag")
			}
			want = append(want, "X-Existing")
			var got []string
			for k := range dst {
				got = append(got, k)
			}
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("copied = %q, want %q", got, want)
			}
			if dst.Get("Icy-Name") != "Test Radio" {
				t.Errorf("Icy-Name = %q", dst.Get("Icy-Name"))
			}
		})
	}
}

func TestCopyResponseHeaderNonCanonicalIcy(t *testing.T) {
	dst := http.Header{}
	CopyResponseHeader(dst, &http.Response{Header: http.Header{"icy-genre": {"news"}}}, false)
	if got := dst.Get("Icy-Genre"); got != "news" {
		t.Errorf("Icy-Genre = %q, want news", got)
	}
}

func TestReadPlaylist(t *testing.T) {
	tests := []struct {
		size    int
		wantErr error
	}{
		{0, nil},
		{MaxPlaylistSize, nil},
		{MaxPlaylistSize + 1, ErrTooLarge},
	}
	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.size), func(t *testing.T) {
			b, err := ReadPlaylist(bytes.NewReader(make([]byte, tt.size)))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ReadPlaylist error = %v, want %v", err, tt.wantErr)
			}
			if err == nil && len(b) != tt.size {
				t.Errorf("len = %d, want %d", len(b), tt.size)
			}
			if err != nil && Classify(err) != ReasonSize {
				t.Errorf("Classify = %q, want size", Classify(err))
			}
		})
	}
}

func TestReadPlaylistReadError(t *testing.T) {
	boom := errors.New("boom")
	if _, err := ReadPlaylist(io.MultiReader(bytes.NewReader([]byte("#EXTM3U")), errReader{boom})); !errors.Is(err, boom) {
		t.Errorf("ReadPlaylist error = %v, want %v", err, boom)
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

func TestLogURL(t *testing.T) {
	long := strings.Repeat("a", 100)
	tests := []struct{ name, in, want string }{
		{"plain", "https://cdn.example.com/live/index.m3u8", "https://cdn.example.com/…/index.m3u8"},
		{"token in path", "https://cdn.example.com/2016498/de/0123456789abcdef0123456789abcdef/7/7.m3u8", "https://cdn.example.com/…/7.m3u8"},
		{"single segment", "http://cdn.example.com/seg.ts", "http://cdn.example.com/seg.ts"},
		{"query", "https://cdn.example.com/live/index.m3u8?token=abc&x=1", "https://cdn.example.com/…/index.m3u8"},
		{"empty query", "https://cdn.example.com/a/b?", "https://cdn.example.com/…/b"},
		{"fragment", "https://cdn.example.com/a/b#frag", "https://cdn.example.com/…/b"},
		{"user info and port", "https://user:pw@cdn.example.com:8443/a/b?x", "https://cdn.example.com:8443/…/b"},
		{"IPv4 literal", "http://192.0.2.7:8080/a/b.ts", "http://192.0.2.7:8080/…/b.ts"},
		{"IPv6 literal", "http://[2001:db8::1]:8080/a/b.ts", "http://[2001:db8::1]:8080/…/b.ts"},
		{"IPv6 literal without port", "http://[::1]/a/b.ts", "http://[::1]/…/b.ts"},
		{"no path", "https://cdn.example.com", "https://cdn.example.com"},
		{"root", "https://cdn.example.com/", "https://cdn.example.com"},
		{"root with query", "https://cdn.example.com/?token=secret", "https://cdn.example.com"},
		{"trailing slash", "https://cdn.example.com/a/b/", "https://cdn.example.com/…/data"},
		{"single segment trailing slash", "https://cdn.example.com/token/", "https://cdn.example.com/…/data"},
		{"dots only", "https://cdn.example.com/a/..", "https://cdn.example.com/…/data"},
		{"odd characters", "https://cdn.example.com/a/b%20c;x=1,äö.ts", "https://cdn.example.com/…/bcx1.ts"},
		{"newline injection", "https://cdn.example.com/a/b%0Aurl=evil.ts", "https://cdn.example.com/…/burlevil.ts"},
		{"long name keeps extension", "https://cdn.example.com/a/" + long + ".m3u8", "https://cdn.example.com/…/" + long[:59] + ".m3u8"},
		{"long name without extension", "https://cdn.example.com/a/" + long, "https://cdn.example.com/…/" + long[:64]},
		{"uppercase scheme", "HTTPS://cdn.example.com/a/b.ts", "https://cdn.example.com/…/b.ts"},
		{"invalid", "http://[::1/x?token=secret", "(invalid URL)"},
		{"no host", "/just/a/path?token=secret", "(invalid URL)"},
		{"opaque", "mailto:someone@example.com", "(invalid URL)"},
		{"empty", "", "(invalid URL)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := LogURL(tt.in)
			if got != tt.want {
				t.Errorf("LogURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
			for _, secret := range []string{"secret", "token=", "user:pw", "0123456789abcdef"} {
				if strings.Contains(got, secret) {
					t.Errorf("LogURL(%q) = %q leaks %q", tt.in, got, secret)
				}
			}
		})
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want Reason
	}{
		{"nil", nil, ""},
		{"guard", fmt.Errorf("x: %w", ErrBlocked), ReasonGuard},
		{"guard inside dial", &dialError{fmt.Errorf("%w: 10.0.0.1", ErrBlocked)}, ReasonGuard},
		{"dial", &dialError{errors.New("refused")}, ReasonConnect},
		{"connect timeout", &dialError{ErrConnectTimeout}, ReasonConnect},
		{"connect timeout unwrapped", ErrConnectTimeout, ReasonConnect},
		{"header timeout", ErrHeaderTimeout, ReasonTimeout},
		{"read timeout", ErrReadTimeout, ReasonTimeout},
		{"deadline", context.DeadlineExceeded, ReasonTimeout},
		{"os deadline", os.ErrDeadlineExceeded, ReasonTimeout},
		{"net timeout", &net.OpError{Op: "read", Err: timeoutErr{}}, ReasonTimeout},
		{"redirects", &Error{URL: "http://cdn.example.com/", Err: ErrTooManyRedirects}, ReasonRedirects},
		{"url size", ErrURLTooLong, ReasonSize},
		{"body size", ErrTooLarge, ReasonSize},
		{"unknown authority", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, ReasonTLS},
		{"hostname", x509.HostnameError{Host: "cdn.example.com", Certificate: &x509.Certificate{}}, ReasonTLS},
		{"record header", tls.RecordHeaderError{Msg: "tls: first record does not look like a TLS handshake"}, ReasonTLS},
		{"alert", &net.OpError{Op: "remote error", Err: tls.AlertError(40)}, ReasonTLS},
		{"tls message", fmt.Errorf("wrapped: %w", errors.New("tls: server selected unsupported protocol version 301")), ReasonTLS},
		{"canceled", context.Canceled, ReasonOther},
		{"other", errors.New("unexpected EOF"), ReasonOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.err); got != tt.want {
				t.Errorf("Classify(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

func TestErrorStatus(t *testing.T) {
	tests := []struct {
		err  error
		want int
	}{
		{ErrHeaderTimeout, http.StatusGatewayTimeout},
		{ErrReadTimeout, http.StatusGatewayTimeout},
		{context.DeadlineExceeded, http.StatusGatewayTimeout},
		{&dialError{ErrConnectTimeout}, http.StatusBadGateway},
		{&dialError{errors.New("refused")}, http.StatusBadGateway},
		{ErrBlocked, http.StatusBadGateway},
		{ErrTooManyRedirects, http.StatusBadGateway},
		{ErrTooLarge, http.StatusBadGateway},
		{errors.New("tls: bad"), http.StatusBadGateway},
		{errors.New("other"), http.StatusBadGateway},
	}
	for _, tt := range tests {
		if got := ErrorStatus(tt.err); got != tt.want {
			t.Errorf("ErrorStatus(%v) = %d, want %d", tt.err, got, tt.want)
		}
	}
}
