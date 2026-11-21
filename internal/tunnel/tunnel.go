// SPDX-License-Identifier: AGPL-3.0-or-later

// Package tunnel runs the userspace WireGuard tunnels (spec 2.1): one
// wireguard-go device with a gVisor netstack per tunnel file, started
// lazily on first use and shared by every channel and viewer of the
// tunnel.
//
// A *Tunnel is the route layer's way to the internet through the tunnel:
// LookupNetIP resolves host names with the tunnel's DNS servers inside the
// netstack (own client: first query to every server, retries after 0.5s,
// every 0.25s until the first answer after a start, answers cached while
// the device runs), DialContext connects to
// resolved addresses. Both start the device if needed; concurrent first
// users share one start.
//
// Keys never appear in errors or logs: the UAPI text is never passed on,
// and wireguard-go's own log output (which names the peer by key
// fragments) reaches the debug log only with the peer shown as the tunnel
// name and anything key-like redacted (wgLogger).
package tunnel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/config/wgquick"
	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel/netstack"
)

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
	// IdleTimeout stops a running tunnel after this long without activity
	// ([tunnels] idle_timeout, spec 2.1). 0 disables idle close.
	IdleTimeout time.Duration
	// Logger receives start and stop events. nil discards them.
	Logger *slog.Logger
	// Clock defaults to the system clock.
	Clock Clock
	// HostResolver resolves Endpoint host names. nil means
	// net.DefaultResolver.
	HostResolver HostResolver
	// OnStart, if set, is called once for every successful tunnel start
	// with the tunnel name and how long the start took (device up
	// including the first handshake, measured with Clock). It is called
	// where the start happens, not per request that waited for it, and not
	// for a failed start. It must not block.
	OnStart func(tunnel string, took time.Duration)
}

// Manager owns the tunnels of one configuration, keyed by name. Apply
// replaces them on a reload.
type Manager struct {
	opts Options
	// connectTimeout and idleTimeout hold the current values of the
	// Options fields; SetTimeouts changes them.
	connectTimeout atomic.Int64
	idleTimeout    atomic.Int64

	// mu guards closed, the tunnel set and the start and retire state of
	// every tunnel.
	mu      sync.Mutex
	closed  bool
	closing chan struct{}
	tunnels map[string]*Tunnel
	sorted  []*Tunnel
	// draining are the tunnels a reload retired whose drain has not
	// finished yet.
	draining map[*Tunnel]struct{}
	starts   sync.WaitGroup
	drains   sync.WaitGroup
}

// Load reads and parses the tunnel files of a configuration with Parse
// and returns a manager for them. No tunnel is started.
func Load(files []config.Tunnel, opts Options) (*Manager, error) {
	tunnels, err := Parse(files, opts.Logger)
	if err != nil {
		return nil, err
	}
	return New(tunnels, opts)
}

// Parse reads and parses tunnel files (spec 5.5): it logs parse warnings
// and the world-readable warning, and rejects invalid files and shared
// private keys, with all errors joined. logger may be nil.
func Parse(files []config.Tunnel, logger *slog.Logger) ([]*wgquick.Tunnel, error) {
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
	if err := wgquick.CheckUniquePrivateKeys(tunnels); err != nil {
		return nil, err
	}
	return tunnels, nil
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
	if err := checkTimeouts(opts.ConnectTimeout, opts.IdleTimeout); err != nil {
		return nil, err
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
	if err := checkTunnels(tunnels); err != nil {
		return nil, err
	}
	m := &Manager{opts: opts, closing: make(chan struct{}), draining: map[*Tunnel]struct{}{}}
	m.connectTimeout.Store(int64(opts.ConnectTimeout))
	m.idleTimeout.Store(int64(opts.IdleTimeout))
	set := make(map[string]*Tunnel, len(tunnels))
	for _, cfg := range tunnels {
		set[cfg.Name] = newTunnel(m, cfg)
	}
	m.setTunnels(set)
	return m, nil
}

// checkTimeouts validates the connect and idle timeouts.
func checkTimeouts(connect, idle time.Duration) error {
	if connect <= 0 {
		return errors.New("tunnel: connect timeout must be positive")
	}
	if idle < 0 {
		return errors.New("tunnel: idle timeout must not be negative")
	}
	return nil
}

// checkTunnels validates a tunnel set: no nil, unique names and unique
// private keys (spec 5.5).
func checkTunnels(tunnels []*wgquick.Tunnel) error {
	if slices.Contains(tunnels, nil) {
		return errors.New("tunnel: nil tunnel")
	}
	names := make(map[string]bool, len(tunnels))
	for _, cfg := range tunnels {
		if names[cfg.Name] {
			return fmt.Errorf("tunnel %q: duplicate name", cfg.Name)
		}
		names[cfg.Name] = true
	}
	return wgquick.CheckUniquePrivateKeys(tunnels)
}

// setTunnels makes tunnels the current set. m.mu is held, or m is new.
func (m *Manager) setTunnels(tunnels map[string]*Tunnel) {
	m.tunnels = tunnels
	m.sorted = slices.SortedFunc(maps.Values(tunnels), func(a, b *Tunnel) int { return strings.Compare(a.cfg.Name, b.cfg.Name) })
}

// connect returns the current connect timeout.
func (m *Manager) connect() time.Duration { return time.Duration(m.connectTimeout.Load()) }

// idle returns the current idle timeout; 0 disables idle close.
func (m *Manager) idle() time.Duration { return time.Duration(m.idleTimeout.Load()) }

// Tunnel returns the tunnel with the given name.
func (m *Manager) Tunnel(name string) (*Tunnel, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tunnels[name]
	return t, ok
}

// Tunnels returns all tunnels, sorted by name.
func (m *Manager) Tunnels() []*Tunnel {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.sorted)
}

// Close aborts pending starts and closes every running device at once,
// including tunnels still draining after a reload. Open connections
// through the tunnels break, so the caller ends its requests first. Later
// calls on the tunnels return ErrClosed. Close is idempotent.
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
	// Reload drains end at once when closing is closed.
	m.drains.Wait()
	for _, t := range m.Tunnels() {
		t.stop("shutdown")
	}
	return nil
}

// Tunnel is one tunnel. It implements the route layer's Network and
// Activity.
type Tunnel struct {
	m   *Manager
	cfg *wgquick.Tunnel
	// has4 and has6: the tunnel has an interface address of the family.
	has4, has6 bool
	// dns are the DNS servers of the file that the tunnel can reach.
	dns []netip.Addr
	// dnsTypes are the DNS query types of a lookup: only families the
	// tunnel has an address for, so an IPv4-only tunnel never waits for
	// AAAA answers.
	dnsTypes []dnsmessage.Type

	run   atomic.Pointer[running]
	start *startCall // guarded by m.mu
	// retired: a reload changed or removed the tunnel (Manager.Apply). It
	// never starts again; retiring is closed then, which cancels a start
	// in progress. Guarded by m.mu.
	retired  bool
	retiring chan struct{}

	// mu guards the activity state below. It is never held while m.mu
	// is taken.
	mu sync.Mutex
	// inflight counts the requests between Begin and end.
	inflight int
	// busy is open while inflight > 0 and closed when it drops to 0.
	busy chan struct{}
	// idle is the armed idle timer, or nil; idleGen identifies it, so a
	// timer that fires after it was replaced does nothing.
	idle    Timer
	idleGen uint64
	// onStop is called when the running device stops.
	onStop func()
	// draining counts the drains (Cooldown, reload) waiting for
	// the requests before they stop the device.
	draining atomic.Int32

	lastActivity atomic.Int64 // unix nanoseconds, 0 = never
	starts       atomic.Int64 // devices created, for tests
}

// running is a started device.
type running struct {
	dev *device.Device
	net *netstack.Net
	// dns resolves with the tunnel's DNS servers and caches the answers
	// for this run only.
	dns *dnsClient
	// started is when the first handshake completed, handshake how long
	// it took after the device was up (Options.Clock).
	started   time.Time
	handshake time.Duration
}

// close ends the lookups in progress, drops the DNS cache and closes the
// device. Every stop (failed start, idle close, drain, Close) ends here,
// so the next start begins with an empty cache. gVisor may still send
// packets while the netstack closes; the netstack copy in ./netstack
// drops them instead of panicking (#47).
func (r *running) close() {
	r.dns.close()
	r.dev.Close()
}

// startCall is a start in progress, shared by all callers that wait for it.
type startCall struct {
	done chan struct{}
	run  *running
	err  error
}

func newTunnel(m *Manager, cfg *wgquick.Tunnel) *Tunnel {
	t := &Tunnel{m: m, cfg: cfg, retiring: make(chan struct{})}
	for _, p := range cfg.Addresses {
		if p.Addr().Is4() {
			t.has4 = true
		} else {
			t.has6 = true
		}
	}
	if t.has4 {
		t.dnsTypes = append(t.dnsTypes, dnsmessage.TypeA)
	}
	if t.has6 {
		t.dnsTypes = append(t.dnsTypes, dnsmessage.TypeAAAA)
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
// first. An IP literal is returned as is if reachable. Answers are cached
// per name and type for their TTL (10s to 5min) until the tunnel stops;
// concurrent lookups of one name share one query. It starts the
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
		found, err := r.dns.lookup(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("tunnel %q: %w", t.cfg.Name, err)
		}
		for _, ip := range found {
			addrs = append(addrs, ip.Unmap())
		}
	}
	resolved := len(addrs)
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
		de := &net.DNSError{Err: "no address reachable through tunnel " + strconv.Quote(t.cfg.Name), Name: host, IsNotFound: true}
		if resolved > 0 {
			// Only addresses outside AllowedIPs: the configuration blocks
			// the name.
			de.UnwrapErr = ErrNotAllowed
		}
		return nil, de
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
	c, err := r.net.DialContextTCPAddrPort(ctx, target)
	if err != nil {
		// Never a typed nil in the net.Conn interface.
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
	if t.retired {
		m.mu.Unlock()
		return nil, errRetired(t.cfg.Name)
	}
	c := t.start
	if c == nil {
		c = &startCall{done: make(chan struct{})}
		t.start = c
		m.starts.Add(1)
		go t.doStart(c)
	}
	m.mu.Unlock()

	if f := startTrace(ctx); f != nil {
		begin := m.opts.Clock.Now()
		defer func() { f(m.opts.Clock.Now().Sub(begin)) }()
	}
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
	m := t.m
	begin := m.opts.Clock.Now()
	r, err := t.startDevice()
	took := m.opts.Clock.Now().Sub(begin)
	m.mu.Lock()
	t.start = nil
	switch {
	case err == nil && m.closed:
		r.close()
		r, err = nil, fmt.Errorf("tunnel %q: %w", t.cfg.Name, ErrClosed)
	case err == nil && t.retired:
		r.close()
		r, err = nil, errRetired(t.cfg.Name)
	}
	if err == nil {
		t.run.Store(r)
	}
	c.run, c.err = r, err
	m.mu.Unlock()
	// Before the waiters go on, so a request that saw the start finished
	// also sees it observed.
	if err == nil && m.opts.OnStart != nil {
		m.opts.OnStart(t.cfg.Name, took)
	}
	close(c.done)
	if err != nil {
		m.opts.Logger.Warn("tunnel start failed", "tunnel", t.cfg.Name, "err", err, "duration", took)
		return
	}
	// duration: the whole start; handshake: its part after the device
	// was up.
	m.opts.Logger.Info("tunnel started", "tunnel", t.cfg.Name, "duration", took, "handshake", r.handshake)
	// A start by callers that are not counted (no Begin) still ends in
	// an idle close.
	t.mu.Lock()
	if t.inflight == 0 {
		t.armIdle()
	}
	t.mu.Unlock()
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
	timeout := m.connect()
	timer := m.opts.Clock.AfterFunc(timeout, cancel)
	defer timer.Stop()
	go func() {
		select {
		case <-m.closing:
			cancel()
		case <-t.retiring:
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
		dev: device.NewDevice(tun, conn.NewDefaultBind(), wgLogger(m.opts.Logger, t.cfg.Name)),
		net: tnet,
		dns: newDNSClient(tnet, t.dns, t.dnsTypes, m.opts.Clock),
	}
	// IpcSet errors may quote the configuration: never pass them on
	// (wgLogger drops wireguard-go's own log line of them).
	if err := r.dev.IpcSet(uapi); err != nil {
		r.close()
		return nil, unavailable("configure device")
	}
	if err := r.dev.Up(); err != nil {
		r.close()
		return nil, unavailable("device up: %v", err)
	}
	hs := m.opts.Clock.Now()
	if peer := r.dev.LookupPeer(pk); peer != nil {
		_ = peer.SendHandshakeInitiation(false)
	}
	if err := waitHandshake(ctx, r.dev); err != nil {
		r.close()
		switch {
		case isClosed(m.closing):
			return nil, fmt.Errorf("tunnel %q: %w", t.cfg.Name, ErrClosed)
		case isClosed(t.retiring):
			return nil, errRetired(t.cfg.Name)
		}
		return nil, unavailable("no handshake within %s", timeout)
	}
	r.started = m.opts.Clock.Now()
	r.handshake = r.started.Sub(hs)
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
