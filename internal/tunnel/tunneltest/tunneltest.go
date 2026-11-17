// SPDX-License-Identifier: AGPL-3.0-or-later

// Package tunneltest runs an in-process WireGuard server peer for tests
// (testing.md, principles): a second wireguard-go device with a gVisor
// netstack, connected to the device under test over loopback UDP, with an
// HTTP server and a DNS server (UDP and TCP) listening on its tunnel
// addresses. The DNS server can lose, truncate or fail queries on demand.
// No root, no kernel interfaces, no real keys: every Peer generates its
// own test-only keys and uses documentation address ranges.
package tunneltest

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config/wgquick"
)

// Default addresses, from the documentation ranges (RFC 5737, RFC 3849).
var (
	// ClientAddr4 is the client's default tunnel address.
	ClientAddr4 = netip.MustParseAddr("192.0.2.2")
	// ClientAddr6 is a client IPv6 tunnel address for dual-stack tests.
	ClientAddr6 = netip.MustParseAddr("2001:db8::2")
	// ServerAddr4 is the peer's default tunnel address: HTTP and DNS
	// listen here, and it is the client's DNS server.
	ServerAddr4 = netip.MustParseAddr("192.0.2.1")
	// ServerAddr6 is a peer IPv6 tunnel address for dual-stack tests.
	ServerAddr6 = netip.MustParseAddr("2001:db8::1")
)

// Options configure a Peer. The zero value gives an IPv4-only peer at
// ServerAddr4 and a client at ClientAddr4 with AllowedIPs 0.0.0.0/0, ::/0.
type Options struct {
	// ClientAddresses are the client's Address entries. Default
	// ClientAddr4/32.
	ClientAddresses []netip.Prefix
	// AllowedIPs are the client's AllowedIPs. Default 0.0.0.0/0, ::/0.
	AllowedIPs []netip.Prefix
	// ServerAddresses are the peer's tunnel addresses. Default
	// ServerAddr4. The peer only accepts client packets of the families it
	// has an address for, so an IPv4-only peer behaves like a provider
	// without IPv6: IPv6 packets from the client vanish.
	ServerAddresses []netip.Addr
	// NoDNS omits the DNS entry from the client file. By default the
	// client's DNS server is the first server address.
	NoDNS bool
	// DNS overrides the client's DNS servers. Addresses that are not
	// server addresses swallow every query, like a dead DNS server.
	DNS []netip.Addr
	// Handler serves HTTP on port 80 of every server address. Default:
	// EchoRemoteAddr.
	Handler http.Handler
}

// Peer is a running server peer.
type Peer struct {
	opts       Options
	dev        *device.Device
	net        *netstack.Net
	bind       *filterBind
	clientPriv [32]byte
	serverPub  [32]byte
	psk        [32]byte

	httpSrv *http.Server
	udp     []net.PacketConn
	tcp     []net.Listener
	wg      sync.WaitGroup

	mu         sync.Mutex
	closed     bool
	conns      map[net.Conn]struct{}
	records    map[string][]netip.Addr
	cnames     map[string]string
	queries    []string
	tcpQueries []string
	dropNext   int // UDP queries to drop; < 0 drops all
	dropped    int
	failNext   int // queries to answer with failRCode; < 0 fails all
	failRCode  dnsmessage.RCode
	truncate   bool

	closeOnce sync.Once
}

// EchoRemoteAddr answers every request with the request's RemoteAddr
// ("ip:port") as seen by the peer: the client's tunnel source address.
var EchoRemoteAddr http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	_, _ = io.WriteString(w, r.RemoteAddr)
})

// Start starts a peer and registers its Close with tb.Cleanup.
func Start(tb testing.TB, opts Options) *Peer {
	tb.Helper()
	p, err := start(opts)
	if err != nil {
		tb.Fatalf("tunneltest: %v", err)
	}
	tb.Cleanup(p.Close)
	return p
}

func start(opts Options) (*Peer, error) {
	if len(opts.ClientAddresses) == 0 {
		opts.ClientAddresses = []netip.Prefix{netip.PrefixFrom(ClientAddr4, 32)}
	}
	if len(opts.AllowedIPs) == 0 {
		opts.AllowedIPs = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}
	}
	if len(opts.ServerAddresses) == 0 {
		opts.ServerAddresses = []netip.Addr{ServerAddr4}
	}
	if opts.Handler == nil {
		opts.Handler = EchoRemoteAddr
	}
	p := &Peer{opts: opts, records: map[string][]netip.Addr{}, cnames: map[string]string{}, conns: map[net.Conn]struct{}{}}
	serverPriv, serverPub := newKeyPair()
	clientPriv, clientPub := newKeyPair()
	p.clientPriv, p.serverPub = clientPriv, serverPub
	if _, err := rand.Read(p.psk[:]); err != nil {
		return nil, err
	}

	tun, tnet, err := netstack.CreateNetTUN(opts.ServerAddresses, nil, wgquick.DefaultMTU)
	if err != nil {
		return nil, err
	}
	p.net = tnet
	p.bind = &filterBind{Bind: conn.NewStdNetBind()}
	p.dev = device.NewDevice(tun, p.bind, &device.Logger{Verbosef: device.DiscardLogf, Errorf: device.DiscardLogf})

	var uapi strings.Builder
	fmt.Fprintf(&uapi, "private_key=%s\nlisten_port=0\n", hex.EncodeToString(serverPriv[:]))
	fmt.Fprintf(&uapi, "public_key=%s\npreshared_key=%s\n", hex.EncodeToString(clientPub[:]), hex.EncodeToString(p.psk[:]))
	for _, a := range opts.ClientAddresses {
		if p.hasServerFamily(a.Addr()) {
			fmt.Fprintf(&uapi, "allowed_ip=%s\n", netip.PrefixFrom(a.Addr(), a.Addr().BitLen()))
		}
	}
	if err := p.dev.IpcSet(uapi.String()); err != nil {
		p.dev.Close()
		return nil, errors.New("configure server device")
	}
	if err := p.dev.Up(); err != nil {
		p.dev.Close()
		return nil, err
	}
	if err := p.serve(); err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}

func (p *Peer) hasServerFamily(a netip.Addr) bool {
	for _, s := range p.opts.ServerAddresses {
		if s.Is4() == a.Is4() {
			return true
		}
	}
	return false
}

func (p *Peer) serve() error {
	p.httpSrv = &http.Server{Handler: p.opts.Handler, ReadHeaderTimeout: 10 * time.Second}
	for _, a := range p.opts.ServerAddresses {
		l, err := p.net.ListenTCPAddrPort(netip.AddrPortFrom(a, 80))
		if err != nil {
			return err
		}
		p.wg.Go(func() { _ = p.httpSrv.Serve(l) })
		u, err := p.net.ListenUDPAddrPort(netip.AddrPortFrom(a, 53))
		if err != nil {
			return err
		}
		p.udp = append(p.udp, u)
		p.wg.Go(func() { p.serveDNS(u) })
		dl, err := p.net.ListenTCPAddrPort(netip.AddrPortFrom(a, 53))
		if err != nil {
			return err
		}
		p.tcp = append(p.tcp, dl)
		p.wg.Go(func() { p.serveDNSTCP(dl) })
	}
	return nil
}

// Close stops the peer. It is safe to call more than once.
func (p *Peer) Close() {
	p.closeOnce.Do(func() {
		if p.httpSrv != nil {
			_ = p.httpSrv.Close()
		}
		for _, u := range p.udp {
			_ = u.Close()
		}
		for _, l := range p.tcp {
			_ = l.Close()
		}
		p.mu.Lock()
		p.closed = true
		for c := range p.conns {
			_ = c.Close()
		}
		p.mu.Unlock()
		p.wg.Wait()
		p.dev.Close()
	})
}

// Endpoint returns the peer's UDP endpoint on loopback.
func (p *Peer) Endpoint() netip.AddrPort {
	return netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(p.bind.port.Load()))
}

// ClientConfig returns the client's tunnel file in wg-quick format, with
// test-only keys and Endpoint = Endpoint().
func (p *Peer) ClientConfig() string {
	var b strings.Builder
	b.WriteString("[Interface]\n")
	fmt.Fprintf(&b, "PrivateKey = %s\n", base64.StdEncoding.EncodeToString(p.clientPriv[:]))
	fmt.Fprintf(&b, "Address = %s\n", joinPrefixes(p.opts.ClientAddresses))
	switch {
	case p.opts.NoDNS:
	case len(p.opts.DNS) > 0:
		s := make([]string, len(p.opts.DNS))
		for i, a := range p.opts.DNS {
			s[i] = a.String()
		}
		fmt.Fprintf(&b, "DNS = %s\n", strings.Join(s, ", "))
	default:
		fmt.Fprintf(&b, "DNS = %s\n", p.opts.ServerAddresses[0])
	}
	b.WriteString("\n[Peer]\n")
	fmt.Fprintf(&b, "PublicKey = %s\n", base64.StdEncoding.EncodeToString(p.serverPub[:]))
	fmt.Fprintf(&b, "PresharedKey = %s\n", base64.StdEncoding.EncodeToString(p.psk[:]))
	fmt.Fprintf(&b, "Endpoint = %s\n", p.Endpoint())
	fmt.Fprintf(&b, "AllowedIPs = %s\n", joinPrefixes(p.opts.AllowedIPs))
	return b.String()
}

// ClientTunnel parses ClientConfig as tunnel name.
func (p *Peer) ClientTunnel(tb testing.TB, name string) *wgquick.Tunnel {
	tb.Helper()
	return ParseTunnel(tb, name, p.ClientConfig())
}

// ParseTunnel parses a wg-quick text and fails tb on error.
func ParseTunnel(tb testing.TB, name, text string) *wgquick.Tunnel {
	tb.Helper()
	t, _, err := wgquick.Parse(name, strings.NewReader(text))
	if err != nil {
		tb.Fatalf("tunneltest: parse %s: %v", name, err)
	}
	return t
}

func joinPrefixes(ps []netip.Prefix) string {
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = p.String()
	}
	return strings.Join(s, ", ")
}

// SetReachable switches the peer's UDP endpoint on or off. While off, the
// peer drops every packet it receives, like a peer that is down.
func (p *Peer) SetReachable(ok bool) {
	p.bind.drop.Store(!ok)
}

// Packets returns the number of UDP packets the peer received, dropped
// ones included.
func (p *Peer) Packets() int64 {
	return p.bind.packets.Load()
}

// SetHost sets the DNS records of name (A and AAAA, by address family).
// Names without records are answered with NXDOMAIN.
func (p *Peer) SetHost(name string, addrs ...netip.Addr) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.records[canonicalName(name)] = addrs
}

// SetCNAME makes name an alias of target. Answers for name carry the
// CNAME chain followed by the records of its end, like the answers of a
// recursive resolver.
func (p *Peer) SetCNAME(name, target string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cnames[canonicalName(name)] = canonicalName(target)
}

// DropDNS makes the DNS server drop the next n UDP queries without an
// answer, like a lossy path. n < 0 drops every UDP query until the next
// DropDNS call; 0 stops dropping. TCP queries are never dropped.
func (p *Peer) DropDNS(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dropNext = n
}

// DroppedDNS returns the number of UDP queries dropped so far.
func (p *Peer) DroppedDNS() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dropped
}

// FailDNS answers the next n queries with rcode (for example
// dnsmessage.RCodeServerFailure) and no records. n < 0 fails every query
// until the next FailDNS call; 0 stops failing.
func (p *Peer) FailDNS(n int, rcode dnsmessage.RCode) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failNext, p.failRCode = n, rcode
}

// SetTruncate switches truncation of UDP answers: while on, every UDP
// answer has the TC bit set and no records, so clients retry over TCP.
func (p *Peer) SetTruncate(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.truncate = on
}

// Queries returns the DNS questions the peer answered so far, over UDP
// and TCP, as "name TYPE" (for example "upstream.test A").
func (p *Peer) Queries() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.queries...)
}

// TCPQueries returns the DNS questions the peer answered over TCP, in
// the format of Queries.
func (p *Peer) TCPQueries() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.tcpQueries...)
}

func canonicalName(name string) string {
	return strings.ToLower(strings.TrimSuffix(name, "."))
}

func (p *Peer) serveDNS(c net.PacketConn) {
	buf := make([]byte, 1500)
	for {
		n, from, err := c.ReadFrom(buf)
		if err != nil {
			return
		}
		if p.dropUDP() {
			continue
		}
		resp, ok := p.answer(buf[:n], false)
		if !ok {
			continue
		}
		_, _ = c.WriteTo(resp, from)
	}
}

// dropUDP reports whether to drop the UDP query just received, and
// counts it if so.
func (p *Peer) dropUDP() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dropNext == 0 {
		return false
	}
	if p.dropNext > 0 {
		p.dropNext--
	}
	p.dropped++
	return true
}

func (p *Peer) serveDNSTCP(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			_ = c.Close()
			return
		}
		p.conns[c] = struct{}{}
		p.mu.Unlock()
		p.wg.Go(func() {
			defer func() {
				p.mu.Lock()
				delete(p.conns, c)
				p.mu.Unlock()
				_ = c.Close()
			}()
			p.serveDNSConn(c)
		})
	}
}

// serveDNSConn answers length-prefixed DNS messages (RFC 1035 4.2.2)
// until the client closes the connection.
func (p *Peer) serveDNSConn(c net.Conn) {
	for {
		var l [2]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return
		}
		req := make([]byte, int(l[0])<<8|int(l[1]))
		if _, err := io.ReadFull(c, req); err != nil {
			return
		}
		resp, ok := p.answer(req, true)
		if !ok {
			return
		}
		out := append([]byte{byte(len(resp) >> 8), byte(len(resp))}, resp...)
		if _, err := c.Write(out); err != nil {
			return
		}
	}
}

func (p *Peer) answer(req []byte, overTCP bool) ([]byte, bool) {
	var parser dnsmessage.Parser
	h, err := parser.Start(req)
	if err != nil {
		return nil, false
	}
	q, err := parser.Question()
	if err != nil {
		return nil, false
	}
	name := canonicalName(q.Name.String())
	query := name + " " + strings.TrimPrefix(q.Type.String(), "Type")

	rh := dnsmessage.Header{ID: h.ID, Response: true, Authoritative: true, RecursionDesired: h.RecursionDesired, RecursionAvailable: true}
	var chain []string // the queried name, then the CNAME targets
	var addrs []netip.Addr
	p.mu.Lock()
	p.queries = append(p.queries, query)
	if overTCP {
		p.tcpQueries = append(p.tcpQueries, query)
	}
	switch {
	case p.failNext != 0:
		if p.failNext > 0 {
			p.failNext--
		}
		rh.RCode = p.failRCode
	case p.truncate && !overTCP:
		rh.Truncated = true
	default:
		chain = append(chain, name)
		for range 8 {
			target, ok := p.cnames[chain[len(chain)-1]]
			if !ok {
				break
			}
			chain = append(chain, target)
		}
		var found bool
		addrs, found = p.records[chain[len(chain)-1]]
		if !found {
			rh.RCode = dnsmessage.RCodeNameError
		}
	}
	p.mu.Unlock()

	b := dnsmessage.NewBuilder(nil, rh)
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil, false
	}
	if err := b.Question(q); err != nil {
		return nil, false
	}
	if err := b.StartAnswers(); err != nil {
		return nil, false
	}
	owner := q.Name
	for _, target := range chain[min(1, len(chain)):] {
		t, err := dnsmessage.NewName(target + ".")
		if err != nil {
			return nil, false
		}
		rrh := dnsmessage.ResourceHeader{Name: owner, Class: dnsmessage.ClassINET, TTL: 60}
		if err := b.CNAMEResource(rrh, dnsmessage.CNAMEResource{CNAME: t}); err != nil {
			return nil, false
		}
		owner = t
	}
	for _, a := range addrs {
		rrh := dnsmessage.ResourceHeader{Name: owner, Class: dnsmessage.ClassINET, TTL: 60}
		switch {
		case q.Type == dnsmessage.TypeA && a.Is4():
			err = b.AResource(rrh, dnsmessage.AResource{A: a.As4()})
		case q.Type == dnsmessage.TypeAAAA && a.Is6():
			err = b.AAAAResource(rrh, dnsmessage.AAAAResource{AAAA: a.As16()})
		}
		if err != nil {
			return nil, false
		}
	}
	resp, err := b.Finish()
	return resp, err == nil
}

// newKeyPair returns a clamped test-only X25519 private key and its
// public key.
func newKeyPair() (priv, pub [32]byte) {
	if _, err := rand.Read(priv[:]); err != nil {
		panic(err)
	}
	priv[0] &= 248
	priv[31] = (priv[31] & 127) | 64
	k, err := ecdh.X25519().NewPrivateKey(priv[:])
	if err != nil {
		panic(err)
	}
	copy(pub[:], k.PublicKey().Bytes())
	return priv, pub
}

// filterBind counts received packets and drops them while drop is set.
type filterBind struct {
	conn.Bind
	drop    atomic.Bool
	packets atomic.Int64
	port    atomic.Uint32
}

func (b *filterBind) Open(port uint16) ([]conn.ReceiveFunc, uint16, error) {
	fns, actual, err := b.Bind.Open(port)
	if err != nil {
		return nil, 0, err
	}
	b.port.Store(uint32(actual))
	wrapped := make([]conn.ReceiveFunc, len(fns))
	for i, fn := range fns {
		wrapped[i] = func(packets [][]byte, sizes []int, eps []conn.Endpoint) (int, error) {
			for {
				n, err := fn(packets, sizes, eps)
				if err != nil || n == 0 {
					return n, err
				}
				b.packets.Add(int64(n))
				if !b.drop.Load() {
					return n, nil
				}
			}
		}
	}
	return wrapped, actual, nil
}
