// SPDX-License-Identifier: AGPL-3.0-or-later

package playertest

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

// manualClock is an injected clock that only moves in advance.
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// vodPlaylist is a media playlist as ffmpeg's hls muxer writes it.
const vodPlaylist = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:2
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-KEY:METHOD=AES-128,URI="key.bin",IV=0x00000000000000000000000000000001
#EXTINF:2.000000,
seg0.ts
#EXTINF:2.000000,
seg1.ts
#EXTINF:2.000000,
seg2.ts
#EXTINF:1.500000,
seg3.ts
#EXTINF:2.000000,
seg4.ts
#EXT-X-ENDLIST
`

const masterPlaylist = "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nindex.m3u8\n"

func testFS() fstest.MapFS {
	f := fstest.MapFS{
		"a/index.m3u8":  {Data: []byte(vodPlaylist)},
		"a/master.m3u8": {Data: []byte(masterPlaylist)},
		"a/key.bin":     {Data: []byte("test-only-key-01")},
		"b/index.m3u8":  {Data: []byte(vodPlaylist)},
		"r/tone.mp3":    {Data: []byte("0123456789abcdefghijklmnopqrstuvwxyz")},
	}
	for i := range 5 {
		f["a/seg"+strconv.Itoa(i)+".ts"] = &fstest.MapFile{Data: []byte("segment " + strconv.Itoa(i) + ";")}
	}
	return f
}

func newTestFixtures(t *testing.T) (*Fixtures, *manualClock, *httptest.Server) {
	t.Helper()
	clock := &manualClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	f := New(testFS(), Options{Now: clock.Now, Window: 2, MetaInt: 10})
	srv := httptest.NewServer(f)
	t.Cleanup(func() {
		f.Close()
		srv.Close()
	})
	return f, clock, srv
}

func get(t *testing.T, srv *httptest.Server, path string, h http.Header) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range h {
		req.Header[k] = v
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(b)
}

// liveHeader is the header of the live form of vodPlaylist.
func liveHeader(seq int) string {
	return "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:" + strconv.Itoa(seq) + "\n" +
		"#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\",IV=0x00000000000000000000000000000001\n"
}

func TestLiveWindow(t *testing.T) {
	seg := func(d string, i int) string { return "#EXTINF:" + d + ",\nseg" + strconv.Itoa(i) + ".ts\n" }
	tests := []struct {
		after time.Duration // since the first request of the fixture
		want  string
	}{
		{0, liveHeader(0) + seg("2.000000", 0) + seg("2.000000", 1)},
		{1999 * time.Millisecond, liveHeader(0) + seg("2.000000", 0) + seg("2.000000", 1)},
		{2 * time.Second, liveHeader(1) + seg("2.000000", 1) + seg("2.000000", 2)},
		// seg3 is 1.5 s long: published 3.5 s after the start.
		{3499 * time.Millisecond, liveHeader(1) + seg("2.000000", 1) + seg("2.000000", 2)},
		{3500 * time.Millisecond, liveHeader(2) + seg("2.000000", 2) + seg("1.500000", 3)},
		// The last segment is out: the event ended.
		{5500 * time.Millisecond, liveHeader(3) + seg("1.500000", 3) + seg("2.000000", 4) + "#EXT-X-ENDLIST\n"},
		{time.Hour, liveHeader(3) + seg("1.500000", 3) + seg("2.000000", 4) + "#EXT-X-ENDLIST\n"},
	}
	_, clock, srv := newTestFixtures(t)
	var elapsed time.Duration
	for _, tt := range tests {
		clock.advance(tt.after - elapsed)
		elapsed = tt.after
		resp, body := get(t, srv, "/live/a/index.m3u8", nil)
		if resp.StatusCode != http.StatusOK || body != tt.want {
			t.Errorf("after %v: status %d, body\n%s\nwant\n%s", tt.after, resp.StatusCode, body, tt.want)
		}
		if ct, cc := resp.Header.Get("Content-Type"), resp.Header.Get("Cache-Control"); ct != "application/vnd.apple.mpegurl" || cc != "no-cache" {
			t.Errorf("after %v: Content-Type %q, Cache-Control %q", tt.after, ct, cc)
		}
	}
}

func TestLiveOriginPerFixture(t *testing.T) {
	_, clock, srv := newTestFixtures(t)
	// Any request of a fixture starts its clock, not only playlists.
	get(t, srv, "/live/a/master.m3u8", nil)
	clock.advance(4 * time.Second)
	if _, body := get(t, srv, "/live/a/index.m3u8", nil); !strings.HasPrefix(body, liveHeader(2)) {
		t.Errorf("fixture a after 4 s:\n%s\nwant media sequence 2", body)
	}
	if _, body := get(t, srv, "/live/b/index.m3u8", nil); !strings.HasPrefix(body, liveHeader(0)) {
		t.Errorf("fixture b on its first request:\n%s\nwant media sequence 0", body)
	}
}

func TestStatic(t *testing.T) {
	tests := []struct {
		name, path string
		header     http.Header
		status     int
		ctype      string
		body       string
	}{
		{"vod playlist as is", "/vod/a/index.m3u8", nil, http.StatusOK, "application/vnd.apple.mpegurl", vodPlaylist},
		{"live master as is", "/live/a/master.m3u8", nil, http.StatusOK, "application/vnd.apple.mpegurl", masterPlaylist},
		{"segment", "/live/a/seg1.ts", nil, http.StatusOK, "video/mp2t", "segment 1;"},
		{"key", "/vod/a/key.bin", nil, http.StatusOK, "application/octet-stream", "test-only-key-01"},
		{"range", "/vod/a/seg0.ts", http.Header{"Range": {"bytes=0-6"}}, http.StatusPartialContent, "video/mp2t", "segment"},
		{"missing file", "/vod/a/seg9.ts", nil, http.StatusNotFound, "", ""},
		{"unknown mode", "/other/a/seg0.ts", nil, http.StatusNotFound, "", ""},
		{"directory", "/vod/a", nil, http.StatusNotFound, "", ""},
		{"raw ts", "/raw/a", nil, http.StatusOK, "video/mp2t", "segment 0;segment 1;segment 2;segment 3;segment 4;"},
		{"raw without playlist", "/raw/r", nil, http.StatusNotFound, "", ""},
	}
	f, _, srv := newTestFixtures(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := get(t, srv, tt.path, tt.header)
			if resp.StatusCode != tt.status {
				t.Fatalf("status %d, want %d", resp.StatusCode, tt.status)
			}
			if tt.status == http.StatusNotFound {
				return
			}
			if ct := resp.Header.Get("Content-Type"); ct != tt.ctype {
				t.Errorf("Content-Type %q, want %q", ct, tt.ctype)
			}
			if body != tt.body {
				t.Errorf("body %q, want %q", body, tt.body)
			}
		})
	}
	reqs := f.Requests()
	if len(reqs) != len(tests) || reqs[0] != "/vod/a/index.m3u8" {
		t.Errorf("Requests() = %v, want the %d test paths in order", reqs, len(tests))
	}
}

// readStream reads n bytes of an endless response.
func readStream(t *testing.T, srv *httptest.Server, path string, h http.Header, n int) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = h
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b := make([]byte, n)
	if _, err := io.ReadFull(resp.Body, b); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	return resp, b
}

func TestIcecast(t *testing.T) {
	const tone = "0123456789abcdefghijklmnopqrstuvwxyz"
	loop := strings.Repeat(tone, 4)
	_, _, srv := newTestFixtures(t)

	t.Run("without metadata", func(t *testing.T) {
		resp, b := readStream(t, srv, "/icecast/r/tone.mp3", nil, len(loop))
		if string(b) != loop {
			t.Errorf("stream %q, want the file in a loop", b)
		}
		if ct, name, mi := resp.Header.Get("Content-Type"), resp.Header.Get("Icy-Name"), resp.Header.Get("Icy-Metaint"); ct != "audio/mpeg" || name == "" || mi != "" {
			t.Errorf("Content-Type %q, icy-name %q, icy-metaint %q", ct, name, mi)
		}
		if resp.ContentLength != -1 {
			t.Errorf("Content-Length %d, want none", resp.ContentLength)
		}
	})

	t.Run("with metadata", func(t *testing.T) {
		resp, b := readStream(t, srv, "/icecast/r/tone.mp3", http.Header{"Icy-Metadata": {"1"}}, 200)
		if mi := resp.Header.Get("Icy-Metaint"); mi != "10" {
			t.Fatalf("icy-metaint %q, want 10", mi)
		}
		// Every 10 audio bytes a metadata block: length byte (x16), text.
		var audio bytes.Buffer
		titles := 0
		for len(b) > 10 {
			audio.Write(b[:10])
			n := int(b[10]) * 16
			if len(b) < 11+n {
				break
			}
			meta := string(bytes.TrimRight(b[11:11+n], "\x00"))
			if meta != "StreamTitle='Test tone';" {
				t.Fatalf("metadata %q", meta)
			}
			titles++
			b = b[11+n:]
		}
		if titles == 0 || !strings.HasPrefix(loop, audio.String()) {
			t.Errorf("%d metadata blocks, audio %q, want the file in a loop", titles, audio.String())
		}
	})
}

func TestShoutcast(t *testing.T) {
	_, _, srv := newTestFixtures(t)
	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := io.WriteString(c, "GET /shoutcast/r/tone.mp3 HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(c)
	status, err := r.ReadString('\n')
	if err != nil || status != "ICY 200 OK\r\n" {
		t.Fatalf("status line %q, %v; want ICY 200 OK", status, err)
	}
	var header []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
		header = append(header, strings.TrimSpace(line))
	}
	if !strings.Contains(strings.Join(header, "\n"), "content-type:audio/mpeg") {
		t.Errorf("header %q lacks content-type:audio/mpeg", header)
	}
	b := make([]byte, 72)
	if _, err := io.ReadFull(r, b); err != nil {
		t.Fatal(err)
	}
	if want := strings.Repeat("0123456789abcdefghijklmnopqrstuvwxyz", 2); string(b) != want {
		t.Errorf("stream %q, want %q", b, want)
	}
}

// TestCloseEndsShoutcast checks that Close ends hijacked streams, which
// http.Server.Close does not.
func TestCloseEndsShoutcast(t *testing.T) {
	f := New(testFS(), Options{})
	srv := httptest.NewServer(f)
	defer srv.Close()
	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := io.WriteString(c, "GET /shoutcast/r/tone.mp3 HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(c)
	if _, err := r.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Logf("stream ended with %v", err)
	}
}
