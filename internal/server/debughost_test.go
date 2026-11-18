// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDebugHostAllowed(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"127.0.0.1:2350", true},
		{"127.0.0.1", true},
		{"192.168.1.20:2350", true},
		{"[::1]:2350", true},
		{"[fe80::1%25eth0]:2350", true},
		{"::1", true},
		{"localhost:2350", true},
		{"LocalHost", true},
		{"localhost.:2350", false},
		{"", false},
		{"libreelec.local:2350", false},
		{"evil.example:2350", false},
		{"127.0.0.1.nip.io:2350", false},
		{"localhost.evil.example", false},
		{"[::1", false},
	}
	for _, tt := range tests {
		if got := debugHostAllowed(tt.host); got != tt.want {
			t.Errorf("debugHostAllowed(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}

func TestDebugHandlerRejectsHostNames(t *testing.T) {
	h := newDebugHandler(&debugHandler{})
	tests := []struct {
		method, host string
		want         int
	}{
		{http.MethodGet, "127.0.0.1:2350", http.StatusOK},
		{http.MethodGet, "localhost:2350", http.StatusOK},
		{http.MethodGet, "rebound.example:2350", http.StatusForbidden},
		{http.MethodGet, "", http.StatusForbidden},
		// A rebound page's POST looks same-origin to the browser; the host
		// check stops it before the cross-origin check would let it pass.
		{http.MethodPost, "rebound.example:2350", http.StatusForbidden},
	}
	for _, tt := range tests {
		r := httptest.NewRequest(tt.method, "/debug/pprof/cmdline", nil)
		r.Host = tt.host
		if tt.method == http.MethodPost {
			r.Header.Set("Sec-Fetch-Site", "same-origin")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tt.want {
			t.Errorf("%s with Host %q: %d, want %d", tt.method, tt.host, w.Code, tt.want)
		}
	}
}
