// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestParseSize(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"8MiB", 8 << 20, true},
		{"512KiB", 512 << 10, true},
		{"2097152B", 2 << 20, true},
		{"32MiB", 32 << 20, true},
		{"0B", 0, true},
		{"007KiB", 7 << 10, true},
		{"", 0, false},
		{"8", 0, false},
		{"MiB", 0, false},
		{"8 MiB", 0, false},
		{" 8MiB", 0, false},
		{"8mib", 0, false},
		{"8MB", 0, false},
		{"8M", 0, false},
		{"8GiB", 0, false},
		{"-1MiB", 0, false},
		{"+1MiB", 0, false},
		{"1.5MiB", 0, false},
		{"1_000B", 0, false},
		{"99999999999999999999B", 0, false},
		{"9999999999999MiB", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, ok := parseSize(tt.in)
			if got != tt.want || ok != tt.ok {
				t.Errorf("parseSize(%q) = %d, %v, want %d, %v", tt.in, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestWatchInterval(t *testing.T) {
	tests := []struct {
		name string
		toml string
		want time.Duration
		err  string
	}{
		{"default", "", 2 * time.Second, ""},
		{"empty section", "[reload]\n", 2 * time.Second, ""},
		{"set", "[reload]\nwatch_interval = \"500ms\"\n", 500 * time.Millisecond, ""},
		{"off", "[reload]\nwatch_interval = \"0\"\n", 0, ""},
		{"off with unit", "[reload]\nwatch_interval = \"0s\"\n", 0, ""},
		{"negative", "[reload]\nwatch_interval = \"-1s\"\n", 0, "reload.watch_interval: must be positive, or \"0\" to turn watching off"},
		{"invalid", "[reload]\nwatch_interval = \"soon\"\n", 0, `reload.watch_interval: invalid duration "soon"`},
		{"wrong type", "[reload]\nwatch_interval = 2\n", 0, "watch_interval"},
		{"unknown key", "[reload]\ninterval = \"2s\"\n", 0, `unknown key "reload.interval"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadDir(writeConfigDir(t, map[string]string{"config.toml": tt.toml}))
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("Load() error = %v, want it to contain %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.WatchInterval(); got != tt.want {
				t.Errorf("WatchInterval() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsHTTPURL(t *testing.T) {
	tests := []struct {
		raw  string
		want bool
	}{
		{"http://a.example/x", true},
		{"https://a.example:8443", true},
		{"HTTPS://a.example/", true}, // url.Parse lower-cases the scheme
		{"http://user@a.example/", true},
		{"ftp://a.example/", false},
		{"http:///x", false},
		{"http:a.example", false},
		{"//a.example/x", false},
		{"/x", false},
		{"", false},
	}
	for _, tt := range tests {
		u, err := url.Parse(tt.raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := IsHTTPURL(u); got != tt.want {
			t.Errorf("IsHTTPURL(%q) = %v, want %v", tt.raw, got, tt.want)
		}
		if got := httpURL(tt.raw); got != tt.want {
			t.Errorf("httpURL(%q) = %v, want %v", tt.raw, got, tt.want)
		}
	}
}
