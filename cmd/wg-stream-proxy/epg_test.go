// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel/tunneltest"
)

const epgFixture = "../../internal/epg/testdata/epg.xml"

// epgServer serves the XMLTV fixture: /epg.xml.gz compressed, /epg.xml
// plain, everything else 404.
func epgServer(t *testing.T) *httptest.Server {
	t.Helper()
	plain, err := os.ReadFile(epgFixture)
	if err != nil {
		t.Fatal(err)
	}
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	if _, err := zw.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/epg.xml", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(plain) })
	mux.HandleFunc("/epg.xml.gz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(zipped.Bytes()) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

const epgChannels = `tunnel = "direct"

[[channel]]
name = "ERT World"
url = "https://a.example.com/ertworld.m3u8"

[channel.playlist]
tvg_id = "ertworld.example"

[[channel]]
name = "ΣΚΑΪ"
url = "https://a.example.com/skai.m3u8"

[channel.playlist]
tvg_id = "skai-old"

[[channel]]
name = "ERT World HD"
url = "https://a.example.com/ertworldhd.m3u8"

[[channel]]
name = "Unknown Thing"
url = "https://a.example.com/unknown.m3u8"
`

// epgConfigDir writes a config dir with the channels above and the given
// config.toml.
func epgConfigDir(t *testing.T, configTOML string) string {
	t.Helper()
	return writeDir(t, map[string]string{
		"config.toml":           configTOML,
		"channels.d/greek.toml": epgChannels,
	})
}

// epgCLI returns a cli whose EPG download may reach the loopback test
// server.
func epgCLI() (*cli, *syncBuffer, *syncBuffer) {
	c, out, errOut := testCLI(nil)
	c.allowPrivate = map[string][]netip.Prefix{"direct": {netip.MustParsePrefix("127.0.0.0/8")}}
	return c, out, errOut
}

const wantIDs = "daserste.example\tDas Erste\tARD Das Erste\n" +
	"ert1.example\tΕΡΤ1\tERT1 HD\n" +
	"ertworld-hd.example\tERT World HD\n" +
	"ertworld.example\tERT World\n" +
	"radio-one.example\tRadio One\n" +
	"skai.example\tΣΚΑΪ\n"

func TestEPGIDs(t *testing.T) {
	srv := epgServer(t)
	for _, file := range []string{"/epg.xml.gz", "/epg.xml"} {
		t.Run(file, func(t *testing.T) {
			dir := epgConfigDir(t, "[epg]\nurl = \""+srv.URL+file+"\"\n")
			c, out, errOut := epgCLI()
			if code := c.run([]string{"epg", "ids", "--config-dir", dir}); code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr: %s", code, errOut)
			}
			if out.String() != wantIDs {
				t.Errorf("stdout:\n%s\nwant:\n%s", out, wantIDs)
			}
			if errOut.String() != "" {
				t.Errorf("stderr = %q, want empty", errOut)
			}
		})
	}
}

func TestEPGErrors(t *testing.T) {
	srv := epgServer(t)
	good := "[epg]\nurl = \"" + srv.URL + "/epg.xml.gz\"\n"
	tests := []struct {
		name       string
		config     string
		allow      bool // allow loopback in the guard via the test hook
		args       []string
		wantCode   int
		wantStderr string
	}{
		{"no subcommand", good, true, []string{"epg"}, 2, "want the subcommand ids"},
		{"unknown subcommand", good, true, []string{"epg", "bogus"}, 2, "want the subcommand ids"},
		{"removed subcommand match", good, true, []string{"epg", "match"}, 2, "want the subcommand ids"},
		{"extra argument", good, true, []string{"epg", "ids", "extra"}, 2, "unexpected arguments: extra"},
		{"no epg url", "", true, []string{"epg", "ids"}, 1, "no [[epg.source]] or [epg] url in config.toml"},
		{"not found", "[epg]\nurl = \"" + srv.URL + "/missing\"\n", true, []string{"epg", "ids"}, 1, "status 404"},
		{"guard blocks loopback", good, false, []string{"epg", "ids"}, 1, "private address blocked"},
		{"allow_private of direct applies", good + "[upstream.allow_private]\ndirect = [\"127.0.0.0/8\"]\n", false, []string{"epg", "ids"}, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := epgConfigDir(t, tt.config)
			c, out, errOut := epgCLI()
			if !tt.allow {
				c.allowPrivate = nil
			}
			code := c.run(append(tt.args[:1:1], append(tt.args[1:], "--config-dir", dir)...))
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d; stderr: %s", code, tt.wantCode, errOut)
			}
			if !strings.Contains(errOut.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want %q", errOut, tt.wantStderr)
			}
			if tt.wantCode != 0 && out.String() != "" {
				t.Errorf("stdout = %q, want empty", out)
			}
		})
	}
}

// TestEPGSources: --source picks an [[epg.source]], by default the first
// one; a source on a tunnel is read through that tunnel.
func TestEPGSources(t *testing.T) {
	srv := epgServer(t)
	peer := tunneltest.Start(t, tunneltest.Options{Handler: http.FileServer(http.Dir("../../internal/epg/testdata"))})
	peer.SetHost("epg.test", tunneltest.ServerAddr4)
	sources := "[[epg.source]]\nname = \"gr\"\nurl = \"" + srv.URL + "/missing.xml\"\n" +
		"[[epg.source]]\nname = \"de\"\nurl = \"" + srv.URL + "/epg.xml.gz\"\n" +
		"[[epg.source]]\nname = \"vpn\"\nurl = \"http://epg.test/epg.xml\"\ntunnel = \"gr\"\n"
	tests := []struct {
		name, config string
		args         []string
		wantCode     int
		wantOut      string
		wantStderr   string
	}{
		{"first source by default", sources, []string{"ids"}, 1, "", "status 404"},
		{"named source", sources, []string{"ids", "--source", "de"}, 0, wantIDs, ""},
		{"source on a tunnel", sources, []string{"ids", "--source", "vpn"}, 0, wantIDs, `source "vpn" uses tunnel "gr": starting it in this process`},
		{"unknown source", sources, []string{"ids", "--source", "nope"}, 2, "", `unknown EPG source "nope" (want one of: gr, de, vpn)`},
		{"source in link mode", "[epg]\nurl = \"" + srv.URL + "/epg.xml.gz\"\n", []string{"ids", "--source", "gr"}, 2, "", `unknown EPG source "gr": config.toml has no [[epg.source]]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeDir(t, map[string]string{
				"config.toml":           tt.config,
				"channels.d/greek.toml": epgChannels,
				"tunnels/gr.conf":       peer.ClientConfig(),
			})
			c, out, errOut := epgCLI()
			code := c.run(append([]string{"epg"}, append(tt.args, "--config-dir", dir)...))
			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d; stderr: %s", code, tt.wantCode, errOut)
			}
			if out.String() != tt.wantOut {
				t.Errorf("stdout:\n%s\nwant:\n%s", out, tt.wantOut)
			}
			if !strings.Contains(errOut.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want %q", errOut, tt.wantStderr)
			}
		})
	}
}
