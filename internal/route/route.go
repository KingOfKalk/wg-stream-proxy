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
	transport *http.Transport
}

// newRoute builds the route name over network n.
func newRoute(name string, n Network, opts Options) *Route {
	r := &Route{
		name:     name,
		network:  n,
		guard:    NewGuard(opts.AllowPrivate[name]),
		upstream: opts.Upstream,
		clock:    opts.Clock,
	}
	if r.clock == nil {
		r.clock = SystemClock()
	}
	r.transport = newTransport(r.dial)
	return r
}

// newTransport returns a transport with the settings of spec 2.1 that
// connects only through dial. A route can hold more than one (e.g. one
// whose dial wraps connections for raw streams) without sharing pooled
// connections.
func newTransport(dial func(ctx context.Context, network, address string) (net.Conn, error)) *http.Transport {
	return &http.Transport{
		Proxy:               nil, // never an environment proxy: it would bypass the route
		DialContext:         dial,
		ForceAttemptHTTP2:   true,
		MaxIdleConnsPerHost: maxIdleConnsPerHost,
		IdleConnTimeout:     idleConnTimeout,
	}
}

// Name returns the tunnel name or config.RouteDirect.
func (r *Route) Name() string { return r.name }

// CloseIdleConnections closes the route's idle upstream connections.
func (r *Route) CloseIdleConnections() { r.transport.CloseIdleConnections() }
