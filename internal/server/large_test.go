// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/hls"
)

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
