// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"encoding/base64"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// dashTOML has a DASH channel found by its .mpd path, one configured as
// dash, an auto channel without a hint, a second channel for the escape
// tests and a disabled one.
const dashTOML = `
tunnel = "direct"

[defaults.upstream]
headers = { User-Agent = "test-agent" }

[[channel]]
name = "News"
url = "{UP}/live/news/manifest.mpd"

[[channel]]
name = "Typed"
url = "{UP}/live/news/manifest.mpd"
[channel.upstream]
type = "dash"

[[channel]]
name = "Auto"
url = "{UP}/live/news/play"

[[channel]]
name = "Other"
url = "{UP}/other.mpd"

[[channel]]
name = "Missing"
url = "{UP}/missing"
[channel.upstream]
type = "dash"

[[channel]]
name = "Off"
url = "{UP}/live/news/manifest.mpd"
enabled = false
`

// dashManifest is a live manifest like a broadcaster's origin packager
// writes it: a relative Period BaseURL, SegmentTemplate with
// $RepresentationID$ and $Time$, thumbnails in a subdirectory, a
// UTCTiming server. abs is the upstream's scheme://host, for an absolute
// template on another path.
func dashManifest(abs string) string {
	return `<?xml version="1.0" encoding="utf-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic" availabilityStartTime="1970-01-01T00:00:00Z" minimumUpdatePeriod="PT2S" timeShiftBufferDepth="PT1M" minBufferTime="PT2S" profiles="urn:mpeg:dash:profile:isoff-live:2011">
  <Period id="1" start="PT0S">
    <BaseURL>dash/</BaseURL>
    <AdaptationSet id="1" contentType="audio" mimeType="audio/mp4" codecs="mp4a.40.2">
      <SegmentTemplate timescale="48000" initialization="News-$RepresentationID$.dash" media="News-$RepresentationID$-$Time$.dash">
        <SegmentTimeline><S t="96000" d="96000" r="1"/></SegmentTimeline>
      </SegmentTemplate>
      <Representation id="audio_gre=96000" bandwidth="96000"/>
    </AdaptationSet>
    <AdaptationSet id="2" contentType="video" mimeType="video/mp4">
      <SegmentTemplate timescale="1000" initialization="News-$RepresentationID$.dash" media="News-$RepresentationID$-$Time$.dash">
        <SegmentTimeline><S t="2000" d="2000" r="1"/></SegmentTimeline>
      </SegmentTemplate>
      <Representation id="video=1067600" bandwidth="1067600" width="640" height="360" codecs="avc1.64001E"/>
    </AdaptationSet>
    <AdaptationSet id="3" contentType="image" mimeType="image/jpeg">
      <SegmentTemplate timescale="1" duration="10" media="thumbnails/News-$RepresentationID$-$Number$.jpeg"/>
      <Representation bandwidth="10000" id="thumbnail" width="240" height="135"/>
    </AdaptationSet>
    <AdaptationSet id="4" contentType="text" mimeType="application/mp4">
      <SegmentTemplate timescale="1000" duration="2000" media="` + abs + `/subs/$RepresentationID$/$Number$.m4s?token=secret"/>
      <Representation id="ttml" bandwidth="8000"/>
    </AdaptationSet>
  </Period>
  <UTCTiming schemeIdUri="urn:mpeg:dash:utc:http-iso:2014" value="https://time.example.org/?iso"/>
</MPD>
`
}

// dashFixture adds the DASH channel to u: /live/news/manifest.mpd
// redirects to /cdn/ch1/manifest.mpd; /live/news/play serves the same
// manifest as text/xml. It returns the segment bodies by upstream path.
func dashFixture(u *upstream, abs string) map[string]string {
	u.add("/live/news/manifest.mpd", file{location: "/cdn/ch1/manifest.mpd"})
	u.add("/cdn/ch1/manifest.mpd", file{ctype: "application/dash+xml", body: dashManifest(abs)})
	u.add("/live/news/play", file{ctype: "text/xml", body: dashManifest(abs)})
	res := map[string]string{
		"/cdn/ch1/dash/News-audio_gre=96000.dash":        "audio init",
		"/cdn/ch1/dash/News-audio_gre=96000-96000.dash":  "audio 96000",
		"/cdn/ch1/dash/News-video=1067600.dash":          "video init",
		"/cdn/ch1/dash/News-video=1067600-2000.dash":     "video 2000",
		"/cdn/ch1/dash/thumbnails/News-thumbnail-1.jpeg": "jpeg",
		"/subs/ttml/1.m4s":                               "subtitles 1",
	}
	for p, b := range res {
		u.add(p, file{ctype: "application/octet-stream", body: b})
	}
	return res
}

var (
	baseURLElem = regexp.MustCompile(`<BaseURL>([^<]*)</BaseURL>`)
	mediaAttr   = regexp.MustCompile(` media="([^"]*)"`)
	timingValue = regexp.MustCompile(`<UTCTiming schemeIdUri="urn:mpeg:dash:utc:http-iso:2014" value="([^"]*)"/>`)
)

// prefixOf decodes the upstream prefix of a prefix URL.
func prefixOf(t *testing.T, prefixURL string) string {
	t.Helper()
	_, rest, _ := strings.Cut(prefixURL, "/p/")
	parts := strings.Split(rest, "/")
	if len(parts) < 5 {
		t.Fatalf("not a prefix URL: %s", prefixURL)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestDASHChannel plays a DASH channel like a player: the manifest's
// BaseURLs are prefix URLs of the channel, the segment URLs the player
// builds from the templates reach the upstream through the proxy, and
// UTCTiming points to the proxy.
func TestDASHChannel(t *testing.T) {
	e := newDirectEnv(t, allowLoopback, dashTOML)
	res := dashFixture(e.up, e.upURL)
	for _, path := range []string{"/c/news.mpd", "/c/news", "/c/typed.mpd", "/c/auto"} {
		t.Run(path, func(t *testing.T) {
			resp, body := e.get(t, path, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d: %s", resp.StatusCode, body)
			}
			for k, want := range map[string]string{
				"Content-Type":   "application/dash+xml",
				"Cache-Control":  "no-cache",
				"Content-Length": strconv.Itoa(len(body)),
			} {
				if got := resp.Header.Get(k); got != want {
					t.Errorf("%s = %q, want %q", k, got, want)
				}
			}
			if strings.Contains(body, "time.example.org") || strings.Contains(body, "secret") && !strings.Contains(body, "/p/") {
				t.Errorf("manifest leaks an upstream URL:\n%s", body)
			}
			bases := baseURLElem.FindAllStringSubmatch(body, -1)
			if len(bases) != 2 {
				t.Fatalf("%d BaseURLs, want the added MPD one and the Period one:\n%s", len(bases), body)
			}
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/c/"), ".mpd")
			for i, want := range []string{e.upURL + "/cdn/ch1/", e.upURL + "/cdn/ch1/dash/"} {
				b := bases[i][1]
				if !strings.HasPrefix(b, e.proxy.URL+"/p/"+id+"/") || !strings.HasSuffix(b, "/") {
					t.Errorf("BaseURL %q is not a prefix URL of %s", b, id)
				}
				if p := id; p == "auto" {
					// The auto channel's manifest is not redirected.
					want = strings.Replace(want, "/cdn/ch1/", "/live/news/", 1)
				}
				if got := prefixOf(t, b); got != want {
					t.Errorf("BaseURL %d prefix %q, want %q", i, got, want)
				}
			}
			if id == "auto" {
				return
			}
			// A player resolves the templates against the Period BaseURL.
			period := bases[1][1]
			for _, seg := range []string{
				"News-audio_gre=96000.dash", "News-audio_gre=96000-96000.dash",
				"News-video=1067600.dash", "News-video=1067600-2000.dash", "thumbnails/News-thumbnail-1.jpeg",
			} {
				r, b := do(t, e.client, http.MethodGet, period+seg, nil)
				if want := res["/cdn/ch1/dash/"+seg]; r.StatusCode != http.StatusOK || b != want {
					t.Errorf("GET %s: status %d, body %q, want %q", seg, r.StatusCode, b, want)
				}
			}
			// The absolute template got its own prefix URL with queries.
			var subs string
			for _, m := range mediaAttr.FindAllStringSubmatch(body, -1) {
				if strings.Contains(m[1], "/p/") {
					subs = m[1]
				}
			}
			if subs == "" || prefixOf(t, subs) != e.upURL+"/subs/" || !strings.Contains(subs, "/q/") {
				t.Fatalf("subtitle template %q is not a prefix URL with queries for /subs/", subs)
			}
			subs = strings.NewReplacer("$RepresentationID$", "ttml", "$Number$", "1").Replace(subs)
			if r, b := do(t, e.client, http.MethodGet, subs, nil); r.StatusCode != http.StatusOK || b != "subtitles 1" {
				t.Errorf("GET subtitles: status %d, body %q", r.StatusCode, b)
			}
			m := timingValue.FindStringSubmatch(body)
			if m == nil || m[1] != e.proxy.URL+"/time" {
				t.Errorf("UTCTiming not served by the proxy:\n%s", body)
			}
		})
	}
	// Upstream headers come from the channel config, for segments too.
	for _, r := range e.up.requests() {
		if strings.HasPrefix(r.Path, "/cdn/ch1/dash/") && r.UserAgent != "test-agent" {
			t.Errorf("segment request %s without the channel's User-Agent", r.Path)
		}
	}
	if q := e.up.requestsFor("/subs/ttml/1.m4s"); len(q) == 0 || q[0].Query != "token=secret" {
		t.Errorf("subtitle requests %v, want the query passed", q)
	}
	if logs := e.logs.String(); strings.Contains(logs, "secret") || strings.Contains(logs, "/cdn/ch1/dash/") {
		t.Errorf("logs contain upstream paths or queries:\n%s", logs)
	}
}

func TestDASHHead(t *testing.T) {
	e := newDirectEnv(t, allowLoopback, dashTOML)
	dashFixture(e.up, e.upURL)
	resp, body := do(t, e.client, http.MethodHead, e.proxy.URL+"/c/news.mpd", nil)
	if resp.StatusCode != http.StatusOK || body != "" || resp.Header.Get("Content-Type") != "application/dash+xml" {
		t.Errorf("HEAD: status %d, body %q, Content-Type %q", resp.StatusCode, body, resp.Header.Get("Content-Type"))
	}
}

func TestDASHRefused(t *testing.T) {
	const secret = "token-a1b2c3"
	tests := []struct {
		name, body, msg, reason string
	}{
		{
			name: "content protection",
			body: `<MPD><Period><AdaptationSet><ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" value="cenc"/>` +
				`<ContentProtection schemeIdUri="urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"><dashif:laurl xmlns:dashif="https://dashif.org/CPS">https://license.example/` + secret + `</dashif:laurl></ContentProtection>` +
				`<SegmentTemplate media="https://cdn.example/` + secret + `/$Number$.m4s"/></AdaptationSet></Period></MPD>`,
			msg: `msg="DRM manifest refused"`, reason: "drm",
		},
		{
			name: "remote period",
			body: `<MPD xmlns:xlink="http://www.w3.org/1999/xlink"><Period xlink:href="https://cdn.example/` + secret + `/period.xml"/></MPD>`,
			msg:  `msg="playlist rejected"`, reason: "other",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newDirectEnv(t, allowLoopback, dashTOML)
			e.up.add("/other.mpd", file{ctype: "application/dash+xml", body: tt.body})
			resp, body := e.get(t, "/c/other.mpd", nil)
			if resp.StatusCode != http.StatusBadGateway {
				t.Errorf("status %d, want 502", resp.StatusCode)
			}
			if strings.Contains(body, secret) || strings.Contains(body, "example") {
				t.Errorf("body leaks the manifest: %q", body)
			}
			logs := e.logs.String()
			if !strings.Contains(logs, tt.msg) || strings.Contains(logs, secret) {
				t.Errorf("logs lack %s or leak the manifest:\n%s", tt.msg, logs)
			}
			compareMetrics(t, e.metrics.Gatherer(), "wsp_upstream_errors_total", errorsHead,
				`wsp_upstream_errors_total{channel="other",reason="`+tt.reason+`"} 1`)
		})
	}
}

// TestPrefixEscapes is the escape test of spec 4.1 through the server:
// no request below a prefix URL reaches the upstream outside the signed
// prefix, for another channel or with a broken signature.
func TestPrefixEscapes(t *testing.T) {
	e := newDirectEnv(t, allowLoopback, dashTOML)
	dashFixture(e.up, e.upURL)
	e.up.add("/cdn/secret.txt", file{ctype: "text/plain", body: "outside"})
	e.up.add("/cdn/ch1/dash/a/b.m4s", file{ctype: "video/mp4", body: "inside"})
	p, err := e.signer.PrefixPath("news", e.upURL+"/cdn/ch1/dash/", false)
	if err != nil {
		t.Fatal(err)
	}
	base := e.proxy.URL + p
	if r, b := do(t, e.client, http.MethodGet, base+"a/b.m4s", nil); r.StatusCode != http.StatusOK || b != "inside" {
		t.Fatalf("valid request: status %d, body %q", r.StatusCode, b)
	}
	parts := strings.Split(p, "/") // "", "p", channel, sig, mode, b64, ""
	swap := func(i int, v string) string {
		q := append([]string(nil), parts...)
		q[i] = v
		return e.proxy.URL + strings.Join(q, "/")
	}
	otherHost := strings.Replace(e.upURL, "127.0.0.1", "localhost", 1)
	b64 := base64.RawURLEncoding.EncodeToString
	noRedirect := *e.client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	before := len(e.up.requests())
	for _, target := range []string{
		base + "../../secret.txt",
		base + "%2e%2e/%2e%2e/secret.txt",
		base + "%2E%2E%2F%2E%2E%2Fsecret.txt",
		base + "a%2Fb.m4s",
		base + "a/..%2f..%2f..%2fsecret.txt",
		base + "..;/..;/secret.txt",
		base + "a//b.m4s",
		base + "/" + otherHost[len("http://"):] + "/cdn/secret.txt",
		base + "a%5Cb.m4s",
		base + "a/b.m4s?x=1",
		base,
		swap(2, "other") + "a/b.m4s",
		swap(2, "off") + "a/b.m4s",
		swap(3, b64(make([]byte, 16))) + "a/b.m4s",
		swap(4, "q") + "a/b.m4s?x=1",
		swap(5, b64([]byte(e.upURL+"/cdn/"))) + "secret.txt",
		swap(5, b64([]byte(otherHost+"/cdn/ch1/dash/"))) + "a/b.m4s",
		swap(5, b64([]byte(strings.Replace(e.upURL, "http:", "https:", 1)+"/cdn/ch1/dash/"))) + "a/b.m4s",
		strings.Replace(base, "/p/", "/r/", 1) + "a/b.m4s",
	} {
		r, b := do(t, &noRedirect, http.MethodGet, target, nil)
		if r.StatusCode == http.StatusOK || strings.Contains(b, "outside") || strings.Contains(b, "inside") {
			t.Errorf("GET %s: status %d, body %q; want no upstream content", target, r.StatusCode, b)
		}
	}
	if after := e.up.requests(); len(after) != before {
		t.Errorf("escape requests reached the upstream: %v", after[before:])
	}
}

// TestPrefixRouteFromConfig checks that a prefix URL carries no route:
// after a reload moves the channel to another tunnel, the same URL goes
// through the new tunnel, never direct.
func TestPrefixRouteFromConfig(t *testing.T) {
	grPeer, _ := newPeer(t, "via gr")
	dePeer, _ := newPeer(t, "via de")
	channel := func(tunnel string) string {
		return "tunnel = \"" + tunnel + "\"\n[[channel]]\nname = \"News\"\nurl = \"http://upstream.test/manifest.mpd\"\n"
	}
	e := startTunnels(t, map[string]string{
		"tunnels/gr.conf":    grPeer.ClientConfig(),
		"tunnels/de.conf":    dePeer.ClientConfig(),
		"channels.d/tv.toml": channel("gr"),
	})
	p, err := newSigner(t).PrefixPath("news", "http://upstream.test/", false)
	if err != nil {
		t.Fatal(err)
	}
	if body := e.get(t, p+"seg.ts", http.StatusOK); body != "via gr" {
		t.Fatalf("before reload: %q", body)
	}
	writeFiles(t, e.dir, map[string]string{"channels.d/tv.toml": channel("de")})
	if err := e.reload(t); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if body := e.get(t, p+"seg.ts", http.StatusOK); body != "via de" {
		t.Errorf("after reload: %q, want the new tunnel's upstream", body)
	}
	if n := e.direct.used(); n != 0 {
		t.Errorf("direct network used %d times", n)
	}
	// Disabled by the next reload: the URL is dead.
	writeFiles(t, e.dir, map[string]string{"channels.d/tv.toml": channel("de") + "enabled = false\n"})
	if err := e.reload(t); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	e.get(t, p+"seg.ts", http.StatusNotFound)
}

func TestTime(t *testing.T) {
	e := newDirectEnv(t, allowLoopback, dashTOML)
	before := time.Now().UTC().Truncate(time.Millisecond)
	resp, body := e.get(t, "/time", nil)
	after := time.Now().UTC()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d, Cache-Control %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	got, err := time.Parse("2006-01-02T15:04:05.000Z", body)
	if err != nil || got.Before(before) || got.After(after) {
		t.Errorf("time %q (%v), want between %v and %v", body, err, before, after)
	}
	if r, b := do(t, e.client, http.MethodHead, e.proxy.URL+"/time", nil); r.StatusCode != http.StatusOK || b != "" {
		t.Errorf("HEAD: status %d, body %q", r.StatusCode, b)
	}
	if len(e.up.requests()) != 0 {
		t.Error("/time asked the upstream")
	}
}

// TestDASHMetricsKind checks spec 7.3: a dash channel counts as
// playlist even if its type is never detected.
func TestDASHMetricsKind(t *testing.T) {
	e := newDirectEnv(t, allowLoopback, dashTOML)
	if resp, _ := e.get(t, "/c/missing.mpd", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want the upstream 404", resp.StatusCode)
	}
	compareMetrics(t, e.metrics.Gatherer(), "wsp_upstream_requests_total", requestsHead,
		`wsp_upstream_requests_total{channel="missing",code="404",kind="playlist"} 1`)
}
