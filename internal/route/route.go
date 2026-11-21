// SPDX-License-Identifier: AGPL-3.0-or-later

package route

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
)

// Transport settings of every route (spec 2.1).
const (
	maxIdleConnsPerHost = 16
	idleConnTimeout     = 90 * time.Second
	// h2SendPingTimeout and h2PingTimeout are the HTTP/2 health check: a
	// connection on which nothing arrived for h2SendPingTimeout gets a
	// ping and is closed if no answer comes within h2PingTimeout, so a
	// silently dead connection does not keep taking requests.
	h2SendPingTimeout = 15 * time.Second
	h2PingTimeout     = 10 * time.Second
)

// Options configure the routes of a Set.
type Options struct {
	// Upstream holds the [upstream] settings: User-Agent and timeouts.
	Upstream config.Upstream
	// AllowPrivate holds the private-range guard exceptions per route
	// name ([upstream.allow_private]). Tests add their fixture ranges.
	AllowPrivate map[string][]netip.Prefix
	// Clock schedules all timeouts; nil means SystemClock.
	Clock Clock
	// Activity counts the requests in flight per route name, normally
	// the tunnel of the route. Routes without one count nothing.
	Activity map[string]Activity
}

// Route is one way to the upstreams: a tunnel or direct. It owns an
// http.Transport whose every connection is resolved and dialed through
// the route's Network and checked by its Guard.
type Route struct {
	name      string
	network   Network
	guard     Guard
	upstream  config.Upstream
	clock     Clock
	activity  Activity
	transport *http.Transport
	// icyTransport serves requests with Request.ICY: its connections
	// accept a Shoutcast v1 "ICY" status line (spec 3.3). It has its own
	// pool, so connections of the normal transport are never wrapped. It
	// keeps plain HTTP connections alive: the wrapper checks every
	// response of such a connection, and a connection that carried an ICY
	// answer takes no further request (icyConn).
	icyTransport *http.Transport
}

// newRoute builds the route name over network n.
func newRoute(name string, n Network, opts Options) *Route {
	r := &Route{
		name:     name,
		network:  n,
		guard:    NewGuard(opts.AllowPrivate[name]),
		upstream: opts.Upstream,
		clock:    opts.Clock,
		activity: opts.Activity[name],
	}
	if r.clock == nil {
		r.clock = SystemClock()
	}
	r.transport = newTransport(r.dial, r.upstream.ConnectTimeout)
	r.icyTransport = newTransport(r.dialICY, r.upstream.ConnectTimeout)
	return r
}

// newTransport returns a transport with the settings of spec 2.1 that
// connects only through dial. A route can hold more than one (e.g. one
// whose dial wraps connections for raw streams) without sharing pooled
// connections.
//
// handshakeTimeout (connect_timeout) bounds the TLS handshake. The
// transport dials with a context detached from the request, so without
// it a handshake the upstream never answers would hold its goroutine and
// connection after the request ended (spec 3.4).
func newTransport(dial func(ctx context.Context, network, address string) (net.Conn, error), handshakeTimeout time.Duration) *http.Transport {
	return &http.Transport{
		Proxy:               nil, // never an environment proxy: it would bypass the route
		DialContext:         dial,
		TLSHandshakeTimeout: handshakeTimeout,
		ForceAttemptHTTP2:   true,
		HTTP2: &http.HTTP2Config{
			SendPingTimeout: h2SendPingTimeout,
			PingTimeout:     h2PingTimeout,
		},
		MaxIdleConnsPerHost: maxIdleConnsPerHost,
		IdleConnTimeout:     idleConnTimeout,
	}
}

// Name returns the tunnel name or config.RouteDirect.
func (r *Route) Name() string { return r.name }

// CloseIdleConnections closes the route's idle upstream connections, in
// both transports.
func (r *Route) CloseIdleConnections() {
	r.transport.CloseIdleConnections()
	r.icyTransport.CloseIdleConnections()
}
