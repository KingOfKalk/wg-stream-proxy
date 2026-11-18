// SPDX-License-Identifier: AGPL-3.0-or-later

package playertest

import (
	"bufio"
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/hls"
	"github.com/KingOfKalk/wg-stream-proxy/internal/server"
	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel/tunneltest"
)

// envPlayer enables the player tests; they need ffmpeg in PATH.
const envPlayer = "WSP_PLAYER_TESTS"

// mediaDir holds the media fixtures (testdata/media/generate.sh).
const mediaDir = "../../../testdata/media"

// fixtureHost is the fixture server's name; the tunnel's DNS server
// resolves it to the server peer.
const fixtureHost = "media.test"

// playerCase is one channel played with ffmpeg.
type playerCase struct {
	name     string
	id       string // channel id
	upstream string // path on fixtureHost
	hls      bool   // HLS channel: /c/{id}.m3u8, else /c/{id}
	seconds  int    // ffmpeg -t
	video    bool   // must decode video frames
	// fetched are path prefixes the fixture server must have served:
	// keys, init segments, renditions.
	fetched []string
	// reloaded is a live media playlist the player must fetch more than
	// once.
	reloaded string
	// skip is the reason to skip the case for now.
	skip string
}

var playerCases = []playerCase{
	{
		name: "ts vod", id: "ts-vod", upstream: "/vod/ts/master.m3u8", hls: true, seconds: 10, video: true,
		fetched: []string{"/vod/ts/index.m3u8", "/vod/ts/seg"},
	},
	{
		name: "ts live", id: "ts-live", upstream: "/live/ts/master.m3u8", hls: true, seconds: 10, video: true,
		fetched: []string{"/live/ts/seg"}, reloaded: "/live/ts/index.m3u8",
	},
	{
		name: "fmp4 vod", id: "fmp4-vod", upstream: "/vod/fmp4/master.m3u8", hls: true, seconds: 10, video: true,
		fetched: []string{"/vod/fmp4/init.mp4", "/vod/fmp4/seg"},
	},
	{
		name: "fmp4 live", id: "fmp4-live", upstream: "/live/fmp4/master.m3u8", hls: true, seconds: 10, video: true,
		fetched: []string{"/live/fmp4/init.mp4", "/live/fmp4/seg"}, reloaded: "/live/fmp4/index.m3u8",
	},
	{
		name: "aes-128 vod", id: "aes-vod", upstream: "/vod/aes/master.m3u8", hls: true, seconds: 10, video: true,
		fetched: []string{"/vod/aes/key.bin", "/vod/aes/seg"},
	},
	{
		name: "aes-128 live", id: "aes-live", upstream: "/live/aes/master.m3u8", hls: true, seconds: 10, video: true,
		fetched: []string{"/live/aes/key.bin", "/live/aes/seg"}, reloaded: "/live/aes/index.m3u8",
	},
	{
		name: "demuxed audio vod", id: "demuxed-vod", upstream: "/vod/demuxed/master.m3u8", hls: true, seconds: 10, video: true,
		fetched: []string{"/vod/demuxed/video/seg", "/vod/demuxed/audio/seg"},
	},
	{
		name: "demuxed audio live", id: "demuxed-live", upstream: "/live/demuxed/master.m3u8", hls: true, seconds: 10, video: true,
		fetched: []string{"/live/demuxed/video/seg", "/live/demuxed/audio/seg"}, reloaded: "/live/demuxed/audio/index.m3u8",
	},
	{
		name: "raw mpeg-ts", id: "raw-ts", upstream: "/raw/ts", seconds: 5, video: true,
		fetched: []string{"/raw/ts"},
	},
	{
		name: "icecast radio", id: "icecast", upstream: "/icecast/radio/tone.mp3", seconds: 5,
		fetched: []string{"/icecast/radio/tone.mp3"},
	},
	// TODO(#15): enable once the raw route rewrites the "ICY 200 OK"
	// status line (spec 3.3, https://github.com/KingOfKalk/wg-stream-proxy/issues/15).
	{
		name: "icy radio", id: "icy", upstream: "/shoutcast/radio/tone.mp3", seconds: 5,
		fetched: []string{"/shoutcast/radio/tone.mp3"}, skip: "needs the ICY status line rewrite of #15",
	},
}

// TestPlayer is L4 of docs/testing.md: ffmpeg stands in for Kodi and
// plays every fixture through the proxy (server.Run) and an in-process
// WireGuard tunnel. It must exit 0 and decode the requested duration.
func TestPlayer(t *testing.T) {
	if os.Getenv(envPlayer) != "1" {
		t.Skipf("set %s=1 to run the ffmpeg player tests (docs/testing.md, L4)", envPlayer)
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatalf("%s=1 needs ffmpeg: %v", envPlayer, err)
	}

	fx := New(os.DirFS(mediaDir), Options{})
	peer := tunneltest.Start(t, tunneltest.Options{Handler: fx})
	peer.SetHost(fixtureHost, tunneltest.ServerAddr4)
	t.Cleanup(fx.Close)
	base := startProxy(t, peer, playerCases)

	for _, tc := range playerCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.skip != "" {
				t.Skip(tc.skip)
			}
			t.Parallel()
			u := base + "/c/" + tc.id
			if tc.hls {
				u += ".m3u8"
			}
			p := play(t, ffmpeg, u, tc.seconds)
			if p.state != "end" {
				t.Errorf("ffmpeg progress %q, want end", p.state)
			}
			// ffmpeg stops at -t; allow one second less for the end of
			// the last packet.
			if want := time.Duration(tc.seconds-1) * time.Second; p.outTime < want {
				t.Errorf("decoded %v, want at least %v", p.outTime, want)
			}
			if tc.video && p.frames < (tc.seconds-1)*25 {
				t.Errorf("decoded %d video frames, want at least %d (25 fps)", p.frames, (tc.seconds-1)*25)
			}
			reqs := fx.Requests()
			for _, prefix := range tc.fetched {
				if !hasPrefix(reqs, prefix) {
					t.Errorf("the player fetched nothing under %s", prefix)
				}
			}
			if tc.reloaded != "" {
				if n := count(reqs, tc.reloaded); n < 2 {
					t.Errorf("the player fetched the live playlist %s %d times, want reloads", tc.reloaded, n)
				}
			}
		})
	}
}

// startProxy runs server.Run with one tunnel to peer and a channel per
// case, and returns the proxy's base URL.
func startProxy(t *testing.T, peer *tunneltest.Peer, cases []playerCase) string {
	t.Helper()
	var channels strings.Builder
	channels.WriteString("tunnel = \"media\"\n")
	for _, tc := range cases {
		channels.WriteString("\n[[channel]]\nname = " + strconv.Quote(tc.name) + "\nid = " + strconv.Quote(tc.id) +
			"\nurl = " + strconv.Quote("http://"+fixtureHost+tc.upstream) + "\n")
	}
	dir := t.TempDir()
	for name, content := range map[string]string{
		"config.toml":           "",
		"tunnels/media.conf":    peer.ClientConfig(),
		"channels.d/media.toml": channels.String(),
	} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.Load(config.Options{Paths: config.Paths{ConfigDir: dir, TunnelsDir: filepath.Join(dir, "tunnels")}})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	signer, err := hls.NewSigner([]byte("test-only-signing-key-0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- server.Run(ctx, server.RunOptions{
			Config:          cfg,
			Signer:          signer,
			Logger:          slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
			Version:         "player-test",
			Listener:        ln,
			ShutdownTimeout: 5 * time.Second,
		})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
		if t.Failed() {
			t.Logf("proxy log:\n%s", logs.String())
		}
	})
	return "http://" + ln.Addr().String()
}

// progress is the last state ffmpeg reported with -progress.
type progress struct {
	state   string // "continue" or "end"
	frames  int
	outTime time.Duration
}

// play runs ffmpeg like a player that decodes everything and discards
// the output. Any decoding error fails the run (-xerror).
func play(t *testing.T, ffmpeg, u string, seconds int) progress {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg,
		"-nostdin", "-hide_banner", "-nostats", "-loglevel", "warning", "-xerror",
		"-progress", "pipe:1",
		// Fail instead of hanging when the proxy stalls (microseconds).
		"-rw_timeout", "15000000",
		"-i", u, "-t", strconv.Itoa(seconds), "-f", "null", "-")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("ffmpeg -i %s: %v\n%s", u, err, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Logf("ffmpeg warnings:\n%s", stderr.String())
	}
	var p progress
	sc := bufio.NewScanner(&stdout)
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), "=")
		switch k {
		case "progress":
			p.state = v
		case "frame":
			p.frames, _ = strconv.Atoi(v)
		case "out_time_us":
			if us, err := strconv.ParseInt(v, 10, 64); err == nil {
				p.outTime = time.Duration(us) * time.Microsecond
			}
		}
	}
	return p
}

func hasPrefix(paths []string, prefix string) bool {
	for _, p := range paths {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

func count(paths []string, path string) int {
	n := 0
	for _, p := range paths {
		if p == path {
			n++
		}
	}
	return n
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
