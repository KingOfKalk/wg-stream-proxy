// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const importM3U = `#EXTM3U
#EXTINF:-1 group-title="News",One
https://one.example.com/live.m3u8
# a comment
`

const importTOML = `tunnel = "gr"

[[channel]]
name = "One"
url = "https://one.example.com/live.m3u8"
group = "News"
`

func TestImport(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.m3u")
	if err := os.WriteFile(in, []byte(importM3U), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.m3u")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(dir, "existing.toml")
	if err := os.WriteFile(existing, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	dash := filepath.Join(dir, "-dash.m3u")
	if err := os.WriteFile(dash, []byte(importM3U), 0o644); err != nil {
		t.Fatal(err)
	}
	warning := "wg-stream-proxy: import: warning: " + in + ":4: comment dropped\n"

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string // substring; "" means empty
		outFile    string // if set: want importTOML in this file
	}{
		{"stdout", []string{"import", in, "--tunnel", "gr"}, 0, importTOML, warning, ""},
		{"flags first", []string{"import", "--tunnel=gr", in}, 0, importTOML, warning, ""},
		{"out", []string{"import", in, "--tunnel", "gr", "--out", filepath.Join(dir, "new.toml")}, 0, "", warning, filepath.Join(dir, "new.toml")},
		{"double dash", []string{"import", "--tunnel", "gr", "--", dash}, 0, importTOML, "comment dropped", ""},
		{"out exists", []string{"import", in, "--out", existing}, 1, "", existing + " already exists, not overwritten", ""},
		{"no file", []string{"import", "--tunnel", "gr"}, 2, "", "import: want exactly one M3U file", ""},
		{"two files", []string{"import", in, in}, 2, "", "import: want exactly one M3U file", ""},
		{"unknown flag", []string{"import", in, "--bogus"}, 2, "", "flag provided but not defined", ""},
		{"missing file", []string{"import", filepath.Join(dir, "missing.m3u")}, 1, "", "missing.m3u", ""},
		{"no channels", []string{"import", empty}, 1, "", "import: " + empty + ": no channels found", ""},
		{"invalid tunnel", []string{"import", in, "--tunnel", "Bad"}, 1, "", `invalid tunnel name "Bad"`, ""},
		{"help", []string{"import", "-h"}, 0, "", "-out", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runCmd(t, tt.args...)

			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stderr %q)", code, tt.wantCode, stderr)
			}
			if stdout != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout, tt.wantStdout)
			}
			if tt.wantStderr == "" && stderr != "" || !strings.Contains(stderr, tt.wantStderr) {
				t.Errorf("stderr = %q, want %q", stderr, tt.wantStderr)
			}
			if tt.outFile != "" {
				b, err := os.ReadFile(tt.outFile)
				if err != nil {
					t.Fatal(err)
				}
				if string(b) != importTOML {
					t.Errorf("%s = %q, want %q", tt.outFile, b, importTOML)
				}
			}
		})
	}
	if b, err := os.ReadFile(existing); err != nil || string(b) != "keep" {
		t.Errorf("existing file changed: %q, %v", b, err)
	}
}
