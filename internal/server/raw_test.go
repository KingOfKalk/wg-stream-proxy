// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel/tunneltest"
)

// radioHeader is the header block of the radio fixtures, written as
// Shoutcast and Icecast do (lower case, no space after the colon).
const radioHeader = "icy-name:Test Radio\r\nicy-genre:Test\r\nicy-br:128\r\nicy-metaint:8192\r\n" +
	"icy-pub:0\r\ncontent-type:audio/mpeg\r\n\r\n"

// radioFirst is the first body write: more than hls.SniffLen, so type
// detection of auto channels does not wait for the lockstep chunks.
var radioFirst = binaryBody('a', 1024)

// radioChunk is the i-th lockstep chunk, much smaller than any buffer.
func radioChunk(i int) string { return strings.Repeat(string(rune('b'+i)), 100) }

// radio is a fixture radio server. It answers on the hijacked connection
// with a raw status line per path, like Icecast ("HTTP/1.0 200 OK") or a
// Shoutcast v1 server ("ICY 200 OK"), and a body without length: first
// radioFirst, then one radioChunk per signal on the stream's next
// channel, then, once next is closed, data until the connection fails.
type radio struct {
	status  map[string]string // path -> status line
	streams chan *radioStream
	stop    chan struct{}
	once    sync.Once
}

// radioStream is one request to the radio fixture.
type radioStream struct {
	path   string
	remote string
	header http.Header
	next   chan struct{} // one more chunk; closed: stream without pause
	gone   chan struct{} // closed when the stream ended
}

func newRadio(status map[string]string) *radio {
	return &radio{status: status, streams: make(chan *radioStream, 16), stop: make(chan struct{})}
}

// close ends all streams.
func (f *radio) close() { f.once.Do(func() { close(f.stop) }) }

func (f *radio) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	status, ok := f.status[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	c, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s := &radioStream{
		path: r.URL.Path, remote: r.RemoteAddr, header: r.Header.Clone(),
		next: make(chan struct{}), gone: make(chan struct{}),
	}
	defer close(s.gone)
	defer func() { _ = c.Close() }()
	done := make(chan struct{})
	defer close(done)
	go func() {
		// Hijacked connections are not closed by the server; a blocked
		// write ends here when the test is over.
		select {
		case <-f.stop:
			_ = c.Close()
		case <-done:
		}
	}()
	f.streams <- s

	if _, err := io.WriteString(c, status+"\r\n"+radioHeader+radioFirst); err != nil {
		return
	}
	for i := 0; ; i++ {
		select {
		case _, ok := <-s.next:
			if !ok {
				endless := binaryBody('z', 4096)
				for {
					if _, err := io.WriteString(c, endless); err != nil {
						return
					}
				}
			}
			if _, err := io.WriteString(c, radioChunk(i)); err != nil {
				return
			}
		case <-f.stop:
			return
		}
	}
}

// readString reads exactly n bytes.
func readString(t *testing.T, r io.Reader, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	return string(b)
}

// TestRunRawStreams is the raw part of L3 (spec 3.3): an Icecast-like
// (HTTP/1.0) and a Shoutcast v1 (ICY) radio, each through an in-process
// tunnel and direct.
func TestRunRawStreams(t *testing.T) {
	status := map[string]string{"/;": "ICY 200 OK", "/stream": "HTTP/1.0 200 OK"}
	f := newRadio(status)
	t.Cleanup(f.close)
	peer := tunneltest.Start(t, tunneltest.Options{Handler: f})
	peer.SetHost("radio.test", tunneltest.ServerAddr4)
	directSrv := httptest.NewServer(f)
	t.Cleanup(directSrv.Close)

	dir := writeConfig(t, map[string]string{
		"config.toml":     allowLoopback,
		"tunnels/gr.conf": peer.ClientConfig(),
		"channels.d/gr.toml": `
[[channel]]
name = "Shoutcast GR"
url = "http://radio.test/;"

[[channel]]
name = "Icecast GR"
url = "http://radio.test/stream"
[channel.upstream]
type = "raw"
`,
		"channels.d/radio.toml": `
tunnel = "direct"

[[channel]]
name = "Shoutcast Direct"
url = "` + directSrv.URL + `/;"

[[channel]]
name = "Icecast Direct"
url = "` + directSrv.URL + `/stream"
[channel.upstream]
type = "raw"
`,
	})
	e := startRun(t, dir, 0)
	// Runs before the Run cleanup: no fixture stream outlives the test.
	t.Cleanup(f.close)

	tests := []struct {
		id     string
		path   string
		tunnel bool
	}{
		{"shoutcast-gr", "/;", true},
		{"icecast-gr", "/stream", true},
		{"shoutcast-direct", "/;", false},
		{"icecast-direct", "/stream", false},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			directBefore := e.direct.used()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, e.url+"/c/"+tt.id, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Icy-MetaData", "1")
			resp, err := e.client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			s := <-f.streams

			if s.path != tt.path {
				t.Errorf("upstream path %q, want %q", s.path, tt.path)
			}
			if got := s.header.Get("Icy-MetaData"); got != "1" {
				t.Errorf("upstream Icy-MetaData = %q, want 1", got)
			}
			if tt.tunnel {
				if ap, err := netip.ParseAddrPort(s.remote); err != nil || ap.Addr() != tunneltest.ClientAddr4 {
					t.Errorf("upstream request from %s, want the tunnel address %s", s.remote, tunneltest.ClientAddr4)
				}
				if n := e.direct.used(); n != directBefore {
					t.Errorf("tunnel channel used the direct network: %d calls", n-directBefore)
				}
			}

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d, want 200", resp.StatusCode)
			}
			for k, want := range map[string]string{
				"Icy-Name": "Test Radio", "Icy-Genre": "Test", "Icy-Br": "128",
				"Icy-Metaint": "8192", "Icy-Pub": "0", "Content-Type": "audio/mpeg",
			} {
				if got := resp.Header.Get(k); got != want {
					t.Errorf("header %s = %q, want %q", k, got, want)
				}
			}
			if resp.ContentLength != -1 {
				t.Errorf("ContentLength = %d, want none", resp.ContentLength)
			}

			if got := readString(t, resp.Body, len(radioFirst)); got != radioFirst {
				t.Fatalf("first bytes %q", got)
			}
			// Lockstep: the fixture sends the next chunk only after the
			// player got the previous one, so every chunk must be flushed.
			for i := range 3 {
				s.next <- struct{}{}
				if got, want := readString(t, resp.Body, len(radioChunk(i))), radioChunk(i); got != want {
					t.Fatalf("chunk %d = %q, want %q", i, got, want)
				}
			}
			// No size limit: keep streaming well beyond any buffer.
			close(s.next)
			if n, err := io.CopyN(io.Discard, resp.Body, 1<<20); err != nil {
				t.Fatalf("endless stream ended after %d bytes: %v", n, err)
			}

			// The player goes away: the upstream request is cancelled.
			_ = resp.Body.Close()
			<-s.gone
		})
	}

	t.Run("HEAD", func(t *testing.T) {
		resp, body := do(t, e.client, http.MethodHead, e.url+"/c/shoutcast-gr", nil)
		s := <-f.streams
		close(s.next)
		<-s.gone // the upstream body is closed at once (spec 3.4)
		if resp.StatusCode != http.StatusOK || body != "" || resp.Header.Get("Icy-Name") != "Test Radio" {
			t.Errorf("HEAD: status %d, icy-name %q, body %q", resp.StatusCode, resp.Header.Get("Icy-Name"), body)
		}
	})

	t.Run("resources use the HTTP-only pool", func(t *testing.T) {
		// An ICY answer to a /r/ resource is not an HTTP response.
		res := newSigner(t).Path("shoutcast-direct", directSrv.URL+"/;")
		resp, _ := do(t, e.client, http.MethodGet, e.url+res, nil)
		s := <-f.streams
		close(s.next)
		<-s.gone
		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("status %d, want 502", resp.StatusCode)
		}
	})
}
