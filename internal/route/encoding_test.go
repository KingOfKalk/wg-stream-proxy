// SPDX-License-Identifier: AGPL-3.0-or-later

package route

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// gzipped returns s gzip-compressed.
func gzipped(s string) string {
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	_, _ = io.WriteString(zw, s)
	_ = zw.Close()
	return b.String()
}

// encodedResponse is an upstream response with the given
// Content-Encoding values, status and body.
func encodedResponse(status int, codings []string, body string) *http.Response {
	h := http.Header{
		"Content-Type":   {"application/octet-stream"},
		"Content-Length": {strconv.Itoa(len(body))},
		"Etag":           {`"v1"`},
	}
	if codings != nil {
		h["Content-Encoding"] = codings
	}
	u, _ := url.Parse("https://cdn.example.com/secretdir/list?token=abc")
	return &http.Response{
		StatusCode:    status,
		Header:        h,
		ContentLength: int64(len(body)),
		Body:          io.NopCloser(strings.NewReader(body)),
		Request:       &http.Request{URL: u},
	}
}

// TestDecodeBody checks spec 3.4: a Content-Encoding the transport did
// not undo is decoded (gzip) or refused, never passed on unseen.
func TestDecodeBody(t *testing.T) {
	const plain = "[playlist]\nFile1=http://radio.example/stream\n"
	gz := gzipped(plain)
	tests := []struct {
		name    string
		status  int
		codings []string
		body    string
		// want
		err     error
		decoded bool // body decoded, headers as after transparent decompression
	}{
		{name: "none", body: plain},
		{name: "empty value", codings: []string{""}, body: plain},
		{name: "identity", codings: []string{"identity"}, body: plain},
		{name: "identity any case and spaces", codings: []string{" Identity ,IDENTITY"}, body: plain},
		{name: "gzip", codings: []string{"gzip"}, body: gz, decoded: true},
		{name: "x-gzip", codings: []string{"x-gzip"}, body: gz, decoded: true},
		{name: "gzip upper case", codings: []string{"GZIP"}, body: gz, decoded: true},
		{name: "gzip with identity", codings: []string{"identity, gzip"}, body: gz, decoded: true},
		{name: "gzip and identity lines", codings: []string{"gzip", "identity"}, body: gz, decoded: true},
		{name: "gzip on 404", status: http.StatusNotFound, codings: []string{"gzip"}, body: gz, decoded: true},
		{name: "br", codings: []string{"br"}, body: "\x1b\x00\x00", err: ErrContentEncoding},
		{name: "deflate", codings: []string{"deflate"}, body: "x", err: ErrContentEncoding},
		{name: "zstd", codings: []string{"zstd"}, body: "x", err: ErrContentEncoding},
		{name: "compress", codings: []string{"compress"}, body: "x", err: ErrContentEncoding},
		{name: "gzip twice", codings: []string{"gzip, gzip"}, body: gz, err: ErrContentEncoding},
		{name: "gzip and br lines", codings: []string{"gzip", "br"}, body: gz, err: ErrContentEncoding},
		{name: "gzip with a parameter", codings: []string{"gzip;q=1"}, body: gz, err: ErrContentEncoding},
		{name: "partial gzip", status: http.StatusPartialContent, codings: []string{"gzip"}, body: gz[:10], err: ErrEncodedPartial},
		{name: "partial br", status: http.StatusPartialContent, codings: []string{"br"}, body: "x", err: ErrEncodedPartial},
		{name: "partial identity", status: http.StatusPartialContent, codings: []string{"identity"}, body: plain},
		{name: "none value", codings: []string{"none"}, body: plain},
		{name: "none any case with identity", codings: []string{"None, identity", "NONE"}, body: plain},
		{name: "gzip with none", codings: []string{"none, gzip"}, body: gz, decoded: true},
		{name: "partial none", status: http.StatusPartialContent, codings: []string{"none"}, body: plain},
		{name: "none with a parameter", codings: []string{"none;x=1"}, body: "x", err: ErrContentEncoding},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := tt.status
			if status == 0 {
				status = http.StatusOK
			}
			resp := encodedResponse(status, tt.codings, tt.body)
			err := DecodeBody(resp)
			if !errors.Is(err, tt.err) || (err == nil) != (tt.err == nil) {
				t.Fatalf("DecodeBody error = %v, want %v", err, tt.err)
			}
			if err != nil {
				if Classify(err) != ReasonOther || ErrorStatus(err) != http.StatusBadGateway {
					t.Errorf("Classify = %q, ErrorStatus = %d, want other and 502", Classify(err), ErrorStatus(err))
				}
				for _, secret := range []string{"secretdir", "token"} {
					if strings.Contains(err.Error(), secret) {
						t.Errorf("error %q leaks %q", err, secret)
					}
				}
				return
			}
			if v, ok := resp.Header["Content-Encoding"]; ok && tt.decoded {
				t.Errorf("Content-Encoding = %q after decoding, want absent", v)
			}
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(b) != plain {
				t.Errorf("body = %q, want %q", b, plain)
			}
			if err := resp.Body.Close(); err != nil {
				t.Errorf("close: %v", err)
			}
			dst := http.Header{}
			CopyResponseHeader(dst, resp, false)
			if tt.decoded {
				if !resp.Uncompressed || resp.ContentLength != -1 {
					t.Errorf("Uncompressed %v, ContentLength %d, want true and -1", resp.Uncompressed, resp.ContentLength)
				}
				for _, k := range []string{"Content-Length", "Etag", "Content-Encoding"} {
					if v, ok := dst[k]; ok {
						t.Errorf("client header %s = %q after decoding, want absent", k, v)
					}
				}
				return
			}
			if resp.Uncompressed || dst.Get("Content-Length") != strconv.Itoa(len(plain)) || dst.Get("Etag") == "" {
				t.Errorf("unchanged body: Uncompressed %v, client headers %v", resp.Uncompressed, dst)
			}
		})
	}
}

func TestDecodeBodyErrors(t *testing.T) {
	gz := gzipped(strings.Repeat("#EXTM3U\n", 100))
	tests := []struct {
		name, body string
		wantErr    bool
	}{
		{name: "empty body", body: ""},
		{name: "not gzip", body: "#EXTM3U\n", wantErr: true},
		{name: "truncated", body: gz[:len(gz)-6], wantErr: true},
		{name: "bad checksum", body: gz[:len(gz)-8] + "\x00\x00\x00\x00" + gz[len(gz)-4:], wantErr: true},
		{name: "trailing garbage", body: gz + "garbage", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := encodedResponse(http.StatusOK, []string{"gzip"}, tt.body)
			if err := DecodeBody(resp); err != nil {
				t.Fatalf("DecodeBody: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			_, err := io.ReadAll(resp.Body)
			if (err != nil) != tt.wantErr {
				t.Errorf("read error = %v, want error %v", err, tt.wantErr)
			}
			if err != nil && Classify(err) != ReasonOther {
				t.Errorf("Classify(%v) = %q, want other", err, Classify(err))
			}
		})
	}
}

// TestDecodeBodyLazy: the gzip header is read with the first Read, not
// by DecodeBody, so a HEAD request closes the body unread (spec 3.4).
func TestDecodeBodyLazy(t *testing.T) {
	rc := &countingBody{r: strings.NewReader(gzipped("x"))}
	resp := encodedResponse(http.StatusOK, []string{"gzip"}, "")
	resp.Body = rc
	if err := DecodeBody(resp); err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if rc.reads != 0 || !rc.closed {
		t.Errorf("reads %d, closed %v, want 0 and true", rc.reads, rc.closed)
	}
}

// TestDecodeBodyLimit: the size limit of a playlist counts decoded bytes
// (spec 3.4), so a small gzip body cannot make the proxy hold more.
func TestDecodeBodyLimit(t *testing.T) {
	const limit = 1 << 20
	bomb := gzipped(strings.Repeat("\x00", 64<<20))
	resp := encodedResponse(http.StatusOK, []string{"gzip"}, bomb)
	if err := DecodeBody(resp); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := ReadPlaylist(resp.Body, limit); !errors.Is(err, ErrTooLarge) {
		t.Errorf("ReadPlaylist of a %d byte bomb: %v, want ErrTooLarge", len(bomb), err)
	}
}

type countingBody struct {
	r      io.Reader
	reads  int
	closed bool
}

func (c *countingBody) Read(p []byte) (int, error) {
	c.reads++
	return c.r.Read(p)
}

func (c *countingBody) Close() error {
	c.closed = true
	return nil
}

// FuzzDecodeBody: whatever the Content-Encoding, DecodeBody either fails
// or leaves a body that is not encoded: no coding other than identity
// is left in the header, and a decoded body drops its length.
func FuzzDecodeBody(f *testing.F) {
	for _, s := range []string{"", "gzip", "x-gzip", "identity", "br", "gzip, identity", " GZip ", "gzip,,", ",", "identity;q=0", "gzip\x00", "none", "NONE, gzip"} {
		f.Add(s, 200, gzipped("#EXTM3U\n"))
	}
	f.Add("gzip", 206, "x")
	f.Fuzz(func(t *testing.T, coding string, status int, body string) {
		resp := encodedResponse(status, []string{coding}, body)
		err := DecodeBody(resp)
		if err != nil {
			if !errors.Is(err, ErrContentEncoding) && !errors.Is(err, ErrEncodedPartial) {
				t.Fatalf("unexpected error %v", err)
			}
			return
		}
		for _, v := range resp.Header.Values("Content-Encoding") {
			for c := range strings.SplitSeq(v, ",") {
				if c = strings.TrimSpace(c); c != "" && !strings.EqualFold(c, "identity") && !strings.EqualFold(c, "none") {
					t.Fatalf("coding %q left on the response", c)
				}
			}
		}
		if resp.Uncompressed && (resp.ContentLength != -1 || resp.Header.Get("Content-Length") != "") {
			t.Fatalf("decoded body keeps its length")
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	})
}

// TestDecodeBodyTransportPartial: a 206 the transport decoded has a
// Content-Range of encoded bytes too.
func TestDecodeBodyTransportPartial(t *testing.T) {
	resp := encodedResponse(http.StatusPartialContent, nil, "#EXTM3U\n")
	resp.Uncompressed = true
	if err := DecodeBody(resp); !errors.Is(err, ErrEncodedPartial) {
		t.Errorf("DecodeBody = %v, want ErrEncodedPartial", err)
	}
	resp = encodedResponse(http.StatusOK, nil, "#EXTM3U\n")
	resp.Uncompressed = true
	if err := DecodeBody(resp); err != nil || !resp.Uncompressed {
		t.Errorf("DecodeBody = %v, Uncompressed %v, want nil and true", err, resp.Uncompressed)
	}
}
