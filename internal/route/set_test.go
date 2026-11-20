// SPDX-License-Identifier: AGPL-3.0-or-later

package route

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
)

func TestNewSet(t *testing.T) {
	gr := &fakeNet{}
	tests := []struct {
		name    string
		direct  Network
		tunnels map[string]Network
		allow   map[string][]netip.Prefix
		wantErr string
	}{
		{"direct only", Host(), nil, nil, ""},
		{
			"tunnels and allowlists", Host(),
			map[string]Network{"gr": gr, "de-1": gr},
			map[string][]netip.Prefix{"direct": {netip.MustParsePrefix("192.168.1.0/24")}, "gr": {netip.MustParsePrefix("10.8.0.0/24")}},
			"",
		},
		{"nil direct", nil, nil, nil, "direct network"},
		{"tunnel named direct", Host(), map[string]Network{"direct": gr}, nil, `"direct"`},
		{"invalid tunnel name", Host(), map[string]Network{"Bad_Name": gr}, nil, `"Bad_Name"`},
		{"nil tunnel network", Host(), map[string]Network{"gr": nil}, nil, `"gr"`},
		{"allowlist for unknown route", Host(), nil, map[string][]netip.Prefix{"gr": nil}, `"gr"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := NewSet(tt.direct, tt.tunnels, Options{Upstream: testUpstream, AllowPrivate: tt.allow})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("NewSet error = %v, want one containing %s", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewSet: %v", err)
			}
			defer s.CloseIdleConnections()
			names := []string{config.RouteDirect}
			for name := range tt.tunnels {
				names = append(names, name)
			}
			for _, name := range names {
				r, ok := s.Route(name)
				if !ok || r.Name() != name {
					t.Errorf("Route(%q) = %v, %v", name, r, ok)
				}
			}
			if _, ok := s.Route("missing"); ok {
				t.Errorf("Route(missing) found")
			}
		})
	}
}

func TestRouteTransportSettings(t *testing.T) {
	s, err := NewSet(Host(), map[string]Network{"gr": &fakeNet{}}, Options{Upstream: testUpstream})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"direct", "gr"} {
		r, _ := s.Route(name)
		for kind, tr := range map[string]*http.Transport{"normal": r.transport, "icy": r.icyTransport} {
			if tr.Proxy != nil {
				t.Errorf("%s %s: Proxy set, environment proxies would bypass the route", name, kind)
			}
			if !tr.ForceAttemptHTTP2 {
				t.Errorf("%s %s: ForceAttemptHTTP2 = false", name, kind)
			}
			if tr.MaxIdleConnsPerHost != 16 {
				t.Errorf("%s %s: MaxIdleConnsPerHost = %d, want 16", name, kind, tr.MaxIdleConnsPerHost)
			}
			// Both keep connections alive: the ICY wrapper checks every
			// response of a connection (#131).
			if tr.DisableKeepAlives {
				t.Errorf("%s %s: DisableKeepAlives = %v", name, kind, tr.DisableKeepAlives)
			}
			if tr.DialContext == nil {
				t.Errorf("%s %s: no DialContext", name, kind)
			}
		}
		if r.transport == r.icyTransport {
			t.Errorf("%s: normal and ICY requests share one transport", name)
		}
	}
	d, _ := s.Route("direct")
	g, _ := s.Route("gr")
	if d.transport == g.transport || d.icyTransport == g.icyTransport {
		t.Errorf("routes share one transport")
	}
}

// Leak test: a recording direct network sees no lookups and no dials for
// a tunnel route, redirects to other hosts included, and the other way
// round.
func TestSetNoLeaksBetweenRoutes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.m3u8" {
			http.Redirect(w, r, "http://other.example.net/seg-1.ts", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	direct := fixtureNet(srv.Listener.Addr().String(), fixtureHosts)
	gr := fixtureNet(srv.Listener.Addr().String(), fixtureHosts)
	s, err := NewSet(direct, map[string]Network{"gr": gr}, Options{Upstream: testUpstream, Clock: newFakeClock()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.CloseIdleConnections()

	fetch := func(route, u string) {
		t.Helper()
		r, _ := s.Route(route)
		readClose(t, mustFetch(t, r, Request{URL: u}))
	}
	fetch("gr", "http://cdn.example.com/index.m3u8")
	fetch("gr", "http://cdn.example.com/key.bin")
	if got := direct.Lookups(); len(got) != 0 {
		t.Errorf("direct lookups for tunnel requests: %q", got)
	}
	if got := direct.Dialed(); len(got) != 0 {
		t.Errorf("direct dials for tunnel requests: %q", got)
	}
	if got := gr.Lookups(); !slicesHave(got, "cdn.example.com", "other.example.net") {
		t.Errorf("tunnel lookups = %q", got)
	}

	before := len(gr.Dialed()) + len(gr.Lookups())
	fetch("direct", "http://cdn.example.com/index.m3u8")
	if after := len(gr.Dialed()) + len(gr.Lookups()); after != before {
		t.Errorf("direct request used the tunnel network")
	}
	if len(direct.Dialed()) == 0 {
		t.Errorf("direct request did not use the direct network")
	}
}

func slicesHave(got []string, want ...string) bool {
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// Production guard: redirects and DNS answers to private addresses are
// blocked, also when DNS changes between connections (rebinding).
func TestProductionGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://10.0.0.1/seg.ts?token=secret", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	hosts := map[string][]netip.Addr{
		"cdn.example.com":    addrs("203.0.113.10"),
		"rebind.example.com": addrs("192.168.1.5"),
		"later.example.com":  addrs("203.0.113.30"),
	}
	gr := fixtureNet(srv.Listener.Addr().String(), hosts)
	s, err := NewSet(Host(), map[string]Network{"gr": gr}, Options{Upstream: testUpstream, Clock: newFakeClock()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.CloseIdleConnections()
	r, _ := s.Route("gr")

	blocked := func(u string) {
		t.Helper()
		resp, err := r.Fetch(context.Background(), Request{URL: u})
		if resp != nil {
			_ = resp.Body.Close()
		}
		if !errors.Is(err, ErrBlocked) || Classify(err) != ReasonGuard || ErrorStatus(err) != http.StatusBadGateway {
			t.Errorf("Fetch(%s) = %v, want a guard error", u, err)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Errorf("error leaks the query: %v", err)
		}
	}
	blocked("http://cdn.example.com/redirect")
	blocked("http://rebind.example.com/live.m3u8")
	blocked("http://127.0.0.1/live.m3u8")
	blocked("http://[::ffff:10.0.0.1]/live.m3u8")

	readClose(t, mustFetch(t, r, Request{URL: "http://later.example.com/a.ts"}))
	r.CloseIdleConnections()
	gr.mu.Lock()
	gr.hosts["later.example.com"] = addrs("10.0.0.7")
	gr.mu.Unlock()
	blocked("http://later.example.com/b.ts")

	for _, d := range gr.Dialed() {
		if !strings.Contains(d, "203.0.113.") {
			t.Errorf("dialed %s, want only public addresses", d)
		}
	}
}

// The host network: real lookups and dials, guarded unless allowlisted.
func TestHostNetwork(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	_, port, _ := net.SplitHostPort(u.Host)
	byName := "http://localhost:" + port + "/"

	s, err := NewSet(Host(), nil, Options{Upstream: testUpstream})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := s.Route("direct")
	for _, target := range []string{srv.URL, byName} {
		_, err := r.Fetch(context.Background(), Request{URL: target})
		if !errors.Is(err, ErrBlocked) {
			t.Errorf("Fetch(%s) without allowlist = %v, want ErrBlocked", target, err)
		}
	}

	allow := map[string][]netip.Prefix{"direct": {netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}}
	s, err = NewSet(Host(), nil, Options{Upstream: testUpstream, AllowPrivate: allow})
	if err != nil {
		t.Fatal(err)
	}
	defer s.CloseIdleConnections()
	r, _ = s.Route("direct")
	for _, target := range []string{srv.URL, byName} {
		if body := readClose(t, mustFetch(t, r, Request{URL: target})); body != "ok" {
			t.Errorf("Fetch(%s) body = %q", target, body)
		}
	}
}

func TestSetNext(t *testing.T) {
	gr, uk, other := &fakeNet{}, &fakeNet{}, &fakeNet{}
	act, otherAct := &countingActivity{}, &countingActivity{}
	base := func() (map[string]Network, Options) {
		return map[string]Network{"gr": gr, "uk": uk}, Options{
			Upstream:     testUpstream,
			AllowPrivate: map[string][]netip.Prefix{"gr": {netip.MustParsePrefix("10.8.0.0/24")}},
			Activity:     map[string]Activity{"gr": act},
		}
	}
	slower := testUpstream
	slower.ReadTimeout *= 2
	tests := []struct {
		name   string
		edit   func(direct *Network, tunnels map[string]Network, opts *Options)
		routes []string // routes of the next set
		kept   []string // routes kept with their pooled connections
	}{
		{"unchanged", func(*Network, map[string]Network, *Options) {}, []string{"direct", "gr", "uk"}, []string{"direct", "gr", "uk"}},
		{
			"tunnel network replaced", func(_ *Network, n map[string]Network, _ *Options) { n["gr"] = other },
			[]string{"direct", "gr", "uk"},
			[]string{"direct", "uk"},
		},
		{
			"tunnel removed", func(_ *Network, n map[string]Network, _ *Options) { delete(n, "uk") },
			[]string{"direct", "gr"},
			[]string{"direct", "gr"},
		},
		{
			"tunnel added", func(_ *Network, n map[string]Network, _ *Options) { n["de"] = other },
			[]string{"de", "direct", "gr", "uk"},
			[]string{"direct", "gr", "uk"},
		},
		{"guard exceptions changed", func(_ *Network, _ map[string]Network, o *Options) {
			o.AllowPrivate = map[string][]netip.Prefix{"gr": {netip.MustParsePrefix("10.9.0.0/24")}}
		}, []string{"direct", "gr", "uk"}, []string{"direct", "uk"}},
		{"guard exceptions added", func(_ *Network, _ map[string]Network, o *Options) {
			o.AllowPrivate["direct"] = []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")}
		}, []string{"direct", "gr", "uk"}, []string{"gr", "uk"}},
		{
			"activity replaced", func(_ *Network, _ map[string]Network, o *Options) { o.Activity = map[string]Activity{"gr": otherAct} },
			[]string{"direct", "gr", "uk"},
			[]string{"direct", "uk"},
		},
		{
			"upstream settings changed", func(_ *Network, _ map[string]Network, o *Options) { o.Upstream = slower },
			[]string{"direct", "gr", "uk"},
			nil,
		},
		{
			"direct network replaced", func(d *Network, _ map[string]Network, _ *Options) { *d = other },
			[]string{"direct", "gr", "uk"},
			[]string{"gr", "uk"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tunnels, opts := base()
			s, err := NewSet(Host(), tunnels, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer s.CloseIdleConnections()
			direct := Host()
			tunnels, opts = base()
			tt.edit(&direct, tunnels, &opts)
			next, dropped, err := s.Next(direct, tunnels, opts)
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			defer next.CloseIdleConnections()
			var wantDropped []string
			for _, name := range []string{"direct", "gr", "uk"} {
				if !slices.Contains(tt.kept, name) {
					wantDropped = append(wantDropped, name)
				}
			}
			var gotDropped []string
			for _, r := range dropped {
				gotDropped = append(gotDropped, r.Name())
				if old, _ := s.Route(r.Name()); old != r {
					t.Errorf("dropped route %s is not the old route", r.Name())
				}
			}
			slices.Sort(gotDropped)
			if !slices.Equal(gotDropped, wantDropped) {
				t.Errorf("dropped = %v, want %v", gotDropped, wantDropped)
			}
			for _, name := range tt.routes {
				r, ok := next.Route(name)
				if !ok {
					t.Fatalf("next set lacks route %s", name)
				}
				old, _ := s.Route(name)
				if kept := slices.Contains(tt.kept, name); (r == old) != kept {
					t.Errorf("route %s kept = %v, want %v", name, r == old, kept)
				}
			}
			if _, ok := next.Route("uk"); ok != slices.Contains(tt.routes, "uk") {
				t.Errorf("route uk present = %v", ok)
			}
		})
	}
}

func TestSetNextErrors(t *testing.T) {
	s, err := NewSet(Host(), map[string]Network{"gr": &fakeNet{}}, Options{Upstream: testUpstream})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Next(Host(), map[string]Network{"Bad_Name": &fakeNet{}}, Options{Upstream: testUpstream}); err == nil {
		t.Error("Next with an invalid tunnel name succeeded")
	}
	if _, _, err := s.Next(nil, nil, Options{Upstream: testUpstream}); err == nil {
		t.Error("Next without direct network succeeded")
	}
}

// uncomparableNet is a Network of a type that == cannot compare.
type uncomparableNet struct {
	*fakeNet
	tags []string
}

func TestSetNextUncomparableNetwork(t *testing.T) {
	n := uncomparableNet{fakeNet: &fakeNet{}, tags: []string{"a"}}
	s, err := NewSet(Host(), map[string]Network{"gr": n}, Options{Upstream: testUpstream})
	if err != nil {
		t.Fatal(err)
	}
	// No panic; the route cannot be proven unchanged, so it is new.
	next, dropped, err := s.Next(Host(), map[string]Network{"gr": n}, Options{Upstream: testUpstream})
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 1 || dropped[0].Name() != "gr" {
		t.Errorf("dropped = %v, want gr", dropped)
	}
	old, _ := s.Route("gr")
	if r, _ := next.Route("gr"); r == old {
		t.Error("route over an uncomparable network kept")
	}
}
