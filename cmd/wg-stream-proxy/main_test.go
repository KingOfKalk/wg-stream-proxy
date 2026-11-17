// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func runCmd(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestVersionPrintsDefaultVersionAndSource(t *testing.T) {
	code, stdout, stderr := runCmd(t, "version")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	want := "wg-stream-proxy dev\nsource: https://github.com/KingOfKalk/wg-stream-proxy\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
}

func TestVersionPrintsInjectedVersion(t *testing.T) {
	old := version
	version = "1.2.3"
	t.Cleanup(func() { version = old })

	code, stdout, _ := runCmd(t, "version")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.HasPrefix(stdout, "wg-stream-proxy 1.2.3\n") {
		t.Errorf("stdout = %q, want prefix %q", stdout, "wg-stream-proxy 1.2.3\n")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestVersionFailsWhenStdoutFails(t *testing.T) {
	var errOut bytes.Buffer

	code := run([]string{"version"}, failingWriter{}, &errOut)

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "write failed") {
		t.Errorf("stderr = %q, want the write error", errOut.String())
	}
}

func TestVersionRejectsArguments(t *testing.T) {
	code, stdout, stderr := runCmd(t, "version", "extra")

	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "version: unexpected arguments") {
		t.Errorf("stderr = %q, want unexpected arguments error", stderr)
	}
}

func TestNotImplementedCommands(t *testing.T) {
	tests := []struct {
		name string
		args []string
		cmd  string
	}{
		{"import", []string{"import", "channels.m3u"}, "import"},
		{"epg ids", []string{"epg", "ids"}, "epg"},
		{"epg match", []string{"epg", "match"}, "epg"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runCmd(t, tt.args...)

			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			want := "wg-stream-proxy: " + tt.cmd + ": not implemented yet\n"
			if stderr != want {
				t.Errorf("stderr = %q, want %q", stderr, want)
			}
		})
	}
}

func TestUnknownCommandPrintsUsage(t *testing.T) {
	code, stdout, stderr := runCmd(t, "bogus")

	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, `unknown command "bogus"`) {
		t.Errorf("stderr = %q, want unknown command error", stderr)
	}
	for _, cmd := range []string{"serve", "check", "import", "epg", "healthcheck", "version"} {
		if !strings.Contains(stderr, cmd) {
			t.Errorf("usage in stderr lacks %q: %q", cmd, stderr)
		}
	}
}
