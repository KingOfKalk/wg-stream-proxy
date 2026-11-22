// SPDX-License-Identifier: AGPL-3.0-or-later

package route

import (
	"context"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// redirectRT answers each request from a table of redirects keyed by the
// request URL without user info: a Location, else 200 "ok". It records
// the URL (without user info) and the headers of every request.
type redirectRT struct {
	loc  map[string]string
	hops []headerHop
}

type headerHop struct {
	at     string
	header http.Header
}

func (rt *redirectRT) RoundTrip(req *http.Request) (*http.Response, error) {
	u := *req.URL
	u.User = nil
	at := u.String()
	rt.hops = append(rt.hops, headerHop{at, req.Header.Clone()})
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok")), Request: req}
	if loc, ok := rt.loc[at]; ok {
		resp.StatusCode = http.StatusFound
		resp.Header.Set("Location", loc)
	}
	return resp, nil
}

// TestFetchRedirectCredentialHeaders: Authorization, Proxy-Authorization,
// Cookie and Www-Authenticate from upstream.headers go along on a redirect
// only to the same scheme, host and port, and never again once a hop left
// it; other configured headers go to every hop. They combine with the
// Authorization from URL user info (spec 3.4).
func TestFetchRedirectCredentialHeaders(t *testing.T) {
	configured := http.Header{
		"authorization":       {"Bearer t0ken"},
		"Proxy-Authorization": {"Basic cHJveHk6cHc="},
		"cookie":              {"session=c00kie"},
		"Www-Authenticate":    {"Bearer"},
		"User-Agent":          {"Channel-UA/2"},
		"Referer":             {"https://www.example.com/"},
		"Origin":              {"https://www.example.com"},
		"X-Custom":            {"1"},
	}
	// all: every configured header, canonical; public: without the
	// credential headers.
	all := http.Header{}
	for k, v := range configured {
		all[http.CanonicalHeaderKey(k)] = v
	}
	public := all.Clone()
	for _, k := range []string{"Authorization", "Proxy-Authorization", "Cookie", "Www-Authenticate"} {
		public.Del(k)
	}
	alice := basic("alice:pw")
	bob := basic("bob:pw2")

	const start = "http://cdn.example.com/start"
	tests := []struct {
		name   string
		url    string
		header http.Header
		loc    map[string]string
		want   []headerHop
	}{
		{
			name: "no redirect",
			url:  start,
			want: []headerHop{{start, all}},
		},
		{
			name: "relative redirect keeps them",
			url:  start,
			loc:  map[string]string{start: "/end"},
			want: []headerHop{{start, all}, {"http://cdn.example.com/end", all}},
		},
		{
			name: "absolute redirect to the same host keeps them",
			url:  start,
			loc:  map[string]string{start: "http://CDN.example.com/end"},
			want: []headerHop{{start, all}, {"http://CDN.example.com/end", all}},
		},
		{
			name: "explicit default port is the same port",
			url:  start,
			loc:  map[string]string{start: "http://cdn.example.com:80/end"},
			want: []headerHop{{start, all}, {"http://cdn.example.com:80/end", all}},
		},
		{
			name: "other host drops them",
			url:  start,
			loc:  map[string]string{start: "http://other.example.net/end"},
			want: []headerHop{{start, all}, {"http://other.example.net/end", public}},
		},
		{
			name: "scheme-relative other host drops them",
			url:  start,
			loc:  map[string]string{start: "//other.example.net/end"},
			want: []headerHop{{start, all}, {"http://other.example.net/end", public}},
		},
		{
			name: "subdomain is another host",
			url:  start,
			loc:  map[string]string{start: "http://edge.cdn.example.com/end"},
			want: []headerHop{{start, all}, {"http://edge.cdn.example.com/end", public}},
		},
		{
			name: "other port drops them",
			url:  start,
			loc:  map[string]string{start: "http://cdn.example.com:8080/end"},
			want: []headerHop{{start, all}, {"http://cdn.example.com:8080/end", public}},
		},
		{
			name: "other scheme drops them",
			url:  start,
			loc:  map[string]string{start: "https://cdn.example.com/end"},
			want: []headerHop{{start, all}, {"https://cdn.example.com/end", public}},
		},
		{
			name: "back from another host stays without",
			url:  start,
			loc: map[string]string{
				start:                           "http://other.example.net/back",
				"http://other.example.net/back": "http://cdn.example.com/end",
			},
			want: []headerHop{{start, all}, {"http://other.example.net/back", public}, {"http://cdn.example.com/end", public}},
		},
		{
			name: "same host hops keep them until another host",
			url:  start,
			loc: map[string]string{
				start:                        "/mid",
				"http://cdn.example.com/mid": "http://other.example.net/end",
			},
			want: []headerHop{{start, all}, {"http://cdn.example.com/mid", all}, {"http://other.example.net/end", public}},
		},
		{
			name:   "configured Authorization wins over user info, both dropped on another host",
			url:    "http://alice:pw@cdn.example.com/start",
			header: http.Header{"Authorization": {"Bearer t0ken"}},
			loc:    map[string]string{start: "http://other.example.net/end"},
			want: []headerHop{
				{start, http.Header{"Authorization": {"Bearer t0ken"}}},
				{"http://other.example.net/end", http.Header{}},
			},
		},
		{
			name:   "location with its own user info on another host",
			url:    "http://alice:pw@cdn.example.com/start",
			header: http.Header{"Authorization": {"Bearer t0ken"}},
			loc:    map[string]string{start: "http://bob:pw2@other.example.net/end"},
			want: []headerHop{
				{start, http.Header{"Authorization": {"Bearer t0ken"}}},
				{"http://other.example.net/end", http.Header{"Authorization": {bob}}},
			},
		},
		{
			name:   "location with its own user info back on the first host",
			url:    start,
			header: http.Header{"Authorization": {"Bearer t0ken"}},
			loc: map[string]string{
				start:                           "http://other.example.net/back",
				"http://other.example.net/back": "http://bob:pw2@cdn.example.com/end",
			},
			want: []headerHop{
				{start, http.Header{"Authorization": {"Bearer t0ken"}}},
				{"http://other.example.net/back", http.Header{}},
				{"http://cdn.example.com/end", http.Header{"Authorization": {bob}}},
			},
		},
		{
			name:   "user info on the same host without configured Authorization",
			url:    "http://alice:pw@cdn.example.com/start",
			header: http.Header{"Cookie": {"session=c00kie"}},
			loc:    map[string]string{start: "/end"},
			want: []headerHop{
				{start, http.Header{"Authorization": {alice}, "Cookie": {"session=c00kie"}}},
				{"http://cdn.example.com/end", http.Header{"Authorization": {alice}, "Cookie": {"session=c00kie"}}},
			},
		},
		{
			name:   "Cookie dropped, location user info used on another host",
			url:    start,
			header: http.Header{"Cookie": {"session=c00kie"}},
			loc:    map[string]string{start: "http://bob:pw2@other.example.net/end"},
			want: []headerHop{
				{start, http.Header{"Cookie": {"session=c00kie"}}},
				{"http://other.example.net/end", http.Header{"Authorization": {bob}}},
			},
		},
		{
			name: "configured headers go to every hop",
			url:  start,
			header: http.Header{
				"User-Agent": {"Channel-UA/2"}, "Referer": {"https://www.example.com/"},
				"Origin": {"https://www.example.com"}, "X-Custom": {"1"},
			},
			loc: map[string]string{start: "http://other.example.net/end"},
			want: []headerHop{
				{start, public},
				{"http://other.example.net/end", public},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			header := tt.header
			if header == nil {
				header = configured
			}
			rt := &redirectRT{loc: tt.loc}
			r := &Route{upstream: testUpstream}
			u := mustParse(t, tt.url)
			resp, _, err := r.follow(context.Background(), rt, u, Request{URL: tt.url, Header: header})
			if err != nil {
				t.Fatalf("follow: %v", err)
			}
			_ = resp.Body.Close()
			if len(rt.hops) != len(tt.want) {
				t.Fatalf("upstream requests %d, want %d", len(rt.hops), len(tt.want))
			}
			// Only the configured names and Authorization are compared;
			// User-Agent defaults to [upstream] user_agent when not set.
			names := slices.Sorted(maps.Keys(all))
			for i, want := range tt.want {
				got := rt.hops[i]
				if got.at != want.at {
					t.Errorf("hop %d at %s, want %s", i, got.at, want.at)
				}
				for _, k := range names {
					if k == "User-Agent" && want.header.Get(k) == "" {
						continue
					}
					if g, w := got.header.Values(k), want.header.Values(k); !slices.Equal(g, w) {
						t.Errorf("hop %d (%s) %s = %q, want %q", i, got.at, k, g, w)
					}
				}
			}
		})
	}
}

// TestFetchRedirectCredentialHeadersEndToEnd: a configured Authorization
// and Cookie reach the first host but not a redirect target on another
// host, over the route's real transport (spec 3.4).
func TestFetchRedirectCredentialHeadersEndToEnd(t *testing.T) {
	ch := make(chan seen, 4)
	r, _, _ := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		ch <- seen{r.Method, r.Host, r.URL.Path, r.Header.Clone()}
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "http://other.example.net/end", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "ok")
	})
	resp := mustFetch(t, r, Request{
		URL:    "http://cdn.example.com/start",
		Header: http.Header{"Authorization": {"Bearer t0ken"}, "Cookie": {"session=c00kie"}, "X-Custom": {"1"}},
	})
	readClose(t, resp)
	close(ch)
	var got []seen
	for s := range ch {
		got = append(got, s)
	}
	if len(got) != 2 {
		t.Fatalf("upstream requests %d, want 2", len(got))
	}
	first, second := got[0], got[1]
	if first.host != "cdn.example.com" || first.header.Get("Authorization") != "Bearer t0ken" || first.header.Get("Cookie") != "session=c00kie" {
		t.Errorf("first hop %s: credentials missing", first.host)
	}
	if second.host != "other.example.net" {
		t.Fatalf("second hop at %s, want other.example.net", second.host)
	}
	for _, k := range []string{"Authorization", "Cookie"} {
		if _, ok := second.header[k]; ok {
			t.Errorf("second hop %s carries %s", second.host, k)
		}
	}
	if second.header.Get("X-Custom") != "1" {
		t.Errorf("second hop %s: X-Custom missing", second.host)
	}
}
