// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
)

// channelsTOML is the channel file of the direct tests.
const channelsTOML = `
tunnel = "direct"
group = "TV"

[defaults.upstream]
headers = { User-Agent = "test-agent", Referer = "https://referer.example/" }

[[channel]]
name = "News"
url = "{UP}/live/master.m3u8"

[[channel]]
name = "Off"
url = "{UP}/live/master.m3u8"
enabled = false

[[channel]]
name = "Radio"
url = "{UP}/radio"

[[channel]]
name = "Forced raw"
url = "{UP}/cdn/v1/master.m3u8"
[channel.upstream]
type = "raw"

[[channel]]
name = "Forced HLS"
url = "{UP}/odd/playlist.bin"
[channel.upstream]
type = "hls"

[[channel]]
name = "Partial"
url = "{UP}/live/playlist"

[[channel]]
name = "Always partial"
url = "{UP}/live/partial"

[[channel]]
name = "Status"
url = "{UP}/status"

[[channel]]
name = "Hang"
url = "{UP}/hang"
`

func newTVEnv(t *testing.T) *directEnv {
	t.Helper()
	e := newDirectEnv(t, allowLoopback, channelsTOML)
	hlsFixture(e.up, e.upURL)
	return e
}

func TestNewRequiresDependencies(t *testing.T) {
	cfg := loadConfig(t, nil)
	routes, err := route.NewSet(route.Host(), nil, route.Options{})
	if err != nil {
		t.Fatal(err)
	}
	signer := newSigner(t)
	tests := map[string]Options{
		"no config": {Routes: routes, Signer: signer},
		"no routes": {Config: cfg, Signer: signer},
		"no signer": {Config: cfg, Routes: routes},
	}
	for name, opts := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := New(opts); err == nil {
				t.Error("New() = nil error, want error")
			}
		})
	}
	if _, err := New(Options{Config: cfg, Routes: routes, Signer: signer}); err != nil {
		t.Errorf("New() without logger: %v", err)
	}
}

func TestHealthz(t *testing.T) {
	e := newTVEnv(t)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		resp, body := do(t, e.client, method, e.proxy.URL+"/healthz", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s /healthz: status %d", method, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
			t.Errorf("Content-Type = %q", ct)
		}
		want := "wg-stream-proxy 1.2.3\nsource: https://github.com/KingOfKalk/wg-stream-proxy\n"
		if method == http.MethodHead {
			want = ""
		}
		if body != want {
			t.Errorf("%s /healthz body = %q, want %q", method, body, want)
		}
	}
}

func TestPlaylistGolden(t *testing.T) {
	e := newTVEnv(t)
	h := http.Header{"X-Forwarded-Proto": {"https"}, "X-Forwarded-Host": {"tv.example.org"}}
	resp, body := e.get(t, "/playlist.m3u8", h)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "audio/x-mpegurl; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	checkGolden(t, "playlist-direct", body)

	resp, body = do(t, e.client, http.MethodHead, e.proxy.URL+"/playlist.m3u8", h)
	if resp.StatusCode != http.StatusOK || body != "" {
		t.Errorf("HEAD: status %d, body %q", resp.StatusCode, body)
	}
	if resp.ContentLength <= 0 {
		t.Errorf("HEAD: Content-Length %d, want the list length", resp.ContentLength)
	}

	_, body = e.get(t, "/playlist.m3u8?group=TV", h)
	if !strings.Contains(body, "/c/news") {
		t.Errorf("?group=TV lacks the TV channels:\n%s", body)
	}
	_, body = e.get(t, "/playlist.m3u8?group=tv", h)
	if body != "#EXTM3U\n" {
		t.Errorf("?group=tv = %q, want only the header (exact match)", body)
	}
}

// TestGlobalDefaults: channels.d/_defaults.toml applies to every channel
// file; an empty value removes a key at file or channel level, for
// User-Agent also [upstream] user_agent (spec 5.4).
func TestGlobalDefaults(t *testing.T) {
	e := newDirectEnvFiles(t, "[upstream]\nuser_agent = \"base-agent\"\n"+allowLoopback, map[string]string{
		"channels.d/_defaults.toml": `
[defaults.upstream]
headers = { user-agent = "global-agent", Referer = "https://global.example/" }

[defaults.playlist]
attrs = { catchup = "default" }

[defaults.playlist.extra.KODIPROP]
"inputstream" = "inputstream.adaptive"
`,
		"channels.d/tv.toml": `
tunnel = "direct"
group = "TV"

[[channel]]
name = "News"
url = "{UP}/live/master.m3u8"

[[channel]]
name = "Plain"
url = "{UP}/plain/master.m3u8"

[channel.upstream]
headers = { REFERER = "" }

[channel.playlist]
attrs = { catchup = "" }

[channel.playlist.extra.KODIPROP]
"inputstream" = ""
`,
		"channels.d/radio.toml": `
tunnel = "direct"
group = "Radio"

[defaults.upstream]
headers = { USER-AGENT = "" }

[defaults.playlist]
attrs = { radio = "true" }

[defaults.playlist.extra.KODIPROP]
"inputstream" = ""

[[channel]]
name = "Radio"
url = "{UP}/radio"
`,
	})
	hlsFixture(e.up, e.upURL)
	e.up.add("/plain/master.m3u8", file{ctype: "application/vnd.apple.mpegurl", body: "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nseg.ts\n"})
	e.up.add("/radio", file{ctype: "audio/mpeg", body: "mp3"})

	h := http.Header{"X-Forwarded-Proto": {"https"}, "X-Forwarded-Host": {"tv.example.org"}}
	resp, body := e.get(t, "/playlist.m3u8", h)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	checkGolden(t, "playlist-global-defaults", body)

	tests := []struct {
		path, upstream, userAgent, referer string
	}{
		{"/c/news.m3u8", "/live/master.m3u8", "global-agent", "https://global.example/"},
		{"/c/plain.m3u8", "/plain/master.m3u8", "global-agent", ""},
		{"/c/radio", "/radio", "", "https://global.example/"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if resp, _ := e.get(t, tt.path, nil); resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d", resp.StatusCode)
			}
			reqs := e.up.requestsFor(tt.upstream)
			if len(reqs) == 0 {
				t.Fatalf("no upstream request for %s", tt.upstream)
			}
			r := reqs[len(reqs)-1]
			if r.UserAgent != tt.userAgent || r.Referer != tt.referer {
				t.Errorf("upstream User-Agent %q, Referer %q; want %q, %q", r.UserAgent, r.Referer, tt.userAgent, tt.referer)
			}
			// Removed headers are not sent at all, not sent empty.
			names := "," + r.Names + ","
			if (tt.userAgent == "") != !strings.Contains(names, ",User-Agent,") ||
				(tt.referer == "") != !strings.Contains(names, ",Referer,") {
				t.Errorf("upstream header names %q: removed headers must be absent, others present", r.Names)
			}
		})
	}
}

// checkGolden compares got with testdata/<name>.golden.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	golden := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("mismatch with %s (-update to rewrite)\n--- got\n%s\n--- want\n%s", golden, got, want)
	}
}

func TestBaseURL(t *testing.T) {
	local := &net.TCPAddr{IP: net.ParseIP("192.0.2.7"), Port: 2349}
	tests := []struct {
		name   string
		public string
		host   string
		header http.Header
		tls    bool
		want   string
	}{
		{name: "request host", host: "wsp.lan:2349", want: "http://wsp.lan:2349"},
		{name: "IPv6 host", host: "[2001:db8::1]:2349", want: "http://[2001:db8::1]:2349"},
		{name: "TLS", host: "wsp.lan", tls: true, want: "https://wsp.lan"},
		{
			name: "public base wins", public: "https://tv.example.org/", host: "wsp.lan:2349",
			header: http.Header{"X-Forwarded-Host": {"proxy.example"}}, want: "https://tv.example.org",
		},
		{name: "public base with path", public: "https://tv.example.org/wsp/", want: "https://tv.example.org/wsp"},
		{
			name: "forwarded proto and host", host: "10.0.0.2:2349",
			header: http.Header{"X-Forwarded-Proto": {"https"}, "X-Forwarded-Host": {"tv.example.org"}}, want: "https://tv.example.org",
		},
		{
			name: "forwarded lists use the first value", host: "10.0.0.2:2349",
			header: http.Header{"X-Forwarded-Proto": {"HTTPS, http"}, "X-Forwarded-Host": {" tv.example.org:8443 , other"}}, want: "https://tv.example.org:8443",
		},
		{
			name: "forwarded proto only", host: "wsp.lan:2349",
			header: http.Header{"X-Forwarded-Proto": {"https"}}, want: "https://wsp.lan:2349",
		},
		{
			name: "forwarded host only", host: "wsp.lan:2349",
			header: http.Header{"X-Forwarded-Host": {"tv.example.org"}}, want: "http://tv.example.org",
		},
		{
			name: "invalid forwarded proto ignored", host: "wsp.lan:2349",
			header: http.Header{"X-Forwarded-Proto": {"ftp"}}, want: "http://wsp.lan:2349",
		},
		{
			name: "invalid forwarded host ignored", host: "wsp.lan:2349",
			header: http.Header{"X-Forwarded-Host": {`evil"host`}}, want: "http://wsp.lan:2349",
		},
		{
			name: "forwarded host with path ignored", host: "wsp.lan:2349",
			header: http.Header{"X-Forwarded-Host": {"evil.example/x"}}, want: "http://wsp.lan:2349",
		},
		{name: "no host: local address", host: "", want: "http://192.0.2.7:2349"},
		{name: "invalid host: local address", host: "a b", want: "http://192.0.2.7:2349"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/playlist.m3u8", nil)
			r.Host = tt.host
			r.Header = tt.header
			if r.Header == nil {
				r.Header = http.Header{}
			}
			if !tt.tls {
				r.TLS = nil
			} else {
				r.TLS = &tls.ConnectionState{}
			}
			r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, net.Addr(local)))
			if got := baseURL(tt.public, r); got != tt.want {
				t.Errorf("baseURL() = %q, want %q", got, tt.want)
			}
		})
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = ""
	if got := baseURL("", r); got != "http://localhost" {
		t.Errorf("baseURL() without host and local address = %q, want http://localhost", got)
	}
}

func TestChannelHLS(t *testing.T) {
	e := newTVEnv(t)
	for _, path := range []string{"/c/news.m3u8", "/c/news"} {
		resp, body := e.get(t, path, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d: %s", path, resp.StatusCode, body)
		}
		for k, want := range map[string]string{
			"Content-Type":   "application/vnd.apple.mpegurl",
			"Cache-Control":  "no-cache",
			"Content-Length": strconv.Itoa(len(body)),
		} {
			if got := resp.Header.Get(k); got != want {
				t.Errorf("GET %s: %s = %q, want %q", path, k, got, want)
			}
		}
		uris := playlistURIs(body)
		if len(uris) != 4 {
			t.Fatalf("GET %s: %d URIs, want 4:\n%s", path, len(uris), body)
		}
		for _, u := range uris {
			if !strings.HasPrefix(u, e.proxy.URL+"/r/news/") {
				t.Errorf("URI %q is not a resource URL of news", u)
			}
		}
		// Unchanged lines stay byte for byte.
		if !strings.Contains(body, "#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,") || strings.Contains(body, "secret") {
			t.Errorf("unexpected rewrite:\n%s", body)
		}
	}
	// Upstream headers come from the channel config.
	reqs := e.up.requestsFor("/cdn/v1/master.m3u8")
	if len(reqs) == 0 {
		t.Fatal("upstream saw no master request")
	}
	for _, r := range reqs {
		if r.UserAgent != "test-agent" || r.Referer != "https://referer.example/" || r.Method != http.MethodGet {
			t.Errorf("upstream request %+v, want GET with the channel's User-Agent and Referer", r)
		}
	}
}

func TestCrawlDirect(t *testing.T) {
	e := newTVEnv(t)
	got, playlists := crawl(t, e.client, e.proxy.URL, "news", e.proxy.URL+"/c/news.m3u8")
	want := hlsFixture(newUpstream(), e.upURL)
	if playlists != 5 {
		t.Errorf("crawled %d playlists, want 5 (master, 2 variants, 2 renditions)", playlists)
	}
	if len(got) != len(want) {
		t.Errorf("crawled %d resources, want %d: %v", len(got), len(want), got)
	}
	for p, b := range want {
		if got[p] != b {
			t.Errorf("resource %s = %q, want %q", p, got[p], b)
		}
	}
	// The resource requests carry the channel's headers too.
	for _, r := range e.up.requests() {
		if r.UserAgent != "test-agent" {
			t.Errorf("upstream request %s without the channel's User-Agent: %q", r.Path, r.UserAgent)
		}
	}
	if strings.Contains(e.logs.String(), "secret") {
		t.Errorf("logs contain an upstream query string:\n%s", e.logs.String())
	}
}

func TestHead(t *testing.T) {
	e := newTVEnv(t)
	resp, body := do(t, e.client, http.MethodHead, e.proxy.URL+"/c/news.m3u8", nil)
	if resp.StatusCode != http.StatusOK || body != "" {
		t.Errorf("HEAD /c: status %d, body %q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/vnd.apple.mpegurl" {
		t.Errorf("HEAD /c: Content-Type %q", ct)
	}
	if r := e.up.requestsFor("/cdn/v1/master.m3u8"); len(r) != 1 || r[0].Method != http.MethodGet {
		t.Errorf("upstream requests %+v, want one GET", r)
	}

	seg := e.resource("news", e.upURL+"/cdn/v1/low/seg7.m4s")
	resp, body = do(t, e.client, http.MethodHead, seg, nil)
	if resp.StatusCode != http.StatusOK || body != "" {
		t.Errorf("HEAD /r: status %d, body %q", resp.StatusCode, body)
	}
	if resp.ContentLength != int64(len("low segment 7")) {
		t.Errorf("HEAD /r: Content-Length %d, want %d", resp.ContentLength, len("low segment 7"))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("HEAD /r: Content-Type %q", ct)
	}

	// A HEAD on a channel without a playlist Content-Type does not read
	// the body: the type comes from the headers only.
	e.up.add("/radio", file{ctype: "audio/mpeg", body: "ID3 audio"})
	resp, _ = do(t, e.client, http.MethodHead, e.proxy.URL+"/c/radio", nil)
	if ct := resp.Header.Get("Content-Type"); resp.StatusCode != http.StatusOK || ct != "audio/mpeg" {
		t.Errorf("HEAD raw: status %d, Content-Type %q", resp.StatusCode, ct)
	}
}

func TestRange(t *testing.T) {
	e := newTVEnv(t)
	seg := e.resource("news", e.upURL+"/cdn/v1/low/seg7.m4s")
	resp, body := do(t, e.client, http.MethodGet, seg, http.Header{"Range": {"bytes=4-10"}})
	if resp.StatusCode != http.StatusPartialContent || body != "segment" {
		t.Errorf("segment Range: status %d, body %q, want 206 %q", resp.StatusCode, body, "segment")
	}
	if cr := resp.Header.Get("Content-Range"); cr != "bytes 4-10/13" {
		t.Errorf("Content-Range = %q", cr)
	}
	if r := e.up.requestsFor("/cdn/v1/low/seg7.m4s"); len(r) != 1 || r[0].Range != "bytes=4-10" {
		t.Errorf("segment upstream requests %+v, want Range forwarded", r)
	}

	resp, body = e.get(t, "/c/news.m3u8", http.Header{"Range": {"bytes=0-9"}})
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(body, "#EXTM3U\n") {
		t.Errorf("playlist Range: status %d, body %q", resp.StatusCode, body)
	}
	for _, r := range e.up.requests() {
		if strings.HasSuffix(r.Path, ".m3u8") && r.Range != "" {
			t.Errorf("Range forwarded for playlist %s: %q", r.Path, r.Range)
		}
	}
}

func TestPlaylistPartialContent(t *testing.T) {
	e := newTVEnv(t)
	const pl = "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\nseg1.ts\n"
	e.up.add("/live/playlist", file{ctype: "application/x-mpegURL", body: pl})
	resp, body := e.get(t, "/c/partial", http.Header{"Range": {"bytes=0-9"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "/r/partial/") || resp.Header.Get("Content-Range") != "" {
		t.Errorf("want the whole rewritten playlist, got headers %v body %q", resp.Header, body)
	}
	reqs := e.up.requestsFor("/live/playlist")
	if len(reqs) != 2 || reqs[0].Range != "bytes=0-9" || reqs[1].Range != "" {
		t.Errorf("upstream requests %+v, want one with Range, then one without", reqs)
	}

	// An upstream that answers 206 even without Range fails.
	e.up.add("/live/partial", file{ctype: "application/vnd.apple.mpegurl", status: http.StatusPartialContent, body: pl})
	resp, _ = e.get(t, "/c/always-partial", http.Header{"Range": {"bytes=0-9"}})
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("206 after refetch: status %d, want 502", resp.StatusCode)
	}
}

func TestDetection(t *testing.T) {
	e := newTVEnv(t)
	// Configured raw: the playlist is piped unchanged.
	resp, body := e.get(t, "/c/forced-raw", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "low/index.m3u8\n") || strings.Contains(body, "/r/") {
		t.Errorf("forced raw: status %d, body %q", resp.StatusCode, body)
	}
	if resp.Header.Get("Content-Length") == "" {
		t.Error("forced raw: Content-Length not relayed")
	}
	// Configured hls: rewritten whatever the Content-Type.
	e.up.add("/odd/playlist.bin", file{ctype: "application/octet-stream", body: "seg.ts\n"})
	resp, body = e.get(t, "/c/forced-hls.m3u8", nil)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(body, e.proxy.URL+"/r/forced-hls/") {
		t.Errorf("forced hls: status %d, body %q", resp.StatusCode, body)
	}
	// Auto: raw bodies are piped with their headers.
	e.up.add("/radio", file{ctype: "audio/mpeg", body: "ID3 not a playlist", header: http.Header{"Icy-Name": {"Jazz"}, "X-Private": {"no"}}})
	resp, body = e.get(t, "/c/radio", http.Header{"Icy-Metadata": {"1"}, "Cookie": {"no"}})
	if resp.StatusCode != http.StatusOK || body != "ID3 not a playlist" {
		t.Errorf("raw: status %d, body %q", resp.StatusCode, body)
	}
	if resp.Header.Get("Icy-Name") != "Jazz" || resp.Header.Get("X-Private") != "" || resp.Header.Get("Content-Type") != "audio/mpeg" {
		t.Errorf("raw headers: %v", resp.Header)
	}
	if r := e.up.requestsFor("/radio"); len(r) == 0 || r[len(r)-1].Icy != "1" {
		t.Errorf("Icy-MetaData not forwarded: %+v", r)
	}
	// Short bodies are sniffed too.
	e.up.add("/radio", file{ctype: "text/plain", body: " \n#EXTM3U\nx.ts\n"})
	_, body = e.get(t, "/c/radio", nil)
	if !strings.Contains(body, "/r/radio/") {
		t.Errorf("sniffed playlist not rewritten: %q", body)
	}
	// An empty body is raw.
	e.up.add("/radio", file{ctype: "text/plain", body: ""})
	resp, body = e.get(t, "/c/radio", nil)
	if resp.StatusCode != http.StatusOK || body != "" {
		t.Errorf("empty body: status %d, body %q", resp.StatusCode, body)
	}
}

func TestNotFound(t *testing.T) {
	e := newTVEnv(t)
	valid := e.signer.Path("news", e.upURL+"/cdn/v1/low/seg7.m4s")
	parts := strings.Split(valid, "/") // "", "r", channel, sig, b64url, name
	flip := func(s string) string {
		if s[0] == 'A' {
			return "B" + s[1:]
		}
		return "A" + s[1:]
	}
	tampered := func(i int) string {
		p := append([]string(nil), parts...)
		p[i] = flip(p[i])
		return strings.Join(p, "/")
	}
	paths := map[string]string{
		"unknown channel":           "/c/nope",
		"unknown channel .m3u8":     "/c/nope.m3u8",
		"disabled channel":          "/c/off",
		"disabled channel .m3u8":    "/c/off.m3u8",
		"empty id":                  "/c/.m3u8",
		"nested":                    "/c/news/x",
		"tampered signature":        tampered(3),
		"tampered URL":              tampered(4),
		"other channel":             strings.Replace(valid, "/r/news/", "/r/radio/", 1),
		"disabled channel resource": e.signer.Path("off", e.upURL+"/cdn/v1/low/seg7.m4s"),
		"unknown channel resource":  e.signer.Path("ghost", e.upURL+"/cdn/v1/low/seg7.m4s"),
		"unsigned":                  "/r/news/" + parts[4] + "/seg7.m4s",
		"resource without name":     strings.Join(parts[:5], "/"),
		"unknown path":              "/favicon.ico",
	}
	for name, p := range paths {
		t.Run(name, func(t *testing.T) {
			resp, _ := e.get(t, p, nil)
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("GET %s: status %d, want 404", p, resp.StatusCode)
			}
		})
	}
	if n := len(e.up.requests()); n != 0 {
		t.Errorf("upstream saw %d requests, want none", n)
	}
	// The valid path works, and its name segment is ignored.
	resp, body := e.get(t, strings.Join(parts[:5], "/")+"/other.bin", nil)
	if resp.StatusCode != http.StatusOK || body != "low segment 7" {
		t.Errorf("valid resource: status %d, body %q", resp.StatusCode, body)
	}
	resp, _ = do(t, e.client, http.MethodPost, e.proxy.URL+"/c/news", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /c/news: status %d, want 405", resp.StatusCode)
	}
}

func TestUpstreamStatus(t *testing.T) {
	e := newTVEnv(t)
	tests := []struct {
		status int
		want   int
		body   bool // upstream body relayed
	}{
		{http.StatusOK, http.StatusOK, true},
		{http.StatusNoContent, http.StatusNoContent, false},
		{http.StatusForbidden, http.StatusForbidden, true},
		{http.StatusNotFound, http.StatusNotFound, true},
		{http.StatusInternalServerError, http.StatusBadGateway, false},
		{http.StatusServiceUnavailable, http.StatusBadGateway, false},
		{http.StatusMultipleChoices, http.StatusBadGateway, false},
	}
	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.status), func(t *testing.T) {
			e.up.add("/status", file{ctype: "application/octet-stream", status: tt.status, body: "upstream body"})
			for _, path := range []string{"/c/status", e.signer.Path("status", e.upURL+"/status")} {
				resp, body := e.get(t, path, nil)
				if resp.StatusCode != tt.want {
					t.Errorf("GET %s: status %d, want %d", path, resp.StatusCode, tt.want)
				}
				if got := body == "upstream body"; got != tt.body {
					t.Errorf("GET %s: body %q, relayed = %v, want %v", path, body, got, tt.body)
				}
			}
		})
	}
	if !strings.Contains(e.logs.String(), "upstream_status=500") {
		t.Errorf("upstream 5xx not logged:\n%s", e.logs.String())
	}
}

func TestUpstreamErrors(t *testing.T) {
	e := newDirectEnv(t, allowLoopback+"\n[upstream]\nheader_timeout = \"20ms\"\n", channelsTOML)
	e.up.add("/hang", file{hang: true})
	e.up.add("/radio", file{ctype: "application/vnd.apple.mpegurl", body: "#EXTM3U\n\x00\n"})
	big := "#EXTM3U\n" + strings.Repeat("#"+strings.Repeat("x", 1023)+"\n", 2049)
	e.up.add("/live/playlist", file{ctype: "application/vnd.apple.mpegurl", body: big})

	resp, _ := e.get(t, "/c/radio", nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("NUL playlist: status %d, want 502", resp.StatusCode)
	}
	resp, _ = e.get(t, "/c/partial", nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("playlist over 2 MiB: status %d, want 502", resp.StatusCode)
	}
	resp, _ = e.get(t, "/c/hang", nil)
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("header timeout: status %d, want 504", resp.StatusCode)
	}
	// A refused connection.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	resp, _ = e.get(t, e.signer.Path("news", "http://"+addr+"/x.ts?token=secret"), nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("connection refused: status %d, want 502", resp.StatusCode)
	}
	logs := e.logs.String()
	if !strings.Contains(logs, "reason=connect") || strings.Contains(logs, "secret") {
		t.Errorf("logs: want reason=connect and no query string:\n%s", logs)
	}
}

func TestProductionGuard(t *testing.T) {
	// No allow_private: the loopback fixture is blocked (spec 3.5).
	e := newDirectEnv(t, "", channelsTOML)
	hlsFixture(e.up, e.upURL)
	resp, _ := e.get(t, "/c/news.m3u8", nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status %d, want 502", resp.StatusCode)
	}
	if n := len(e.up.requests()); n != 0 {
		t.Errorf("upstream saw %d requests, want none", n)
	}
	if !strings.Contains(e.logs.String(), "reason=guard") {
		t.Errorf("guard refusal not logged:\n%s", e.logs.String())
	}
}

func TestClientDisconnectCancelsUpstream(t *testing.T) {
	e := newTVEnv(t)
	e.up.add("/radio", file{ctype: "audio/mpeg", body: strings.Repeat("a", 4096), endless: true})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, e.proxy.URL+"/c/radio", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(resp.Body, make([]byte, 64<<10)); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	_ = resp.Body.Close()
	e.client.CloseIdleConnections()
	<-e.up.gone // the upstream handler saw its client (the proxy) go away
}
