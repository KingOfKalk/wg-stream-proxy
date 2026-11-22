// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"encoding/base64"
	"strings"
	"testing"
)

// TestURLCredentials: a channel URL with user info authenticates every
// upstream request of the channel, also the resources under /r/ resolved
// against it, and never logs the credentials (spec 3.4, 8).
func TestURLCredentials(t *testing.T) {
	e := newDirectEnv(t, allowLoopback, `
tunnel = "direct"

[[channel]]
name = "Cam"
url = "http://alice:s3cr3t-pw@{UPHOST}/live/master.m3u8"
`)
	// The absolute variant URI carries the user info itself; the other
	// URIs and the redirect are relative.
	want := hlsFixture(e.up, "http://alice:s3cr3t-pw@"+strings.TrimPrefix(e.upURL, "http://"))
	got, _ := crawl(t, e.client, e.proxy.URL, "cam", e.proxy.URL+"/c/cam.m3u8")
	if len(got) != len(want) {
		t.Errorf("crawled %d resources, want %d", len(got), len(want))
	}
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cr3t-pw"))
	reqs := e.up.requests()
	if len(reqs) == 0 {
		t.Fatal("upstream saw no request")
	}
	for _, r := range reqs {
		if r.Auth != auth {
			t.Errorf("upstream %s: Authorization %q, want %q", r.Path, r.Auth, auth)
		}
	}
	if logs := e.logs.String(); strings.Contains(logs, "s3cr3t") || strings.Contains(logs, auth[len("Basic "):]) {
		t.Errorf("logs carry the credentials:\n%s", logs)
	}
}
