// SPDX-License-Identifier: AGPL-3.0-or-later

package route

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFetchTrace(t *testing.T) {
	r, n, _ := newFixture(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/a" {
			http.Redirect(w, req, "/b?token=second", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "ok")
	})

	// A new connection, one redirect over the same connection.
	first := new(Trace)
	resp := mustFetch(t, r, Request{URL: "http://cdn.example.com/a?token=first", Trace: first})
	if body := readClose(t, resp); body != "ok" {
		t.Fatalf("body = %q", body)
	}
	p := first.Phases()
	switch {
	case p.Redirects != 1:
		t.Errorf("Redirects = %d, want 1", p.Redirects)
	case p.URL != "http://cdn.example.com/b":
		t.Errorf("URL = %q, want the final URL without query", p.URL)
	case p.ConnectAttempts != 1:
		t.Errorf("ConnectAttempts = %d, want 1", p.ConnectAttempts)
	case p.Connect <= 0:
		t.Errorf("Connect = %s, want > 0", p.Connect)
	case p.FirstByte <= 0:
		t.Errorf("FirstByte = %s, want > 0", p.FirstByte)
	case p.NetworkStarted || p.NetworkStart != 0:
		t.Errorf("NetworkStarted = %t, NetworkStart = %s; the network reported no wait", p.NetworkStarted, p.NetworkStart)
	case p.TLS != 0:
		t.Errorf("TLS = %s for plain HTTP", p.TLS)
	}

	// The next request reuses the pooled connection: no lookup, no dial.
	second := new(Trace)
	readClose(t, mustFetch(t, r, Request{URL: "http://cdn.example.com/b", Trace: second}))
	p = second.Phases()
	if !p.Reused || p.ConnectAttempts != 0 || p.Connect != 0 || p.DNS != 0 {
		t.Errorf("pooled connection: %+v, want Reused and no DNS or connect", p)
	}

	// The network's start wait is reported apart from the lookup.
	r.CloseIdleConnections()
	third := new(Trace)
	n.onLookup = func() { third.NetworkWait(time.Hour) }
	readClose(t, mustFetch(t, r, Request{URL: "http://cdn.example.com/b", Trace: third}))
	p = third.Phases()
	if !p.NetworkStarted || p.NetworkStart != time.Hour || p.DNS >= time.Hour {
		t.Errorf("network wait: %+v, want NetworkStart 1h, not counted as DNS", p)
	}
}

func TestFetchTraceTLSError(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	defer srv.Close()
	r := testRoute(fixtureNet(srv.Listener.Addr().String(), fixtureHosts), newFakeClock())
	defer r.CloseIdleConnections()

	tr := new(Trace)
	_, err := r.Fetch(context.Background(), Request{URL: "https://cdn.example.com/live.m3u8?token=secret", Trace: tr})
	if Classify(err) != ReasonTLS {
		t.Fatalf("Fetch: %v, want a TLS error", err)
	}
	p := tr.Phases()
	if p.TLS <= 0 || p.FirstByte != 0 {
		t.Errorf("TLS = %s, FirstByte = %s; want a TLS time and no response", p.TLS, p.FirstByte)
	}
	if p.URL != "https://cdn.example.com/live.m3u8" || strings.Contains(p.URL, "secret") {
		t.Errorf("URL = %q, want it without query", p.URL)
	}
}

func TestTraceNil(t *testing.T) {
	var tr *Trace
	tr.NetworkWait(time.Second)
	if p := tr.Phases(); p != (Phases{}) {
		t.Errorf("nil Trace: Phases() = %+v, want zero", p)
	}
}

// TestPhasesHappened checks the flags that tell a phase that did not
// happen from a fast one: a lookup, a dial, a TLS handshake, a response.
func TestPhasesHappened(t *testing.T) {
	r, _, _ := newFixture(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })

	// New connection by host name: lookup and dial, no TLS.
	cold := new(Trace)
	readClose(t, mustFetch(t, r, Request{URL: "http://cdn.example.com/a", Trace: cold}))
	p := cold.Phases()
	if p.Lookups != 1 || p.ConnectAttempts != 1 || p.TLSHandshakes != 0 || !p.GotFirstByte {
		t.Errorf("cold: Lookups = %d, ConnectAttempts = %d, TLSHandshakes = %d, GotFirstByte = %t; want 1, 1, 0, true",
			p.Lookups, p.ConnectAttempts, p.TLSHandshakes, p.GotFirstByte)
	}

	// Pooled connection: only the response.
	warm := new(Trace)
	readClose(t, mustFetch(t, r, Request{URL: "http://cdn.example.com/a", Trace: warm}))
	p = warm.Phases()
	if !p.Reused || p.Lookups != 0 || p.ConnectAttempts != 0 || p.TLSHandshakes != 0 || !p.GotFirstByte {
		t.Errorf("pooled: %+v, want only a response", p)
	}

	// IP literal on a new connection: no lookup.
	r.CloseIdleConnections()
	lit := new(Trace)
	readClose(t, mustFetch(t, r, Request{URL: "http://192.0.2.7/a", Trace: lit}))
	p = lit.Phases()
	if p.Lookups != 0 || p.ConnectAttempts != 1 || !p.GotFirstByte {
		t.Errorf("IP literal: Lookups = %d, ConnectAttempts = %d, GotFirstByte = %t; want 0, 1, true", p.Lookups, p.ConnectAttempts, p.GotFirstByte)
	}
}

// TestPhasesTLSCounted: a TLS handshake is counted, also a failed one,
// which has no response.
func TestPhasesTLSCounted(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	defer srv.Close()
	r := testRoute(fixtureNet(srv.Listener.Addr().String(), fixtureHosts), newFakeClock())
	defer r.CloseIdleConnections()

	tr := new(Trace)
	_, _ = r.Fetch(context.Background(), Request{URL: "https://cdn.example.com/x", Trace: tr})
	if p := tr.Phases(); p.TLSHandshakes != 1 || p.GotFirstByte {
		t.Errorf("TLSHandshakes = %d, GotFirstByte = %t; want 1, false", p.TLSHandshakes, p.GotFirstByte)
	}
}
