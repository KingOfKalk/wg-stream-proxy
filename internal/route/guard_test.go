// SPDX-License-Identifier: AGPL-3.0-or-later

package route

import (
	"errors"
	"net/netip"
	"testing"
)

func TestGuardBlockedRanges(t *testing.T) {
	tests := []struct {
		ip      string
		blocked bool
	}{
		// Loopback.
		{"127.0.0.1", true},
		{"127.255.255.254", true},
		{"::1", true},
		// RFC 1918.
		{"10.0.0.1", true},
		{"10.255.255.255", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"172.15.255.255", false},
		{"172.32.0.0", false},
		{"192.168.1.10", true},
		// RFC 6598.
		{"100.64.0.1", true},
		{"100.127.255.255", true},
		{"100.63.255.255", false},
		{"100.128.0.0", false},
		// Link-local.
		{"169.254.169.254", true},
		{"fe80::1", true},
		{"fe80::1%eth0", true},
		// IPv6 ULA.
		{"fc00::1", true},
		{"fd12:3456::1", true},
		// Unspecified ("this network").
		{"0.0.0.0", true},
		{"0.1.2.3", true},
		{"::", true},
		// Multicast.
		{"224.0.0.1", true},
		{"239.255.255.250", true},
		{"ff02::1", true},
		{"ff0e::1", true},
		// IPv4-mapped forms.
		{"::ffff:127.0.0.1", true},
		{"::ffff:10.1.2.3", true},
		{"::ffff:100.64.0.1", true},
		{"::ffff:169.254.0.1", true},
		{"::ffff:0.0.0.0", true},
		{"::ffff:224.0.0.1", true},
		{"::ffff:203.0.113.7", false},
		// Public addresses (documentation ranges stand in for them).
		{"203.0.113.7", false},
		{"198.51.100.1", false},
		{"192.0.2.1", false},
		{"8.8.8.8", false},
		{"2001:db8::1", false},
		{"2606:4700::1", false},
	}
	g := NewGuard(nil)
	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			ip := netip.MustParseAddr(tt.ip)
			err := g.Check(ip)
			if got := err != nil; got != tt.blocked {
				t.Fatalf("Check(%s) = %v, want blocked %v", tt.ip, err, tt.blocked)
			}
			if err != nil && !errors.Is(err, ErrBlocked) {
				t.Errorf("Check(%s) error %v does not wrap ErrBlocked", tt.ip, err)
			}
		})
	}
}

func TestGuardInvalidAddrBlocked(t *testing.T) {
	if err := NewGuard(nil).Check(netip.Addr{}); !errors.Is(err, ErrBlocked) {
		t.Fatalf("Check(invalid) = %v, want ErrBlocked", err)
	}
}

func TestGuardAllowlist(t *testing.T) {
	g := NewGuard([]netip.Prefix{
		netip.MustParsePrefix("192.168.1.0/24"),
		netip.MustParsePrefix("fd00:1::/64"),
	})
	tests := []struct {
		ip      string
		blocked bool
	}{
		{"192.168.1.20", false},
		{"::ffff:192.168.1.20", false},
		{"192.168.2.20", true},
		{"10.8.0.1", true},
		{"fd00:1::5", false},
		{"fd00:2::5", true},
		{"203.0.113.7", false},
	}
	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			err := g.Check(netip.MustParseAddr(tt.ip))
			if got := err != nil; got != tt.blocked {
				t.Fatalf("Check(%s) = %v, want blocked %v", tt.ip, err, tt.blocked)
			}
		})
	}
}

func TestGuardDoesNotKeepCallerSlice(t *testing.T) {
	allow := []netip.Prefix{netip.MustParsePrefix("10.8.0.0/24")}
	g := NewGuard(allow)
	allow[0] = netip.MustParsePrefix("192.168.0.0/16")
	if err := g.Check(netip.MustParseAddr("10.8.0.1")); err != nil {
		t.Errorf("Check after caller changed its slice = %v, want allowed", err)
	}
}
