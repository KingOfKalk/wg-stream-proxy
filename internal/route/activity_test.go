// SPDX-License-Identifier: AGPL-3.0-or-later

package route

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// countingActivity records Begin and end calls.
type countingActivity struct {
	mu       sync.Mutex
	inflight int
	begins   int
	ends     int
}

func (a *countingActivity) Begin() func() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.inflight++
	a.begins++
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.inflight--
		a.ends++
	}
}

func (a *countingActivity) state() (inflight, begins, ends int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inflight, a.begins, a.ends
}

func (a *countingActivity) want(t *testing.T, inflight, begins int) {
	t.Helper()
	gotIn, gotBegins, gotEnds := a.state()
	if gotIn != inflight || gotBegins != begins || gotEnds != begins-inflight {
		t.Errorf("activity: in flight %d, begins %d, ends %d; want %d, %d, %d",
			gotIn, gotBegins, gotEnds, inflight, begins, begins-inflight)
	}
}

func TestFetchCountsActivity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/stream", http.StatusFound)
		case "/stream":
			_, _ = io.WriteString(w, strings.Repeat("a", 64<<10))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	tests := []struct {
		name string
		url  string
		// wantErr: Fetch fails, nothing stays counted.
		wantErr bool
	}{
		{name: "response", url: "http://cdn.example.com/stream"},
		{name: "redirect counts once", url: "http://cdn.example.com/redirect"},
		{name: "error status", url: "http://cdn.example.com/missing"},
		{name: "dial error", url: "http://unknown.example.com/stream", wantErr: true},
		{name: "invalid URL", url: "ftp://cdn.example.com/stream", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			act := &countingActivity{}
			s, err := NewSet(Host(), map[string]Network{"gr": fixtureNet(srv.Listener.Addr().String(), fixtureHosts)},
				Options{Upstream: testUpstream, Clock: newFakeClock(), Activity: map[string]Activity{"gr": act}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(s.CloseIdleConnections)
			r, _ := s.Route("gr")

			resp, err := r.Fetch(context.Background(), Request{URL: tt.url})
			if tt.wantErr {
				if err == nil {
					_ = resp.Body.Close()
					t.Fatal("Fetch succeeded, want error")
				}
				_, begins, _ := act.state()
				act.want(t, 0, begins)
				return
			}
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			// The open body is a request in flight.
			act.want(t, 1, 1)
			if _, err := io.ReadFull(resp.Body, make([]byte, 1)); err != nil && resp.StatusCode == http.StatusOK {
				t.Fatalf("read: %v", err)
			}
			act.want(t, 1, 1)
			_ = resp.Body.Close()
			act.want(t, 0, 1)
			// A second Close does not end it twice.
			_ = resp.Body.Close()
			act.want(t, 0, 1)

			// The pooled connection is not activity.
			resp = mustFetch(t, r, Request{URL: tt.url})
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			act.want(t, 0, 2)
		})
	}
}

func TestRouteWithoutActivity(t *testing.T) {
	r, _, _ := newFixture(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	if got := readClose(t, mustFetch(t, r, Request{URL: "http://cdn.example.com/"})); got != "ok" {
		t.Errorf("body = %q", got)
	}
}

func TestNewSetActivityForUnknownRoute(t *testing.T) {
	_, err := NewSet(Host(), nil, Options{Upstream: testUpstream, Activity: map[string]Activity{"gr": &countingActivity{}}})
	if err == nil || !strings.Contains(err.Error(), `"gr"`) {
		t.Errorf("NewSet error = %v, want one naming \"gr\"", err)
	}
}
