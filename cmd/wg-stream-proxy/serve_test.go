// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
