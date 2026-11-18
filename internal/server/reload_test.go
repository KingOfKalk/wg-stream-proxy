// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel/tunneltest"
)

// reload reloads the proxy and returns the result. The first request
// makes sure Run serves.
func (e *runEnv) reload(t *testing.T) error {
	t.Helper()
	if resp, _ := do(t, e.client, http.MethodGet, e.url+"/healthz", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: status %d", resp.StatusCode)
	}
	return e.reloader.Reload()
}

func (e *runEnv) wrapped(name string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.wraps[name]
}

// get fetches path and fails t unless the status is want; it returns the
// body.
func (e *runEnv) get(t *testing.T, path string, want int) string {
	t.Helper()
	resp, body := do(t, e.client, http.MethodGet, e.url+path, nil)
	if resp.StatusCode != want {
		t.Fatalf("GET %s: status %d, want %d: %s", path, resp.StatusCode, want, body)
	}
	return body
}

// writeFiles writes (or, for an empty content, removes) files of a
// config dir.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if content == "" {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// openStream starts a GET whose body the caller reads and closes.
func (e *runEnv) openStream(t *testing.T, path string) io.ReadCloser {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, e.url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("GET %s: status %d", path, resp.StatusCode)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp.Body
}

func readSome(t *testing.T, r io.Reader) {
	t.Helper()
	if _, err := io.ReadFull(r, make([]byte, 4096)); err != nil {
		t.Fatalf("read stream: %v", err)
	}
}

func TestReloadAppliesValidChange(t *testing.T) {
	up := newUpstream()
	up.add("/a", file{ctype: "text/plain", body: "A"})
	up.add("/b", file{ctype: "text/plain", body: "B"})
	upSrv := httptest.NewServer(up)
	t.Cleanup(upSrv.Close)
	channel := func(name, path string) string {
		return "[[channel]]\nname = \"" + name + "\"\nurl = \"" + upSrv.URL + path + "\"\n"
	}
	dir := writeConfig(t, map[string]string{
		"config.toml":        allowLoopback,
		"channels.d/tv.toml": "tunnel = \"direct\"\n" + channel("News", "/a"),
	})
	var level slog.LevelVar
	e := startRun(t, dir, 0, func(o *RunOptions) { o.LogLevel = &level })
	if body := e.get(t, "/c/news", http.StatusOK); body != "A" {
		t.Fatalf("before reload: %q", body)
	}

	writeFiles(t, dir, map[string]string{
		// listen needs a restart; log_level applies.
		"config.toml":        "listen = \"127.0.0.1:1\"\nlog_level = \"debug\"\n" + allowLoopback,
		"channels.d/tv.toml": "tunnel = \"direct\"\n" + channel("News", "/b") + channel("Sport", "/a"),
	})
	if err := e.reload(t); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if list := e.get(t, "/playlist.m3u8", http.StatusOK); !strings.Contains(list, "/c/sport\n") {
		t.Errorf("new channel missing from the channel list:\n%s", list)
	}
	if body := e.get(t, "/c/news", http.StatusOK); body != "B" {
		t.Errorf("changed URL not used: %q", body)
	}
	if body := e.get(t, "/c/sport", http.StatusOK); body != "A" {
		t.Errorf("new channel: %q", body)
	}
	if level.Level() != slog.LevelDebug {
		t.Errorf("log level after reload = %s, want debug", level.Level())
	}
	logs := e.logs.String()
	for _, want := range []string{`msg=reloaded`, `listen changed, restart required`} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %q:\n%s", want, logs)
		}
	}
	// Still serving on the old address.
	e.get(t, "/healthz", http.StatusOK)
}

func TestReloadRejectsInvalidChange(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	up := newUpstream()
	up.add("/a", file{ctype: "text/plain", body: "A"})
	upSrv := httptest.NewServer(up)
	t.Cleanup(upSrv.Close)
	tv := "tunnel = \"direct\"\n[[channel]]\nname = \"News\"\nurl = \"" + upSrv.URL + "/a\"\n"
	tests := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{
			name: "config and channel errors",
			files: map[string]string{
				"config.toml":        "bogus = 1\n" + allowLoopback,
				"channels.d/tv.toml": tv + "[[channel]]\nname = \"Lost\"\nurl = \"http://tv.example.com/\"\ntunnel = \"nowhere\"\n",
			},
			want: []string{"bogus", "nowhere"},
		},
		{
			name:  "tunnel file errors",
			files: map[string]string{"tunnels/gr.conf": "[Interface]\nPostUp = true\n", "tunnels/uk.conf": "[Peer]\n"},
			want:  []string{"PostUp", `tunnel \"uk\"`},
		},
		{
			name:  "shared private key",
			files: map[string]string{"tunnels/uk.conf": peer.ClientConfig()},
			want:  []string{"same PrivateKey"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeConfig(t, map[string]string{
				"config.toml":        allowLoopback,
				"tunnels/gr.conf":    peer.ClientConfig(),
				"channels.d/tv.toml": tv,
			})
			e := startRun(t, dir, 0)
			before := e.get(t, "/playlist.m3u8", http.StatusOK)

			writeFiles(t, dir, tt.files)
			err := e.reload(t)
			if err == nil {
				t.Fatal("Reload succeeded, want error")
			}
			logs := e.logs.String()
			for _, want := range append(tt.want, "reload rejected") {
				if !strings.Contains(logs, want) {
					t.Errorf("logs lack %q:\n%s", want, logs)
				}
			}
			// The old config keeps serving.
			if got := e.get(t, "/playlist.m3u8", http.StatusOK); got != before {
				t.Errorf("channel list changed:\n%s\nwant\n%s", got, before)
			}
			if body := e.get(t, "/c/news", http.StatusOK); body != "A" {
				t.Errorf("old channel: %q", body)
			}
			if strings.Contains(logs, "msg=reloaded") {
				t.Errorf("rejected reload logged as applied:\n%s", logs)
			}
		})
	}
}

// tunnelEnv is a proxy with tunnels to in-process peers, each serving
// its own upstream at upstream.test.
type tunnelEnv struct {
	*runEnv
	dir   string
	clock *tunnelClock
}

// newPeer starts a peer whose upstream serves /seg.ts with body and an
// endless /radio stream.
func newPeer(t *testing.T, body string) (*tunneltest.Peer, *upstream) {
	t.Helper()
	up := newUpstream()
	up.add("/seg.ts", file{ctype: "video/mp2t", body: body})
	up.add("/radio", file{ctype: "audio/mpeg", body: strings.Repeat("r", 1024), endless: true})
	p := tunneltest.Start(t, tunneltest.Options{Handler: up})
	p.SetHost("upstream.test", tunneltest.ServerAddr4)
	return p, up
}

// tunnelChannels is a channel file for tunnel name: a segment channel
// <name>-seg and a radio <name>-radio.
func tunnelChannels(name string) string {
	return "tunnel = \"" + name + "\"\n" +
		"[[channel]]\nname = \"" + name + " seg\"\nurl = \"http://upstream.test/seg.ts\"\n" +
		"[[channel]]\nname = \"" + name + " radio\"\nurl = \"http://upstream.test/radio\"\n"
}

func startTunnels(t *testing.T, files map[string]string) *tunnelEnv {
	t.Helper()
	dir := writeConfig(t, files)
	clk := newTunnelClock()
	return &tunnelEnv{runEnv: startRun(t, dir, 0, func(o *RunOptions) { o.TunnelClock = clk }), dir: dir, clock: clk}
}

func (e *tunnelEnv) starts(name string) int {
	return strings.Count(e.logs.String(), `msg="tunnel started" tunnel=`+name+"\n")
}

// closedByReload is the log of a tunnel closed after a reload drain.
func closedByReload(name string) string {
	return `msg="tunnel closed" tunnel=` + name + " reason=reload"
}

func TestReloadKeepsUnchangedTunnel(t *testing.T) {
	peer, up := newPeer(t, "seg")
	conf := peer.ClientConfig()
	// Segments (/r/) use the normal connection pool; /c/ requests of auto
	// channels use the ICY transport, which does not keep connections.
	seg := func(id string) string { return newSigner(t).Path(id, "http://upstream.test/seg.ts") }
	e := startTunnels(t, map[string]string{"tunnels/gr.conf": conf, "channels.d/gr.toml": tunnelChannels("gr")})
	e.get(t, seg("gr-seg"), http.StatusOK)

	// Only comments in the tunnel file change; a channel is added.
	writeFiles(t, e.dir, map[string]string{
		"tunnels/gr.conf":    "# moved to another host\n" + strings.ReplaceAll(conf, "\n", " # same\n"),
		"channels.d/gr.toml": tunnelChannels("gr") + "[[channel]]\nname = \"More\"\nurl = \"http://upstream.test/seg.ts\"\n",
	})
	if err := e.reload(t); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	e.get(t, seg("gr-seg"), http.StatusOK)
	e.get(t, seg("more"), http.StatusOK)

	if n := e.starts("gr"); n != 1 {
		t.Errorf("tunnel starts = %d, want 1 (same device, no new handshake)", n)
	}
	if n := e.wrapped("gr"); n != 1 {
		t.Errorf("tunnel network wrapped %d times, want 1", n)
	}
	// The pooled upstream connection is reused across the reload.
	reqs := up.requests()
	if len(reqs) != 3 || reqs[0].Remote != reqs[1].Remote || reqs[1].Remote != reqs[2].Remote {
		t.Errorf("upstream requests came from %v, want one pooled connection", reqs)
	}
	if strings.Contains(e.logs.String(), `msg="tunnel closed"`) {
		t.Errorf("unchanged tunnel closed:\n%s", e.logs.String())
	}
}

func TestReloadRecreatesChangedTunnel(t *testing.T) {
	oldPeer, _ := newPeer(t, "old")
	nextPeer, _ := newPeer(t, "new")
	e := startTunnels(t, map[string]string{"tunnels/gr.conf": oldPeer.ClientConfig(), "channels.d/gr.toml": tunnelChannels("gr")})
	if body := e.get(t, "/c/gr-seg", http.StatusOK); body != "old" {
		t.Fatalf("before reload: %q", body)
	}

	writeFiles(t, e.dir, map[string]string{"tunnels/gr.conf": nextPeer.ClientConfig()})
	if err := e.reload(t); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	// Nothing in flight: the old tunnel closes at once.
	e.logs.waitFor(t, closedByReload("gr"), 1)
	if body := e.get(t, "/c/gr-seg", http.StatusOK); body != "new" {
		t.Errorf("after reload: %q, want the new tunnel's upstream", body)
	}
	if n := e.starts("gr"); n != 2 {
		t.Errorf("tunnel starts = %d, want 2", n)
	}
}

func TestReloadDrainsRemovedTunnel(t *testing.T) {
	gr, _ := newPeer(t, "gr")
	uk, _ := newPeer(t, "uk")
	e := startTunnels(t, map[string]string{
		"tunnels/gr.conf": gr.ClientConfig(), "channels.d/gr.toml": tunnelChannels("gr"),
		"tunnels/uk.conf": uk.ClientConfig(), "channels.d/uk.toml": tunnelChannels("uk"),
	})
	stream := e.openStream(t, "/c/uk-radio")
	readSome(t, stream)

	writeFiles(t, e.dir, map[string]string{"tunnels/uk.conf": "", "channels.d/uk.toml": ""})
	if err := e.reload(t); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	// New requests see the new config; the stream goes on.
	e.get(t, "/c/uk-radio", http.StatusNotFound)
	e.get(t, "/c/gr-seg", http.StatusOK)
	e.clock.waitArmed(t, 30*time.Second)
	e.clock.advance(30*time.Second - time.Nanosecond)
	readSome(t, stream)
	if strings.Contains(e.logs.String(), closedByReload("uk")) {
		t.Fatal("removed tunnel closed with a stream in flight")
	}

	// The stream ends: the tunnel closes.
	_ = stream.Close()
	e.logs.waitFor(t, closedByReload("uk"), 1)
	if strings.Contains(e.logs.String(), closedByReload("gr")) {
		t.Error("unchanged tunnel gr closed by the reload")
	}
}

func TestReloadDuringTunnelStart(t *testing.T) {
	down, _ := newPeer(t, "down")
	down.SetReachable(false)
	up, _ := newPeer(t, "up")
	e := startTunnels(t, map[string]string{"tunnels/gr.conf": down.ClientConfig(), "channels.d/gr.toml": tunnelChannels("gr")})

	const waiters = 2
	codes := make(chan int, waiters)
	var wg sync.WaitGroup
	for range waiters {
		wg.Go(func() {
			resp, err := e.client.Get(e.url + "/c/gr-seg")
			if err != nil {
				t.Error(err)
				codes <- 0
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			codes <- resp.StatusCode
		})
	}
	// The start waits for a handshake (connect_timeout 10s on the tunnel
	// clock, which never advances here).
	e.clock.waitArmed(t, 10*time.Second)

	writeFiles(t, e.dir, map[string]string{"tunnels/gr.conf": up.ClientConfig()})
	if err := e.reload(t); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusBadGateway {
			t.Errorf("request waiting for the cancelled start: status %d, want 502", code)
		}
	}
	// The new tunnel starts lazily on the next request.
	if body := e.get(t, "/c/gr-seg", http.StatusOK); body != "up" {
		t.Errorf("after reload: %q", body)
	}
}

func TestReloadConcurrent(t *testing.T) {
	dir := writeConfig(t, map[string]string{"config.toml": ""})
	e := startRun(t, dir, 0)
	e.get(t, "/healthz", http.StatusOK)
	const n = 8
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() { errs <- e.reloader.Reload() })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("Reload: %v", err)
		}
	}
	if got := strings.Count(e.logs.String(), "msg=reloaded"); got != n {
		t.Errorf("reloads = %d, want %d", got, n)
	}
}

func TestReloaderNotServing(t *testing.T) {
	dir := writeConfig(t, map[string]string{"config.toml": ""})
	r := NewReloader(func() (*config.Config, error) { return loadDir(t, dir), nil })
	if err := r.Reload(); !errors.Is(err, ErrNotServing) {
		t.Errorf("Reload before Run: err = %v, want ErrNotServing", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunOptions{Config: loadDir(t, dir), Signer: newSigner(t), Listener: ln, Reloader: r})
	}()
	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()
	if resp, _ := do(t, client, http.MethodGet, "http://"+ln.Addr().String()+"/healthz", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: status %d", resp.StatusCode)
	}
	if err := r.Reload(); err != nil {
		t.Errorf("Reload while serving: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := r.Reload(); !errors.Is(err, ErrNotServing) {
		t.Errorf("Reload after Run: err = %v, want ErrNotServing", err)
	}
}
