// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/epg"
	"github.com/KingOfKalk/wg-stream-proxy/internal/metrics"
	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel/tunneltest"
)

// epgNow is the time of the EPG tests.
var epgNow = time.Date(2026, 11, 19, 10, 30, 0, 0, time.UTC)

// epgClock is the clock of the guide: it stands still, and reports each
// armed timer except the fetch timeouts on idle, so a test sees when a
// round of fetches is done. Its timers never fire.
type epgClock struct{ idle chan time.Duration }

func newEPGClock() *epgClock { return &epgClock{idle: make(chan time.Duration, 100)} }

func (c *epgClock) Now() time.Time { return epgNow }

func (c *epgClock) AfterFunc(d time.Duration, _ func()) route.Timer {
	if d != epg.FetchTimeout {
		c.idle <- d
	}
	return time.NewTimer(time.Hour) // stopped by the guide, never fires in a test
}

func (c *epgClock) waitIdle(t *testing.T) {
	t.Helper()
	select {
	case <-c.idle:
	case <-time.After(20 * time.Second):
		t.Fatal("EPG refresh does not finish")
	}
}

// xmltv returns an XMLTV document: a channel and a programme titled
// "<tag> <id>" per id, and an ended programme of each.
func xmltv(tag string, ids ...string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n<!DOCTYPE tv SYSTEM \"xmltv.dtd\">\n<tv>\n")
	for _, id := range ids {
		b.WriteString(`  <channel id="` + id + `"><display-name>` + tag + " " + id + "</display-name></channel>\n")
	}
	for _, id := range ids {
		b.WriteString(`  <programme start="20261119080000 +0000" stop="20261119090000 +0000" channel="` + id + `"><title>ended</title></programme>` + "\n")
		b.WriteString(`  <programme start="20261119100000 +0100" stop="20261119120000 +0100" channel="` + id + `"><title>` + tag + " " + id + "</title></programme>\n")
	}
	b.WriteString("</tv>\n")
	return b.String()
}

func gzipString(t *testing.T, s string) string {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

const epgTVChannels = `tunnel = "direct"
[[channel]]
name = "A"
url = "https://live.example.com/a.m3u8"
playlist.tvg_id = "a.example"
[[channel]]
name = "B"
url = "https://live.example.com/b.m3u8"
playlist = { tvg_id = "b.example", epg_source = "de" }
[[channel]]
name = "Off"
url = "https://live.example.com/off.m3u8"
enabled = false
playlist.tvg_id = "off.example"
`

// epgOneChannel names no EPG source.
const epgOneChannel = "tunnel = \"direct\"\n[[channel]]\nname = \"A\"\nurl = \"https://live.example.com/a.m3u8\"\nplaylist.tvg_id = \"a.example\"\n"

// TestEPGEndToEnd: the proxy fetches two sources, filters and merges
// them, serves the guide plain and gzip and links it from the channel
// list; a reload that changes the channels fetches again.
func TestEPGEndToEnd(t *testing.T) {
	up := newUpstream()
	up.add("/gr/epg.xml.gz", file{ctype: "application/gzip", body: gzipString(t, xmltv("gr", "a.example", "b.example", "off.example", "x.example"))})
	up.add("/de/guide.xml", file{ctype: "application/xml", body: xmltv("de", "a.example", "b.example", "c.example")})
	upSrv := httptest.NewServer(up)
	t.Cleanup(upSrv.Close)
	configTOML := "[upstream]\nuser_agent = \"wsp-test\"\n" + allowLoopback +
		"[[epg.source]]\nname = \"gr\"\nurl = \"" + upSrv.URL + "/gr/epg.xml.gz\"\n" +
		"[[epg.source]]\nname = \"de\"\nurl = \"" + upSrv.URL + "/de/guide.xml\"\ntunnel = \"direct\"\n"
	dir := writeConfig(t, map[string]string{"config.toml": configTOML, "channels.d/tv.toml": epgTVChannels})
	clk := newEPGClock()
	e := startRun(t, dir, 0, func(o *RunOptions) { o.EPGClock = clk })
	clk.waitIdle(t)

	pl := e.get(t, "/playlist.m3u8", http.StatusOK)
	if want := `#EXTM3U x-tvg-url="` + e.url + `/epg.xml.gz" url-tvg="` + e.url + `/epg.xml.gz"`; !strings.HasPrefix(pl, want+"\n") {
		t.Errorf("channel list header:\n%s\nwant %s", pl, want)
	}

	resp, plain := do(t, e.client, http.MethodGet, e.url+"/epg.xml", nil)
	want := `<?xml version="1.0" encoding="UTF-8"?>` + "\n<tv>\n" +
		`<channel id="a.example"><display-name>gr a.example</display-name></channel>` + "\n" +
		`<channel id="b.example"><display-name>de b.example</display-name></channel>` + "\n" +
		`<programme start="20261119100000 +0100" stop="20261119120000 +0100" channel="a.example"><title>gr a.example</title></programme>` + "\n" +
		`<programme start="20261119100000 +0100" stop="20261119120000 +0100" channel="b.example"><title>de b.example</title></programme>` + "\n" +
		"</tv>\n"
	if resp.StatusCode != http.StatusOK || plain != want {
		t.Fatalf("GET /epg.xml: %d\n%s\nwant\n%s", resp.StatusCode, plain, want)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/xml; charset=utf-8" {
		t.Errorf("Content-Type %q", ct)
	}
	etag, modified := resp.Header.Get("ETag"), resp.Header.Get("Last-Modified")
	if etag == "" || modified != epgNow.Format(http.TimeFormat) || resp.Header.Get("Cache-Control") != "no-cache" {
		t.Errorf("headers %v", resp.Header)
	}

	// The gzip form, untouched by the client's transparent decompression.
	req, _ := http.NewRequest(http.MethodGet, e.url+"/epg.xml.gz", nil)
	req.Header.Set("Accept-Encoding", "identity")
	zresp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	zbody, _ := io.ReadAll(zresp.Body)
	_ = zresp.Body.Close()
	zr, err := gzip.NewReader(bytes.NewReader(zbody))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	unzipped, _ := io.ReadAll(zr)
	if string(unzipped) != want || zresp.Header.Get("Content-Type") != "application/gzip" || zresp.Header.Get("ETag") == etag {
		t.Errorf("GET /epg.xml.gz: %v\n%s", zresp.Header, unzipped)
	}

	// Conditional and HEAD requests.
	if resp, _ := do(t, e.client, http.MethodGet, e.url+"/epg.xml", http.Header{"If-None-Match": {etag}}); resp.StatusCode != http.StatusNotModified {
		t.Errorf("If-None-Match: status %d", resp.StatusCode)
	}
	if resp, body := do(t, e.client, http.MethodHead, e.url+"/epg.xml.gz", nil); resp.StatusCode != http.StatusOK || body != "" || resp.ContentLength != int64(len(zbody)) {
		t.Errorf("HEAD: status %d, length %d, body %q", resp.StatusCode, resp.ContentLength, body)
	}

	// Each source was fetched once, sequentially, with the upstream
	// User-Agent; the log names host and file only.
	for _, p := range []string{"/gr/epg.xml.gz", "/de/guide.xml"} {
		if reqs := up.requestsFor(p); len(reqs) != 1 || reqs[0].UserAgent != "wsp-test" {
			t.Errorf("%s: requests %+v", p, reqs)
		}
	}
	if logs := e.logs.String(); !strings.Contains(logs, `msg="epg source fetched" source=gr route=direct url=`+upSrv.URL+"/…/epg.xml.gz") ||
		!strings.Contains(logs, `msg="epg guide updated" channels=2 programmes=2`) ||
		!strings.Contains(logs, "filtered_bytes=") {
		t.Errorf("logs:\n%s", logs)
	}

	// Metrics per source.
	m := e.get(t, "/metrics", http.StatusOK)
	for _, line := range []string{
		`wsp_epg_fetches_total{result="ok",source="gr"} 1`,
		`wsp_epg_fetches_total{result="status",source="de"} 0`,
		`wsp_epg_last_success_timestamp_seconds{source="de"} ` + strconv.FormatFloat(float64(epgNow.Unix()), 'g', -1, 64),
	} {
		if !strings.Contains(m, line) {
			t.Errorf("metrics lack %q", line)
		}
	}

	// A reload that adds a channel fetches every source again.
	writeFiles(t, dir, map[string]string{"channels.d/tv.toml": epgTVChannels +
		"[[channel]]\nname = \"C\"\nurl = \"https://live.example.com/c.m3u8\"\nplaylist.tvg_id = \"c.example\"\n"})
	if err := e.reload(t); err != nil {
		t.Fatal(err)
	}
	clk.waitIdle(t)
	if body := e.get(t, "/epg.xml", http.StatusOK); !strings.Contains(body, "<title>de c.example</title>") {
		t.Errorf("after reload:\n%s", body)
	}
	if n := len(up.requestsFor("/gr/epg.xml.gz")); n != 2 {
		t.Errorf("gr fetched %d times, want 2", n)
	}

	// Back to link mode: no guide, the channel list links [epg] url.
	writeFiles(t, dir, map[string]string{"config.toml": allowLoopback + "[epg]\nurl = \"https://epg.example.org/epg.xml.gz\"\n", "channels.d/tv.toml": epgOneChannel})
	if err := e.reload(t); err != nil {
		t.Fatal(err)
	}
	e.get(t, "/epg.xml.gz", http.StatusNotFound)
	if pl := e.get(t, "/playlist.m3u8", http.StatusOK); !strings.HasPrefix(pl, `#EXTM3U x-tvg-url="https://epg.example.org/epg.xml.gz"`) {
		t.Errorf("channel list in link mode:\n%s", pl)
	}
}

// gate is an upstream that answers only after open is called.
type gate struct {
	h    http.Handler
	ch   chan struct{}
	once sync.Once
}

func (g *gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case <-g.ch:
	case <-r.Context().Done():
		return
	}
	g.h.ServeHTTP(w, r)
}

func (g *gate) open() { g.once.Do(func() { close(g.ch) }) }

// TestEPGBeforeFirstFetch: 503 until a source was fetched; a source that
// fails keeps no guide, the other one serves.
func TestEPGBeforeFirstFetch(t *testing.T) {
	up := newUpstream()
	up.add("/de.xml", file{body: xmltv("de", "a.example")})
	g := &gate{h: up, ch: make(chan struct{})}
	t.Cleanup(g.open)
	upSrv := httptest.NewServer(g)
	t.Cleanup(upSrv.Close)
	configTOML := allowLoopback +
		"[[epg.source]]\nname = \"gr\"\nurl = \"" + upSrv.URL + "/missing.xml\"\n" +
		"[[epg.source]]\nname = \"de\"\nurl = \"" + upSrv.URL + "/de.xml\"\n"
	dir := writeConfig(t, map[string]string{"config.toml": configTOML, "channels.d/tv.toml": epgTVChannels})
	clk := newEPGClock()
	e := startRun(t, dir, 0, func(o *RunOptions) { o.EPGClock = clk })

	resp, body := do(t, e.client, http.MethodGet, e.url+"/epg.xml.gz", nil)
	if resp.StatusCode != http.StatusServiceUnavailable || strings.Contains(body, "<tv") {
		t.Errorf("before the first fetch: %d %q", resp.StatusCode, body)
	}
	g.open()
	clk.waitIdle(t)
	if body := e.get(t, "/epg.xml", http.StatusOK); !strings.Contains(body, "<title>de a.example</title>") {
		t.Errorf("guide:\n%s", body)
	}
	if logs := e.logs.String(); !strings.Contains(logs, `msg="epg source failed: keeping the last good copy" source=gr`) ||
		!strings.Contains(logs, "result=status") || !strings.Contains(logs, "serving_copy_age=none") {
		t.Errorf("logs:\n%s", logs)
	}
}

// TestEPGThroughTunnel: a source on a tunnel is fetched through it,
// never directly; the guard applies to sources like to channels.
func TestEPGThroughTunnel(t *testing.T) {
	up := newUpstream()
	up.add("/epg.xml", file{body: xmltv("vpn", "a.example")})
	peer := tunneltest.Start(t, tunneltest.Options{Handler: up})
	peer.SetHost("epg.test", tunneltest.ServerAddr4)
	configTOML := "[[epg.source]]\nname = \"vpn\"\nurl = \"http://epg.test/epg.xml\"\ntunnel = \"gr\"\n" +
		"[[epg.source]]\nname = \"lan\"\nurl = \"http://127.0.0.1:1/epg.xml\"\n"
	dir := writeConfig(t, map[string]string{"config.toml": configTOML, "tunnels/gr.conf": peer.ClientConfig(), "channels.d/tv.toml": epgOneChannel})
	clk := newEPGClock()
	e := startRun(t, dir, 0, func(o *RunOptions) { o.EPGClock = clk })
	clk.waitIdle(t)
	if body := e.get(t, "/epg.xml", http.StatusOK); !strings.Contains(body, "<title>vpn a.example</title>") {
		t.Errorf("guide:\n%s", body)
	}
	if e.tunnel("gr").used() == 0 {
		t.Error("the tunnel was not used")
	}
	if n := e.direct.dials.Load(); n != 0 {
		t.Errorf("direct dials = %d, want 0 (the lan source is blocked by the guard)", n)
	}
	if m := e.get(t, "/metrics", http.StatusOK); !strings.Contains(m, `wsp_epg_fetches_total{result="guard",source="lan"} 1`) {
		t.Errorf("metrics:\n%s", m)
	}
}

// TestEPGNoSources: without [[epg.source]] there is no guide.
func TestEPGNoSources(t *testing.T) {
	e := newDirectEnv(t, "", "tunnel = \"direct\"\n[[channel]]\nname = \"A\"\nurl = \"https://live.example.com/a.m3u8\"\n")
	for _, p := range []string{"/epg.xml", "/epg.xml.gz"} {
		if resp, _ := e.get(t, p, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d", p, resp.StatusCode)
		}
	}
	if !slices.Equal(epg.Results, metrics.EPGResults) {
		t.Errorf("fetch results differ: epg %v, metrics %v", epg.Results, metrics.EPGResults)
	}
}
