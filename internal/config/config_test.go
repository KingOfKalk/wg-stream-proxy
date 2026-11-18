// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"strings"
	"testing"
)

func TestLogoKindString(t *testing.T) {
	tests := []struct {
		k    LogoKind
		want string
	}{
		{LogoNone, "none"},
		{LogoURL, "url"},
		{LogoLocal, "local"},
		{LogoRaw, "raw"},
		{LogoKind(42), "LogoKind(42)"},
	}
	for _, tt := range tests {
		if got := tt.k.String(); got != tt.want {
			t.Errorf("LogoKind(%d).String() = %q, want %q", int(tt.k), got, tt.want)
		}
	}
}

// loadChannel loads the first channel of one channel file with the given
// body (after tunnel = "direct").
func loadChannel(t *testing.T, body string) Channel {
	t.Helper()
	dir := writeConfigDir(t, map[string]string{"channels.d/tv.toml": "tunnel = \"direct\"\n" + body})
	cfg, err := loadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	chs := cfg.Channels()
	if len(chs) != 1 {
		t.Fatalf("channels = %d, want 1", len(chs))
	}
	return chs[0]
}

func TestChannelEqual(t *testing.T) {
	const base = "[[channel]]\nname = \"News\"\nurl = \"https://example.com/a.m3u8\"\n"
	rich := func(extra string) string {
		return base + extra
	}
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"same", base, base, true},
		{"comments and spacing", base, "# note\n[[channel]]\nname   = \"News\"   # n\nurl = \"https://example.com/a.m3u8\"\n", true},
		{
			"headers in another order",
			rich("upstream.headers = { Referer = \"https://r.example\", Origin = \"https://o.example\" }\n"),
			rich("upstream.headers = { Origin = \"https://o.example\", Referer = \"https://r.example\" }\n"),
			true,
		},
		{
			"attrs in another order",
			rich("playlist.attrs = { radio = \"true\", tvg-chno = \"5\" }\n"),
			rich("playlist.attrs = { tvg-chno = \"5\", radio = \"true\" }\n"),
			true,
		},
		{
			"extra in another order",
			rich("playlist.extra = { KODIPROP = { b = \"2\", a = \"1\" }, EXTVLCOPT = { c = \"3\" } }\n"),
			rich("playlist.extra = { EXTVLCOPT = { c = \"3\" }, KODIPROP = { a = \"1\", b = \"2\" } }\n"),
			true,
		},
		{"url", base, strings.Replace(base, "a.m3u8", "b.m3u8", 1), false},
		{"name", base, strings.Replace(base, "News", "Sport", 1), false},
		{"enabled", base, rich("enabled = false\n"), false},
		{"type", base, rich("upstream.type = \"hls\"\n"), false},
		{"group", base, rich("group = \"Other\"\n"), false},
		{"tvg_id", base, rich("playlist.tvg_id = \"x\"\n"), false},
		{"logo", base, rich("playlist.logo = \"https://example.com/l.png\"\n"), false},
		{"header added", base, rich("upstream.headers = { Referer = \"https://r.example\" }\n"), false},
		{
			"header value",
			rich("upstream.headers = { Referer = \"https://r.example\" }\n"),
			rich("upstream.headers = { Referer = \"https://s.example\" }\n"),
			false,
		},
		{"attr added", base, rich("playlist.attrs = { radio = \"true\" }\n"), false},
		{
			"extra value",
			rich("playlist.extra = { KODIPROP = { a = \"1\" } }\n"),
			rich("playlist.extra = { KODIPROP = { a = \"2\" } }\n"),
			false,
		},
		{"extra added", base, rich("playlist.extra = { KODIPROP = { a = \"1\" } }\n"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b := loadChannel(t, tt.a), loadChannel(t, tt.b)
			if got := a.Equal(b); got != tt.want {
				t.Errorf("a.Equal(b) = %v, want %v", got, tt.want)
			}
			if got := b.Equal(a); got != tt.want {
				t.Errorf("b.Equal(a) = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestChannelEqualIgnoresFile(t *testing.T) {
	a := loadChannel(t, "[[channel]]\nname = \"News\"\nurl = \"https://example.com/a.m3u8\"\ngroup = \"G\"\n")
	b := a
	b.file = "channels.d/other.toml"
	if !a.Equal(b) {
		t.Error("channels differing only in their source file are not equal")
	}
}
