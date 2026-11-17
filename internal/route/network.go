// SPDX-License-Identifier: AGPL-3.0-or-later

package route

import (
	"context"
	"net"
	"net/netip"
)

// Network is one route's way to the internet: a tunnel or the host.
//
// Implementations must return promptly once ctx is done, in both methods:
// the route cancels losing connection attempts and enforces
// connect_timeout through ctx.
type Network interface {
	// LookupNetIP resolves host inside this network (tunnel DNS for tunnels).
	LookupNetIP(ctx context.Context, host string) ([]netip.Addr, error)
	// DialContext connects to an already resolved "ip:port" (network "tcp", "tcp4", "tcp6").
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Host returns the host network used by the direct route: the host
// resolver and a plain net.Dialer.
func Host() Network { return hostNetwork{} }

type hostNetwork struct{}

func (hostNetwork) LookupNetIP(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

func (hostNetwork) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, address)
}
