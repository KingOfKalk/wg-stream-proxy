// SPDX-License-Identifier: AGPL-3.0-or-later

package playlist

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/goleak"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
)

var update = flag.Bool("update", false, "rewrite golden files")

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// load reads the config dir testdata/<dir>/config.
func load(t *testing.T, dir string) *config.Config {
	t.Helper()
	cfgDir := filepath.Join("testdata", dir, "config")
	cfg, err := config.Load(config.Options{Paths: config.Paths{
		ConfigDir:  cfgDir,
		TunnelsDir: filepath.Join(cfgDir, "tunnels"),
	}})
	if err != nil {
		t.Fatalf("load %s: %v", dir, err)
	}
	return cfg
}

// checkGolden compares got with testdata/<name>.golden.
func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	golden := filepath.Join("testdata", name+".golden")
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
		t.Errorf("Render() mismatch (-update to rewrite)\n--- got\n%s\n--- want\n%s", got, want)
	}
}

func TestRenderGolden(t *testing.T) {
	tests := []struct {
		name  string // golden file
		dir   string // config dir under testdata
		base  string
		noEPG bool
	}{
		// The example of spec 6, byte for byte.
		{name: "spec-example", dir: "spec-example", base: "http://wsp.lan:2349"},
		// Greek names, commas and quotes in names and values, empty
		// values, disabled channel, logo kinds, extra ordering, the
		// .m3u8 suffix rule, a base URL with a path prefix.
		{name: "channels", dir: "channels", base: "https://tv.example.org/wsp"},
		{name: "no-epg", dir: "spec-example", base: "http://[2001:db8::1]:2349", noEPG: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := load(t, tt.dir)
			opts := Options{BaseURL: tt.base, EPGURL: cfg.EPGURL()}
			if tt.noEPG {
				opts.EPGURL = ""
			}
			checkGolden(t, tt.name, Render(cfg.Channels(), opts))
		})
	}
}

func TestRenderEmpty(t *testing.T) {
	if got := string(Render(nil, Options{BaseURL: "http://h"})); got != "#EXTM3U\n" {
		t.Errorf("Render(nil) = %q, want %q", got, "#EXTM3U\n")
	}
}

func TestChannelPath(t *testing.T) {
	cfg := load(t, "channels")
	want := map[string]string{
		"ert1-eidiseis":           "/c/ert1-eidiseis.m3u8", // path contains .m3u8
		"skai-live-hd":            "/c/skai-live-hd.m3u8",  // type hls
		"news-24":                 "/c/news-24",            // .M3U8 does not count
		"radio-jazz":              "/c/radio-jazz",
		"raw-with-playlist-path":  "/c/raw-with-playlist-path.m3u8", // contains .m3u8
		"query-only":              "/c/query-only",                  // query ignored
		"disabled-channel":        "/c/disabled-channel.m3u8",
		"dash-by-path":            "/c/dash-by-path.mpd",            // path contains .mpd
		"dash-by-type":            "/c/dash-by-type.mpd",            // type dash
		"hls-type-with-mpd-path":  "/c/hls-type-with-mpd-path.m3u8", // type hls wins
		"dash-type-with-hls-path": "/c/dash-type-with-hls-path.mpd", // type dash wins
	}
	for _, ch := range cfg.Channels() {
		w, ok := want[ch.ID()]
		if !ok {
			t.Errorf("unexpected channel %q", ch.ID())
			continue
		}
		if got := ChannelPath(ch); got != w {
			t.Errorf("ChannelPath(%s) = %q, want %q", ch.ID(), got, w)
		}
	}
}
