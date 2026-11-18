// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel/tunneltest"
)

// runEnv is the proxy started with Run on a loopback listener, with
// recording networks for the direct route and every tunnel (spec 9).
type runEnv struct {
	url    string
	client *http.Client
	logs   *syncBuffer
	direct *recorder

	mu      sync.Mutex
	tunnels map[string]*recorder
}

func (e *runEnv) tunnel(name string) *recorder {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.tunnels[name]
}

func loadDir(t *testing.T, dir string) *config.Config {
	t.Helper()
	cfg, err := config.Load(config.Options{Paths: config.Paths{ConfigDir: dir, TunnelsDir: filepath.Join(dir, "tunnels")}})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

// startRun runs the proxy for the config dir until the test ends. mods
// adjust the options.
func startRun(t *testing.T, dir string, shutdown time.Duration, mods ...func(*RunOptions)) *runEnv {
	t.Helper()
	e := &runEnv{logs: &syncBuffer{}, direct: &recorder{n: route.Host()}, tunnels: map[string]*recorder{}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e.url = "http://" + ln.Addr().String()
	e.client = &http.Client{Transport: &http.Transport{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	opts := RunOptions{
		Config:          loadDir(t, dir),
		Signer:          newSigner(t),
		Logger:          newLogger(e.logs),
		Version:         "test",
		Listener:        ln,
		ShutdownTimeout: shutdown,
		Direct:          e.direct,
		WrapTunnel: func(name string, n route.Network) route.Network {
			r := &recorder{n: n}
			e.mu.Lock()
			e.tunnels[name] = r
			e.mu.Unlock()
			return r
		},
	}
	for _, mod := range mods {
		mod(&opts)
	}
	go func() { done <- Run(ctx, opts) }()
	t.Cleanup(func() {
		e.client.CloseIdleConnections()
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	return e
}

// TestRunTunnelAndDirect is the L3 path of testing.md: an HLS channel
// through an in-process tunnel and one direct, with recording networks.
func TestRunTunnelAndDirect(t *testing.T) {
	peerUp := newUpstream()
	peer := tunneltest.Start(t, tunneltest.Options{Handler: peerUp})
	peer.SetHost("upstream.test", tunneltest.ServerAddr4)
	tunnelRes := hlsFixture(peerUp, "http://upstream.test")

	directUp := newUpstream()
	directSrv := httptest.NewServer(directUp)
	t.Cleanup(directSrv.Close)
	directRes := hlsFixture(directUp, directSrv.URL)

	dir := writeConfig(t, map[string]string{
		"config.toml":     allowLoopback,
		"tunnels/gr.conf": peer.ClientConfig(),
		// Route from the file stem (spec 5.2).
		"channels.d/gr.toml": `
group = "Ελληνικά"
[[channel]]
name = "Ελληνικά Νέα, 24/7"
url = "http://upstream.test/live/master.m3u8"
[channel.playlist]
tvg_id = "nea.gr"
attrs = { tvg-chno = "1" }
[channel.playlist.extra.KODIPROP]
"inputstream.adaptive.manifest_type" = "hls"

[[channel]]
name = "Off"
url = "http://upstream.test/live/master.m3u8"
enabled = false
`,
		"channels.d/de.toml": `
tunnel = "direct"
[[channel]]
name = "Das Erste"
url = "` + directSrv.URL + `/live/master.m3u8"
`,
	})
	e := startRun(t, dir, 0)

	// The channel list starts no tunnel and makes no upstream request.
	resp, body := do(t, e.client, http.MethodGet, e.url+"/playlist.m3u8",
		http.Header{"X-Forwarded-Proto": {"https"}, "X-Forwarded-Host": {"tv.example.org"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("playlist: status %d", resp.StatusCode)
	}
	checkGolden(t, "playlist-run", body)

	gr := e.tunnel("gr")
	if gr == nil {
		t.Fatal("tunnel gr not wrapped")
	}
	if gr.used() != 0 || e.direct.used() != 0 {
		t.Fatalf("playlist used a network: tunnel %d, direct %d", gr.used(), e.direct.used())
	}

	// direct: never touches the tunnel.
	got, _ := crawl(t, e.client, e.url, "das-erste", e.url+"/c/das-erste.m3u8")
	if !maps(got, directRes) {
		t.Errorf("direct resources = %v, want %v", got, directRes)
	}
	if gr.used() != 0 {
		t.Errorf("direct channel used the tunnel: %d calls", gr.used())
	}
	directUsed := e.direct.used()
	if directUsed == 0 {
		t.Error("direct channel did not use the direct network")
	}

	// Tunnel: playlists, keys and segments all through the tunnel.
	got, playlists := crawl(t, e.client, e.url, "ellinika-nea-24-7", e.url+"/c/ellinika-nea-24-7.m3u8")
	if !maps(got, tunnelRes) || playlists != 5 {
		t.Errorf("tunnel resources = %v (%d playlists), want %v", got, playlists, tunnelRes)
	}
	if n := e.direct.used(); n != directUsed {
		t.Errorf("tunnel channel used the direct network: %d calls", n-directUsed)
	}
	if gr.dials.Load() == 0 {
		t.Error("tunnel channel did not dial through the tunnel")
	}
	reqs := peerUp.requests()
	if len(reqs) < len(tunnelRes)+5 {
		t.Errorf("server peer saw %d requests, want at least %d", len(reqs), len(tunnelRes)+5)
	}
	for _, r := range reqs {
		ap, err := netip.ParseAddrPort(r.Remote)
		if err != nil || ap.Addr() != tunneltest.ClientAddr4 {
			t.Errorf("server peer saw %s from %s, want the tunnel address %s", r.Path, r.Remote, tunneltest.ClientAddr4)
		}
	}
	if !slices.Contains(peer.Queries(), "upstream.test A") {
		t.Errorf("tunnel DNS queries = %v, want upstream.test A", peer.Queries())
	}

	// Range and HEAD through the tunnel.
	sig := newSigner(t)
	seg := e.url + sig.Path("ellinika-nea-24-7", "http://upstream.test/cdn/v1/low/seg8.m4s")
	resp, body = do(t, e.client, http.MethodGet, seg, http.Header{"Range": {"bytes=0-2"}})
	if resp.StatusCode != http.StatusPartialContent || body != "low" {
		t.Errorf("Range through tunnel: status %d, body %q", resp.StatusCode, body)
	}
	resp, body = do(t, e.client, http.MethodHead, seg, nil)
	if resp.StatusCode != http.StatusOK || body != "" || resp.ContentLength != int64(len("low segment 8")) {
		t.Errorf("HEAD through tunnel: status %d, length %d, body %q", resp.StatusCode, resp.ContentLength, body)
	}

	// Disabled channel and tampered resource.
	for _, p := range []string{
		"/c/off.m3u8",
		sig.Path("off", "http://upstream.test/cdn/v1/low/seg8.m4s"),
		strings.Replace(sig.Path("ellinika-nea-24-7", "http://upstream.test/cdn/v1/low/seg8.m4s"), "/r/ellinika-nea-24-7/", "/r/das-erste/", 1),
	} {
		if resp, _ := do(t, e.client, http.MethodGet, e.url+p, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", p, resp.StatusCode)
		}
	}
	if strings.Contains(e.logs.String(), "secret") {
		t.Errorf("logs contain an upstream query string:\n%s", e.logs.String())
	}
}

func maps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func TestRunShutdownEndsStreams(t *testing.T) {
	up := newUpstream()
	up.add("/radio", file{ctype: "audio/mpeg", body: strings.Repeat("a", 1024), endless: true})
	upSrv := httptest.NewServer(up)
	t.Cleanup(upSrv.Close)
	dir := writeConfig(t, map[string]string{
		"config.toml":           allowLoopback,
		"channels.d/radio.toml": "tunnel = \"direct\"\n[[channel]]\nname = \"Radio\"\nurl = \"" + upSrv.URL + "/radio\"\n",
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunOptions{
			Config: loadDir(t, dir), Signer: newSigner(t), Logger: newLogger(logs),
			Listener: ln, ShutdownTimeout: time.Millisecond,
		})
	}()
	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String()+"/c/radio", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.ReadFull(resp.Body, make([]byte, 4096)); err != nil {
		t.Fatalf("read stream: %v", err)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run: %v", err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err == nil {
		t.Error("stream ended cleanly, want it cut at shutdown")
	}
	<-up.gone
	for _, want := range []string{"shutting down", "shutdown timeout"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs lack %q:\n%s", want, logs.String())
		}
	}
}

func TestRunGracefulShutdown(t *testing.T) {
	dir := writeConfig(t, map[string]string{"config.toml": ""})
	e := startRun(t, dir, 0)
	resp, _ := do(t, e.client, http.MethodGet, e.url+"/healthz", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz: status %d", resp.StatusCode)
	}
}

func TestRunErrors(t *testing.T) {
	cfg := loadDir(t, writeConfig(t, nil))
	signer := newSigner(t)
	listen := func() net.Listener {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		return ln
	}
	closed := listen()
	_ = closed.Close()
	badTunnel := loadDir(t, writeConfig(t, map[string]string{
		"tunnels/gr.conf": "[Interface]\nPostUp = true\n",
	}))
	tests := []struct {
		name string
		opts RunOptions
		want string
	}{
		{"no config", RunOptions{Signer: signer, Listener: listen()}, "config"},
		{"no signer", RunOptions{Config: cfg, Listener: listen()}, "signer"},
		{"no listener", RunOptions{Config: cfg, Signer: signer}, "listener"},
		{"invalid tunnel file", RunOptions{Config: badTunnel, Signer: signer, Listener: listen()}, "PostUp"},
		{"closed listener", RunOptions{Config: cfg, Signer: signer, Listener: closed}, "closed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Run(t.Context(), tt.opts)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Run() = %v, want error with %q", err, tt.want)
			}
			if ln := tt.opts.Listener; ln != nil {
				if _, err := ln.Accept(); !errors.Is(err, net.ErrClosed) {
					t.Errorf("listener not closed: Accept() = %v", err)
				}
			}
		})
	}
}
