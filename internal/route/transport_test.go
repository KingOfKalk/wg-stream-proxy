// SPDX-License-Identifier: AGPL-3.0-or-later

package route

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestFetchTLSHandshakeTimeout: an upstream that accepts the TCP
// connection and never answers the ClientHello (inside a tunnel: a
// path-MTU black hole) holds the connection at most connect_timeout,
// also when the request ended before: Go's transport dials with a
// context detached from the request (spec 3.4, #157). Nothing calls
// CloseIdleConnections.
func TestFetchTLSHandshakeTimeout(t *testing.T) {
	const timeout = 200 * time.Millisecond
	// limit bounds every wait of the test; without a handshake timeout
	// the handshake would wait forever.
	const limit = 5 * time.Second
	tests := []struct {
		name   string
		icy    bool
		cancel bool // the client goes away during the handshake
	}{
		{"handshake timeout fails the request", false, false},
		{"handshake timeout fails the ICY request", true, false},
		{"client gone first", false, true},
		{"client gone first, ICY", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = ln.Close() }()
			accepted := make(chan net.Conn, 1)
			go func() {
				if c, err := ln.Accept(); err == nil {
					accepted <- c
				}
			}()
			up := testUpstream
			up.ConnectTimeout = timeout
			// The fake clock never fires header_timeout: only the
			// handshake timeout or the client can end the request.
			r := newRoute("gr", fixtureNet(ln.Addr().String(), fixtureHosts), Options{Upstream: up, Clock: newFakeClock()})

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			res := make(chan error, 1)
			go func() {
				resp, err := r.Fetch(ctx, Request{URL: "https://cdn.example.com/live.m3u8", ICY: tt.icy})
				if resp != nil {
					_ = resp.Body.Close()
				}
				res <- err
			}()
			var c net.Conn
			select {
			case c = <-accepted:
			case <-time.After(limit):
				t.Fatal("no connection to the upstream")
			}
			defer func() { _ = c.Close() }()
			start := time.Now()
			if tt.cancel {
				cancel()
			}

			select {
			case err = <-res:
			case <-time.After(limit):
				t.Fatalf("Fetch still waits for the handshake after %v", limit)
			}
			switch {
			case tt.cancel:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("Fetch error = %v, want context.Canceled", err)
				}
			default:
				if err == nil {
					t.Fatal("Fetch succeeded without a handshake")
				}
				if got := Classify(err); got != ReasonTLS {
					t.Errorf("Classify(%v) = %q, want %q", err, got, ReasonTLS)
				}
				if got := ErrorStatus(err); got != http.StatusBadGateway {
					t.Errorf("ErrorStatus = %d, want 502", got)
				}
			}

			// The upstream side sees the connection closed: the
			// ClientHello, then EOF (or a reset).
			_ = c.SetReadDeadline(time.Now().Add(limit))
			_, err = io.Copy(io.Discard, c)
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				t.Fatalf("upstream connection still open %v after the request", limit)
			}
			if d := time.Since(start); d > timeout+2*time.Second {
				t.Errorf("connection closed after %v, want about connect_timeout %v", d, timeout)
			}
		})
	}
}
