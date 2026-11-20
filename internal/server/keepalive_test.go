// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// keepAliveTOML has auto and raw channels on one upstream host and port:
// an HLS media playlist, a file served over HTTP and a Shoutcast v1
// stream, so all /c/ requests share the route's ICY transport.
const keepAliveTOML = `
tunnel = "direct"

[[channel]]
name = "Playlist"
url = "{UP}/live/index.m3u8"

[[channel]]
name = "File"
url = "{UP}/file.mp3"
[channel.upstream]
type = "raw"

[[channel]]
name = "Radio Auto"
url = "{UP}/;"

[[channel]]
name = "Radio Raw"
url = "{UP}/;"
[channel.upstream]
type = "raw"
`

// keepAlivePaths are the upstream paths of the keepAliveTOML channels.
var keepAlivePaths = map[string]string{
	"playlist": "/live/index.m3u8", "file": "/file.mp3", "radio-auto": "/;", "radio-raw": "/;",
}

func newKeepAliveEnv(t *testing.T) *directEnv {
	t.Helper()
	e := newDirectEnv(t, allowLoopback, keepAliveTOML)
	e.up.add("/live/index.m3u8", file{ctype: "application/vnd.apple.mpegurl", body: "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\nseg1.ts\n"})
	e.up.add("/file.mp3", file{ctype: "audio/mpeg", body: "MP3DATA"})
	e.up.add("/;", file{icy: true, ctype: "audio/mpeg", body: "AUDIO"})
	return e
}

// getChannel requests /c/{id} of a keepAliveTOML channel and checks the
// answer.
func getChannel(t *testing.T, e *directEnv, id string) {
	t.Helper()
	resp, body := e.get(t, "/c/"+id, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/c/%s: status %d, body %q", id, resp.StatusCode, body)
	}
	switch id {
	case "playlist":
		if !strings.HasPrefix(body, "#EXTM3U") || !strings.Contains(body, "/r/playlist/") {
			t.Errorf("/c/%s: body %q, want a rewritten playlist", id, body)
		}
	case "file":
		if body != "MP3DATA" {
			t.Errorf("/c/%s: body %q", id, body)
		}
	default:
		if body != "AUDIO" || resp.Header.Get("Icy-Name") != "Test Radio" {
			t.Errorf("/c/%s: body %q, icy-name %q", id, body, resp.Header.Get("Icy-Name"))
		}
	}
}

// connections returns the number of upstream connections that carried
// requests for path.
func connections(e *directEnv, path string) int {
	var remotes []string
	for _, r := range e.up.requestsFor(path) {
		if !slices.Contains(remotes, r.Remote) {
			remotes = append(remotes, r.Remote)
		}
	}
	return len(remotes)
}

// Repeated /c/ polls of an auto channel whose URL is an HLS media
// playlist, and of a raw channel answered over plain HTTP, reuse one
// upstream connection (#131).
func TestChannelPollsReuseUpstreamConnection(t *testing.T) {
	const polls = 4
	for _, id := range []string{"playlist", "file"} {
		t.Run(id, func(t *testing.T) {
			e := newKeepAliveEnv(t)
			for range polls {
				getChannel(t, e, id)
			}
			path := keepAlivePaths[id]
			if n := len(e.up.requestsFor(path)); n != polls {
				t.Fatalf("%d upstream requests, want %d", n, polls)
			}
			if n := connections(e, path); n != 1 {
				t.Errorf("%d polls used %d upstream connections, want 1", polls, n)
			}
		})
	}
}

// A Shoutcast v1 answer works on a fresh connection and on a kept-alive
// connection that carried plain-HTTP responses of the same host before
// (#63), for auto and raw channels; the connection of an ICY answer is
// not used again.
func TestICYAnswerOnKeptAliveConnection(t *testing.T) {
	tests := []struct {
		name  string
		steps []string
	}{
		{"auto ICY on a fresh connection", []string{"radio-auto", "radio-auto"}},
		{"raw ICY on a fresh connection", []string{"radio-raw", "playlist"}},
		{"auto ICY after playlist polls", []string{"playlist", "playlist", "radio-auto", "playlist"}},
		{"raw ICY after playlist polls", []string{"playlist", "radio-raw", "playlist", "radio-auto"}},
		{"raw ICY after a raw HTTP stream", []string{"file", "radio-raw", "file", "radio-raw"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newKeepAliveEnv(t)
			for _, id := range tt.steps {
				getChannel(t, e, id)
			}
			reqs := e.up.requests()
			if len(reqs) != len(tt.steps) {
				t.Fatalf("%d upstream requests, want %d", len(reqs), len(tt.steps))
			}
			for i, r := range reqs {
				if i == 0 {
					continue
				}
				prev := reqs[i-1]
				// A plain-HTTP answer leaves its connection idle for the
				// next request (the case of #63); an ICY answer does not.
				wantReuse := prev.Path != "/;"
				if reused := r.Remote == prev.Remote; reused != wantReuse {
					t.Errorf("request %d (%s) reused the connection of %s: %v, want %v", i, r.Path, prev.Path, reused, wantReuse)
				}
			}
		})
	}
}
