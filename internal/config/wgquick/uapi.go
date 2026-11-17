// SPDX-License-Identifier: AGPL-3.0-or-later

package wgquick

import (
	"fmt"
	"net/netip"
	"strings"
)

// UAPIConfig returns the configuration text for wireguard-go's IpcSet:
// private key, the single peer with its endpoint and allowed IPs. Keys are
// hex encoded. endpoint is the resolved IP of Peer.Endpoint.Host (spec
// 2.1: the host resolver runs when the tunnel starts); the port comes from
// the tunnel file. Address, DNS and MTU configure the netstack, not the
// device, and are not part of the text.
//
// The result contains the keys: never log it.
func (t *Tunnel) UAPIConfig(endpoint netip.Addr) (string, error) {
	if err := t.validate(); err != nil {
		return "", err
	}
	if !endpoint.IsValid() || endpoint.Zone() != "" {
		return "", &Error{Tunnel: t.Name, Msg: "invalid endpoint address"}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "private_key=%s\n", t.PrivateKey.hex())
	b.WriteString("replace_peers=true\n")
	fmt.Fprintf(&b, "public_key=%s\n", t.Peer.PublicKey.hex())
	if !t.Peer.PresharedKey.IsZero() {
		fmt.Fprintf(&b, "preshared_key=%s\n", t.Peer.PresharedKey.hex())
	}
	fmt.Fprintf(&b, "endpoint=%s\n", netip.AddrPortFrom(endpoint.Unmap(), t.Peer.Endpoint.Port))
	fmt.Fprintf(&b, "persistent_keepalive_interval=%d\n", t.Peer.PersistentKeepalive)
	b.WriteString("replace_allowed_ips=true\n")
	for _, p := range t.Peer.AllowedIPs {
		fmt.Fprintf(&b, "allowed_ip=%s\n", p)
	}
	return b.String(), nil
}

// validate checks the invariants Parse guarantees, so a Tunnel built by
// hand cannot produce an invalid device configuration.
func (t *Tunnel) validate() error {
	invalid := func(what string) error {
		return &Error{Tunnel: t.Name, Msg: "invalid " + what}
	}
	switch {
	case t.PrivateKey.IsZero():
		return invalid("PrivateKey")
	case t.Peer.PublicKey.IsZero():
		return invalid("PublicKey")
	case len(t.Addresses) == 0 || !allPrefixes(t.Addresses, false):
		return invalid("Address")
	case !allAddrs(t.DNS):
		return invalid("DNS")
	case t.MTU < minMTU || t.MTU > maxMTU:
		return invalid("MTU")
	case t.Peer.Endpoint.Port == 0 || (t.Peer.Endpoint.Host == "" || !validEndpointHost(t.Peer.Endpoint.Host)):
		return invalid("Endpoint")
	case len(t.Peer.AllowedIPs) == 0 || !allPrefixes(t.Peer.AllowedIPs, true):
		return invalid("AllowedIPs")
	}
	return nil
}

func allPrefixes(ps []netip.Prefix, masked bool) bool {
	for _, p := range ps {
		if !p.IsValid() || p.Addr().Is4In6() || (masked && p != p.Masked()) {
			return false
		}
	}
	return true
}

func allAddrs(as []netip.Addr) bool {
	for _, a := range as {
		if !a.IsValid() || a.Zone() != "" || a.Is4In6() {
			return false
		}
	}
	return true
}

func validEndpointHost(host string) bool {
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.Zone() == "" && !addr.Is4In6()
	}
	return validHostname(host)
}
