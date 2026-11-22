// SPDX-License-Identifier: AGPL-3.0-or-later

package route

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func basic(userpass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(userpass))
}

// authHop is one upstream request: host and path, and the Authorization
// values it carried.
type authHop struct {
	at   string
	auth []string
}

// authFixture: /start redirects to loc (answers "ok" if loc is empty),
// /back redirects to http://cdn.example.com/end, every other path
// answers "ok". It records every request.
func authFixture(t *testing.T, loc string) (*Route, func() []authHop) {
	t.Helper()
	ch := make(chan authHop, 8)
	r, _, _ := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		ch <- authHop{r.Host + r.URL.Path, r.Header.Values("Authorization")}
		switch {
		case r.URL.Path == "/start" && loc != "":
			http.Redirect(w, r, loc, http.StatusFound)
		case r.URL.Path == "/back":
			http.Redirect(w, r, "http://cdn.example.com/end", http.StatusFound)
		default:
			_, _ = io.WriteString(w, "ok")
		}
	})
	return r, func() []authHop {
		close(ch)
		var hops []authHop
		for h := range ch {
			hops = append(hops, h)
		}
		return hops
	}
}

// TestFetchURLCredentials: user info in an upstream URL is sent as
// Authorization: Basic, on a redirect only to the same scheme, host and
// port, unless the Location has its own (spec 3.4).
func TestFetchURLCredentials(t *testing.T) {
	alice := []string{basic("alice:p@ss w")}
	bob := []string{basic("bob:pw2")}
	tests := []struct {
		name     string
		url      string
		loc      string
		header   http.Header
		want     []authHop
		wantUser string // user info of resp.Request.URL
	}{
		{
			name:     "no redirect",
			url:      "http://alice:p%40ss%20w@cdn.example.com/start",
			want:     []authHop{{"cdn.example.com/start", alice}},
			wantUser: "alice:p%40ss%20w",
		},
		{
			name: "without user info none",
			url:  "http://cdn.example.com/start",
			want: []authHop{{"cdn.example.com/start", nil}},
		},
		{
			name:     "user without password",
			url:      "http://alice@cdn.example.com/start",
			want:     []authHop{{"cdn.example.com/start", []string{basic("alice:")}}},
			wantUser: "alice",
		},
		{
			name:     "relative redirect keeps it",
			url:      "http://alice:p%40ss%20w@cdn.example.com/start",
			loc:      "/end",
			want:     []authHop{{"cdn.example.com/start", alice}, {"cdn.example.com/end", alice}},
			wantUser: "alice:p%40ss%20w",
		},
		{
			name:     "absolute redirect to the same host keeps it",
			url:      "http://alice:p%40ss%20w@cdn.example.com/start",
			loc:      "http://cdn.example.com/end",
			want:     []authHop{{"cdn.example.com/start", alice}, {"cdn.example.com/end", alice}},
			wantUser: "alice:p%40ss%20w",
		},
		{
			name:     "explicit default port is the same port",
			url:      "http://alice:p%40ss%20w@cdn.example.com/start",
			loc:      "http://cdn.example.com:80/end",
			want:     []authHop{{"cdn.example.com/start", alice}, {"cdn.example.com:80/end", alice}},
			wantUser: "alice:p%40ss%20w",
		},
		{
			name: "other port drops it",
			url:  "http://alice:p%40ss%20w@cdn.example.com/start",
			loc:  "http://cdn.example.com:8080/end",
			want: []authHop{{"cdn.example.com/start", alice}, {"cdn.example.com:8080/end", nil}},
		},
		{
			name: "other host drops it",
			url:  "http://alice:p%40ss%20w@cdn.example.com/start",
			loc:  "http://other.example.net/end",
			want: []authHop{{"cdn.example.com/start", alice}, {"other.example.net/end", nil}},
		},
		{
			name: "scheme-relative other host drops it",
			url:  "http://alice:p%40ss%20w@cdn.example.com/start",
			loc:  "//other.example.net/end",
			want: []authHop{{"cdn.example.com/start", alice}, {"other.example.net/end", nil}},
		},
		{
			name: "back from another host stays without",
			url:  "http://alice:p%40ss%20w@cdn.example.com/start",
			loc:  "http://other.example.net/back",
			want: []authHop{{"cdn.example.com/start", alice}, {"other.example.net/back", nil}, {"cdn.example.com/end", nil}},
		},
		{
			name:     "location with its own user info on another host",
			url:      "http://alice:p%40ss%20w@cdn.example.com/start",
			loc:      "http://bob:pw2@other.example.net/end",
			want:     []authHop{{"cdn.example.com/start", alice}, {"other.example.net/end", bob}},
			wantUser: "bob:pw2",
		},
		{
			name:     "location with its own user info on the same host",
			url:      "http://alice:p%40ss%20w@cdn.example.com/start",
			loc:      "http://bob:pw2@cdn.example.com/end",
			want:     []authHop{{"cdn.example.com/start", alice}, {"cdn.example.com/end", bob}},
			wantUser: "bob:pw2",
		},
		{
			name:     "location with user info after a URL without",
			url:      "http://cdn.example.com/start",
			loc:      "http://bob:pw2@other.example.net/end",
			want:     []authHop{{"cdn.example.com/start", nil}, {"other.example.net/end", bob}},
			wantUser: "bob:pw2",
		},
		{
			name:     "configured Authorization wins",
			url:      "http://alice:p%40ss%20w@cdn.example.com/start",
			loc:      "/end",
			header:   http.Header{"authorization": {"Bearer t0ken"}},
			want:     []authHop{{"cdn.example.com/start", []string{"Bearer t0ken"}}, {"cdn.example.com/end", []string{"Bearer t0ken"}}},
			wantUser: "alice:p%40ss%20w",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, hops := authFixture(t, tt.loc)
			resp := mustFetch(t, r, Request{URL: tt.url, Header: tt.header})
			readClose(t, resp)
			got := hops()
			if !slices.EqualFunc(got, tt.want, func(a, b authHop) bool { return a.at == b.at && slices.Equal(a.auth, b.auth) }) {
				t.Errorf("upstream requests\n got %q\nwant %q", got, tt.want)
			}
			// The final URL keeps the credentials it was fetched with, so
			// resources resolved against it (spec 3.2, 4) carry them too.
			if u := resp.Request.URL.User.String(); u != tt.wantUser {
				t.Errorf("final URL user info = %q, want %q", u, tt.wantUser)
			}
		})
	}
}

// TestFetchURLCredentialsNotInErrors: an error names the URL without its
// user info (spec 8), also after a redirect that kept it.
func TestFetchURLCredentialsNotInErrors(t *testing.T) {
	r, hops := authFixture(t, "/missing.m3u8")
	_, err := r.Fetch(context.Background(), Request{
		URL:      "http://alice:s3cr3t@cdn.example.com/start",
		Redirect: func(*url.URL) bool { return false },
	})
	hops()
	if !errors.Is(err, ErrRedirectRefused) {
		t.Fatalf("Fetch error = %v, want ErrRedirectRefused", err)
	}
	for _, s := range []string{"alice", "s3cr3t"} {
		if strings.Contains(err.Error(), s) {
			t.Errorf("error %q leaks %q", err, s)
		}
	}
}

func TestSameOrigin(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"http://cdn.example.com/a", "http://cdn.example.com/b", true},
		{"http://cdn.example.com/a", "http://CDN.Example.com/b", true},
		{"http://cdn.example.com/a", "http://cdn.example.com:80/b", true},
		{"https://cdn.example.com:443/a", "https://cdn.example.com/b", true},
		{"http://[2001:db8::1]/a", "http://[2001:db8::1]:80/b", true},
		{"http://cdn.example.com/a", "https://cdn.example.com/b", false},
		{"https://cdn.example.com/a", "http://cdn.example.com:443/b", false},
		{"http://cdn.example.com/a", "http://cdn.example.com:8080/b", false},
		{"http://cdn.example.com/a", "http://other.example.net/a", false},
		{"http://cdn.example.com/a", "http://cdn.example.com.other.example.net/a", false},
		{"http://example.com/a", "http://cdn.example.com/a", false},
	}
	for _, tt := range tests {
		a, b := mustParse(t, tt.a), mustParse(t, tt.b)
		if got := sameOrigin(a, b); got != tt.want {
			t.Errorf("sameOrigin(%s, %s) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func mustParse(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
