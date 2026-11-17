// SPDX-License-Identifier: AGPL-3.0-or-later

// Package tunnel runs the userspace WireGuard tunnels (spec 2.1): one
// wireguard-go device with a gVisor netstack per tunnel file, started
// lazily on first use and shared by every channel and viewer of the
// tunnel.
//
// A *Tunnel is the route layer's way to the internet through the tunnel:
// LookupNetIP resolves host names with the tunnel's DNS servers inside the
// netstack, DialContext connects to resolved addresses. Both start the
// device if needed; concurrent first users share one start.
//
// Keys never appear in errors or logs: the UAPI text and wireguard-go's
// own log output (which prints key fragments) are never passed on.
package tunnel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/config/wgquick"
)

// DefaultIPv6ConnectTimeout is the default for Options.IPv6ConnectTimeout.
const DefaultIPv6ConnectTimeout = 2 * time.Second

// handshakeFresh is the age up to which a handshake counts as current for
// Up (spec 7.3, wsp_tunnel_up).
const handshakeFresh = 180 * time.Second

// handshakePoll is how often a start checks for the first handshake.
// wireguard-go has no handshake callback.
const handshakePoll = 10 * time.Millisecond

var (
	// ErrUnavailable: the tunnel could not be started, or no handshake
	// completed within the connect timeout (spec 2.1). The server answers
	// 502; the next request retries. It is never a net.Error timeout.
	ErrUnavailable = errors.New("tunnel unavailable")
	// ErrNotAllowed: the address is outside the tunnel's AllowedIPs, or
	// the tunnel has no address of its family (fail closed, spec 5.5).
	// Nothing is sent.
	ErrNotAllowed = errors.New("address not reachable through tunnel")
	// ErrIPv6Unreachable: an IPv6 connection through a tunnel that also
	// carries IPv4 was not established within IPv6ConnectTimeout. The
	// tunnel's IPv6 path is likely broken; callers fall back to IPv4.
	ErrIPv6Unreachable = errors.New("no IPv6 connection through tunnel")
	// ErrNoDNS: the tunnel file has no DNS server inside AllowedIPs, so
	// host names cannot be resolved inside the tunnel.
	ErrNoDNS = errors.New("tunnel has no reachable DNS server")
	// ErrClosed: the manager was closed.
	ErrClosed = errors.New("tunnel manager closed")
)

// Clock is the time source, injected for tests.
type Clock interface {
	Now() time.Time
	// AfterFunc calls f in its own goroutine after d.
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is a pending AfterFunc call.
type Timer interface {
	Stop() bool
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }

// HostResolver resolves the Endpoint host name on the host network;
// *net.Resolver implements it.
type HostResolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Options configure a Manager.
type Options struct {
	// ConnectTimeout bounds a tunnel start including the first handshake
	// ([upstream] connect_timeout). Required.
	ConnectTimeout time.Duration
	// IPv6ConnectTimeout bounds an IPv6 TCP connect through a tunnel that
	// also carries IPv4, so a broken IPv6 path fails before the caller's
	// connect timeout and the caller can fall back to IPv4. 0 means
	// DefaultIPv6ConnectTimeout.
	IPv6ConnectTimeout time.Duration
	// Logger receives start and stop events. nil discards them.
	Logger *slog.Logger
	// Clock defaults to the system clock.
	Clock Clock
	// HostResolver resolves Endpoint host names. nil means
	// net.DefaultResolver.
	HostResolver HostResolver
}

// Manager owns the tunnels of one configuration, keyed by name.
type Manager struct {
	opts    Options
	tunnels map[string]*Tunnel
	sorted  []*Tunnel

	// mu guards closed and the start state of every tunnel.
	mu      sync.Mutex
	closed  bool
	closing chan struct{}
	starts  sync.WaitGroup
}

// Load reads and parses the tunnel files of a configuration (spec 5.5):
// it logs parse warnings and the world-readable warning, and rejects
// invalid files and shared private keys. No tunnel is started.
func Load(files []config.Tunnel, opts Options) (*Manager, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	var errs []error
	tunnels := make([]*wgquick.Tunnel, 0, len(files))
	for _, f := range files {
		t, warns, err := loadFile(f)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, w := range warns {
			logger.Warn(w.Msg, "tunnel", w.Tunnel, "line", w.Line)
		}
		tunnels = append(tunnels, t)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return New(tunnels, opts)
}

func loadFile(f config.Tunnel) (*wgquick.Tunnel, []wgquick.Warning, error) {
	fh, err := os.Open(f.Path)
	if err != nil {
		return nil, nil, fmt.Errorf("tunnel %q: %w", f.Name, err)
	}
	defer func() { _ = fh.Close() }()
	fi, err := fh.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("tunnel %q: %w", f.Name, err)
	}
	t, warns, err := wgquick.Parse(f.Name, fh)
	if err != nil {
		return nil, nil, err
	}
	if w, ok := wgquick.ModeWarning(f.Name, fi.Mode()); ok {
		warns = append(warns, w)
	}
	return t, warns, nil
}

// New returns a manager for already parsed tunnels. Names must be unique;
// private keys too (spec 5.5). No tunnel is started.
func New(tunnels []*wgquick.Tunnel, opts Options) (*Manager, error) {
	if opts.ConnectTimeout <= 0 {
		return nil, errors.New("tunnel: connect timeout must be positive")
	}
	switch {
	case opts.IPv6ConnectTimeout < 0:
		return nil, errors.New("tunnel: IPv6 connect timeout must not be negative")
	case opts.IPv6ConnectTimeout == 0:
		opts.IPv6ConnectTimeout = DefaultIPv6ConnectTimeout
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Clock == nil {
		opts.Clock = realClock{}
	}
	if opts.HostResolver == nil {
		opts.HostResolver = net.DefaultResolver
	}
	if slices.Contains(tunnels, nil) {
		return nil, errors.New("tunnel: nil tunnel")
	}
	if err := wgquick.CheckUniquePrivateKeys(tunnels); err != nil {
		return nil, err
	}
	m := &Manager{opts: opts, tunnels: make(map[string]*Tunnel, len(tunnels)), closing: make(chan struct{})}
	for _, cfg := range tunnels {
		if _, dup := m.tunnels[cfg.Name]; dup {
			return nil, fmt.Errorf("tunnel %q: duplicate name", cfg.Name)
		}
		t := newTunnel(m, cfg)
		m.tunnels[cfg.Name] = t
		m.sorted = append(m.sorted, t)
	}
	slices.SortFunc(m.sorted, func(a, b *Tunnel) int { return strings.Compare(a.cfg.Name, b.cfg.Name) })
	return m, nil
}

// Tunnel returns the tunnel with the given name.
func (m *Manager) Tunnel(name string) (*Tunnel, bool) {
	t, ok := m.tunnels[name]
	return t, ok
}

// Tunnels returns all tunnels, sorted by name.
func (m *Manager) Tunnels() []*Tunnel {
	return slices.Clone(m.sorted)
}

// Close aborts pending starts and closes every running device. Open
// connections through the tunnels break. Later calls on the tunnels
// return ErrClosed. Close is idempotent.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	close(m.closing)
	m.mu.Unlock()

	m.starts.Wait()
	for _, t := range m.sorted {
		if r := t.run.Swap(nil); r != nil {
			r.close()
			m.opts.Logger.Info("tunnel closed", "tunnel", t.cfg.Name)
		}
	}
	return nil
}

// Tunnel is one tunnel. It implements the route layer's Network.
type Tunnel struct {
	m   *Manager
	cfg *wgquick.Tunnel
	// has4 and has6: the tunnel has an interface address of the family.
	has4, has6 bool
	// dns are the DNS servers of the file that the tunnel can reach.
	dns []netip.Addr

	run   atomic.Pointer[running]
	start *startCall // guarded by m.mu

	lastActivity atomic.Int64 // unix nanoseconds, 0 = never
	starts       atomic.Int64 // devices created, for tests
}

// running is a started device.
type running struct {
	dev *device.Device
	net *netstack.Net
}

func (r *running) close() {
	r.dev.Close()
}

// startCall is a start in progress, shared by all callers that wait for it.
type startCall struct {
	done chan struct{}
	run  *running
	err  error
}

func newTunnel(m *Manager, cfg *wgquick.Tunnel) *Tunnel {
	t := &Tunnel{m: m, cfg: cfg}
	for _, p := range cfg.Addresses {
		if p.Addr().Is4() {
			t.has4 = true
		} else {
			t.has6 = true
		}
	}
	for _, a := range cfg.DNS {
		if t.check(a) == nil {
			t.dns = append(t.dns, a)
		}
	}
	return t
}

// Name returns the tunnel name.
func (t *Tunnel) Name() string { return t.cfg.Name }

// Running reports whether the device is started.
func (t *Tunnel) Running() bool { return t.run.Load() != nil }

// LastHandshake returns the time of the latest completed handshake of the
// running device, or the zero time (spec 7.3).
func (t *Tunnel) LastHandshake() time.Time {
	r := t.run.Load()
	if r == nil {
		return time.Time{}
	}
	return lastHandshake(r.dev)
}

// Up reports whether the device runs and its latest handshake is younger
// than 180s (spec 7.3, wsp_tunnel_up).
func (t *Tunnel) Up() bool {
	hs := t.LastHandshake()
	return !hs.IsZero() && t.m.opts.Clock.Now().Sub(hs) < handshakeFresh
}

// LastActivity returns the time of the latest LookupNetIP or DialContext
// call, or the zero time.
func (t *Tunnel) LastActivity() time.Time {
	ns := t.lastActivity.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

func (t *Tunnel) touch() {
	t.lastActivity.Store(t.m.opts.Clock.Now().UnixNano())
}

// LookupNetIP resolves host with the tunnel's DNS servers, inside the
// tunnel (spec 2.1). It returns only addresses DialContext accepts, IPv4
// first. An IP literal is returned as is if reachable. It starts the
// tunnel if needed.
func (t *Tunnel) LookupNetIP(ctx context.Context, host string) ([]netip.Addr, error) {
	t.touch()
	r, err := t.ensure(ctx)
	if err != nil {
		return nil, err
	}
	var addrs []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{ip.Unmap()}
	} else {
		if len(t.dns) == 0 {
			return nil, fmt.Errorf("tunnel %q: lookup %s: %w", t.cfg.Name, host, ErrNoDNS)
		}
		names, err := r.net.LookupContextHost(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("tunnel %q: %w", t.cfg.Name, err)
		}
		for _, n := range names {
			if ip, err := netip.ParseAddr(n); err == nil {
				addrs = append(addrs, ip.Unmap())
			}
		}
	}
	addrs = slices.DeleteFunc(addrs, func(a netip.Addr) bool { return t.check(a) != nil })
	// IPv4 first: a provider's IPv6 is more often broken than its IPv4.
	slices.SortStableFunc(addrs, func(a, b netip.Addr) int {
		switch {
		case a.Is4() == b.Is4():
			return 0
		case a.Is4():
			return -1
		}
		return 1
	})
	if len(addrs) == 0 {
		return nil, &net.DNSError{Err: "no address reachable through tunnel " + strconv.Quote(t.cfg.Name), Name: host, IsNotFound: true}
	}
	return addrs, nil
}

// check returns ErrNotAllowed unless addr can be reached through the
// tunnel: an interface address of its family and inside AllowedIPs.
func (t *Tunnel) check(addr netip.Addr) error {
	if (addr.Is4() && !t.has4) || (addr.Is6() && !t.has6) || !t.cfg.Allows(addr) {
		return fmt.Errorf("tunnel %q: %s: %w", t.cfg.Name, addr, ErrNotAllowed)
	}
	return nil
}

// DialContext opens a TCP connection through the tunnel to address, an
// "ip:port" resolved before (network "tcp", "tcp4" or "tcp6"). Addresses
// outside AllowedIPs or of a family without a tunnel address fail with
// ErrNotAllowed before anything is started or sent. It starts the tunnel
// if needed.
func (t *Tunnel) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	t.touch()
	ap, err := netip.ParseAddrPort(address)
	if err != nil || ap.Addr().Zone() != "" {
		return nil, &net.OpError{Op: "dial", Net: network, Err: fmt.Errorf("tunnel %q: address must be ip:port", t.cfg.Name)}
	}
	addr := ap.Addr().Unmap()
	switch {
	case network == "tcp":
	case network == "tcp4" && addr.Is4(), network == "tcp6" && addr.Is6():
	case network == "tcp4", network == "tcp6":
		return nil, &net.OpError{Op: "dial", Net: network, Err: &net.AddrError{Err: "address family mismatch", Addr: address}}
	default:
		return nil, &net.OpError{Op: "dial", Net: network, Err: net.UnknownNetworkError(network)}
	}
	if err := t.check(addr); err != nil {
		return nil, err
	}
	r, err := t.ensure(ctx)
	if err != nil {
		return nil, err
	}
	target := netip.AddrPortFrom(addr, ap.Port())
	if addr.Is4() || !t.cfg.HasIPv4() {
		c, err := r.net.DialContextTCPAddrPort(ctx, target)
		if err != nil {
			// Never a typed nil in the net.Conn interface.
			return nil, err
		}
		return c, nil
	}
	// IPv6 with IPv4 as fallback: bound the attempt.
	dctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var expired atomic.Bool
	timer := t.m.opts.Clock.AfterFunc(t.m.opts.IPv6ConnectTimeout, func() {
		expired.Store(true)
		cancel()
	})
	c, err := r.net.DialContextTCPAddrPort(dctx, target)
	timer.Stop()
	if err != nil && expired.Load() && ctx.Err() == nil {
		return nil, fmt.Errorf("tunnel %q: %s: %w", t.cfg.Name, target, ErrIPv6Unreachable)
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// ensure returns the running device, starting it if needed. Concurrent
// callers share one start. A caller whose ctx ends stops waiting; the
// start goes on for the others.
func (t *Tunnel) ensure(ctx context.Context) (*running, error) {
	if r := t.run.Load(); r != nil {
		return r, nil
	}
	m := t.m
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, fmt.Errorf("tunnel %q: %w", t.cfg.Name, ErrClosed)
	}
	if r := t.run.Load(); r != nil {
		m.mu.Unlock()
		return r, nil
	}
	c := t.start
	if c == nil {
		c = &startCall{done: make(chan struct{})}
		t.start = c
		m.starts.Add(1)
		go t.doStart(c)
	}
	m.mu.Unlock()

	select {
	case <-c.done:
		return c.run, c.err
	case <-ctx.Done():
		// An expired deadline is not wrapped: it is a net.Error timeout and
		// would map to 504, but a start without handshake is a 502.
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, fmt.Errorf("tunnel %q: %w: start not finished: %w", t.cfg.Name, ErrUnavailable, ctx.Err())
		}
		return nil, fmt.Errorf("tunnel %q: %w: start not finished within the deadline", t.cfg.Name, ErrUnavailable)
	}
}

func (t *Tunnel) doStart(c *startCall) {
	defer t.m.starts.Done()
	r, err := t.startDevice()
	m := t.m
	m.mu.Lock()
	t.start = nil
	if err == nil && m.closed {
		r.close()
		r, err = nil, fmt.Errorf("tunnel %q: %w", t.cfg.Name, ErrClosed)
	}
	if err == nil {
		t.run.Store(r)
	}
	c.run, c.err = r, err
	m.mu.Unlock()
	close(c.done)
	if err != nil {
		m.opts.Logger.Warn("tunnel start failed", "tunnel", t.cfg.Name, "err", err)
		return
	}
	m.opts.Logger.Info("tunnel started", "tunnel", t.cfg.Name)
}

// startDevice creates the device and waits for the first handshake, both
// bounded by the connect timeout. On error nothing is left running.
func (t *Tunnel) startDevice() (*running, error) {
	m := t.m
	unavailable := func(format string, args ...any) error {
		return fmt.Errorf("tunnel %q: %w: %s", t.cfg.Name, ErrUnavailable, fmt.Sprintf(format, args...))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timer := m.opts.Clock.AfterFunc(m.opts.ConnectTimeout, cancel)
	defer timer.Stop()
	go func() {
		select {
		case <-m.closing:
			cancel()
		case <-ctx.Done():
		}
	}()

	endpoint, err := t.resolveEndpoint(ctx)
	if err != nil {
		return nil, unavailable("resolve endpoint: %v", err)
	}
	uapi, err := t.cfg.UAPIConfig(endpoint)
	if err != nil {
		return nil, unavailable("%v", err)
	}
	pk, ok := uapiPublicKey(uapi)
	if !ok {
		return nil, unavailable("invalid peer key")
	}

	local := make([]netip.Addr, len(t.cfg.Addresses))
	for i, p := range t.cfg.Addresses {
		local[i] = p.Addr()
	}
	tun, tnet, err := netstack.CreateNetTUN(local, t.dns, t.cfg.MTU)
	if err != nil {
		return nil, unavailable("create netstack: %v", err)
	}
	t.starts.Add(1)
	r := &running{
		dev: device.NewDevice(tun, conn.NewDefaultBind(), &device.Logger{Verbosef: device.DiscardLogf, Errorf: device.DiscardLogf}),
		net: tnet,
	}
	// IpcSet errors may quote the configuration: never pass them on.
	if err := r.dev.IpcSet(uapi); err != nil {
		r.close()
		return nil, unavailable("configure device")
	}
	if err := r.dev.Up(); err != nil {
		r.close()
		return nil, unavailable("device up: %v", err)
	}
	if peer := r.dev.LookupPeer(pk); peer != nil {
		_ = peer.SendHandshakeInitiation(false)
	}
	if err := waitHandshake(ctx, r.dev); err != nil {
		r.close()
		if isClosed(m.closing) {
			return nil, fmt.Errorf("tunnel %q: %w", t.cfg.Name, ErrClosed)
		}
		return nil, unavailable("no handshake within %s", m.opts.ConnectTimeout)
	}
	return r, nil
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// resolveEndpoint returns the peer endpoint IP: the literal, or the host
// name resolved by the host resolver, IPv4 preferred.
func (t *Tunnel) resolveEndpoint(ctx context.Context) (netip.Addr, error) {
	host := t.cfg.Peer.Endpoint.Host
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip, nil
	}
	addrs, err := t.m.opts.HostResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return netip.Addr{}, err
	}
	for _, a := range addrs {
		if a.Unmap().Is4() {
			return a.Unmap(), nil
		}
	}
	if len(addrs) == 0 {
		return netip.Addr{}, errors.New("no address")
	}
	return addrs[0], nil
}

// uapiPublicKey extracts the peer public key from the UAPI text, to look
// up the peer for the first handshake initiation.
func uapiPublicKey(uapi string) (device.NoisePublicKey, bool) {
	var pk device.NoisePublicKey
	for line := range strings.SplitSeq(uapi, "\n") {
		if v, ok := strings.CutPrefix(line, "public_key="); ok {
			return pk, pk.FromHex(v) == nil
		}
	}
	return pk, false
}

// waitHandshake polls the device until a handshake has completed or ctx
// ends.
func waitHandshake(ctx context.Context, dev *device.Device) error {
	tick := time.NewTicker(handshakePoll)
	defer tick.Stop()
	for {
		if !lastHandshake(dev).IsZero() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// lastHandshake reads the peer's last handshake time from the device. The
// UAPI dump contains the keys; only the handshake lines are parsed and
// the buffer is wiped.
func lastHandshake(dev *device.Device) time.Time {
	var buf bytes.Buffer
	defer func() { b := buf.Bytes(); clear(b[:cap(b)]) }()
	if err := dev.IpcGetOperation(&buf); err != nil {
		return time.Time{}
	}
	var sec, nsec int64
	for line := range bytes.SplitSeq(buf.Bytes(), []byte("\n")) {
		if v, ok := bytes.CutPrefix(line, []byte("last_handshake_time_sec=")); ok {
			sec, _ = strconv.ParseInt(string(v), 10, 64)
		} else if v, ok := bytes.CutPrefix(line, []byte("last_handshake_time_nsec=")); ok {
			nsec, _ = strconv.ParseInt(string(v), 10, 64)
		}
	}
	if sec == 0 && nsec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, nsec)
}
