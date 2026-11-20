// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// brokenChannelFile and brokenTunnelFile are the rejected edits of the
// tests: a typo in a channel file and an invalid tunnel file name. The
// proxy keeps serving its loaded config; healthcheck, tunnel list and
// probe only read the listen addresses, so they must keep working.
var (
	brokenChannelFile = map[string]string{"channels.d/broken.toml": "[[chanel]]\nname = \"News\"\n"}
	brokenTunnelFile  = map[string]string{"tunnels/my_tunnel.conf": "[Interface]\n"}
)

// brokenFiles returns the broken edits the commands are run with.
func brokenFiles() map[string]map[string]string {
	return map[string]map[string]string{
		"broken channel file": brokenChannelFile,
		"broken tunnel file":  brokenTunnelFile,
	}
}

// serveWithBrokenFiles runs serve in-process on free listen and debug
// addresses, then writes broken files into its config dir after startup:
// a reload would reject them, the proxy keeps serving its loaded config.
// cmd runs against the same config dir with a cli of its own while the
// proxy serves on. It returns serve's exit code and logs.
func serveWithBrokenFiles(t *testing.T, broken map[string]string, cmd func(dir string, c *cli, out, errOut *syncBuffer)) (code int, logs string) {
	t.Helper()
	// Both ports are picked while both listeners are open: after a close
	// the OS may hand the same port out again, and serve rejects a debug
	// port equal to the listen port.
	var addrs [2]string
	var lns [2]net.Listener
	for i := range addrs {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addrs[i], lns[i] = ln.Addr().String(), ln
	}
	for _, ln := range lns {
		_ = ln.Close()
	}
	listen, debug := addrs[0], addrs[1]
	dir := writeDir(t, map[string]string{
		"config.toml": "listen = \"" + listen + "\"\n[debug]\nlisten = \"" + debug + "\"\n" +
			"[reload]\nwatch_interval = \"0\"\n[upstream.allow_private]\ndirect = [\"127.0.0.0/8\"]\n",
		"channels.d/tv.toml": "tunnel = \"direct\"\n[[channel]]\nname = \"News\"\nurl = \"https://tv.example.com/news.m3u8\"\n",
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, _, srvLog := testCLI(nil)
	srv.ctx = ctx
	srv.listening = func(net.Addr) {
		for name, content := range broken {
			p := filepath.Join(dir, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Error(err)
			}
			if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
				t.Error(err)
			}
		}
	}
	srv.debugListening = func(net.Addr) {
		go func() {
			defer cancel()
			c, out, errOut := testCLI(nil)
			cmd(dir, c, out, errOut)
		}()
	}
	return srv.run([]string{"serve", "--config-dir", dir}), srvLog.String()
}

// TestHealthcheckBrokenFiles: healthcheck reads only listen, so a
// channel or tunnel file the proxy rejects as a reload does not fail it
// while the proxy serves on.
func TestHealthcheckBrokenFiles(t *testing.T) {
	for name, broken := range brokenFiles() {
		t.Run(name, func(t *testing.T) {
			var code int
			var out, errOut *syncBuffer
			serveCode, logs := serveWithBrokenFiles(t, broken, func(dir string, c *cli, o, e *syncBuffer) {
				out, errOut = o, e
				code = c.run([]string{"healthcheck", "--config-dir", dir})
			})
			if serveCode != 0 {
				t.Fatalf("serve exit code = %d, want 0 (a broken file must not stop it); logs:\n%s", serveCode, logs)
			}
			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr: %s", code, errOut)
			}
			if out.String() != "" || errOut.String() != "" {
				t.Errorf("output on success: stdout %q, stderr %q", out, errOut)
			}
			if strings.Contains(logs, "reload") {
				t.Errorf("serve reloaded or rejected a reload (watching is off):\n%s", logs)
			}
		})
	}
}

// TestTunnelListBrokenFiles: tunnel list reads only [debug] listen, so
// it still reaches the debug listener of the running proxy.
func TestTunnelListBrokenFiles(t *testing.T) {
	for name, broken := range brokenFiles() {
		t.Run(name, func(t *testing.T) {
			var code int
			var out, errOut *syncBuffer
			serveCode, logs := serveWithBrokenFiles(t, broken, func(dir string, c *cli, o, e *syncBuffer) {
				out, errOut = o, e
				code = c.run([]string{"tunnel", "list", "--config-dir", dir})
			})
			if serveCode != 0 {
				t.Fatalf("serve exit code = %d, want 0 (a broken file must not stop it); logs:\n%s", serveCode, logs)
			}
			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr: %s", code, errOut)
			}
			if !strings.Contains(out.String(), "NAME") {
				t.Errorf("stdout lacks the table header:\n%s", out)
			}
			if errOut.String() != "" {
				t.Errorf("stderr = %q, want empty", errOut)
			}
		})
	}
}

// TestProbeBrokenFiles: probe reads only [debug] listen to find the
// running proxy, so a channel or tunnel file the proxy rejects as a
// reload does not stop it.
func TestProbeBrokenFiles(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("ID3-audio-bytes"))
	}))
	t.Cleanup(up.Close)
	for name, broken := range brokenFiles() {
		t.Run(name, func(t *testing.T) {
			var code int
			var out, errOut *syncBuffer
			serveCode, logs := serveWithBrokenFiles(t, broken, func(dir string, c *cli, o, e *syncBuffer) {
				out, errOut = o, e
				code = c.run([]string{"probe", "--config-dir", dir, up.URL + "/radio"})
			})
			if serveCode != 0 {
				t.Fatalf("serve exit code = %d, want 0 (a broken file must not stop it); logs:\n%s", serveCode, logs)
			}
			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr: %s", code, errOut)
			}
			if !strings.Contains(out.String(), "route direct (via running proxy)") {
				t.Errorf("stdout lacks the proxy mode:\n%s", out)
			}
			if errOut.String() != "" {
				t.Errorf("stderr = %q, want empty", errOut)
			}
		})
	}
}
