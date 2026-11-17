// SPDX-License-Identifier: AGPL-3.0-or-later

package route

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
)

// ErrBlocked is returned when the private-range guard rejects an address
// (spec 3.5).
var ErrBlocked = errors.New("route: private address blocked")

// blockedPrefixes are the IPv4 ranges the netip helpers do not cover.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),     // "this network" (RFC 1122), includes 0.0.0.0
	netip.MustParsePrefix("100.64.0.0/10"), // RFC 6598 shared address space
}

// Guard is the private-range guard of one route: it blocks loopback,
// RFC 1918, RFC 6598, link-local, IPv6 ULA, unspecified and multicast
// addresses, including their IPv4-mapped forms, unless an allowlist
// prefix of the route contains the address (spec 3.5).
type Guard struct {
	allow []netip.Prefix
}

// NewGuard returns a guard with the given per-route exceptions
// ([upstream.allow_private]).
func NewGuard(allow []netip.Prefix) Guard {
	return Guard{allow: slices.Clone(allow)}
}

// Check returns nil if ip may be dialed, else an error wrapping ErrBlocked.
func (g Guard) Check(ip netip.Addr) error {
	// Zoned addresses never match a netip.Prefix, so compare without zone.
	a := ip.WithZone("").Unmap()
	if !blocked(a) {
		return nil
	}
	for _, p := range g.allow {
		if p.Contains(a) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrBlocked, ip)
}

// blocked reports whether an unmapped address without zone is in one of
// the guarded ranges. Invalid addresses are blocked.
func blocked(a netip.Addr) bool {
	if !a.IsValid() || a.IsUnspecified() || a.IsLoopback() || a.IsPrivate() ||
		a.IsLinkLocalUnicast() || a.IsMulticast() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
