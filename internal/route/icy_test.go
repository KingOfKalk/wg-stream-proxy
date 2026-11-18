// SPDX-License-Identifier: AGPL-3.0-or-later

package route

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// chunkConn is a net.Conn whose reads return the given chunks one per
// Read (a chunk larger than the caller's buffer is split), then err.
type chunkConn struct {
	net.Conn // nil: only Read and Close are used
	chunks   [][]byte
	err      error
	reads    int
}

func (c *chunkConn) Read(p []byte) (int, error) {
	c.reads++
	if len(c.chunks) == 0 {
		return 0, c.err
	}
	n := copy(p, c.chunks[0])
	if n == len(c.chunks[0]) {
		c.chunks = c.chunks[1:]
	} else {
		c.chunks[0] = c.chunks[0][n:]
	}
	return n, nil
}

func (c *chunkConn) Close() error { return nil }

// split cuts s at the given offsets.
func split(s string, at ...int) [][]byte {
	var out [][]byte
	prev := 0
	for _, i := range at {
		out = append(out, []byte(s[prev:i]))
		prev = i
	}
	return append(out, []byte(s[prev:]))
}

// bytesOf turns s into one chunk per byte.
func bytesOf(s string) [][]byte {
	out := make([][]byte, len(s))
	for i := range s {
		out[i] = []byte{s[i]}
	}
	return out
}

// readAllWith reads c to the end with buffers of size n and returns the
// data and the final error.
func readAllWith(c net.Conn, n int) (string, error) {
	var b strings.Builder
	buf := make([]byte, n)
	for {
		k, err := c.Read(buf)
		b.Write(buf[:k])
		if err != nil {
			return b.String(), err
		}
	}
}

const (
	icyResponse  = "ICY 200 OK\r\nicy-name:Radio\r\ncontent-type:audio/mpeg\r\n\r\nAUDIO"
	icyRewritten = "HTTP/1.0 200 OK\r\nicy-name:Radio\r\ncontent-type:audio/mpeg\r\n\r\nAUDIO"
	httpResponse = "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"
)

var errBroken = errors.New("broken pipe")

func TestICYConn(t *testing.T) {
	tests := []struct {
		name   string
		chunks [][]byte
		err    error
		bufLen int
		want   string
	}{
		{"ICY in one read", split(icyResponse), io.EOF, 512, icyRewritten},
		{"ICY one byte per read", bytesOf(icyResponse), io.EOF, 512, icyRewritten},
		{"ICY split in the prefix", split(icyResponse, 2, 3), io.EOF, 512, icyRewritten},
		{"ICY prefix alone, then the rest", split(icyResponse, 4), io.EOF, 512, icyRewritten},
		{"ICY read with a one-byte buffer", split(icyResponse), io.EOF, 1, icyRewritten},
		{"ICY read with a three-byte buffer", bytesOf(icyResponse), io.EOF, 3, icyRewritten},
		{"ICY 404", split("ICY 404 Resource Not Found\r\n\r\n"), io.EOF, 512, "HTTP/1.0 404 Resource Not Found\r\n\r\n"},
		{"HTTP/1.1 unchanged", split(httpResponse), io.EOF, 512, httpResponse},
		{"HTTP/1.1 one byte per read", bytesOf(httpResponse), io.EOF, 512, httpResponse},
		{"HTTP/1.0 unchanged", split("HTTP/1.0 200 OK\r\n\r\nAUDIO"), io.EOF, 512, "HTTP/1.0 200 OK\r\n\r\nAUDIO"},
		{"TLS record unchanged", split("\x16\x03\x01\x02\x00ICY "), io.EOF, 512, "\x16\x03\x01\x02\x00ICY "},
		{"lower-case icy unchanged", split("icy 200 OK\r\n\r\n"), io.EOF, 512, "icy 200 OK\r\n\r\n"},
		{"ICY without space unchanged", split("ICY200 OK"), io.EOF, 512, "ICY200 OK"},
		{"diverges after three bytes", bytesOf("ICX 200"), io.EOF, 512, "ICX 200"},
		{"only a later response is ICY", split(httpResponse + "ICY 200 OK\r\n\r\n"), io.EOF, 512, httpResponse + "ICY 200 OK\r\n\r\n"},
		{"partial prefix then EOF", split("IC"), io.EOF, 512, "IC"},
		{"partial prefix then error", bytesOf("ICY"), errBroken, 512, "ICY"},
		{"exact prefix then EOF", split("ICY "), io.EOF, 512, "HTTP/1.0 "},
		{"nothing then EOF", nil, io.EOF, 512, ""},
		{"nothing then error", nil, errBroken, 512, ""},
		{"empty reads before data", [][]byte{{}, {}, []byte("ICY 200 OK\r\n")}, io.EOF, 512, "HTTP/1.0 200 OK\r\n"},
		{"data then error", split(icyResponse, 7), errBroken, 4, icyRewritten},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newICYConn(&chunkConn{chunks: tt.chunks, err: tt.err})
			got, err := readAllWith(c, tt.bufLen)
			if got != tt.want {
				t.Errorf("read %q, want %q", got, tt.want)
			}
			if !errors.Is(err, tt.err) {
				t.Errorf("error %v, want %v", err, tt.err)
			}
		})
	}
}

func TestICYConnZeroLengthRead(t *testing.T) {
	inner := &chunkConn{chunks: split(icyResponse), err: io.EOF}
	c := newICYConn(inner)
	if n, err := c.Read(nil); n != 0 || err != nil {
		t.Fatalf("Read(nil) = %d, %v; want 0, nil", n, err)
	}
	if inner.reads != 0 {
		t.Errorf("Read(nil) read from the connection")
	}
	if got, _ := readAllWith(c, 512); got != icyRewritten {
		t.Errorf("read %q, want %q", got, icyRewritten)
	}
}

// The wrapper reads at most the four bytes of "ICY " itself; after that,
// reads go straight to the connection with the caller's buffer.
func TestICYConnPassesReadsThroughAfterDecision(t *testing.T) {
	inner := &chunkConn{chunks: split(httpResponse+"body-0123456789", len(httpResponse)), err: io.EOF}
	c := newICYConn(inner)
	got, err := readAllWith(c, 512)
	if got != httpResponse+"body-0123456789" || err != io.EOF {
		t.Fatalf("read %q, %v", got, err)
	}
	if inner.reads != 4 {
		t.Errorf("%d reads from the connection, want 4 (first 4 bytes, rest of the head, body, EOF)", inner.reads)
	}
}

// The wrapper is usable by Go's HTTP parser.
func TestICYConnHTTPParser(t *testing.T) {
	for _, chunks := range [][][]byte{split(icyResponse), bytesOf(icyResponse)} {
		c := newICYConn(&chunkConn{chunks: chunks, err: io.EOF})
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatalf("ReadResponse: %v", err)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK || resp.ProtoMinor != 0 ||
			resp.Header.Get("Icy-Name") != "Radio" || string(body) != "AUDIO" {
			t.Errorf("got %d HTTP/1.%d icy-name %q body %q", resp.StatusCode, resp.ProtoMinor, resp.Header.Get("Icy-Name"), body)
		}
	}
}

func FuzzICYConn(f *testing.F) {
	f.Add([]byte(icyResponse), uint8(1), uint8(3))
	f.Add([]byte(httpResponse), uint8(0), uint8(1))
	f.Add([]byte("ICY "), uint8(2), uint8(1))
	f.Add([]byte("ICY"), uint8(1), uint8(2))
	f.Add([]byte("\x16\x03\x01"), uint8(5), uint8(7))
	f.Add([]byte{}, uint8(0), uint8(0))
	f.Fuzz(func(t *testing.T, data []byte, chunk, bufLen uint8) {
		// Chunk sizes 1..chunk+1 rotating, buffers of 1..256 bytes.
		var chunks [][]byte
		for i, rest := 0, data; len(rest) > 0; i++ {
			n := min(len(rest), 1+i%(int(chunk)+1))
			chunks = append(chunks, rest[:n])
			rest = rest[n:]
		}
		c := newICYConn(&chunkConn{chunks: chunks, err: io.EOF})
		got, err := readAllWith(c, 1+int(bufLen))
		if err != io.EOF {
			t.Fatalf("error %v, want EOF", err)
		}
		want := string(data)
		if rest, ok := strings.CutPrefix(want, "ICY "); ok {
			want = "HTTP/1.0 " + rest
		}
		if got != want {
			t.Fatalf("read %q from %q, want %q", got, data, want)
		}
	})
}

// icyHandler answers like a Shoutcast v1 server: a raw "ICY 200 OK"
// response on the hijacked connection, closed after the body.
func icyHandler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = io.WriteString(c, icyResponse)
	}
}

func TestFetchICY(t *testing.T) {
	tests := []struct {
		name string
		icy  bool
		ok   bool
	}{
		{"ICY transport", true, true},
		{"normal transport refuses ICY", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _, _ := newFixture(t, icyHandler(t))
			resp, err := r.Fetch(context.Background(), Request{URL: "http://cdn.example.com/;", ICY: tt.icy})
			if !tt.ok {
				if err == nil {
					_ = resp.Body.Close()
					t.Fatal("Fetch succeeded, want a malformed-response error")
				}
				if Classify(err) != ReasonOther || ErrorStatus(err) != http.StatusBadGateway {
					t.Errorf("Classify = %s, ErrorStatus = %d; want other, 502", Classify(err), ErrorStatus(err))
				}
				return
			}
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			body := readClose(t, resp)
			if resp.StatusCode != http.StatusOK || resp.Header.Get("Icy-Name") != "Radio" || body != "AUDIO" {
				t.Errorf("got %d icy-name %q body %q", resp.StatusCode, resp.Header.Get("Icy-Name"), body)
			}
		})
	}
}

// countingNet counts dials of fixture connections.
type countingNet struct {
	*fakeNet
	dials atomic.Int64
}

func (n *countingNet) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	c, err := n.fakeNet.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	n.dials.Add(1)
	return c, nil
}

// The ICY transport has no connection pool: HLS connections of the
// normal transport are never wrapped, and every ICY request dials.
func TestICYTransportSeparatePool(t *testing.T) {
	_, fixture, _ := newFixture(t, recorder(make(chan seen, 8)))
	n := &countingNet{fakeNet: fixture}
	r := testRoute(n, newFakeClock())
	t.Cleanup(r.CloseIdleConnections)

	fetch := func(icy bool) {
		t.Helper()
		resp := mustFetch(t, r, Request{URL: "http://cdn.example.com/seg.ts", ICY: icy})
		if body := readClose(t, resp); body != "ok" {
			t.Fatalf("body %q", body)
		}
	}
	fetch(false)
	fetch(false)
	if d := n.dials.Load(); d != 1 {
		t.Fatalf("normal transport: %d dials, want 1 (keep-alive)", d)
	}
	fetch(true)
	fetch(true)
	if d := n.dials.Load(); d != 3 {
		t.Fatalf("after 2 ICY requests: %d dials, want 3 (one each, no keep-alive)", d)
	}
}

// A Shoutcast v1 server may answer an HTTP request (status page) with
// keep-alive and the next one, on the same port, with a raw ICY stream.
// The ICY transport must not reuse a connection for that second request:
// the wrapper only rewrites the first response of a connection (#51).
func TestFetchICYAfterKeepAliveResponse(t *testing.T) {
	icy := icyHandler(t)
	r, _, _ := newFixture(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/stream" {
			icy(w, req)
			return
		}
		_, _ = io.WriteString(w, "status")
	})

	resp := mustFetch(t, r, Request{URL: "http://cdn.example.com/7.html", ICY: true})
	if body := readClose(t, resp); body != "status" {
		t.Fatalf("status page body %q", body)
	}
	resp, err := r.Fetch(context.Background(), Request{URL: "http://cdn.example.com/stream", ICY: true})
	if err != nil {
		t.Fatalf("ICY stream after a keep-alive response: %v", err)
	}
	if body := readClose(t, resp); body != "AUDIO" || resp.Header.Get("Icy-Name") != "Radio" {
		t.Errorf("ICY stream: body %q, icy-name %q", body, resp.Header.Get("Icy-Name"))
	}
}
