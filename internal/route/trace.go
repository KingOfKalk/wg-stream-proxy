// SPDX-License-Identifier: AGPL-3.0-or-later

package route

import (
	"context"
	"crypto/tls"
	"net/http/httptrace"
	"net/url"
	"sync"
	"time"
)

// Trace collects where the time of a request went, for the debug log
// (spec 8). Pass a new Trace in Request.Trace; without one Fetch records
// nothing and costs nothing extra. A Trace may be used for more than one
// Fetch of the same client request (a refetch): durations add up, the
// first-byte time counts from the first Fetch.
//
// Durations are measured with the wall clock, not the route's Clock:
// they are reported, never acted on. They are best effort: a connection
// the transport dials for one request may serve another.
//
// A nil *Trace is valid and records nothing.
type Trace struct {
	mu       sync.Mutex
	start    time.Time
	tlsStart time.Time
	p        Phases
}

// Phases are the durations a Trace recorded.
type Phases struct {
	// NetworkStarted: a lookup or dial waited for the route's network to
	// start (a tunnel start); NetworkStart is the time it waited.
	NetworkStarted bool
	NetworkStart   time.Duration
	// DNS is the time of host lookups, Connect the time of connection
	// setup including the Happy-Eyeballs fallback, both without
	// NetworkStart. ConnectAttempts counts the addresses tried.
	DNS             time.Duration
	Connect         time.Duration
	ConnectAttempts int
	// TLS is the time of TLS handshakes.
	TLS time.Duration
	// FirstByte is the time from the start of Fetch to the first byte of
	// the last upstream response; 0 without response.
	FirstByte time.Duration
	// Reused: the last upstream request used a pooled connection.
	Reused bool
	// Redirects counts the redirects followed.
	Redirects int
	// URL is the last upstream URL tried, reduced by LogURL.
	URL string
}

// NetworkWait records that a lookup or dial of the request waited d for
// the route's network to start. Networks report it through their own
// hook (tunnel.WithStartTrace); the time is not counted as DNS or
// connect.
func (t *Trace) NetworkWait(d time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.p.NetworkStarted = true
	t.p.NetworkStart += d
}

// Phases returns the durations recorded so far.
func (t *Trace) Phases() Phases {
	if t == nil {
		return Phases{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.p
}

type traceKey struct{}

// begin starts the trace of a Fetch and returns ctx with it, for the
// route's dial and the transport (httptrace).
func (t *Trace) begin(ctx context.Context) context.Context {
	t.mu.Lock()
	if t.start.IsZero() {
		t.start = time.Now()
	}
	t.mu.Unlock()
	ctx = context.WithValue(ctx, traceKey{}, t)
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.p.Reused = info.Reused
		},
		TLSHandshakeStart: func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.tlsStart = time.Now()
		},
		TLSHandshakeDone: func(tls.ConnectionState, error) {
			t.mu.Lock()
			defer t.mu.Unlock()
			if !t.tlsStart.IsZero() {
				t.p.TLS += time.Since(t.tlsStart)
				t.tlsStart = time.Time{}
			}
		},
		GotFirstResponseByte: func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.p.FirstByte = time.Since(t.start)
		},
	})
}

// traceFrom returns the Trace of a Fetch context, or nil.
func traceFrom(ctx context.Context) *Trace {
	t, _ := ctx.Value(traceKey{}).(*Trace)
	return t
}

// traceMark is the start of a dial phase: the time and the network wait
// recorded so far.
type traceMark struct {
	at   time.Time
	wait time.Duration
}

// mark returns the start of a dial phase.
func (t *Trace) mark() traceMark {
	if t == nil {
		return traceMark{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return traceMark{at: time.Now(), wait: t.p.NetworkStart}
}

// since returns the time since m without the network wait recorded since
// m. t.mu is held.
func (t *Trace) since(m traceMark) time.Duration {
	return max(0, time.Since(m.at)-(t.p.NetworkStart-m.wait))
}

// lookupDone adds the lookup that started at m.
func (t *Trace) lookupDone(m traceMark) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.p.DNS += t.since(m)
}

// connectDone adds the connection setup that started at m with attempts
// addresses tried.
func (t *Trace) connectDone(m traceMark, attempts int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.p.Connect += t.since(m)
	t.p.ConnectAttempts += attempts
}

// redirected counts a redirect.
func (t *Trace) redirected() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.p.Redirects++
}

// setURL records u, reduced by LogURL, as the last URL tried.
func (t *Trace) setURL(u *url.URL) {
	if t == nil {
		return
	}
	s := LogURL(u.String())
	t.mu.Lock()
	defer t.mu.Unlock()
	t.p.URL = s
}
