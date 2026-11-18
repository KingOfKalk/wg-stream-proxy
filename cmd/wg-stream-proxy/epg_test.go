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
	c.epgAllowPrivate = map[string][]netip.Prefix{"direct": {netip.MustParsePrefix("127.0.0.0/8")}}
	return c, out, errOut
}

const wantIDs = "daserste.example\tDas Erste\tARD Das Erste\n" +
	"ert1.example\tΕΡΤ1\tERT1 HD\n" +
	"ertworld-hd.example\tERT World HD\n" +
	"ertworld.example\tERT World\n" +
	"radio-one.example\tRadio One\n" +
	"skai.example\tΣΚΑΪ\n"

const wantMatch = `# ert-world (ERT World) in greek.toml: tvg_id "ertworld.example" is in the EPG

# skai (ΣΚΑΪ) in greek.toml
# current tvg_id "skai-old" is not in the EPG
[channel.playlist]
tvg_id = "skai.example"  # ΣΚΑΪ (1.00)

# ert-world-hd (ERT World HD) in greek.toml
[channel.playlist]
tvg_id = "ertworld-hd.example"  # ERT World HD (1.00)
# alternatives:
# tvg_id = "ertworld.example"  # ERT World (0.80)

# unknown-thing (Unknown Thing) in greek.toml: no match
`

func TestEPGIDsAndMatch(t *testing.T) {
	srv := epgServer(t)
	for _, file := range []string{"/epg.xml.gz", "/epg.xml"} {
		dir := epgConfigDir(t, "[epg]\nurl = \""+srv.URL+file+"\"\n")
		tests := []struct {
			name string
			args []string
			want string
		}{
			{"ids", []string{"epg", "ids", "--config-dir", dir}, wantIDs},
			{"match", []string{"epg", "match", "--config-dir", dir}, wantMatch},
		}
		for _, tt := range tests {
			t.Run(tt.name+file, func(t *testing.T) {
				c, out, errOut := epgCLI()
				if code := c.run(tt.args); code != 0 {
					t.Fatalf("exit code = %d, want 0; stderr: %s", code, errOut)
				}
				if out.String() != tt.want {
					t.Errorf("stdout:\n%s\nwant:\n%s", out, tt.want)
				}
				if errOut.String() != "" {
					t.Errorf("stderr = %q, want empty", errOut)
				}
			})
		}
	}
}

func TestEPGMatchWritesNothing(t *testing.T) {
	srv := epgServer(t)
	dir := epgConfigDir(t, "[epg]\nurl = \""+srv.URL+"/epg.xml.gz\"\n")
	before, err := os.ReadFile(dir + "/channels.d/greek.toml")
	if err != nil {
		t.Fatal(err)
	}
	c, _, errOut := epgCLI()
	if code := c.run([]string{"epg", "match", "--config-dir", dir}); code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, errOut)
	}
	after, _ := os.ReadFile(dir + "/channels.d/greek.toml")
	if !bytes.Equal(before, after) {
		t.Error("epg match changed the channel file")
	}
	entries, _ := os.ReadDir(dir + "/channels.d")
	if len(entries) != 1 {
		t.Errorf("channels.d has %d files, want 1", len(entries))
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
		{"no subcommand", good, true, []string{"epg"}, 2, "want a subcommand: ids or match"},
		{"unknown subcommand", good, true, []string{"epg", "bogus"}, 2, "want a subcommand: ids or match"},
		{"extra argument", good, true, []string{"epg", "ids", "extra"}, 2, "unexpected arguments: extra"},
		{"no epg url", "", true, []string{"epg", "ids"}, 1, "no [epg] url in config.toml"},
		{"not found", "[epg]\nurl = \"" + srv.URL + "/missing\"\n", true, []string{"epg", "ids"}, 1, "status 404"},
		{"guard blocks loopback", good, false, []string{"epg", "ids"}, 1, "private address blocked"},
		{"allow_private of direct applies", good + "[upstream.allow_private]\ndirect = [\"127.0.0.0/8\"]\n", false, []string{"epg", "ids"}, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := epgConfigDir(t, tt.config)
			c, out, errOut := epgCLI()
			if !tt.allow {
				c.epgAllowPrivate = nil
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

const importEPGM3U = `#EXTM3U
#EXTINF:-1 tvg-id="ertworld.example",ERT World
https://a.example.com/ertworld.m3u8
#EXTINF:-1 tvg-id="skai-old",ΣΚΑΪ
https://a.example.com/skai.m3u8
#EXTINF:-1,Das Erste
https://a.example.com/daserste.m3u8
#EXTINF:-1,Unknown Thing
https://a.example.com/unknown.m3u8
`

const importEPGTOML = `tunnel = "direct"

[[channel]]
name = "ERT World"
url = "https://a.example.com/ertworld.m3u8"

[channel.playlist]
tvg_id = "ertworld.example"

[[channel]]
name = "ΣΚΑΪ"
url = "https://a.example.com/skai.m3u8"

[channel.playlist]
# tvg_id = "skai.example"  # EPG: ΣΚΑΪ

[[channel]]
name = "Das Erste"
url = "https://a.example.com/daserste.m3u8"

[channel.playlist]
# tvg_id = "daserste.example"  # EPG: Das Erste

[[channel]]
name = "Unknown Thing"
url = "https://a.example.com/unknown.m3u8"
`

func TestImportMatchEPG(t *testing.T) {
	srv := epgServer(t)
	dir := epgConfigDir(t, "[epg]\nurl = \""+srv.URL+"/epg.xml.gz\"\n")
	in := dir + "/in.m3u"
	if err := os.WriteFile(in, []byte(importEPGM3U), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("match", func(t *testing.T) {
		c, out, errOut := epgCLI()
		if code := c.run([]string{"import", in, "--tunnel", "direct", "--match-epg", "--config-dir", dir}); code != 0 {
			t.Fatalf("exit code = %d; stderr: %s", code, errOut)
		}
		if out.String() != importEPGTOML {
			t.Errorf("stdout:\n%s\nwant:\n%s", out, importEPGTOML)
		}
		if want := `warning: ` + in + `:4: tvg-id "skai-old" is not in the EPG, dropped`; !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr = %q, want %q", errOut, want)
		}
	})
	t.Run("config dir from env", func(t *testing.T) {
		c, out, errOut := epgCLI()
		c.env.LookupEnv = func(k string) (string, bool) {
			if k == "WG_STREAM_PROXY_CONFIG_DIR" {
				return dir, true
			}
			return "", false
		}
		if code := c.run([]string{"import", "--match-epg", "--tunnel", "direct", in}); code != 0 {
			t.Fatalf("exit code = %d; stderr: %s", code, errOut)
		}
		if out.String() != importEPGTOML {
			t.Errorf("stdout:\n%s", out)
		}
	})
	t.Run("without flag no EPG access", func(t *testing.T) {
		c, out, _ := testCLI(nil) // no loopback exception: a download would fail
		if code := c.run([]string{"import", in, "--tunnel", "direct"}); code != 0 {
			t.Fatalf("exit code = %d", code)
		}
		if !strings.Contains(out.String(), `tvg_id = "skai-old"`) || strings.Contains(out.String(), "# tvg_id") {
			t.Errorf("stdout:\n%s", out)
		}
	})
	t.Run("no epg url", func(t *testing.T) {
		bare := epgConfigDir(t, "")
		c, out, errOut := epgCLI()
		code := c.run([]string{"import", in, "--match-epg", "--config-dir", bare})
		if code != 1 || out.String() != "" || !strings.Contains(errOut.String(), "import: --match-epg: no [epg] url in config.toml") {
			t.Errorf("code = %d, stdout %q, stderr %q", code, out, errOut)
		}
	})
	t.Run("no config dir", func(t *testing.T) {
		c, out, errOut := epgCLI()
		code := c.run([]string{"import", in, "--match-epg", "--config-dir", dir + "/missing"})
		if code != 1 || out.String() != "" || errOut.String() == "" {
			t.Errorf("code = %d, stdout %q, stderr %q", code, out, errOut)
		}
	})
	t.Run("download fails", func(t *testing.T) {
		c, out, errOut := testCLI(nil) // guard blocks the loopback server
		code := c.run([]string{"import", in, "--match-epg", "--config-dir", dir})
		if code != 1 || out.String() != "" || !strings.Contains(errOut.String(), "private address blocked") {
			t.Errorf("code = %d, stdout %q, stderr %q", code, out, errOut)
		}
	})
}
