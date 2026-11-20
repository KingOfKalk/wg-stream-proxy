// SPDX-License-Identifier: AGPL-3.0-or-later

package probe

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func TestCheckURL(t *testing.T) {
	tests := []struct {
		url string
		ok  bool
	}{
		{"http://a.example/x", true},
		{"https://a.example:8443/x?token=secret", true},
		{"", false},
		{"radio", false},
		{"ftp://a.example/x", false},
		{"http:///x", false},
		{"http://a.example/%zz", false},
	}
	for _, tt := range tests {
		err := CheckURL(tt.url)
		if (err == nil) != tt.ok {
			t.Errorf("CheckURL(%q) = %v, want ok=%v", tt.url, err, tt.ok)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Errorf("error repeats the URL: %v", err)
		}
	}
}

func TestSetHeader(t *testing.T) {
	tests := []struct {
		name, key, value string
		want             http.Header
		err              string
	}{
		{"canonical name", "x-token", "a=b", http.Header{"X-Token": {"a=b"}}, ""},
		{"empty value drops", "Referer", "", http.Header{}, ""},
		{"empty User-Agent sends none", "user-agent", "", http.Header{"User-Agent": {""}}, ""},
		{"invalid name", "X Token", "1", nil, "invalid header name"},
		{"empty name", "", "1", nil, "invalid header name"},
		{"line break", "X-Token", "a\r\nb", nil, "invalid value for header"},
		{"NUL", "X-Token", "a\x00b", nil, "invalid value for header"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{"Referer": {"https://old.example/"}}
			err := SetHeader(h, tt.key, tt.value)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Errorf("error = %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range tt.want {
				if got := h[k]; len(got) != 1 || got[0] != v[0] {
					t.Errorf("%s = %v, want %v", k, got, v)
				}
			}
			if _, ok := h["Referer"]; ok != (tt.key != "Referer") {
				t.Errorf("Referer present = %v after setting %q", ok, tt.key)
			}
		})
	}
}

// TestModeInOutput: the mode shows in both forms.
func TestModeInOutput(t *testing.T) {
	tests := []struct {
		mode, text, json string
	}{
		{ModeProxy, "route direct (via running proxy)\n", `"mode": "proxy"`},
		{"", "route direct\n", ""},
	}
	for _, tt := range tests {
		r := &Report{Mode: tt.mode, Route: "direct", OK: true}
		var text, js bytes.Buffer
		if err := r.WriteText(&text); err != nil {
			t.Fatal(err)
		}
		if err := r.WriteJSON(&js); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(text.String(), tt.text) {
			t.Errorf("mode %q: text = %q, want prefix %q", tt.mode, text.String(), tt.text)
		}
		if tt.json != "" && !strings.Contains(js.String(), tt.json) || tt.json == "" && strings.Contains(js.String(), `"mode"`) {
			t.Errorf("mode %q: json = %s", tt.mode, js.String())
		}
	}
}
