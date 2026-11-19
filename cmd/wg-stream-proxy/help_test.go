// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"strings"
	"testing"
)

const wantDescription = "Route each IPTV and internet radio channel through its own userspace WireGuard tunnel (or direct) and serve them as one M3U playlist for Kodi, VLC and other players."

var allCommands = []string{"serve", "check", "probe", "import", "epg", "healthcheck", "tunnel", "version", "help"}

func TestTopLevelHelp(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"help command", []string{"help"}},
		{"short flag", []string{"-h"}},
		{"long flag", []string{"--help"}},
		{"single dash long flag", []string{"-help"}},
	}
	var first string
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runCmd(t, tt.args...)

			if code != 0 {
				t.Errorf("exit code = %d, want 0", code)
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want empty", stderr)
			}
			if !strings.HasPrefix(stdout, wantDescription+"\n") {
				t.Errorf("stdout does not start with the description:\n%s", stdout)
			}
			if !strings.Contains(stdout, "usage: wg-stream-proxy [command] [flags]\n") {
				t.Errorf("stdout lacks the usage line:\n%s", stdout)
			}
			for _, cmd := range allCommands {
				if !strings.Contains(stdout, "\n  "+cmd+" ") {
					t.Errorf("stdout lacks the command %q:\n%s", cmd, stdout)
				}
			}
			if !strings.Contains(stdout, `wg-stream-proxy help <command>`) {
				t.Errorf("stdout does not point to help <command>:\n%s", stdout)
			}
			if first == "" {
				first = stdout
			} else if stdout != first {
				t.Errorf("stdout differs from the first variant:\n%s\nwant:\n%s", stdout, first)
			}
		})
	}
}

func TestCommandHelp(t *testing.T) {
	configFlags := []string{"-config-dir", "-listen", "-log-level", "-log-format"}
	tests := []struct {
		topic string
		want  []string // substrings besides the usage line
	}{
		{"serve", configFlags},
		{"check", configFlags},
		{"probe", append([]string{"-route", "-header", "-json", "-standalone", "<url>"}, configFlags...)},
		{"healthcheck", configFlags},
		{"import", []string{"-tunnel", "-out", "-match-epg", "-config-dir"}},
		{"epg", []string{"epg ids", "epg match"}},
		{"epg ids", configFlags},
		{"epg match", configFlags},
		{"tunnel", []string{"tunnel list", "tunnel stop"}},
		{"tunnel list", configFlags},
		{"tunnel stop", configFlags},
		{"version", []string{"source"}},
		{"help", []string{"help [command]"}},
	}
	for _, tt := range tests {
		t.Run(tt.topic, func(t *testing.T) {
			words := strings.Fields(tt.topic)
			variants := [][]string{
				append([]string{"help"}, words...),
				append(append([]string{}, words...), "-h"),
				append(append([]string{}, words...), "--help"),
			}
			var first string
			for i, args := range variants {
				code, stdout, stderr := runCmd(t, args...)

				if code != 0 {
					t.Errorf("%v: exit code = %d, want 0 (stderr %q)", args, code, stderr)
				}
				if stderr != "" {
					t.Errorf("%v: stderr = %q, want empty", args, stderr)
				}
				if !strings.Contains(stdout, "usage: wg-stream-proxy "+tt.topic) {
					t.Errorf("%v: stdout lacks the usage line:\n%s", args, stdout)
				}
				for _, w := range tt.want {
					if !strings.Contains(stdout, w) {
						t.Errorf("%v: stdout lacks %q:\n%s", args, w, stdout)
					}
				}
				if i == 0 {
					first = stdout
				} else if stdout != first {
					t.Errorf("%v: stdout differs from help %s:\n%s\nwant:\n%s", args, tt.topic, stdout, first)
				}
			}
		})
	}
}

func TestHelpAfterFlags(t *testing.T) {
	code, stdout, stderr := runCmd(t, "serve", "--listen", "127.0.0.1:0", "-h")

	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "usage: wg-stream-proxy serve") {
		t.Errorf("stdout = %q, want the help of serve", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
}

func TestHelpErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"unknown command", []string{"bogus"}, `unknown command "bogus"`},
		{"help unknown command", []string{"help", "bogus"}, `help: unknown command "bogus"`},
		{"help unknown subcommand", []string{"help", "epg", "bogus"}, `help: unknown command "epg bogus"`},
		{"help for the command of a command", []string{"help", "serve", "extra"}, `help: unknown command "serve extra"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runCmd(t, tt.args...)

			if code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if !strings.Contains(stderr, tt.want) {
				t.Errorf("stderr lacks %q:\n%s", tt.want, stderr)
			}
			for _, cmd := range allCommands {
				if !strings.Contains(stderr, "\n  "+cmd+" ") {
					t.Errorf("stderr lacks the command list entry %q:\n%s", cmd, stderr)
				}
			}
		})
	}
}

func TestFlagErrorPointsToHelp(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"serve", "--bogus"}, `see "wg-stream-proxy help serve"`},
		{[]string{"epg", "ids", "--bogus"}, `see "wg-stream-proxy help epg ids"`},
		{[]string{"import", "x.m3u", "--bogus"}, `see "wg-stream-proxy help import"`},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			code, stdout, stderr := runCmd(t, tt.args...)

			if code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if !strings.Contains(stderr, "flag provided but not defined") || !strings.Contains(stderr, tt.want) {
				t.Errorf("stderr = %q, want the flag error and %q", stderr, tt.want)
			}
		})
	}
}

// TestNoArgumentsRunsServe: without a command the proxy serves; it does
// not print help (spec 7.2).
func TestNoArgumentsRunsServe(t *testing.T) {
	c, out, errOut := testCLI(map[string]string{"WG_STREAM_PROXY_CONFIG_DIR": "/nonexistent/wsp"})

	if code := c.run(nil); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty", out.String())
	}
	if !strings.Contains(errOut.String(), "config dir") || strings.Contains(errOut.String(), "usage:") {
		t.Errorf("stderr = %q, want the serve startup error", errOut)
	}
}
