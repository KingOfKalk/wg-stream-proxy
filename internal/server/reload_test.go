// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"errors"
	"fmt"
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
	up.add("/radio", file{ctype: "audio/mpeg", body: binaryBody('r', 1024), endless: true})
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
	return strings.Count(e.logs.String(), `msg="tunnel started" tunnel=`+name+" duration=")
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
	// A reload builds new routes: the first request after it opens a new
	// upstream connection, the next one reuses it.
	reqs := up.requests()
	if len(reqs) != 3 || reqs[0].Remote == reqs[1].Remote || reqs[1].Remote != reqs[2].Remote {
		t.Errorf("upstream requests came from %v, want a new connection after the reload, then one pooled", reqs)
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
	// The armed timer only shows that one waiter started the tunnel. Each
	// waiter's first lookup blocks in the start, so once both have called
	// into the old tunnel's network, both wait for the start that the
	// reload cancels; a later one would be served by the new tunnel.
	e.tunnel("gr").waitUsed(t, waiters)

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

// reloadLines returns the log lines of the reload with msg.
func reloadLines(logs, msg string) []string {
	var out []string
	for line := range strings.SplitSeq(logs, "\n") {
		if strings.Contains(line, "msg="+msg) {
			out = append(out, line)
		}
	}
	return out
}

// TestReloadLogsChannelChanges: the reloaded line reports channel and
// tunnel changes separately; channel ids appear only at debug level.
func TestReloadLogsChannelChanges(t *testing.T) {
	oldPeer, _ := newPeer(t, "old")
	nextPeer, _ := newPeer(t, "new")
	news := func(extra string) string {
		return "tunnel = \"direct\"\n[[channel]]\nname = \"News\"\nurl = \"http://news.example.com/a\"\n" + extra +
			"[[channel]]\nname = \"Sport\"\nurl = \"http://sport.example.com/a\"\n"
	}
	film := "tunnel = \"direct\"\n[[channel]]\nname = \"Film\"\nurl = \"http://film.example.com/a\"\n"
	e := startTunnels(t, map[string]string{
		"tunnels/gr.conf":    oldPeer.ClientConfig(),
		"channels.d/gr.toml": tunnelChannels("gr"),
		"channels.d/tv.toml": news("playlist.extra = { KODIPROP = { a = \"1\", b = \"2\" } }\n"),
	})

	const (
		zeroT = " tunnels_added=[] tunnels_changed=[] tunnels_removed=[]"
		zeroC = " channels_added=0 channels_changed=0 channels_removed=0"
	)
	// Each step starts from the state the previous one left.
	tests := []struct {
		name     string
		files    map[string]string
		channels int
		// want is the info line after "channels=N tunnels=1".
		want string
		// debug is the end of the debug line with the channel ids; ""
		// if the reload changed no channel and logs none.
		debug string
	}{
		{
			name:     "only playlist.extra of one channel changes",
			files:    map[string]string{"channels.d/tv.toml": news("playlist.extra = { KODIPROP = { a = \"1\", b = \"3\" } }\n")},
			channels: 4,
			want:     " channels_added=0 channels_changed=1 channels_removed=0" + zeroT,
			debug:    "added=[] changed=[news] removed=[]",
		},
		{
			name:     "same content in another order",
			files:    map[string]string{"channels.d/tv.toml": news("playlist.extra = { KODIPROP = { b = \"3\", a = \"1\" } }\n")},
			channels: 4,
			want:     zeroC + zeroT,
		},
		{
			name:     "channel file added",
			files:    map[string]string{"channels.d/film.toml": film},
			channels: 5,
			want:     " channels_added=1 channels_changed=0 channels_removed=0" + zeroT,
			debug:    "added=[film] changed=[] removed=[]",
		},
		{
			name:     "channel file removed",
			files:    map[string]string{"channels.d/film.toml": ""},
			channels: 4,
			want:     " channels_added=0 channels_changed=0 channels_removed=1" + zeroT,
			debug:    "added=[] changed=[] removed=[film]",
		},
		{
			name:     "global defaults added",
			files:    map[string]string{"channels.d/_defaults.toml": "[defaults.playlist]\nattrs = { catchup = \"default\" }\n"},
			channels: 4,
			want:     " channels_added=0 channels_changed=4 channels_removed=0" + zeroT,
			debug:    `added=[] changed="[gr-radio gr-seg news sport]" removed=[]`,
		},
		{
			// News sets KODIPROP a itself, so its merged model stays.
			name: "global defaults key a channel overrides",
			files: map[string]string{"channels.d/_defaults.toml": "[defaults.playlist]\nattrs = { catchup = \"default\" }\n" +
				"[defaults.playlist.extra.KODIPROP]\na = \"9\"\n"},
			channels: 4,
			want:     " channels_added=0 channels_changed=3 channels_removed=0" + zeroT,
			debug:    `added=[] changed="[gr-radio gr-seg sport]" removed=[]`,
		},
		{
			name:     "global defaults removed",
			files:    map[string]string{"channels.d/_defaults.toml": ""},
			channels: 4,
			want:     " channels_added=0 channels_changed=4 channels_removed=0" + zeroT,
			debug:    `added=[] changed="[gr-radio gr-seg news sport]" removed=[]`,
		},
		{
			name:     "tunnel file changed",
			files:    map[string]string{"tunnels/gr.conf": nextPeer.ClientConfig()},
			channels: 4,
			want:     zeroC + " tunnels_added=[] tunnels_changed=[gr] tunnels_removed=[]",
		},
		{
			name:     "nothing changed",
			files:    map[string]string{},
			channels: 4,
			want:     zeroC + zeroT,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			infoBefore := len(reloadLines(e.logs.String(), "reloaded"))
			dbgBefore := len(reloadLines(e.logs.String(), `"reload: channel changes"`))
			writeFiles(t, e.dir, tt.files)
			if err := e.reload(t); err != nil {
				t.Fatalf("Reload: %v", err)
			}
			logs := e.logs.String()
			lines := reloadLines(logs, "reloaded")
			if len(lines) != infoBefore+1 {
				t.Fatalf("reloaded lines = %d, want %d:\n%s", len(lines), infoBefore+1, logs)
			}
			got := lines[len(lines)-1]
			want := fmt.Sprintf("channels=%d tunnels=1", tt.channels) + tt.want
			if !strings.Contains(got, " level=INFO ") || !strings.HasSuffix(got, want) {
				t.Errorf("reloaded line:\n%s\nwant it to end with\n%s", got, want)
			}
			if strings.Contains(got, "news") || strings.Contains(got, "film") || strings.Contains(got, "http") {
				t.Errorf("info line lists ids or URLs: %s", got)
			}
			dbg := reloadLines(logs, `"reload: channel changes"`)
			if tt.debug == "" {
				if len(dbg) != dbgBefore {
					t.Errorf("debug channel line for a reload without channel changes: %v", dbg[dbgBefore:])
				}
				return
			}
			if len(dbg) != dbgBefore+1 {
				t.Fatalf("debug channel lines = %d, want %d:\n%s", len(dbg), dbgBefore+1, logs)
			}
			d := dbg[len(dbg)-1]
			if !strings.Contains(d, " level=DEBUG ") || !strings.HasSuffix(d, tt.debug) || strings.Contains(d, "http") {
				t.Errorf("debug line %q, want level DEBUG ending with %q and no URL", d, tt.debug)
			}
		})
	}
}
