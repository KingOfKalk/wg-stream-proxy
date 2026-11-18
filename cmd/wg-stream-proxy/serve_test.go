// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
)

// syncBuffer is a goroutine-safe output sink.
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

// testCLI returns a cli with the given environment and no system
// config dir.
func testCLI(env map[string]string) (*cli, *syncBuffer, *syncBuffer) {
	out, errOut := &syncBuffer{}, &syncBuffer{}
	c := &cli{
		stdout: out,
		stderr: errOut,
		env: config.Env{
			LookupEnv:   func(k string) (string, bool) { v, ok := env[k]; return v, ok },
			DirExists:   func(string) bool { return false },
			UserHomeDir: func() (string, error) { return "/nonexistent-home", nil },
		},
		readFile: os.ReadFile,
		ctx:      context.Background(),
	}
	return c, out, errOut
}

// writeDir writes a config dir from relative path -> content.
func writeDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const exampleDir = "../../examples/config"

func TestCheckExampleConfig(t *testing.T) {
	c, out, errOut := testCLI(nil)
	if code := c.run([]string{"check", "--config-dir", exampleDir}); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut)
	}
	if want := "config ok: 2 channels (2 enabled), 1 tunnels\n"; out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
}

func TestCheckListsAllErrors(t *testing.T) {
	dir := writeDir(t, map[string]string{
		"config.toml":        "listen = \"nope\"\n",
		"channels.d/tv.toml": "[[channel]]\nname = \"News\"\nurl = \"https://tv.example.com/news.m3u8\"\n",
		"tunnels/gr.conf":    "[Interface]\nPostUp = true\n",
	})
	c, out, errOut := testCLI(map[string]string{"WG_STREAM_PROXY_SIGNING_KEY": "tiny-key-value"})
	if code := c.run([]string{"check", "--config-dir", dir}); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty", out.String())
	}
	for _, want := range []string{
		"invalid config",
		"listen",      // config.toml
		"no route",    // channel without tunnel (fail closed)
		"PostUp",      // tunnel file
		"signing key", // key too short
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
	}
	if strings.Contains(errOut.String(), "tiny-key-value") {
		t.Errorf("stderr contains the signing key:\n%s", errOut)
	}
}

func TestCheckMissingConfigDir(t *testing.T) {
	c, _, errOut := testCLI(map[string]string{"WG_STREAM_PROXY_CONFIG_DIR": "/nonexistent/wsp"})
	if code := c.run([]string{"check"}); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "config dir") {
		t.Errorf("stderr = %q, want a config dir error", errOut)
	}
}

func TestServeStartupErrors(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	good := writeDir(t, map[string]string{"config.toml": ""})
	tests := []struct {
		name string
		args []string
		env  map[string]string
		code int
		want string
	}{
		{"default command, missing config dir", nil, map[string]string{"WG_STREAM_PROXY_CONFIG_DIR": "/nonexistent/wsp"}, 1, "config dir"},
		{"invalid config", []string{"--config-dir", good, "--log-level", "loud"}, nil, 1, "log_level"},
		{"listen error", []string{"serve", "--config-dir", good, "--listen", busy.Addr().String()}, nil, 1, "listen"},
		{"signing key conflict", []string{"--config-dir", good}, map[string]string{
			"WG_STREAM_PROXY_SIGNING_KEY": strings.Repeat("k", 32), "WG_STREAM_PROXY_SIGNING_KEY_FILE": "/k",
		}, 1, "set only one"},
		{"unknown flag", []string{"serve", "--bogus"}, nil, 2, "bogus"},
		{"extra argument", []string{"serve", "extra"}, nil, 2, "unexpected arguments"},
		{"check extra argument", []string{"check", "extra"}, nil, 2, "unexpected arguments"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, out, errOut := testCLI(tt.env)
			if code := c.run(tt.args); code != tt.code {
				t.Errorf("exit code = %d, want %d; stderr:\n%s", code, tt.code, errOut)
			}
			if out.String() != "" {
				t.Errorf("stdout = %q, want empty", out.String())
			}
			if !strings.Contains(errOut.String(), tt.want) {
				t.Errorf("stderr lacks %q:\n%s", tt.want, errOut)
			}
		})
	}
}

func TestServeHelp(t *testing.T) {
	c, _, errOut := testCLI(nil)
	if code := c.run([]string{"serve", "-h"}); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	for _, flag := range []string{"-config-dir", "-listen", "-log-level", "-log-format"} {
		if !strings.Contains(errOut.String(), flag) {
			t.Errorf("help lacks %s:\n%s", flag, errOut)
		}
	}
}

func TestServe(t *testing.T) {
	dir := writeDir(t, map[string]string{
		"channels.d/tv.toml": "tunnel = \"direct\"\n[[channel]]\nname = \"News\"\nurl = \"https://tv.example.com/news.m3u8\"\n",
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, out, errOut := testCLI(map[string]string{"WG_STREAM_PROXY_LISTEN": "127.0.0.1:0"})
	c.ctx = ctx
	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()
	var health, list string
	c.listening = func(addr net.Addr) {
		// Requests wait in the listen backlog until the server runs.
		go func() {
			defer cancel()
			health = get(t, client, "http://"+addr.String()+"/healthz")
			list = get(t, client, "http://"+addr.String()+"/playlist.m3u8")
		}()
	}
	if code := c.run([]string{"--config-dir", dir, "--log-format", "json", "--log-level", "debug"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut)
	}
	if want := "wg-stream-proxy dev\nsource: https://github.com/KingOfKalk/wg-stream-proxy\n"; health != want {
		t.Errorf("/healthz = %q, want %q", health, want)
	}
	if !strings.Contains(list, "/c/news.m3u8\n") {
		t.Errorf("/playlist.m3u8 = %q", list)
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty", out.String())
	}
	var msgs []string
	for _, line := range strings.Split(strings.TrimSpace(errOut.String()), "\n") {
		var rec struct{ Msg string }
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		msgs = append(msgs, rec.Msg)
	}
	joined := strings.Join(msgs, "\n")
	for _, want := range []string{"signing key", "serving", "shutting down", "stopped"} {
		if !strings.Contains(joined, want) {
			t.Errorf("logs lack %q:\n%s", want, errOut)
		}
	}
}

func get(t *testing.T, c *http.Client, u string) string {
	resp, err := c.Get(u)
	if err != nil {
		t.Errorf("GET %s: %v", u, err)
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Errorf("GET %s: %v", u, err)
	}
	return string(b)
}

func TestServeReloadsOnSIGHUP(t *testing.T) {
	channel := func(name string) string {
		return "[[channel]]\nname = \"" + name + "\"\nurl = \"https://tv.example.com/" + name + ".m3u8\"\n"
	}
	dir := writeDir(t, map[string]string{"channels.d/tv.toml": "tunnel = \"direct\"\n" + channel("News")})
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte(strings.Repeat("a", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, _, errOut := testCLI(map[string]string{
		"WG_STREAM_PROXY_LISTEN":           "127.0.0.1:0",
		"WG_STREAM_PROXY_SIGNING_KEY_FILE": keyFile,
	})
	c.ctx = ctx
	c.hangup = make(chan os.Signal)
	reloaded := make(chan error)
	c.reloaded = func(err error) { reloaded <- err }
	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()
	var before, after string
	var results []error
	c.listening = func(addr net.Addr) {
		go func() {
			defer cancel()
			list := "http://" + addr.String() + "/playlist.m3u8"
			before = get(t, client, list)
			// A valid change with a new signing key: applied, the key kept.
			if err := os.WriteFile(filepath.Join(dir, "channels.d", "tv.toml"), []byte("tunnel = \"direct\"\n"+channel("News")+channel("Sport")), 0o600); err != nil {
				t.Error(err)
				return
			}
			if err := os.WriteFile(keyFile, []byte(strings.Repeat("b", 32)), 0o600); err != nil {
				t.Error(err)
				return
			}
			c.hangup <- syscall.SIGHUP
			results = append(results, <-reloaded)
			// An invalid change: rejected.
			if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("bogus = 1\n"), 0o600); err != nil {
				t.Error(err)
				return
			}
			c.hangup <- syscall.SIGHUP
			results = append(results, <-reloaded)
			after = get(t, client, list)
		}()
	}
	if code := c.run([]string{"--config-dir", dir}); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut)
	}
	if strings.Contains(before, "/c/sport") || !strings.Contains(after, "/c/sport") {
		t.Errorf("channel list before reload:\n%s\nafter:\n%s", before, after)
	}
	if len(results) != 2 || results[0] != nil || results[1] == nil {
		t.Errorf("reload results = %v, want [nil, error]", results)
	}
	for _, want := range []string{"signing key changed, restart required", "msg=reloaded", "bogus", "reload rejected"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("logs lack %q:\n%s", want, errOut)
		}
	}
	if strings.Contains(errOut.String(), strings.Repeat("b", 32)) {
		t.Errorf("logs contain the signing key:\n%s", errOut)
	}
}

func TestReloadLoaderKeepsRestartSettings(t *testing.T) {
	dir := writeDir(t, map[string]string{"config.toml": ""})
	oldTunnels := filepath.Join(dir, "tunnels")
	key := []byte(strings.Repeat("k", 32))
	tests := []struct {
		name      string
		env       map[string]string
		generated bool
		want      []string
	}{
		{"nothing changed", map[string]string{"WG_STREAM_PROXY_SIGNING_KEY": string(key)}, false, nil},
		{"random key stays random", nil, true, nil},
		{
			"tunnels dir moved",
			map[string]string{"WG_STREAM_PROXY_TUNNELS_DIR": t.TempDir(), "WG_STREAM_PROXY_SIGNING_KEY": string(key)},
			false,
			[]string{"tunnels dir changed, restart required"},
		},
		{"key set", map[string]string{"WG_STREAM_PROXY_SIGNING_KEY": string(key)}, true, []string{"signing key changed"}},
		{"key changed", map[string]string{"WG_STREAM_PROXY_SIGNING_KEY": strings.Repeat("n", 32)}, false, []string{"signing key changed"}},
		{"key removed", nil, false, []string{"signing key changed"}},
		{"key invalid", map[string]string{"WG_STREAM_PROXY_SIGNING_KEY": "short"}, false, []string{"signing key: restart required"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _, _ := testCLI(tt.env)
			var logs syncBuffer
			log := newLogger(&logs, config.LogFormatText, slog.LevelInfo)
			first := config.Options{Paths: config.Paths{ConfigDir: dir, TunnelsDir: oldTunnels}}
			cfg, err := c.reloadLoader(config.Flags{ConfigDir: dir}, first, log, key, tt.generated)()
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if got := cfg.Paths().TunnelsDir; got != oldTunnels {
				t.Errorf("tunnels dir = %s, want the old %s", got, oldTunnels)
			}
			out := logs.String()
			if len(tt.want) == 0 && out != "" {
				t.Errorf("unexpected logs:\n%s", out)
			}
			for _, want := range tt.want {
				if !strings.Contains(out, want) {
					t.Errorf("logs lack %q:\n%s", want, out)
				}
			}
			if strings.Contains(out, string(key)) || strings.Contains(out, strings.Repeat("n", 32)) {
				t.Errorf("logs contain a signing key:\n%s", out)
			}
		})
	}
}
