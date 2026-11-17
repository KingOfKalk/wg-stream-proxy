// SPDX-License-Identifier: AGPL-3.0-or-later

// Package wgquick parses wg-quick tunnel files (spec 5.5) into a validated
// model and converts it to the UAPI configuration text of wireguard-go.
//
// Keys never appear in errors, warnings, fmt or slog output; see Key.
package wgquick

import (
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"unicode"
)

const (
	// DefaultMTU applies when a tunnel file has no MTU.
	DefaultMTU = 1420
	// minMTU is the IPv6 minimum link MTU.
	minMTU = 1280
	maxMTU = 65535
	// maxFileSize bounds a tunnel file; real ones are a few hundred bytes.
	maxFileSize = 64 << 10
)

// Tunnel is one parsed and validated tunnel file.
type Tunnel struct {
	// Name is the tunnel name (file stem), as passed to Parse.
	Name       string
	PrivateKey Key
	// Addresses are the interface addresses in file order, IPv4 and IPv6,
	// host bits kept; IPv4-mapped IPv6 addresses are unmapped.
	Addresses []netip.Prefix
	// DNS are the resolver addresses used inside the tunnel.
	DNS  []netip.Addr
	MTU  int
	Peer Peer
}

// Peer is the single [Peer] of a tunnel file.
type Peer struct {
	PublicKey Key
	// PresharedKey is zero when the file has none.
	PresharedKey Key
	Endpoint     Endpoint
	// AllowedIPs are masked prefixes in file order. Upstream IPs outside
	// them are unreachable through this tunnel (fail closed).
	AllowedIPs []netip.Prefix
	// PersistentKeepalive in seconds, 0 = off.
	PersistentKeepalive uint16
}

// Endpoint is the peer endpoint. Host is a host name or a canonical IP
// literal; host names are resolved by the host resolver at tunnel start
// (spec 2.1).
type Endpoint struct {
	Host string
	Port uint16
}

// String returns host:port, with brackets around IPv6 literals.
func (e Endpoint) String() string {
	return net.JoinHostPort(e.Host, strconv.Itoa(int(e.Port)))
}

// Error is a config error in a tunnel file. Line is 1-based; 0 means the
// file as a whole. Msg never contains values from the file.
type Error struct {
	Tunnel string
	Line   int
	Msg    string
}

func (e *Error) Error() string {
	return location(e.Tunnel, e.Line) + e.Msg
}

// Warning is a non-fatal finding in a tunnel file, with the same fields as
// Error.
type Warning struct {
	Tunnel string
	Line   int
	Msg    string
}

func (w Warning) String() string {
	return location(w.Tunnel, w.Line) + w.Msg
}

func location(tunnel string, line int) string {
	if line > 0 {
		return fmt.Sprintf("tunnel %q: line %d: ", tunnel, line)
	}
	return fmt.Sprintf("tunnel %q: ", tunnel)
}

// HasIPv4 reports whether the tunnel can carry IPv4: it has an IPv4
// address and an IPv4 AllowedIPs prefix.
func (t *Tunnel) HasIPv4() bool {
	return t.hasFamily(netip.Addr.Is4)
}

// HasIPv6 reports whether the tunnel can carry IPv6: it has an IPv6
// address and an IPv6 AllowedIPs prefix. It says nothing about whether
// the upstream network routes IPv6.
func (t *Tunnel) HasIPv6() bool {
	return t.hasFamily(netip.Addr.Is6)
}

func (t *Tunnel) hasFamily(is func(netip.Addr) bool) bool {
	return anyPrefix(t.Addresses, is) && anyPrefix(t.Peer.AllowedIPs, is)
}

func anyPrefix(ps []netip.Prefix, is func(netip.Addr) bool) bool {
	for _, p := range ps {
		if is(p.Addr()) {
			return true
		}
	}
	return false
}

// Allows reports whether addr lies within the peer's AllowedIPs.
func (t *Tunnel) Allows(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, p := range t.Peer.AllowedIPs {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// section is the part of the file a line belongs to.
type section int

const (
	sectionNone section = iota
	sectionInterface
	sectionPeer
)

func (s section) String() string {
	if s == sectionPeer {
		return "[Peer]"
	}
	return "[Interface]"
}

// Key names per section, lowercase to canonical. Keys and section names
// are case-insensitive, like in wg(8).
var keys = map[section]map[string]string{
	sectionInterface: canonical("PrivateKey", "Address", "DNS", "MTU",
		"ListenPort", "Table", "SaveConfig", "PreUp", "PostUp", "PreDown", "PostDown"),
	sectionPeer: canonical("PublicKey", "PresharedKey", "Endpoint", "AllowedIPs", "PersistentKeepalive"),
}

func canonical(names ...string) map[string]string {
	m := make(map[string]string, len(names))
	for _, n := range names {
		m[strings.ToLower(n)] = n
	}
	return m
}

type parser struct {
	t          *Tunnel
	warns      []Warning
	sec        section
	interfaces int
	peers      int
	seen       map[string]bool
}

// Parse reads one wg-quick tunnel file and returns the validated tunnel.
// name is the tunnel name (file stem); its format is checked by the caller
// (spec 5.2). On error the tunnel and warnings are nil. Errors are *Error,
// except read errors from r.
//
// Format details beyond wg-quick(8): '#' starts a comment anywhere in a
// line; keys and section names are case-insensitive; Address, DNS and
// AllowedIPs may repeat and take values separated by commas and/or
// whitespace; every other key may appear once; unknown keys and sections
// are errors.
func Parse(name string, r io.Reader) (*Tunnel, []Warning, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxFileSize+1))
	if err != nil {
		return nil, nil, fmt.Errorf("tunnel %q: read: %w", name, err)
	}
	if len(data) > maxFileSize {
		return nil, nil, &Error{Tunnel: name, Msg: fmt.Sprintf("file larger than %d bytes", maxFileSize)}
	}
	p := &parser{t: &Tunnel{Name: name}, seen: map[string]bool{}}
	text := strings.TrimPrefix(string(data), "\ufeff")
	for i, line := range strings.Split(text, "\n") {
		if err := p.line(i+1, line); err != nil {
			return nil, nil, err
		}
	}
	if err := p.finish(); err != nil {
		return nil, nil, err
	}
	return p.t, p.warns, nil
}

func (p *parser) errorf(line int, format string, args ...any) error {
	return &Error{Tunnel: p.t.Name, Line: line, Msg: fmt.Sprintf(format, args...)}
}

func (p *parser) warn(line int, msg string) {
	p.warns = append(p.warns, Warning{Tunnel: p.t.Name, Line: line, Msg: msg})
}

func (p *parser) line(n int, line string) error {
	if i := strings.IndexByte(line, '#'); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	switch {
	case line == "":
		return nil
	case line[0] == '[':
		return p.header(n, line)
	}
	key, value, ok := strings.Cut(line, "=")
	key, value = strings.TrimSpace(key), strings.TrimSpace(value)
	if !ok || key == "" {
		return p.errorf(n, "expected key = value")
	}
	if p.sec == sectionNone {
		return p.errorf(n, "key outside of a section")
	}
	name, ok := keys[p.sec][strings.ToLower(key)]
	if !ok {
		if echoable(key) {
			return p.errorf(n, "unknown key %s in %s", key, p.sec)
		}
		return p.errorf(n, "unknown key in %s", p.sec)
	}
	return p.keyValue(n, name, value)
}

func (p *parser) header(n int, line string) error {
	if !strings.HasSuffix(line, "]") {
		return p.errorf(n, "invalid section header")
	}
	sec := strings.TrimSpace(line[1 : len(line)-1])
	switch strings.ToLower(sec) {
	case "interface":
		p.interfaces++
		if p.interfaces > 1 {
			return p.errorf(n, "more than one [Interface]")
		}
		p.sec = sectionInterface
	case "peer":
		p.peers++
		if p.peers > 1 {
			return p.errorf(n, "more than one [Peer]")
		}
		p.sec = sectionPeer
	default:
		if echoable(sec) {
			return p.errorf(n, "unknown section [%s]", sec)
		}
		return p.errorf(n, "unknown section")
	}
	return nil
}

// echoable reports whether a name from the file may appear in messages:
// short and letters only, so it cannot carry a key.
func echoable(s string) bool {
	if s == "" || len(s) > 24 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}

func (p *parser) keyValue(n int, name, value string) error {
	switch name {
	case "PreUp", "PostUp", "PreDown", "PostDown":
		return p.errorf(n, "%s not supported", name)
	case "ListenPort", "Table", "SaveConfig":
		p.warn(n, name+" ignored")
		return nil
	}
	if value == "" {
		return p.errorf(n, "empty value for %s", name)
	}
	switch name {
	case "Address", "DNS", "AllowedIPs":
		return p.list(n, name, value)
	}
	if p.seen[name] {
		return p.errorf(n, "duplicate %s", name)
	}
	p.seen[name] = true
	var ok bool
	switch name {
	case "PrivateKey":
		p.t.PrivateKey, ok = parseNonZeroKey(value)
	case "MTU":
		p.t.MTU, ok = parseMTU(value)
	case "PublicKey":
		p.t.Peer.PublicKey, ok = parseNonZeroKey(value)
	case "PresharedKey":
		k, err := parseKey(value)
		p.t.Peer.PresharedKey, ok = k, err == nil
	case "Endpoint":
		p.t.Peer.Endpoint, ok = parseEndpoint(value)
	case "PersistentKeepalive":
		p.t.Peer.PersistentKeepalive, ok = parseKeepalive(value)
	}
	if !ok {
		return p.errorf(n, "invalid %s", name)
	}
	return nil
}

func (p *parser) list(n int, name, value string) error {
	items := strings.FieldsFunc(value, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	if len(items) == 0 {
		return p.errorf(n, "empty value for %s", name)
	}
	for _, item := range items {
		switch name {
		case "Address":
			pfx, ok := parsePrefix(item)
			if !ok {
				return p.errorf(n, "invalid Address")
			}
			p.t.Addresses = append(p.t.Addresses, pfx)
		case "AllowedIPs":
			pfx, ok := parsePrefix(item)
			if !ok {
				return p.errorf(n, "invalid AllowedIPs")
			}
			p.t.Peer.AllowedIPs = append(p.t.Peer.AllowedIPs, pfx.Masked())
		case "DNS":
			addr, err := netip.ParseAddr(item)
			if err != nil {
				// wg-quick treats non-IP entries as search domains, which
				// have no meaning for the proxy.
				p.warn(n, "DNS search domain ignored")
				continue
			}
			if addr.Zone() != "" {
				return p.errorf(n, "invalid DNS")
			}
			p.t.DNS = append(p.t.DNS, addr.Unmap())
		}
	}
	return nil
}

func (p *parser) finish() error {
	missing := func(what string) error {
		return &Error{Tunnel: p.t.Name, Msg: "missing " + what}
	}
	switch {
	case p.interfaces == 0:
		return missing("[Interface]")
	case p.peers == 0:
		return missing("[Peer]")
	case !p.seen["PrivateKey"]:
		return missing("PrivateKey")
	case len(p.t.Addresses) == 0:
		return missing("Address")
	case !p.seen["PublicKey"]:
		return missing("PublicKey")
	case !p.seen["Endpoint"]:
		return missing("Endpoint")
	case len(p.t.Peer.AllowedIPs) == 0:
		return missing("AllowedIPs")
	}
	if !p.seen["MTU"] {
		p.t.MTU = DefaultMTU
	}
	if len(p.t.DNS) == 0 {
		p.warn(0, "no DNS server: upstream host names cannot be resolved")
	}
	return nil
}

func parseNonZeroKey(s string) (Key, bool) {
	k, err := parseKey(s)
	if err != nil || k.IsZero() {
		return Key{}, false
	}
	return k, true
}

func parseMTU(s string) (int, bool) {
	mtu, err := strconv.Atoi(s)
	if err != nil || mtu < minMTU || mtu > maxMTU {
		return 0, false
	}
	return mtu, true
}

func parseKeepalive(s string) (uint16, bool) {
	if strings.EqualFold(s, "off") {
		return 0, true
	}
	v, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return 0, false
	}
	return uint16(v), true
}

// parsePrefix accepts a prefix or a bare IP (a host prefix). Zones are
// rejected; IPv4-mapped IPv6 forms are unmapped.
func parsePrefix(s string) (netip.Prefix, bool) {
	if !strings.Contains(s, "/") {
		addr, err := netip.ParseAddr(s)
		if err != nil || addr.Zone() != "" {
			return netip.Prefix{}, false
		}
		addr = addr.Unmap()
		return netip.PrefixFrom(addr, addr.BitLen()), true
	}
	pfx, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, false
	}
	if pfx.Addr().Is4In6() && pfx.Bits() >= 96 {
		pfx = netip.PrefixFrom(pfx.Addr().Unmap(), pfx.Bits()-96)
	}
	return pfx, true
}

func parseEndpoint(s string) (Endpoint, bool) {
	host, portStr, err := net.SplitHostPort(s)
	if err != nil || host == "" {
		return Endpoint{}, false
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || port == 0 {
		return Endpoint{}, false
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.Zone() != "" {
			return Endpoint{}, false
		}
		host = addr.Unmap().String()
	} else if !validHostname(host) {
		return Endpoint{}, false
	}
	return Endpoint{Host: host, Port: uint16(port)}, true
}

// validHostname checks the characters and length of a DNS name; the host
// resolver decides the rest.
func validHostname(s string) bool {
	if len(s) > 253 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '.' {
			return false
		}
	}
	return true
}
