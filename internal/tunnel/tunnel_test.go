// SPDX-License-Identifier: AGPL-3.0-or-later

package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config/wgquick"
	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel/tunneltest"
)

const testConnectTimeout = 10 * time.Second

// recordingResolver is a host resolver that records every lookup.
type recordingResolver struct {
	mu      sync.Mutex
	lookups []string
	hosts   map[string][]netip.Addr
}

func (r *recordingResolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lookups = append(r.lookups, network+" "+host)
	if addrs, ok := r.hosts[host]; ok {
		return addrs, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func (r *recordingResolver) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.lookups)
}

type testEnv struct {
	clock    *fakeClock
	resolver *recordingResolver
}

func newManager(t *testing.T, tunnels ...*wgquick.Tunnel) (*Manager, *testEnv) {
	t.Helper()
	env := &testEnv{clock: newFakeClock(), resolver: &recordingResolver{hosts: map[string][]netip.Addr{}}}
	m, err := New(tunnels, Options{
		ConnectTimeout: testConnectTimeout,
		Clock:          env.clock,
		HostResolver:   env.resolver,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m, env
}

func mustTunnel(t *testing.T, m *Manager, name string) *Tunnel {
	t.Helper()
	tun, ok := m.Tunnel(name)
	if !ok {
		t.Fatalf("Tunnel(%q) not found", name)
	}
	return tun
}

// get fetches url through the tunnel the way the route layer does:
// resolve inside the tunnel, then dial the first address.
func get(t *testing.T, tun *Tunnel, url string) (string, error) {
	t.Helper()
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			ips, err := tun.LookupNetIP(ctx, host)
			if err != nil {
				return nil, err
			}
			return tun.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
		},
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

func remoteIP(t *testing.T, body string) netip.Addr {
	t.Helper()
	ap, err := netip.ParseAddrPort(body)
	if err != nil {
		t.Fatalf("peer answered %q, want ip:port", body)
	}
	return ap.Addr()
}

func TestRequestArrivesWithTunnelSourceAddress(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	m, _ := newManager(t, peer.ClientTunnel(t, "gr"))
	tun := mustTunnel(t, m, "gr")

	body, err := get(t, tun, "http://"+tunneltest.ServerAddr4.String()+"/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := remoteIP(t, body); got != tunneltest.ClientAddr4 {
		t.Errorf("peer saw source %v, want tunnel address %v", got, tunneltest.ClientAddr4)
	}
	if !tun.Running() || !tun.Up() || tun.LastHandshake().IsZero() {
		t.Errorf("Running=%v Up=%v LastHandshake=%v, want running, up, handshake set", tun.Running(), tun.Up(), tun.LastHandshake())
	}
}

func TestHostNameResolvedByTunnelDNS(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	peer.SetHost("upstream.test", tunneltest.ServerAddr4)
	m, env := newManager(t, peer.ClientTunnel(t, "gr"))
	tun := mustTunnel(t, m, "gr")

	body, err := get(t, tun, "http://upstream.test/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := remoteIP(t, body); got != tunneltest.ClientAddr4 {
		t.Errorf("source %v, want %v", got, tunneltest.ClientAddr4)
	}
	if q := peer.Queries(); !slices.Contains(q, "upstream.test A") {
		t.Errorf("tunnel DNS queries = %v, want upstream.test A", q)
	}
	if calls := env.resolver.calls(); len(calls) != 0 {
		t.Errorf("host resolver used: %v", calls)
	}
}

func TestLookupNetIP(t *testing.T) {
	v4 := netip.MustParseAddr("198.51.100.7")
	v6 := netip.MustParseAddr("2001:db8:1::7")
	outside := netip.MustParseAddr("203.0.113.9")
	tests := []struct {
		name    string
		opts    tunneltest.Options
		host    string
		records []netip.Addr
		want    []netip.Addr
		wantErr bool
	}{
		{
			name: "IPv4-only tunnel drops AAAA answers", host: "dual.test",
			records: []netip.Addr{v6, v4}, want: []netip.Addr{v4},
		},
		{
			name: "dual-stack tunnel returns IPv4 first", host: "dual.test",
			opts: tunneltest.Options{ClientAddresses: []netip.Prefix{
				netip.PrefixFrom(tunneltest.ClientAddr4, 32), netip.PrefixFrom(tunneltest.ClientAddr6, 128),
			}},
			records: []netip.Addr{v6, v4}, want: []netip.Addr{v4, v6},
		},
		{
			name: "addresses outside AllowedIPs are dropped", host: "split.test",
			opts:    tunneltest.Options{AllowedIPs: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.51.100.0/24")}},
			records: []netip.Addr{outside, v4}, want: []netip.Addr{v4},
		},
		{
			name: "nothing reachable", host: "outside.test",
			opts:    tunneltest.Options{AllowedIPs: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}},
			records: []netip.Addr{outside}, wantErr: true,
		},
		{name: "unknown host", host: "missing.test", wantErr: true},
		{name: "IP literal", host: "198.51.100.7", want: []netip.Addr{v4}},
		{name: "IP literal outside AllowedIPs", host: "203.0.113.9", opts: tunneltest.Options{AllowedIPs: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			peer := tunneltest.Start(t, tt.opts)
			if tt.records != nil {
				peer.SetHost(tt.host, tt.records...)
			}
			m, _ := newManager(t, peer.ClientTunnel(t, "gr"))
			got, err := mustTunnel(t, m, "gr").LookupNetIP(t.Context(), tt.host)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("LookupNetIP = %v, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("LookupNetIP: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("LookupNetIP = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLookupWithoutDNS(t *testing.T) {
	tests := map[string]tunneltest.Options{
		"no DNS entry":           {NoDNS: true},
		"DNS outside AllowedIPs": {AllowedIPs: []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")}},
	}
	for name, opts := range tests {
		t.Run(name, func(t *testing.T) {
			peer := tunneltest.Start(t, opts)
			m, env := newManager(t, peer.ClientTunnel(t, "gr"))
			_, err := mustTunnel(t, m, "gr").LookupNetIP(t.Context(), "upstream.test")
			if !errors.Is(err, ErrNoDNS) {
				t.Errorf("err = %v, want ErrNoDNS", err)
			}
			if calls := env.resolver.calls(); len(calls) != 0 {
				t.Errorf("host resolver used: %v", calls)
			}
			if q := peer.Queries(); len(q) != 0 {
				t.Errorf("DNS queries sent: %v", q)
			}
		})
	}
}

func TestEndpointHostResolvedByHostResolver(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	ep := peer.Endpoint()
	conf := strings.Replace(peer.ClientConfig(), "Endpoint = "+ep.String(), "Endpoint = wg.test:"+strconv.Itoa(int(ep.Port())), 1)
	m, env := newManager(t, tunneltest.ParseTunnel(t, "gr", conf))
	env.resolver.hosts["wg.test"] = []netip.Addr{netip.MustParseAddr("::1"), ep.Addr()}

	if _, err := get(t, mustTunnel(t, m, "gr"), "http://"+tunneltest.ServerAddr4.String()+"/"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if calls := env.resolver.calls(); !slices.Equal(calls, []string{"ip wg.test"}) {
		t.Errorf("host resolver calls = %v, want [ip wg.test]", calls)
	}
}

func TestEndpointResolveFailure(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	conf := strings.Replace(peer.ClientConfig(), "Endpoint = "+peer.Endpoint().String(), "Endpoint = missing.test:51820", 1)
	m, _ := newManager(t, tunneltest.ParseTunnel(t, "gr", conf))
	tun := mustTunnel(t, m, "gr")
	_, err := tun.DialContext(t.Context(), "tcp", tunneltest.ServerAddr4.String()+":80")
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
	if tun.Running() {
		t.Error("tunnel running after failed start")
	}
}

func TestConcurrentFirstRequestsShareOneStart(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	m, _ := newManager(t, peer.ClientTunnel(t, "gr"))
	tun := mustTunnel(t, m, "gr")

	const n = 8
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			body, err := get(t, tun, "http://"+tunneltest.ServerAddr4.String()+"/")
			if err == nil && !strings.HasPrefix(body, tunneltest.ClientAddr4.String()+":") {
				err = fmt.Errorf("unexpected body %q", body)
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if got := tun.starts.Load(); got != 1 {
		t.Errorf("devices created = %d, want 1", got)
	}
}

func TestNoHandshakeFailsThenRecovers(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	peer.SetReachable(false)
	m, env := newManager(t, peer.ClientTunnel(t, "gr"))
	tun := mustTunnel(t, m, "gr")
	target := tunneltest.ServerAddr4.String() + ":80"

	done := make(chan error, 1)
	go func() {
		c, err := tun.DialContext(t.Context(), "tcp", target)
		if c != nil {
			_ = c.Close()
		}
		done <- err
	}()
	env.clock.waitTimers(1)
	select {
	case err := <-done:
		t.Fatalf("dial returned before connect_timeout: %v", err)
	default:
	}
	env.clock.advance(testConnectTimeout)
	err := <-done
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Errorf("no-handshake error is a timeout (would map to 504): %v", err)
	}
	if tun.Running() {
		t.Error("tunnel running after failed start")
	}
	if peer.Packets() == 0 {
		t.Error("peer saw no handshake attempt")
	}

	peer.SetReachable(true)
	if _, err := get(t, tun, "http://"+target+"/"); err != nil {
		t.Fatalf("after recovery: %v", err)
	}
	if got := tun.starts.Load(); got != 2 {
		t.Errorf("starts = %d, want 2 (failed + retry)", got)
	}
}

func TestOverlappingTunnels(t *testing.T) {
	handler := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, name+" "+r.RemoteAddr)
		})
	}
	a := tunneltest.Start(t, tunneltest.Options{Handler: handler("a")})
	b := tunneltest.Start(t, tunneltest.Options{Handler: handler("b")})
	m, _ := newManager(t, a.ClientTunnel(t, "a"), b.ClientTunnel(t, "b"))

	var wg sync.WaitGroup
	for _, name := range []string{"a", "b", "a", "b"} {
		wg.Go(func() {
			body, err := get(t, mustTunnel(t, m, name), "http://"+tunneltest.ServerAddr4.String()+"/")
			if err != nil {
				t.Errorf("%s: %v", name, err)
				return
			}
			gotName, addr, _ := strings.Cut(body, " ")
			if gotName != name {
				t.Errorf("request via %s answered by %s", name, gotName)
			}
			if got := remoteIP(t, addr); got != tunneltest.ClientAddr4 {
				t.Errorf("%s: source %v", name, got)
			}
		})
	}
	wg.Wait()
}

func TestDialOutsideAllowedIPsSendsNothing(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{
		AllowedIPs: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")},
	})
	m, _ := newManager(t, peer.ClientTunnel(t, "gr"))
	tun := mustTunnel(t, m, "gr")

	for _, target := range []string{"198.51.100.1:80", "[2001:db8::1]:80", "[::ffff:198.51.100.1]:80"} {
		c, err := tun.DialContext(t.Context(), "tcp", target)
		if c != nil {
			_ = c.Close()
		}
		if !errors.Is(err, ErrNotAllowed) {
			t.Errorf("dial %s: err = %v, want ErrNotAllowed", target, err)
		}
	}
	if tun.Running() {
		t.Error("device started for a rejected dial")
	}
	if n := peer.Packets(); n != 0 {
		t.Errorf("peer received %d packets, want 0", n)
	}
}

func TestDialFamilyWithoutTunnelAddress(t *testing.T) {
	// AllowedIPs covers IPv6, but the tunnel has no IPv6 address.
	peer := tunneltest.Start(t, tunneltest.Options{})
	m, _ := newManager(t, peer.ClientTunnel(t, "gr"))
	tun := mustTunnel(t, m, "gr")
	_, err := tun.DialContext(t.Context(), "tcp", "[2001:db8::1]:80")
	if !errors.Is(err, ErrNotAllowed) {
		t.Errorf("err = %v, want ErrNotAllowed", err)
	}
	if tun.Running() {
		t.Error("device started for a rejected dial")
	}
}

func TestDialValidation(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	m, _ := newManager(t, peer.ClientTunnel(t, "gr"))
	tun := mustTunnel(t, m, "gr")
	tests := []struct {
		network, address string
	}{
		{"udp", "192.0.2.1:53"},
		{"tcp", "upstream.test:80"},
		{"tcp", "192.0.2.1"},
		{"tcp6", "192.0.2.1:80"},
		{"tcp4", "[2001:db8::1]:80"},
		{"tcp", "[fe80::1%eth0]:80"},
	}
	for _, tt := range tests {
		c, err := tun.DialContext(t.Context(), tt.network, tt.address)
		if c != nil {
			_ = c.Close()
		}
		if err == nil {
			t.Errorf("DialContext(%q, %q) succeeded, want error", tt.network, tt.address)
		}
	}
	if tun.Running() {
		t.Error("device started for an invalid dial")
	}
}

func TestIPv6WithoutUpstreamIPv6FailsFast(t *testing.T) {
	// The tunnel has an IPv6 address and ::/0, the peer has no IPv6: v6
	// packets vanish, like at a provider without working IPv6.
	peer := tunneltest.Start(t, tunneltest.Options{
		ClientAddresses: []netip.Prefix{
			netip.PrefixFrom(tunneltest.ClientAddr4, 32), netip.PrefixFrom(tunneltest.ClientAddr6, 128),
		},
	})
	m, env := newManager(t, peer.ClientTunnel(t, "gr"))
	tun := mustTunnel(t, m, "gr")
	if _, err := get(t, tun, "http://"+tunneltest.ServerAddr4.String()+"/"); err != nil {
		t.Fatalf("IPv4 get: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		c, err := tun.DialContext(t.Context(), "tcp", "["+tunneltest.ServerAddr6.String()+"]:80")
		if c != nil {
			_ = c.Close()
		}
		done <- err
	}()
	env.clock.waitTimers(1)
	env.clock.advance(DefaultIPv6ConnectTimeout)
	err := <-done
	if !errors.Is(err, ErrIPv6Unreachable) {
		t.Fatalf("err = %v, want ErrIPv6Unreachable", err)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Errorf("IPv6 fallback error is a timeout: %v", err)
	}
	// IPv4 keeps working.
	if _, err := get(t, tun, "http://"+tunneltest.ServerAddr4.String()+"/"); err != nil {
		t.Fatalf("IPv4 get after IPv6 failure: %v", err)
	}
}

func TestIPv6ThroughTunnel(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{
		ClientAddresses: []netip.Prefix{
			netip.PrefixFrom(tunneltest.ClientAddr4, 32), netip.PrefixFrom(tunneltest.ClientAddr6, 128),
		},
		ServerAddresses: []netip.Addr{tunneltest.ServerAddr4, tunneltest.ServerAddr6},
	})
	m, _ := newManager(t, peer.ClientTunnel(t, "gr"))
	body, err := get(t, mustTunnel(t, m, "gr"), "http://["+tunneltest.ServerAddr6.String()+"]/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := remoteIP(t, body); got != tunneltest.ClientAddr6 {
		t.Errorf("source %v, want %v", got, tunneltest.ClientAddr6)
	}
}

func TestCallerContextEndsWhileStarting(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	peer.SetReachable(false)
	m, env := newManager(t, peer.ClientTunnel(t, "gr"))
	tun := mustTunnel(t, m, "gr")

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := tun.LookupNetIP(ctx, "upstream.test")
		done <- err
	}()
	env.clock.waitTimers(1)
	cancel()
	err := <-done
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want ErrUnavailable and context.Canceled", err)
	}
	// Close aborts the pending start.
	if err := m.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestClose(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	m, _ := newManager(t, peer.ClientTunnel(t, "gr"))
	tun := mustTunnel(t, m, "gr")
	if _, err := get(t, tun, "http://"+tunneltest.ServerAddr4.String()+"/"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if tun.Running() || tun.Up() || !tun.LastHandshake().IsZero() {
		t.Error("tunnel still running after Close")
	}
	if _, err := tun.DialContext(t.Context(), "tcp", tunneltest.ServerAddr4.String()+":80"); !errors.Is(err, ErrClosed) {
		t.Errorf("dial after Close: err = %v, want ErrClosed", err)
	}
	if _, err := tun.LookupNetIP(t.Context(), "upstream.test"); !errors.Is(err, ErrClosed) {
		t.Errorf("lookup after Close: err = %v, want ErrClosed", err)
	}
}

func TestStatus(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	m, env := newManager(t, peer.ClientTunnel(t, "gr"))
	tun := mustTunnel(t, m, "gr")
	if tun.Name() != "gr" {
		t.Errorf("Name = %q", tun.Name())
	}
	if tun.Running() || tun.Up() || !tun.LastHandshake().IsZero() || !tun.LastActivity().IsZero() {
		t.Error("fresh tunnel reports state")
	}
	if _, err := get(t, tun, "http://"+tunneltest.ServerAddr4.String()+"/"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := tun.LastActivity(); !got.Equal(env.clock.Now()) {
		t.Errorf("LastActivity = %v, want %v", got, env.clock.Now())
	}
	if !tun.Up() {
		t.Error("Up = false after handshake")
	}
	env.clock.advance(handshakeFresh + time.Minute)
	if tun.Up() {
		t.Error("Up = true with a handshake older than 180s")
	}
	if !tun.Running() {
		t.Error("Running = false")
	}
}

func TestManagerTunnels(t *testing.T) {
	a := tunneltest.Start(t, tunneltest.Options{})
	b := tunneltest.Start(t, tunneltest.Options{})
	m, _ := newManager(t, b.ClientTunnel(t, "b"), a.ClientTunnel(t, "a"))
	var names []string
	for _, tun := range m.Tunnels() {
		names = append(names, tun.Name())
	}
	if !slices.Equal(names, []string{"a", "b"}) {
		t.Errorf("Tunnels = %v, want [a b]", names)
	}
	if _, ok := m.Tunnel("missing"); ok {
		t.Error("Tunnel(missing) found")
	}
}

func TestCallerDeadlineWhileStartingIsNotATimeout(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	peer.SetReachable(false)
	m, _ := newManager(t, peer.ClientTunnel(t, "gr"))
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := mustTunnel(t, m, "gr").DialContext(ctx, "tcp", tunneltest.ServerAddr4.String()+":80")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		t.Errorf("start deadline error is a timeout (would map to 504): %v", err)
	}
}

func TestDialRefusedReturnsNilConn(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	m, _ := newManager(t, peer.ClientTunnel(t, "gr"))
	c, err := mustTunnel(t, m, "gr").DialContext(t.Context(), "tcp", tunneltest.ServerAddr4.String()+":81")
	if err == nil {
		t.Fatal("dial to a closed port succeeded")
	}
	if c != nil {
		t.Errorf("conn = %#v, want nil interface", c)
	}
}
