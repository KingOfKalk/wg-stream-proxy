// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"encoding/json"
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

// dump is a stable, test-only view of a Config for golden files.
type dump struct {
	Paths         Paths
	LogosDir      string
	Listen        string
	LogLevel      string
	LogFormat     LogFormat
	PublicBaseURL string
	UserAgent     string
	Timeouts      map[string]string
	AllowPrivate  map[string][]netip.Prefix
	EPGURL        string
	Tunnels       []Tunnel
	Channels      []dumpChannel
}

type dumpChannel struct {
	File    string
	ID      string
	Name    string
	URL     string
	Route   string
	Group   string
	Enabled bool
	Type    UpstreamType
	Header  map[string][]string
	TVGID   string
	Logo    string // kind:value
	Attrs   []Attr
	Extra   []Extra
}

func dumpConfig(c *Config) dump {
	u := c.Upstream()
	d := dump{
		Paths:         c.Paths(),
		LogosDir:      c.LogosDir(),
		Listen:        c.Listen(),
		LogLevel:      c.LogLevel().String(),
		LogFormat:     c.LogFormat(),
		PublicBaseURL: c.PublicBaseURL(),
		UserAgent:     u.UserAgent,
		Timeouts: map[string]string{
			"connect": u.ConnectTimeout.String(),
			"header":  u.HeaderTimeout.String(),
			"read":    u.ReadTimeout.String(),
			"idle":    c.IdleTimeout().String(),
		},
		AllowPrivate: c.AllowPrivate(),
		EPGURL:       c.EPGURL(),
		Tunnels:      c.Tunnels(),
	}
	for _, ch := range c.Channels() {
		d.Channels = append(d.Channels, dumpChannel{
			File: ch.File(), ID: ch.ID(), Name: ch.Name(), URL: ch.URL(),
			Route: ch.Route(), Group: ch.Group(), Enabled: ch.Enabled(),
			Type: ch.Type(), Header: ch.Header(), TVGID: ch.TVGID(),
			Logo: ch.Logo().Kind.String() + ":" + ch.Logo().Value, Attrs: ch.Attrs(), Extra: ch.Extra(),
		})
	}
	return d
}

// TestLoadGolden loads every testdata/load/<case>/config dir and compares
// the result (or the error) with testdata/load/<case>/want.golden.
func TestLoadGolden(t *testing.T) {
	cases, err := filepath.Glob("testdata/load/*")
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("no golden cases")
	}
	for _, dir := range cases {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			cfgDir := filepath.Join(dir, "config")
			cfg, err := Load(Options{Paths: Paths{
				ConfigDir:  cfgDir,
				TunnelsDir: filepath.Join(cfgDir, "tunnels"),
			}})
			var got []byte
			if err != nil {
				got = []byte("error:\n" + err.Error() + "\n")
			} else {
				got, err = json.MarshalIndent(dumpConfig(cfg), "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, '\n')
			}
			golden := filepath.Join(dir, "want.golden")
			if *update {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Errorf("Load() mismatch (-update to rewrite)\n--- got\n%s\n--- want\n%s", got, want)
			}
		})
	}
}

// writeConfigDir creates a config dir from relative path -> content.
func writeConfigDir(t *testing.T, files map[string]string) string {
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

func loadDir(dir string) (*Config, error) {
	return Load(Options{Paths: Paths{ConfigDir: dir, TunnelsDir: filepath.Join(dir, "tunnels")}})
}

// ch returns a minimal valid channel table with extra lines appended.
func ch(lines ...string) string {
	return "[[channel]]\nname = \"Example\"\nurl = \"https://example.com/live.m3u8\"\n" +
		strings.Join(lines, "\n") + "\n"
}

func TestLoadErrors(t *testing.T) {
	const gr = "tunnels/gr.conf"
	tests := []struct {
		name  string
		files map[string]string
		want  []string // substrings of the error; "!x": x must not appear
	}{
		// config.toml (5.3)
		{"toml syntax", map[string]string{"config.toml": "listen = "}, []string{"config.toml:1:"}},
		{"unknown top-level key", map[string]string{"config.toml": "foo = 1\n"}, []string{"config.toml:1:1: unknown key \"foo\""}},
		{"unknown nested key", map[string]string{"config.toml": "[upstream]\nproxy = \"x\"\n"}, []string{"config.toml:2:1: unknown key \"upstream.proxy\""}},
		{"unknown table", map[string]string{"config.toml": "[server]\nport = 1\n"}, []string{"unknown key \"server"}},
		{"wrong type", map[string]string{"config.toml": "listen = 2\n"}, []string{"config.toml:1:10: key \"listen\": cannot decode TOML integer into string"}},
		{"wrong type not validated further", map[string]string{"config.toml": "listen = 2\n"}, []string{"!invalid address"}},
		{"log level", map[string]string{"config.toml": "log_level = \"verbose\"\n"}, []string{"log_level: invalid value \"verbose\""}},
		{"log format", map[string]string{"config.toml": "log_format = \"xml\"\n"}, []string{"log_format: invalid value \"xml\""}},
		{"listen", map[string]string{"config.toml": "listen = \"nope\"\n"}, []string{"listen: invalid address \"nope\""}},
		{"listen port", map[string]string{"config.toml": "listen = \":http\"\n"}, []string{"listen: invalid address \":http\""}},
		{"duration syntax", map[string]string{"config.toml": "[upstream]\nconnect_timeout = \"10\"\n"}, []string{"upstream.connect_timeout: invalid duration \"10\""}},
		{"duration negative", map[string]string{"config.toml": "[upstream]\nread_timeout = \"-1s\"\n"}, []string{"upstream.read_timeout: must be positive"}},
		{"header timeout zero", map[string]string{"config.toml": "[upstream]\nheader_timeout = \"0s\"\n"}, []string{"upstream.header_timeout: must be positive"}},
		{"idle zero", map[string]string{"config.toml": "[tunnels]\nidle_timeout = \"0s\"\n"}, []string{"tunnels.idle_timeout: must be positive"}},
		{"public base url", map[string]string{"config.toml": "public_base_url = \"wsp.lan\"\n"}, []string{"public_base_url: must be an absolute http(s) URL"}},
		{"epg url", map[string]string{"config.toml": "[epg]\nurl = \"ftp://example.com/epg.xml\"\n"}, []string{"epg.url: must be an absolute http(s) URL"}},
		{"user agent line break", map[string]string{"config.toml": "[upstream]\nuser_agent = \"a\\nb\"\n"}, []string{"upstream.user_agent: must not contain line breaks"}},
		{"allow_private unknown route", map[string]string{"config.toml": "[upstream.allow_private]\nxx = [\"10.0.0.0/8\"]\n"}, []string{"upstream.allow_private.xx: unknown route \"xx\""}},
		{"allow_private no prefix", map[string]string{"config.toml": "[upstream.allow_private]\ndirect = [\"10.0.0.1\"]\n"}, []string{"upstream.allow_private.direct: invalid prefix \"10.0.0.1\""}},
		{"allow_private host bits", map[string]string{"config.toml": "[upstream.allow_private]\ndirect = [\"10.0.0.1/24\"]\n"}, []string{"upstream.allow_private.direct: \"10.0.0.1/24\" has host bits set, use \"10.0.0.0/24\""}},

		// tunnel names (5.2)
		{"tunnel named direct", map[string]string{"tunnels/direct.conf": ""}, []string{"direct.conf: tunnel name \"direct\" is reserved"}},
		{"invalid tunnel name", map[string]string{"tunnels/GR.conf": ""}, []string{"GR.conf: invalid tunnel name \"GR\": use [a-z0-9-], 1-48 characters"}},

		// channel files (5.2, 5.4)
		{"channel toml syntax", map[string]string{"channels.d/a.toml": "[[channel]\n"}, []string{"a.toml:1:"}},
		{"unknown file key", map[string]string{"channels.d/a.toml": "groups = \"x\"\n"}, []string{"a.toml:1:1: unknown key \"groups\""}},
		{"unknown channel key", map[string]string{gr: "", "channels.d/gr.toml": ch("foo = 1")}, []string{"gr.toml:4:1: unknown key \"channel.foo\""}},
		{"unknown defaults table", map[string]string{"channels.d/a.toml": "[defaults.server]\nx = 1\n"}, []string{"unknown key \"defaults.server"}},
		{"missing name", map[string]string{"channels.d/a.toml": "[[channel]]\nurl = \"https://example.com/\"\ntunnel = \"direct\"\n"}, []string{"a.toml: channel 1: name: required"}},
		{"blank name", map[string]string{"channels.d/a.toml": "[[channel]]\nname = \" \"\nurl = \"https://example.com/\"\ntunnel = \"direct\"\n"}, []string{"a.toml: channel 1: name: required"}},
		{"missing url", map[string]string{"channels.d/a.toml": "[[channel]]\nname = \"A\"\ntunnel = \"direct\"\n"}, []string{"a.toml: channel 1 (\"A\"): url: required"}},
		{"url scheme", map[string]string{"channels.d/a.toml": "[[channel]]\nname = \"A\"\nurl = \"rtmp://example.com/live\"\ntunnel = \"direct\"\n"}, []string{"url: must be an absolute http(s) URL"}},
		{"url relative", map[string]string{"channels.d/a.toml": "[[channel]]\nname = \"A\"\nurl = \"/live.m3u8\"\ntunnel = \"direct\"\n"}, []string{"url: must be an absolute http(s) URL"}},
		{"url too long", map[string]string{"channels.d/a.toml": "[[channel]]\nname = \"A\"\nurl = \"https://example.com/" + strings.Repeat("a", 4100) + "\"\ntunnel = \"direct\"\n"}, []string{"url: longer than 4096 bytes"}},
		{"name line break", map[string]string{"channels.d/a.toml": "tunnel = \"direct\"\n[[channel]]\nname = \"A\\nB\"\nurl = \"https://example.com/\"\n"}, []string{"name: must not contain line breaks"}},
		{"invalid id", map[string]string{"channels.d/a.toml": "tunnel = \"direct\"\n" + ch("id = \"Ert World\"")}, []string{"id: invalid id \"Ert World\": use [a-z0-9-], 1-48 characters"}},
		{"empty slug", map[string]string{"channels.d/a.toml": "tunnel = \"direct\"\n[[channel]]\nname = \"!!!\"\nurl = \"https://example.com/\"\n"}, []string{"a.toml: channel 1 (\"!!!\"): id: name gives an empty slug, set id"}},
		{"duplicate explicit id", map[string]string{
			"channels.d/a.toml": "tunnel = \"direct\"\n" + ch("id = \"x\""),
			"channels.d/b.toml": "tunnel = \"direct\"\n" + ch("id = \"x\""),
		}, []string{"b.toml: channel 1 (\"Example\"): id: duplicate channel id \"x\", also used by", "a.toml channel 1"}},
		{"slug collision", map[string]string{
			"channels.d/a.toml": "tunnel = \"direct\"\n[[channel]]\nname = \"ERT World\"\nurl = \"https://example.com/1\"\n[[channel]]\nname = \"ert world!\"\nurl = \"https://example.com/2\"\n",
		}, []string{"a.toml: channel 2 (\"ert world!\"): id: duplicate channel id \"ert-world\""}},
		{"no route fails closed", map[string]string{"channels.d/news.toml": ch()}, []string{"news.toml: channel 1 (\"Example\"): no route: set tunnel on the channel or at the top of the file, or add a tunnel file \"news.conf\""}},
		{"stem does not imply direct", map[string]string{"channels.d/direct.toml": ch()}, []string{"no route"}},
		{"unknown channel tunnel", map[string]string{gr: "", "channels.d/a.toml": ch("tunnel = \"xx\"")}, []string{"a.toml: channel 1 (\"Example\"): tunnel: unknown tunnel \"xx\""}},
		{"unknown file tunnel", map[string]string{"channels.d/a.toml": "tunnel = \"xx\"\n" + ch()}, []string{"a.toml: tunnel: unknown tunnel \"xx\""}},
		{"empty tunnel", map[string]string{"channels.d/a.toml": ch("tunnel = \"\"")}, []string{"tunnel: unknown tunnel \"\""}},
		{"upstream type", map[string]string{gr: "", "channels.d/gr.toml": ch("[channel.upstream]", "type = \"dash\"")}, []string{"upstream.type: invalid value \"dash\": use auto, hls or raw"}},
		{"defaults upstream type", map[string]string{gr: "", "channels.d/gr.toml": "[defaults.upstream]\ntype = \"dash\"\n"}, []string{"gr.toml: defaults.upstream.type: invalid value \"dash\""}},
		{"header case duplicate", map[string]string{gr: "", "channels.d/gr.toml": ch("[channel.upstream]", "headers = { Referer = \"a\", referer = \"b\" }")}, []string{"upstream.headers: duplicate header \"Referer\" (names are case-insensitive)"}},
		{"header name", map[string]string{gr: "", "channels.d/gr.toml": ch("[channel.upstream]", "headers = { \"Bad Header\" = \"x\" }")}, []string{"upstream.headers: invalid header name \"Bad Header\""}},
		{"header value", map[string]string{gr: "", "channels.d/gr.toml": "[defaults.upstream]\nheaders = { X-A = \"a\\r\\nX-B: b\" }\n"}, []string{"gr.toml: defaults.upstream.headers: invalid value for header \"X-A\""}},
		{"reserved attr tvg-id", map[string]string{gr: "", "channels.d/gr.toml": ch("[channel.playlist]", "attrs = { tvg-id = \"x\" }")}, []string{"playlist.attrs: \"tvg-id\" is reserved, use the dedicated field"}},
		{"reserved attr tvg-name", map[string]string{gr: "", "channels.d/gr.toml": ch("[channel.playlist]", "attrs = { tvg-name = \"x\" }")}, []string{"\"tvg-name\" is reserved"}},
		{"reserved attr tvg-logo", map[string]string{gr: "", "channels.d/gr.toml": ch("[channel.playlist]", "attrs = { tvg-logo = \"x\" }")}, []string{"\"tvg-logo\" is reserved"}},
		{"reserved attr group-title", map[string]string{gr: "", "channels.d/gr.toml": ch("[channel.playlist]", "attrs = { group-title = \"x\" }")}, []string{"\"group-title\" is reserved"}},
		{"reserved attr any case", map[string]string{gr: "", "channels.d/gr.toml": "[defaults.playlist]\nattrs = { TVG-ID = \"x\" }\n"}, []string{"gr.toml: defaults.playlist.attrs: \"TVG-ID\" is reserved"}},
		{"attr name", map[string]string{gr: "", "channels.d/gr.toml": ch("[channel.playlist]", "attrs = { \"a b\" = \"x\" }")}, []string{"playlist.attrs: invalid attribute name \"a b\""}},
		{"attr value line break", map[string]string{gr: "", "channels.d/gr.toml": ch("[channel.playlist]", "attrs = { a = \"x\\ny\" }")}, []string{"playlist.attrs: value of \"a\" must not contain line breaks"}},
		{"forbidden EXTM3U", map[string]string{gr: "", "channels.d/gr.toml": ch("[channel.playlist.extra.EXTM3U]", "a = \"b\"")}, []string{"playlist.extra: directive \"EXTM3U\" is forbidden"}},
		{"forbidden EXTINF", map[string]string{gr: "", "channels.d/gr.toml": ch("[channel.playlist.extra.EXTINF]", "a = \"b\"")}, []string{"directive \"EXTINF\" is forbidden"}},
		{"forbidden EXTGRP", map[string]string{gr: "", "channels.d/gr.toml": ch("[channel.playlist.extra.EXTGRP]", "a = \"b\"")}, []string{"directive \"EXTGRP\" is forbidden"}},
		{"forbidden EXT-X", map[string]string{gr: "", "channels.d/gr.toml": "[defaults.playlist.extra.EXT-X-STREAM-INF]\na = \"b\"\n"}, []string{"gr.toml: defaults.playlist.extra: directive \"EXT-X-STREAM-INF\" is forbidden"}},
		{"directive lowercase", map[string]string{gr: "", "channels.d/gr.toml": ch("[channel.playlist.extra.kodiprop]", "a = \"b\"")}, []string{"playlist.extra: invalid directive \"kodiprop\": use [A-Z][A-Z0-9-]*"}},
		{"directive digit first", map[string]string{gr: "", "channels.d/gr.toml": ch("[channel.playlist.extra.1ABC]", "a = \"b\"")}, []string{"invalid directive \"1ABC\""}},
		{"directive underscore", map[string]string{gr: "", "channels.d/gr.toml": ch("[channel.playlist.extra.KODI_PROP]", "a = \"b\"")}, []string{"invalid directive \"KODI_PROP\""}},
		{"extra unquoted dotted key", map[string]string{gr: "", "channels.d/gr.toml": ch("[channel.playlist.extra.KODIPROP]", "inputstream.adaptive.manifest_type = \"hls\"")}, []string{"gr.toml:5:", "dotted keys in playlist.extra must be quoted"}},
		{"extra empty key", map[string]string{gr: "", "channels.d/gr.toml": ch("[channel.playlist.extra.KODIPROP]", "\"\" = \"x\"")}, []string{"playlist.extra.KODIPROP: empty key"}},
		{"extra key line break", map[string]string{gr: "", "channels.d/gr.toml": ch("[channel.playlist.extra.KODIPROP]", "\"a\\nb\" = \"x\"")}, []string{"playlist.extra.KODIPROP: key \"a\\nb\" must not contain line breaks"}},
		{"extra value line break", map[string]string{gr: "", "channels.d/gr.toml": ch("[channel.playlist.extra.KODIPROP]", "a = \"x\\ry\"")}, []string{"playlist.extra.KODIPROP: value of \"a\" must not contain line breaks"}},
		{"group line break", map[string]string{gr: "", "channels.d/gr.toml": "group = \"a\\nb\"\n"}, []string{"gr.toml: group: must not contain line breaks"}},
		{"channel group line break", map[string]string{gr: "", "channels.d/gr.toml": ch("group = \"a\\nb\"")}, []string{"channel 1 (\"Example\"): group: must not contain line breaks"}},
		{"tvg_id line break", map[string]string{gr: "", "channels.d/gr.toml": ch("[channel.playlist]", "tvg_id = \"a\\nb\"")}, []string{"playlist.tvg_id: must not contain line breaks"}},
		{"logo line break", map[string]string{gr: "", "channels.d/gr.toml": ch("[channel.playlist]", "logo = \"a\\nb\"")}, []string{"playlist.logo: must not contain line breaks"}},
		// channels.d/_defaults.toml (5.4)
		{"global toml syntax", map[string]string{"channels.d/_defaults.toml": "[defaults\n"}, []string{"_defaults.toml:1:"}},
		{"global unknown key", map[string]string{"channels.d/_defaults.toml": "[defaults.upstream]\nproxy = \"x\"\n"}, []string{"_defaults.toml:2:1: unknown key \"defaults.upstream.proxy\""}},
		{"global tunnel", map[string]string{gr: "", "channels.d/_defaults.toml": "tunnel = \"gr\"\n"}, []string{"_defaults.toml: tunnel: not allowed in _defaults.toml"}},
		{"global tunnel does not route", map[string]string{
			gr: "", "channels.d/_defaults.toml": "tunnel = \"gr\"\n", "channels.d/news.toml": ch(),
		}, []string{"news.toml: channel 1 (\"Example\"): no route"}},
		{"global group", map[string]string{"channels.d/_defaults.toml": "group = \"TV\"\n"}, []string{"_defaults.toml: group: not allowed in _defaults.toml"}},
		{"global channel", map[string]string{"channels.d/_defaults.toml": "tunnel = \"direct\"\n" + ch()}, []string{"_defaults.toml: [[channel]]: not allowed in _defaults.toml", "_defaults.toml: tunnel: not allowed"}},
		{"global tvg_id", map[string]string{"channels.d/_defaults.toml": "[defaults.playlist]\ntvg_id = \"x\"\n"}, []string{"_defaults.toml: defaults.playlist.tvg_id: not allowed in _defaults.toml"}},
		{"global upstream type", map[string]string{"channels.d/_defaults.toml": "[defaults.upstream]\ntype = \"dash\"\n"}, []string{"_defaults.toml: defaults.upstream.type: invalid value \"dash\""}},
		{"global header name", map[string]string{"channels.d/_defaults.toml": "[defaults.upstream]\nheaders = { \"Bad Header\" = \"x\" }\n"}, []string{"_defaults.toml: defaults.upstream.headers: invalid header name \"Bad Header\""}},
		{"global header case duplicate", map[string]string{"channels.d/_defaults.toml": "[defaults.upstream]\nheaders = { Referer = \"a\", REFERER = \"\" }\n"}, []string{"_defaults.toml: defaults.upstream.headers: duplicate header \"Referer\""}},
		{"global reserved attr", map[string]string{"channels.d/_defaults.toml": "[defaults.playlist]\nattrs = { Group-Title = \"x\" }\n"}, []string{"_defaults.toml: defaults.playlist.attrs: \"Group-Title\" is reserved"}},
		{"global forbidden directive", map[string]string{"channels.d/_defaults.toml": "[defaults.playlist.extra.EXTINF]\na = \"b\"\n"}, []string{"_defaults.toml: defaults.playlist.extra: directive \"EXTINF\" is forbidden"}},
		{"global extra value line break", map[string]string{"channels.d/_defaults.toml": "[defaults.playlist.extra.KODIPROP]\na = \"x\\ny\"\n"}, []string{"_defaults.toml: defaults.playlist.extra.KODIPROP: value of \"a\" must not contain line breaks"}},
		{"reserved file name", map[string]string{"channels.d/_tv.toml": "tunnel = \"direct\"\n" + ch()}, []string{"_tv.toml: reserved file name: channels.d names starting with \"_\" are reserved"}},
		{"reserved file name case", map[string]string{"channels.d/_Defaults.toml": ""}, []string{"_Defaults.toml: reserved file name"}},

		{"all errors reported", map[string]string{
			"config.toml":       "log_level = \"x\"\n",
			"channels.d/a.toml": "[[channel]]\nurl = \"https://example.com/\"\ntunnel = \"direct\"\n[[channel]]\nname = \"B\"\ntunnel = \"direct\"\n",
		}, []string{"log_level", "channel 1: name: required", "channel 2 (\"B\"): url: required"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadDir(writeConfigDir(t, tt.files))
			if err == nil {
				t.Fatalf("Load() error = nil, want %q", tt.want)
			}
			for _, w := range tt.want {
				if not, ok := strings.CutPrefix(w, "!"); ok {
					if strings.Contains(err.Error(), not) {
						t.Errorf("Load() error = %q\nmust not contain %q", err, not)
					}
					continue
				}
				if !strings.Contains(err.Error(), w) {
					t.Errorf("Load() error = %q\nwant substring %q", err, w)
				}
			}
		})
	}
}

func TestLoadDirs(t *testing.T) {
	t.Run("config dir missing", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "missing")
		_, err := loadDir(dir)
		if err == nil || !strings.Contains(err.Error(), "config dir") {
			t.Fatalf("Load() error = %v, want config dir error", err)
		}
	})
	t.Run("config dir is a file", func(t *testing.T) {
		dir := writeConfigDir(t, map[string]string{"f": ""})
		_, err := loadDir(filepath.Join(dir, "f"))
		if err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Fatalf("Load() error = %v, want not a directory", err)
		}
	})
	t.Run("tunnels dir elsewhere", func(t *testing.T) {
		dir := writeConfigDir(t, map[string]string{
			"channels.d/gr.toml": ch(),
			"wg/gr.conf":         "",
		})
		cfg, err := Load(Options{Paths: Paths{ConfigDir: dir, TunnelsDir: filepath.Join(dir, "wg")}})
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.Channels()[0].Route(); got != "gr" {
			t.Errorf("Route() = %q, want gr", got)
		}
		if got := cfg.Tunnels(); len(got) != 1 || got[0].Path != filepath.Join(dir, "wg", "gr.conf") {
			t.Errorf("Tunnels() = %+v", got)
		}
	})
	t.Run("tunnels dir unreadable is an error", func(t *testing.T) {
		dir := writeConfigDir(t, map[string]string{"tunnels": ""}) // a file, not a dir
		_, err := loadDir(dir)
		if err == nil || !strings.Contains(err.Error(), "tunnels") {
			t.Fatalf("Load() error = %v, want tunnels dir error", err)
		}
	})
}

func TestLoadOverrides(t *testing.T) {
	dir := writeConfigDir(t, map[string]string{
		"config.toml": "listen = \":1000\"\nlog_level = \"warn\"\nlog_format = \"json\"\npublic_base_url = \"http://a.example.com\"\n",
	})
	paths := Paths{ConfigDir: dir, TunnelsDir: filepath.Join(dir, "tunnels")}

	cfg, err := Load(Options{Paths: paths})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen() != ":1000" || cfg.LogLevel().String() != "WARN" || cfg.LogFormat() != LogFormatJSON || cfg.PublicBaseURL() != "http://a.example.com" {
		t.Errorf("config.toml values not used: %+v", dumpConfig(cfg))
	}

	cfg, err = Load(Options{Paths: paths, Overrides: Overrides{
		Listen: "127.0.0.1:2000", LogLevel: "debug", LogFormat: "text", PublicBaseURL: "https://b.example.com",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen() != "127.0.0.1:2000" || cfg.LogLevel().String() != "DEBUG" || cfg.LogFormat() != LogFormatText || cfg.PublicBaseURL() != "https://b.example.com" {
		t.Errorf("overrides not applied: %+v", dumpConfig(cfg))
	}

	_, err = Load(Options{Paths: paths, Overrides: Overrides{LogLevel: "loud"}})
	if err == nil || !strings.Contains(err.Error(), "log_level (flag or environment): invalid value \"loud\"") {
		t.Errorf("Load() error = %v, want invalid override", err)
	}
}

func TestConfigIsImmutable(t *testing.T) {
	dir := writeConfigDir(t, map[string]string{
		"config.toml":        "[upstream.allow_private]\ndirect = [\"192.168.1.0/24\"]\n",
		"tunnels/gr.conf":    "",
		"channels.d/gr.toml": ch("[channel.upstream]", "headers = { Referer = \"https://example.com/\" }", "[channel.playlist]", "attrs = { a = \"1\" }", "[channel.playlist.extra.KODIPROP]", "k = \"v\""),
	})
	cfg, err := loadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Channels()[0]
	c.Header().Set("Referer", "changed")
	c.Attrs()[0].Value = "changed"
	c.Extra()[0].Value = "changed"
	cfg.AllowPrivate()["direct"][0] = netip.MustParsePrefix("0.0.0.0/0")
	cfg.Tunnels()[0].Name = "changed"
	cfg.Channels()[0] = Channel{}

	c2, ok := cfg.Channel("example")
	if !ok {
		t.Fatal("Channel(example) not found")
	}
	if c2.Header().Get("Referer") != "https://example.com/" || c2.Attrs()[0].Value != "1" || c2.Extra()[0].Value != "v" {
		t.Errorf("channel was modified through an accessor")
	}
	if cfg.AllowPrivate()["direct"][0].String() != "192.168.1.0/24" || cfg.Tunnels()[0].Name != "gr" || cfg.Channels()[0].ID() != "example" {
		t.Errorf("config was modified through an accessor")
	}
	if _, ok := cfg.Channel("missing"); ok {
		t.Errorf("Channel(missing) found")
	}
}

// TestExampleConfig: the example config dir loads, its _defaults.toml
// gives every channel inputstream.adaptive and radio.toml opts out.
func TestExampleConfig(t *testing.T) {
	cfg, err := loadDir("../../examples/config")
	if err != nil {
		t.Fatal(err)
	}
	adaptive := Extra{Directive: "KODIPROP", Key: "inputstream", Value: "inputstream.adaptive"}
	want := map[string]bool{"ert-world": true, "das-erste": true, "example-radio": false}
	if len(cfg.Channels()) != len(want) {
		t.Fatalf("channels = %d, want %d", len(cfg.Channels()), len(want))
	}
	for _, c := range cfg.Channels() {
		if got := slices.Contains(c.Extra(), adaptive); got != want[c.ID()] {
			t.Errorf("channel %s: inputstream.adaptive = %v, want %v (extra %v)", c.ID(), got, want[c.ID()], c.Extra())
		}
	}
}
