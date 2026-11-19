// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fpPaths returns the paths of a config dir with a separate tunnels dir.
func fpPaths(t *testing.T, files map[string]string) Paths {
	t.Helper()
	dir := writeConfigDir(t, files)
	return Paths{ConfigDir: dir, TunnelsDir: filepath.Join(dir, "tunnels")}
}

// setTime sets the modification time of a file relative to a fixed base.
func setTime(t *testing.T, p string, sec int) {
	t.Helper()
	mt := time.Unix(1_700_000_000+int64(sec), 0)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func TestFingerprint(t *testing.T) {
	files := map[string]string{
		"config.toml":               "listen = \":1\"\n",
		"channels.d/tv.toml":        "x",
		"channels.d/_defaults.toml": "d",
		"tunnels/gr.conf":           "t",
	}
	tests := []struct {
		name    string
		change  func(t *testing.T, p Paths)
		changed bool
	}{
		{"nothing", func(*testing.T, Paths) {}, false},
		{"touch same size", func(t *testing.T, p Paths) {
			setTime(t, filepath.Join(p.ConfigDir, "channels.d", "tv.toml"), 5)
		}, true},
		{"size changes, same mtime", func(t *testing.T, p Paths) {
			f := filepath.Join(p.ConfigDir, "channels.d", "tv.toml")
			if err := os.WriteFile(f, []byte("xx"), 0o600); err != nil {
				t.Fatal(err)
			}
			setTime(t, f, 0)
		}, true},
		{"config.toml", func(t *testing.T, p Paths) {
			setTime(t, filepath.Join(p.ConfigDir, "config.toml"), 5)
		}, true},
		{"defaults", func(t *testing.T, p Paths) {
			setTime(t, filepath.Join(p.ConfigDir, "channels.d", "_defaults.toml"), 5)
		}, true},
		{"tunnel", func(t *testing.T, p Paths) {
			setTime(t, filepath.Join(p.TunnelsDir, "gr.conf"), 5)
		}, true},
		{"channel file added", func(t *testing.T, p Paths) {
			if err := os.WriteFile(filepath.Join(p.ConfigDir, "channels.d", "new.toml"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"channel file removed", func(t *testing.T, p Paths) {
			if err := os.Remove(filepath.Join(p.ConfigDir, "channels.d", "tv.toml")); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"channel file renamed", func(t *testing.T, p Paths) {
			d := filepath.Join(p.ConfigDir, "channels.d")
			if err := os.Rename(filepath.Join(d, "tv.toml"), filepath.Join(d, "tv2.toml")); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"tunnel added", func(t *testing.T, p Paths) {
			if err := os.WriteFile(filepath.Join(p.TunnelsDir, "uk.conf"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"tunnel removed", func(t *testing.T, p Paths) {
			if err := os.Remove(filepath.Join(p.TunnelsDir, "gr.conf")); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"config.toml removed", func(t *testing.T, p Paths) {
			if err := os.Remove(filepath.Join(p.ConfigDir, "config.toml")); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"files a reload ignores", func(t *testing.T, p Paths) {
			for _, f := range []string{"channels.d/.tv.toml.swp", "channels.d/tv.toml~", "channels.d/notes.txt", "tunnels/signing.key", "other.toml"} {
				if err := os.WriteFile(filepath.Join(p.ConfigDir, filepath.FromSlash(f)), []byte("z"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(filepath.Join(p.ConfigDir, "channels.d", "sub.toml"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := fpPaths(t, files)
			for f := range files {
				setTime(t, filepath.Join(p.ConfigDir, filepath.FromSlash(f)), 0)
			}
			before := Fingerprint(p)
			tt.change(t, p)
			after := Fingerprint(p)
			if got := before != after; got != tt.changed {
				t.Errorf("fingerprint changed = %v, want %v\nbefore: %q\nafter:  %q", got, tt.changed, before, after)
			}
			if again := Fingerprint(p); again != after {
				t.Errorf("fingerprint is not stable:\n%q\n%q", after, again)
			}
		})
	}
}

// TestFingerprintMissing: missing config.toml, channels.d and tunnels
// dir are fine (they count as empty, spec 5.1), and so is a missing
// config dir.
func TestFingerprintMissing(t *testing.T) {
	dir := t.TempDir()
	p := Paths{ConfigDir: dir, TunnelsDir: filepath.Join(dir, "tunnels")}
	empty := Fingerprint(p)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if Fingerprint(p) == empty {
		t.Errorf("a new config.toml does not change the fingerprint")
	}
	_ = Fingerprint(Paths{ConfigDir: filepath.Join(dir, "gone"), TunnelsDir: filepath.Join(dir, "gone", "tunnels")})
}

// TestFingerprintFollowsSymlinks: like Load, a symlink counts with the
// size and mtime of its target (Kubernetes ConfigMap mounts, secrets).
func TestFingerprintFollowsSymlinks(t *testing.T) {
	p := fpPaths(t, map[string]string{"data/tv.toml": "x"})
	d := filepath.Join(p.ConfigDir, "channels.d")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(p.ConfigDir, "data", "tv.toml")
	if err := os.Symlink(target, filepath.Join(d, "tv.toml")); err != nil {
		t.Fatal(err)
	}
	setTime(t, target, 0)
	before := Fingerprint(p)
	setTime(t, target, 9)
	if Fingerprint(p) == before {
		t.Errorf("a change of the symlink target does not change the fingerprint")
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	_ = Fingerprint(p) // dangling link: must not panic
}
