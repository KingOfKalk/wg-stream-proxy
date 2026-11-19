// SPDX-License-Identifier: AGPL-3.0-or-later

package route

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// fixtureHosts resolve to documentation addresses, which the production
// guard treats as public.
var fixtureHosts = map[string][]netip.Addr{
	"cdn.example.com":   addrs("203.0.113.10"),
	"other.example.net": addrs("198.51.100.20"),
}

// fixtureNet resolves hosts and connects every dial to target, a loopback
// fixture server, whatever the resolved address.
func fixtureNet(target string, hosts map[string][]netip.Addr) *fakeNet {
	return &fakeNet{
		hosts: hosts,
		fallback: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", target)
		},
	}
}

// newFixture starts h and returns a route "gr" (production guard) whose
// network sends every connection to it.
func newFixture(t *testing.T, h http.HandlerFunc) (*Route, *fakeNet, *fakeClock) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	n := fixtureNet(srv.Listener.Addr().String(), fixtureHosts)
	clk := newFakeClock()
	r := testRoute(n, clk)
	t.Cleanup(r.CloseIdleConnections)
	return r, n, clk
}

func mustFetch(t *testing.T, r *Route, req Request) *http.Response {
	t.Helper()
	resp, err := r.Fetch(context.Background(), req)
	if err != nil {
		t.Fatalf("Fetch(%s): %v", req.URL, err)
	}
	return resp
}

func readClose(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

type seen struct {
	method string
	host   string
	path   string
	header http.Header
}

// recorder answers 200 "ok" and reports every request on ch.
func recorder(ch chan<- seen) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ch <- seen{r.Method, r.Host, r.URL.Path, r.Header.Clone()}
		_, _ = io.WriteString(w, "ok")
	}
}

func TestFetchRequestHeaders(t *testing.T) {
	referer := http.Header{"Referer": {"https://www.example.com/"}}
	tests := []struct {
		name   string
		path   string
		header http.Header
		client http.Header
		want   http.Header
		absent []string
	}{
		{
			name:   "merged headers",
			path:   "/live/seg.ts",
			header: http.Header{"User-Agent": {"Channel-UA/2"}, "Referer": {"https://www.example.com/"}, "X-Custom": {"1"}},
			want:   http.Header{"User-Agent": {"Channel-UA/2"}, "Referer": {"https://www.example.com/"}, "X-Custom": {"1"}},
		},
		{
			name:   "user agent from upstream settings",
			path:   "/seg.ts",
			header: referer,
			want:   http.Header{"User-Agent": {"test-agent/1.0"}, "Referer": {"https://www.example.com/"}},
		},
		{
			name: "only range headers from the client",
			path: "/seg.ts",
			client: http.Header{
				"Range": {"bytes=0-99"}, "If-Range": {`"v1"`}, "Icy-Metadata": {"1"},
				"Cookie": {"a=b"}, "Authorization": {"Basic eDp5"}, "X-Forwarded-For": {"192.0.2.1"},
				"Accept-Language": {"de"}, "Referer": {"http://player.example/"}, "User-Agent": {"Kodi/21"},
			},
			want:   http.Header{"Range": {"bytes=0-99"}, "If-Range": {`"v1"`}, "Icy-Metadata": {"1"}, "User-Agent": {"test-agent/1.0"}},
			absent: []string{"Cookie", "Authorization", "X-Forwarded-For", "Accept-Language", "Referer"},
		},
		{
			name:   "no range for m3u8",
			path:   "/live/index.m3u8",
			client: http.Header{"Range": {"bytes=0-"}, "Icy-Metadata": {"1"}},
			want:   http.Header{"Icy-Metadata": {"1"}},
			absent: []string{"Range"},
		},
		{
			name:   "no range for m3u, case-insensitive",
			path:   "/radio/list.M3U",
			client: http.Header{"Range": {"bytes=0-"}},
			absent: []string{"Range"},
		},
		{
			name:   "no range for m3u8 with query",
			path:   "/index.m3u8?token=abc",
			client: http.Header{"Range": {"bytes=0-"}},
			absent: []string{"Range"},
		},
		{
			name:   "no range for mpd",
			path:   "/live/manifest.MPD?t=1",
			client: http.Header{"Range": {"bytes=0-"}},
			absent: []string{"Range"},
		},
		{
			name:   "query does not make a playlist",
			path:   "/seg.ts?type=.m3u8",
			client: http.Header{"Range": {"bytes=10-"}},
			want:   http.Header{"Range": {"bytes=10-"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := make(chan seen, 1)
			r, _, _ := newFixture(t, recorder(ch))
			resp := mustFetch(t, r, Request{URL: "http://cdn.example.com" + tt.path, Header: tt.header, Client: tt.client})
			if body := readClose(t, resp); body != "ok" {
				t.Errorf("body = %q", body)
			}
			got := <-ch
			if got.method != http.MethodGet {
				t.Errorf("method = %s, want GET", got.method)
			}
			for k, v := range tt.want {
				if !slices.Equal(got.header[k], v) {
					t.Errorf("header %s = %q, want %q", k, got.header[k], v)
				}
			}
			for _, k := range tt.absent {
				if v, ok := got.header[k]; ok {
					t.Errorf("header %s = %q, want absent", k, v)
				}
			}
		})
	}
}

func TestFetchEmptyUserAgentSendsNone(t *testing.T) {
	ch := make(chan seen, 1)
	srv := httptest.NewServer(recorder(ch))
	defer srv.Close()
	up := testUpstream
	up.UserAgent = ""
	r := newRoute("gr", fixtureNet(srv.Listener.Addr().String(), fixtureHosts), Options{Upstream: up, Clock: newFakeClock()})
	defer r.CloseIdleConnections()

	readClose(t, mustFetch(t, r, Request{URL: "http://cdn.example.com/"}))
	if v, ok := (<-ch).header["User-Agent"]; ok {
		t.Errorf("User-Agent = %q, want none", v)
	}
}

// hopHandler: /hop/N redirects to /hop/N-1 until /hop/0 answers "done".
// Even hops use an absolute URL on the other host, odd hops a relative one.
func hopHandler(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/hop/"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if n == 0 {
		_, _ = io.WriteString(w, "done")
		return
	}
	loc := fmt.Sprintf("/hop/%d", n-1)
	if n%2 == 0 {
		host := "other.example.net"
		if r.Host == host {
			host = "cdn.example.com"
		}
		loc = "http://" + host + loc
	}
	http.Redirect(w, r, loc, http.StatusFound)
}

func TestFetchRedirectLimit(t *testing.T) {
	r, _, _ := newFixture(t, hopHandler)

	resp := mustFetch(t, r, Request{URL: "http://cdn.example.com/hop/10"})
	if body := readClose(t, resp); body != "done" {
		t.Errorf("body after 10 redirects = %q, want done", body)
	}
	if got := resp.Request.URL.String(); got != "http://other.example.net/hop/0" {
		t.Errorf("final URL = %s", got)
	}

	resp, err := r.Fetch(context.Background(), Request{URL: "http://cdn.example.com/hop/11"})
	if resp != nil {
		t.Errorf("response after 11 redirects = %v, want nil", resp.Status)
	}
	if !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("Fetch error = %v, want ErrTooManyRedirects", err)
	}
	if got := Classify(err); got != ReasonRedirects {
		t.Errorf("Classify = %q, want %q", got, ReasonRedirects)
	}
	if got := ErrorStatus(err); got != http.StatusBadGateway {
		t.Errorf("ErrorStatus = %d, want 502", got)
	}
}

func TestFetchRedirectHeaders(t *testing.T) {
	tests := []struct {
		name        string
		header      http.Header
		wantReferer []string
	}{
		{"no automatic referer", nil, nil},
		{"configured referer kept", http.Header{"Referer": {"https://www.example.com/"}}, []string{"https://www.example.com/"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := make(chan seen, 2)
			r, _, _ := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
				ch <- seen{r.Method, r.Host, r.URL.Path, r.Header.Clone()}
				if r.URL.Path == "/a.ts" {
					http.Redirect(w, r, "http://other.example.net/b.m3u8", http.StatusMovedPermanently)
					return
				}
				_, _ = io.WriteString(w, "#EXTM3U\n")
			})
			readClose(t, mustFetch(t, r, Request{
				URL:    "http://cdn.example.com/a.ts",
				Header: tt.header,
				Client: http.Header{"Range": {"bytes=0-"}, "Icy-Metadata": {"1"}},
			}))
			first, second := <-ch, <-ch
			if first.header.Get("Range") != "bytes=0-" {
				t.Errorf("first hop Range = %q", first.header.Get("Range"))
			}
			if second.host != "other.example.net" || second.path != "/b.m3u8" {
				t.Fatalf("second hop = %s%s", second.host, second.path)
			}
			if v, ok := second.header["Range"]; ok {
				t.Errorf("Range sent to playlist after redirect: %q", v)
			}
			if got := second.header.Get("Icy-Metadata"); got != "1" {
				t.Errorf("second hop Icy-Metadata = %q", got)
			}
			if got := second.header.Get("User-Agent"); got != "test-agent/1.0" {
				t.Errorf("second hop User-Agent = %q", got)
			}
			if got := second.header["Referer"]; !slices.Equal(got, tt.wantReferer) {
				t.Errorf("second hop Referer = %q, want %q", got, tt.wantReferer)
			}
		})
	}
}

func TestFetchRedirectErrors(t *testing.T) {
	long := "http://cdn.example.com/" + strings.Repeat("a", MaxURLLen)
	tests := []struct {
		name       string
		location   string
		wantErr    error
		wantReason Reason
	}{
		{"location too long", long, ErrURLTooLong, ReasonSize},
		{"unsupported scheme", "ftp://cdn.example.com/x", errBadRedirect, ReasonOther},
		{"unparsable location", "http://[::1/x?token=secret", errBadRedirect, ReasonOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _, _ := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", tt.location)
				w.WriteHeader(http.StatusFound)
			})
			_, err := r.Fetch(context.Background(), Request{URL: "http://cdn.example.com/start"})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Fetch error = %v, want %v", err, tt.wantErr)
			}
			if got := Classify(err); got != tt.wantReason {
				t.Errorf("Classify = %q, want %q", got, tt.wantReason)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Errorf("error leaks the Location query: %v", err)
			}
		})
	}
}

func TestFetchRedirectWithoutLocationReturned(t *testing.T) {
	for _, code := range []int{http.StatusFound, http.StatusNotModified, http.StatusMultipleChoices} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			r, _, _ := newFixture(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) })
			resp := mustFetch(t, r, Request{URL: "http://cdn.example.com/x"})
			readClose(t, resp)
			if resp.StatusCode != code {
				t.Errorf("status = %d, want %d", resp.StatusCode, code)
			}
			if got := ClientStatus(resp.StatusCode); got != http.StatusBadGateway {
				t.Errorf("ClientStatus(%d) = %d, want 502", code, got)
			}
		})
	}
}

func TestFetchURLValidation(t *testing.T) {
	maxURL := "http://cdn.example.com/" + strings.Repeat("a", MaxURLLen-len("http://cdn.example.com/"))
	tests := []struct {
		name       string
		url        string
		wantErr    error
		wantReason Reason
	}{
		{"max length", maxURL, nil, ""},
		{"too long", maxURL + "a", ErrURLTooLong, ReasonSize},
		{"unsupported scheme", "ftp://cdn.example.com/x", errInvalidURL, ReasonOther},
		{"relative", "/live.m3u8", errInvalidURL, ReasonOther},
		{"no host", "http:///live.m3u8", errInvalidURL, ReasonOther},
		{"unparsable", "http://[::1/x", errInvalidURL, ReasonOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, n, _ := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})
			resp, err := r.Fetch(context.Background(), Request{URL: tt.url})
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Fetch: %v", err)
				}
				readClose(t, resp)
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Fetch error = %v, want %v", err, tt.wantErr)
			}
			if got := Classify(err); got != tt.wantReason {
				t.Errorf("Classify = %q, want %q", got, tt.wantReason)
			}
			if d := n.Dialed(); len(d) != 0 {
				t.Errorf("dials = %q, want none", d)
			}
		})
	}
}

func TestFetchStatusPassedThrough(t *testing.T) {
	for _, code := range []int{200, 206, 404, 410, 500, 503} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			r, _, _ := newFixture(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) })
			resp := mustFetch(t, r, Request{URL: "http://cdn.example.com/x"})
			readClose(t, resp)
			if resp.StatusCode != code {
				t.Errorf("status = %d, want %d", resp.StatusCode, code)
			}
		})
	}
}

func TestFetchHeaderTimeout(t *testing.T) {
	entered := make(chan struct{})
	r, _, clk := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	})
	res := make(chan error, 1)
	go func() {
		resp, err := r.Fetch(context.Background(), Request{URL: "http://cdn.example.com/slow.m3u8"})
		if resp != nil {
			_ = resp.Body.Close()
		}
		res <- err
	}()
	<-entered
	clk.Advance(testUpstream.HeaderTimeout)
	err := <-res
	if !errors.Is(err, ErrHeaderTimeout) {
		t.Fatalf("Fetch error = %v, want ErrHeaderTimeout", err)
	}
	if got := Classify(err); got != ReasonTimeout {
		t.Errorf("Classify = %q, want %q", got, ReasonTimeout)
	}
	if got := ErrorStatus(err); got != http.StatusGatewayTimeout {
		t.Errorf("ErrorStatus = %d, want 504", got)
	}
}

// chunks writes "a", then one more byte per step, then returns.
func chunks(steps <-chan string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "a")
		w.(http.Flusher).Flush()
		for {
			select {
			case s, ok := <-steps:
				if !ok {
					return
				}
				_, _ = io.WriteString(w, s)
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			}
		}
	}
}

func readByte(r io.Reader) <-chan error {
	ch := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(r, make([]byte, 1))
		ch <- err
	}()
	return ch
}

func TestFetchReadTimeout(t *testing.T) {
	steps := make(chan string)
	r, _, clk := newFixture(t, chunks(steps))
	resp := mustFetch(t, r, Request{URL: "http://cdn.example.com/radio"})
	defer func() { _ = resp.Body.Close() }()
	if err := <-readByte(resp.Body); err != nil {
		t.Fatalf("first read: %v", err)
	}

	res := readByte(resp.Body)
	clk.WaitActive(1)
	clk.Advance(testUpstream.ReadTimeout)
	err := <-res
	if !errors.Is(err, ErrReadTimeout) {
		t.Fatalf("read error = %v, want ErrReadTimeout", err)
	}
	if got := Classify(err); got != ReasonTimeout {
		t.Errorf("Classify = %q, want %q", got, ReasonTimeout)
	}
}

func TestFetchReadTimerResetsOnEveryRead(t *testing.T) {
	steps := make(chan string)
	r, _, clk := newFixture(t, chunks(steps))
	resp := mustFetch(t, r, Request{URL: "http://cdn.example.com/radio"})
	defer func() { _ = resp.Body.Close() }()
	if err := <-readByte(resp.Body); err != nil {
		t.Fatalf("first read: %v", err)
	}
	// Two gaps of 2/3 read_timeout each: more than read_timeout in total,
	// but never that long without data.
	gap := testUpstream.ReadTimeout * 2 / 3
	for i := range 2 {
		res := readByte(resp.Body)
		clk.WaitActive(1)
		clk.Advance(gap)
		steps <- "b"
		if err := <-res; err != nil {
			t.Fatalf("read %d: %v", i+2, err)
		}
	}
	// Time spent outside Read (a slow client) does not count.
	clk.Advance(testUpstream.ReadTimeout * 2)
	close(steps)
	if rest, err := io.ReadAll(resp.Body); err != nil || len(rest) != 0 {
		t.Errorf("rest = %q, %v; want EOF", rest, err)
	}
}

func TestFetchClientDisconnectCancelsUpstream(t *testing.T) {
	upstreamDone := make(chan struct{})
	r, _, _ := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		_, _ = io.WriteString(w, "a")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := r.Fetch(ctx, Request{URL: "http://cdn.example.com/radio"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := <-readByte(resp.Body); err != nil {
		t.Fatalf("first read: %v", err)
	}
	cancel()
	<-upstreamDone // the fixture saw the upstream request end
	if err := <-readByte(resp.Body); err == nil {
		t.Errorf("read after cancel succeeded")
	}
}

func TestFetchGzipTransparentlyDecompressed(t *testing.T) {
	const playlist = "#EXTM3U\n#EXTINF:6,\nseg.ts\n"
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = io.WriteString(zw, playlist)
	_ = zw.Close()
	r, _, _ := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Length", strconv.Itoa(gz.Len()))
		_, _ = w.Write(gz.Bytes())
	})
	resp := mustFetch(t, r, Request{URL: "http://cdn.example.com/index.m3u8"})
	if body := readClose(t, resp); body != playlist {
		t.Errorf("body = %q, want %q", body, playlist)
	}
	dst := http.Header{}
	CopyResponseHeader(dst, resp, false)
	if got := dst.Get("Content-Type"); got != "application/vnd.apple.mpegurl" {
		t.Errorf("Content-Type = %q", got)
	}
	for _, k := range []string{"Content-Length", "Etag", "Content-Encoding"} {
		if v, ok := dst[k]; ok {
			t.Errorf("%s = %q after decompression, want absent", k, v)
		}
	}
}

func TestFetchTLSError(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	defer srv.Close()
	r := testRoute(fixtureNet(srv.Listener.Addr().String(), fixtureHosts), newFakeClock())
	defer r.CloseIdleConnections()

	_, err := r.Fetch(context.Background(), Request{URL: "https://cdn.example.com/live.m3u8"})
	if err == nil {
		t.Fatal("Fetch with an untrusted certificate succeeded")
	}
	if got := Classify(err); got != ReasonTLS {
		t.Errorf("Classify(%v) = %q, want %q", err, got, ReasonTLS)
	}
}

func TestFetchHTTP2(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Proto)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	r := testRoute(fixtureNet(srv.Listener.Addr().String(), fixtureHosts), newFakeClock())
	defer r.CloseIdleConnections()
	r.transport.TLSClientConfig = &tls.Config{RootCAs: srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs, ServerName: "example.com"}

	resp := mustFetch(t, r, Request{URL: "https://cdn.example.com/"})
	if body := readClose(t, resp); body != "HTTP/2.0" {
		t.Errorf("protocol = %q, want HTTP/2.0", body)
	}
}

func TestFetchErrorReducesURL(t *testing.T) {
	r, _, _ := newFixture(t, func(http.ResponseWriter, *http.Request) {})
	_, err := r.Fetch(context.Background(), Request{URL: "http://user:pw@missing.example.com/s3cr3t-id/live.m3u8?token=secret#frag"})
	if err == nil {
		t.Fatal("Fetch of an unknown host succeeded")
	}
	msg := err.Error()
	if !strings.Contains(msg, "http://missing.example.com/…/live.m3u8") {
		t.Errorf("error %q does not name the URL", msg)
	}
	for _, s := range []string{"secret", "s3cr3t", "token", "pw", "frag"} {
		if strings.Contains(msg, s) {
			t.Errorf("error %q leaks %q", msg, s)
		}
	}
	var e *Error
	if !errors.As(err, &e) || e.URL != "http://missing.example.com/…/live.m3u8" {
		t.Errorf("Error.URL = %v", e)
	}
	if got := Classify(err); got != ReasonConnect {
		t.Errorf("Classify = %q, want %q", got, ReasonConnect)
	}
}
