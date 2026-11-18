// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/hls"
)

// largePlaylist returns a live media playlist of n segments shaped like the
// long DVR windows of some public broadcasters (issue #76, about 1.4 MB
// for 5500 segments), and the segment URIs.
func largePlaylist(n int) (string, []string) {
	var b strings.Builder
	segs := make([]string, n)
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:6\n#EXT-X-MEDIA-SEQUENCE:1731931200\n")
	for i := range n {
		seq := 1731931200 + i
		b.WriteString("#EXTINF:6.000000,\n")
		if i%2 == 0 {
			fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:2026-11-18T12:%02d:%02d.000+00:00\n", i/10%60, i*6%60)
		}
		segs[i] = fmt.Sprintf("media_b5000000_t1080p_lang-eng_%019d_%010d.ts?session=%s&seg=%d&sig=%s",
			int64(seq)*1000003, seq, strings.Repeat("0123456789abcdef", 4), seq, strings.Repeat("A", 43))
		b.WriteString(segs[i] + "\n")
	}
	return b.String(), segs
}

// TestLargePlaylist sends a 1.4 MB media playlist through /r/: every
// segment URI becomes a signed resource URL of the channel.
func TestLargePlaylist(t *testing.T) {
	e := newTVEnv(t)
	body, segs := largePlaylist(5500)
	if len(body) < 1300_000 || len(body) > 1500_000 {
		t.Fatalf("fixture is %d bytes, want about 1.4 MB", len(body))
	}
	e.up.add("/live/1080p/index.m3u8", file{ctype: "application/vnd.apple.mpegurl", body: body})

	resp, out := e.get(t, e.signer.Path("news", e.upURL+"/live/1080p/index.m3u8"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Length"); got != strconv.Itoa(len(out)) {
		t.Errorf("Content-Length %s, body %d bytes", got, len(out))
	}
	prefix := e.proxy.URL + "/r/"
	i := 0
	for line := range strings.Lines(out) {
		line = strings.TrimSuffix(line, "\n")
		if !strings.HasPrefix(line, "http") {
			if strings.Contains(line, "media_") {
				t.Fatalf("unrewritten URI %q", line)
			}
			continue
		}
		path, ok := strings.CutPrefix(line, prefix)
		if !ok || i >= len(segs) {
			t.Fatalf("line %q: not a resource URL of the proxy, or too many", line)
		}
		channel, upstream, err := e.signer.Parse("/r/" + path)
		if err != nil || channel != "news" || upstream != e.upURL+"/live/1080p/"+segs[i] {
			t.Fatalf("segment %d: Parse = %q, %q, %v", i, channel, upstream, err)
		}
		i++
	}
	if i != len(segs) {
		t.Errorf("%d segment URLs, want %d", i, len(segs))
	}
}

// TestPlaylistSizeLimit checks [upstream] max_playlist_size: default 8 MiB,
// configurable, larger playlists give 502 (spec 3.4).
func TestPlaylistSizeLimit(t *testing.T) {
	tests := []struct {
		name   string
		config string
		size   int
		want   int
	}{
		{"default limit", "", 8 << 20, http.StatusOK},
		{"over default limit", "", 8<<20 + 1, http.StatusBadGateway},
		{"configured limit", "\n[upstream]\nmax_playlist_size = \"1KiB\"\n", 1024, http.StatusOK},
		{"over configured limit", "\n[upstream]\nmax_playlist_size = \"1KiB\"\n", 1025, http.StatusBadGateway},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newDirectEnv(t, allowLoopback+tt.config, channelsTOML)
			body := "#EXTM3U\n" + strings.Repeat("#", tt.size-len("#EXTM3U\n"))
			e.up.add("/live/big.m3u8", file{ctype: "application/vnd.apple.mpegurl", body: body})
			resp, out := e.get(t, e.signer.Path("news", e.upURL+"/live/big.m3u8"), nil)
			if resp.StatusCode != tt.want {
				t.Fatalf("status %d, want %d", resp.StatusCode, tt.want)
			}
			if tt.want == http.StatusOK && out != body {
				t.Errorf("body of %d bytes changed", len(body))
			}
		})
	}
}

// TestPlaylistSizeBound: every valid max_playlist_size is a size the
// rewriter accepts.
func TestPlaylistSizeBound(t *testing.T) {
	if config.MaxPlaylistSizeLimit != hls.MaxPlaylistSize {
		t.Errorf("config.MaxPlaylistSizeLimit = %d, hls.MaxPlaylistSize = %d", config.MaxPlaylistSizeLimit, hls.MaxPlaylistSize)
	}
}
