// SPDX-License-Identifier: AGPL-3.0-or-later

package tunnel

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/config/wgquick"
	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel/tunneltest"
)

func writeTunnel(t *testing.T, dir, name, text string, mode os.FileMode) config.Tunnel {
	t.Helper()
	path := filepath.Join(dir, name+".conf")
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return config.Tunnel{Name: name, Path: path}
}

func TestLoad(t *testing.T) {
	a := tunneltest.Start(t, tunneltest.Options{})
	b := tunneltest.Start(t, tunneltest.Options{NoDNS: true})
	dir := t.TempDir()
	files := []config.Tunnel{
		writeTunnel(t, dir, "a", a.ClientConfig(), 0o600),
		writeTunnel(t, dir, "b", b.ClientConfig(), 0o644),
	}
	var logs bytes.Buffer
	m, err := Load(files, Options{
		ConnectTimeout: time.Second,
		Logger:         slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer func() { _ = m.Close() }()
	if len(m.Tunnels()) != 2 {
		t.Errorf("Tunnels = %d, want 2", len(m.Tunnels()))
	}
	out := logs.String()
	for _, want := range []string{"world-readable", "no DNS server"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "world-readable") != 1 {
		t.Errorf("world-readable warning for the wrong files:\n%s", out)
	}
}

func TestLoadErrors(t *testing.T) {
	p := tunneltest.Start(t, tunneltest.Options{})
	conf := p.ClientConfig()
	dir := t.TempDir()
	tests := []struct {
		name  string
		files []config.Tunnel
		want  string
	}{
		{
			name:  "missing file",
			files: []config.Tunnel{{Name: "gone", Path: filepath.Join(dir, "gone.conf")}},
			want:  `tunnel "gone"`,
		},
		{
			name:  "parse error",
			files: []config.Tunnel{writeTunnel(t, dir, "bad", "[Interface]\nPostUp = true\n", 0o600)},
			want:  "PostUp not supported",
		},
		{
			name: "same private key",
			files: []config.Tunnel{
				writeTunnel(t, dir, "one", conf, 0o600),
				writeTunnel(t, dir, "two", conf, 0o600),
			},
			want: "same PrivateKey",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := Load(tt.files, Options{ConnectTimeout: time.Second})
			if err == nil {
				_ = m.Close()
				t.Fatal("Load succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestNewErrors(t *testing.T) {
	p := tunneltest.Start(t, tunneltest.Options{})
	tun := p.ClientTunnel(t, "gr")
	tests := []struct {
		name    string
		tunnels []*wgquick.Tunnel
		opts    Options
	}{
		{"no connect timeout", []*wgquick.Tunnel{tun}, Options{}},
		{"negative IPv6 timeout", []*wgquick.Tunnel{tun}, Options{ConnectTimeout: time.Second, IPv6ConnectTimeout: -1}},
		{"nil tunnel", []*wgquick.Tunnel{nil}, Options{ConnectTimeout: time.Second}},
		{"duplicate name", []*wgquick.Tunnel{tun, p.ClientTunnel(t, "gr")}, Options{ConnectTimeout: time.Second}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := New(tt.tunnels, tt.opts)
			if err == nil {
				_ = m.Close()
				t.Fatal("New succeeded, want error")
			}
		})
	}
}

func TestErrorsNeverContainKeys(t *testing.T) {
	p := tunneltest.Start(t, tunneltest.Options{})
	conf := p.ClientConfig()
	dir := t.TempDir()
	_, err := Load([]config.Tunnel{
		writeTunnel(t, dir, "one", conf, 0o600),
		writeTunnel(t, dir, "two", conf, 0o600),
	}, Options{ConnectTimeout: time.Second})
	if err == nil {
		t.Fatal("want error")
	}
	for line := range strings.SplitSeq(conf, "\n") {
		k, v, ok := strings.Cut(line, " = ")
		if ok && strings.HasSuffix(k, "Key") && strings.Contains(err.Error(), v) {
			t.Errorf("error contains %s", k)
		}
	}
}
