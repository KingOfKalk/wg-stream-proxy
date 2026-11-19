// SPDX-License-Identifier: AGPL-3.0-or-later

package route

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
)

var testUpstream = config.Upstream{
	UserAgent:      "test-agent/1.0",
	ConnectTimeout: 10 * time.Second,
	HeaderTimeout:  15 * time.Second,
	ReadTimeout:    30 * time.Second,
}

// testRoute returns a route "gr" over n with the production guard plus
// the given exceptions.
func testRoute(n Network, clk Clock, allow ...string) *Route {
	var prefixes []netip.Prefix
	for _, s := range allow {
		prefixes = append(prefixes, netip.MustParsePrefix(s))
	}
	return newRoute("gr", n, Options{
		Upstream:     testUpstream,
		AllowPrivate: map[string][]netip.Prefix{"gr": prefixes},
		Clock:        clk,
	})
}

func addrs(ss ...string) []netip.Addr {
	var out []netip.Addr
	for _, s := range ss {
		out = append(out, netip.MustParseAddr(s))
	}
	return out
}

// dialResult is the outcome of a dial started in the background.
type dialResult struct {
	conn net.Conn
	err  error
}

func dialAsync(r *Route, ctx context.Context, network, address string) <-chan dialResult {
	ch := make(chan dialResult, 1)
	go func() {
		c, err := r.dial(ctx, network, address)
		ch <- dialResult{c, err}
	}()
	return ch
}

func TestDialImmediate(t *testing.T) {
	tests := []struct {
		name        string
		network     string
		address     string
		hosts       map[string][]netip.Addr
		failing     []string // addresses whose dial fails at once
		allow       []string
		wantLookups []string
		wantDialed  []string
		wantErr     error // nil: success
		wantReason  Reason
	}{
		{
			name:       "IPv4 literal skips lookup",
			network:    "tcp",
			address:    "203.0.113.5:80",
			wantDialed: []string{"tcp4 203.0.113.5:80"},
		},
		{
			name:       "IPv6 literal skips lookup",
			network:    "tcp",
			address:    "[2001:db8::5]:443",
			wantDialed: []string{"tcp6 [2001:db8::5]:443"},
		},
		{
			name:        "hostname resolved in the network",
			network:     "tcp",
			address:     "cdn.example.com:8080",
			hosts:       map[string][]netip.Addr{"cdn.example.com": addrs("203.0.113.10")},
			wantLookups: []string{"cdn.example.com"},
			wantDialed:  []string{"tcp4 203.0.113.10:8080"},
		},
		{
			name:        "IPv4-mapped answer dialed as IPv4",
			network:     "tcp",
			address:     "cdn.example.com:80",
			hosts:       map[string][]netip.Addr{"cdn.example.com": addrs("::ffff:203.0.113.10")},
			wantLookups: []string{"cdn.example.com"},
			wantDialed:  []string{"tcp4 203.0.113.10:80"},
		},
		{
			name:        "unknown host",
			network:     "tcp",
			address:     "missing.example.com:80",
			wantLookups: []string{"missing.example.com"},
			wantErr:     &net.DNSError{},
			wantReason:  ReasonConnect,
		},
		{
			name:       "private literal blocked",
			network:    "tcp",
			address:    "10.0.0.1:80",
			wantErr:    ErrBlocked,
			wantReason: ReasonGuard,
		},
		{
			name:        "private answer blocked",
			network:     "tcp",
			address:     "rebind.example.com:80",
			hosts:       map[string][]netip.Addr{"rebind.example.com": addrs("192.168.1.5", "::1")},
			wantLookups: []string{"rebind.example.com"},
			wantErr:     ErrBlocked,
			wantReason:  ReasonGuard,
		},
		{
			name:        "private candidates dropped from mixed answer",
			network:     "tcp",
			address:     "mixed.example.com:80",
			hosts:       map[string][]netip.Addr{"mixed.example.com": addrs("127.0.0.1", "203.0.113.10", "fd00::1")},
			wantLookups: []string{"mixed.example.com"},
			wantDialed:  []string{"tcp4 203.0.113.10:80"},
		},
		{
			name:       "allowlisted private literal",
			network:    "tcp",
			address:    "10.8.0.1:80",
			allow:      []string{"10.8.0.0/24"},
			wantDialed: []string{"tcp4 10.8.0.1:80"},
		},
		{
			name:        "tcp4 skips IPv6 answers",
			network:     "tcp4",
			address:     "dual.example.com:80",
			hosts:       map[string][]netip.Addr{"dual.example.com": addrs("2001:db8::1", "203.0.113.1")},
			wantLookups: []string{"dual.example.com"},
			wantDialed:  []string{"tcp4 203.0.113.1:80"},
		},
		{
			name:        "tcp6 without IPv6 answers",
			network:     "tcp6",
			address:     "v4.example.com:80",
			hosts:       map[string][]netip.Addr{"v4.example.com": addrs("203.0.113.1")},
			wantLookups: []string{"v4.example.com"},
			wantErr:     errNoAddress,
			wantReason:  ReasonConnect,
		},
		{
			name:        "IPv6 unreachable falls back to IPv4 at once",
			network:     "tcp",
			address:     "dual.example.com:80",
			hosts:       map[string][]netip.Addr{"dual.example.com": addrs("2001:db8::1", "203.0.113.1")},
			failing:     []string{"[2001:db8::1]:80"},
			wantLookups: []string{"dual.example.com"},
			wantDialed:  []string{"tcp6 [2001:db8::1]:80", "tcp4 203.0.113.1:80"},
		},
		{
			name:        "families interleaved, first error returned",
			network:     "tcp",
			address:     "dual.example.com:80",
			hosts:       map[string][]netip.Addr{"dual.example.com": addrs("2001:db8::1", "2001:db8::2", "203.0.113.1")},
			failing:     []string{"[2001:db8::1]:80", "[2001:db8::2]:80", "203.0.113.1:80"},
			wantLookups: []string{"dual.example.com"},
			wantDialed:  []string{"tcp6 [2001:db8::1]:80", "tcp4 203.0.113.1:80", "tcp6 [2001:db8::2]:80"},
			wantErr:     syscall.ENETUNREACH,
			wantReason:  ReasonConnect,
		},
		{
			name:       "invalid address",
			network:    "tcp",
			address:    "no-port",
			wantErr:    errInvalidAddress,
			wantReason: ReasonConnect,
		},
		{
			name:       "unsupported network",
			network:    "udp",
			address:    "203.0.113.5:53",
			wantErr:    errInvalidAddress,
			wantReason: ReasonConnect,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := &fakeNet{hosts: tt.hosts, dials: map[string]dialFunc{}}
			for _, a := range tt.failing {
				n.dials[a] = fail(syscall.ENETUNREACH)
			}
			peers := make(chan net.Conn, 4)
			n.fallback = func(context.Context, string, string) (net.Conn, error) {
				c, s := net.Pipe()
				peers <- s
				return c, nil
			}
			r := testRoute(n, newFakeClock(), tt.allow...)

			conn, err := r.dial(context.Background(), tt.network, tt.address)
			if conn != nil {
				_ = conn.Close()
				_ = (<-peers).Close()
			}
			if tt.wantErr == nil && err != nil {
				t.Fatalf("dial: %v", err)
			}
			if tt.wantErr != nil {
				var dnsErr *net.DNSError
				if errors.As(tt.wantErr, &dnsErr) {
					if !errors.As(err, &dnsErr) {
						t.Fatalf("dial error = %v, want a DNS error", err)
					}
				} else if !errors.Is(err, tt.wantErr) {
					t.Fatalf("dial error = %v, want %v", err, tt.wantErr)
				}
				if got := Classify(err); got != tt.wantReason {
					t.Errorf("Classify(%v) = %q, want %q", err, got, tt.wantReason)
				}
			}
			if got := n.Lookups(); !slices.Equal(got, tt.wantLookups) {
				t.Errorf("lookups = %q, want %q", got, tt.wantLookups)
			}
			if got := n.Dialed(); !slices.Equal(got, tt.wantDialed) {
				t.Errorf("dials = %q, want %q", got, tt.wantDialed)
			}
		})
	}
}

// A tunnel with ::/0 in AllowedIPs but no working IPv6 upstream: the IPv6
// attempt hangs, IPv4 must take over after the fallback delay instead of
// waiting for connect_timeout.
func TestDialIPv6HangsFallsBackToIPv4(t *testing.T) {
	peers := make(chan net.Conn, 1)
	n := &fakeNet{
		hosts:   map[string][]netip.Addr{"dual.example.com": addrs("2001:db8::1", "203.0.113.1")},
		dials:   map[string]dialFunc{"[2001:db8::1]:443": hang, "203.0.113.1:443": pipe(peers)},
		started: make(chan string, 2),
	}
	clk := newFakeClock()
	r := testRoute(n, clk)

	res := dialAsync(r, context.Background(), "tcp", "dual.example.com:443")
	if got := <-n.started; got != "[2001:db8::1]:443" {
		t.Fatalf("first attempt = %s, want IPv6", got)
	}
	clk.Advance(fallbackDelay)
	if got := <-n.started; got != "203.0.113.1:443" {
		t.Fatalf("second attempt = %s, want IPv4", got)
	}
	out := <-res
	if out.err != nil {
		t.Fatalf("dial: %v", out.err)
	}
	peer := <-peers
	// The connection is the IPv4 pipe: what we write arrives at its peer.
	go func() { _, _ = out.conn.Write([]byte("x")) }()
	buf := make([]byte, 1)
	if _, err := io.ReadFull(peer, buf); err != nil {
		t.Fatalf("read from IPv4 peer: %v", err)
	}
	_ = out.conn.Close()
	_ = peer.Close()
}

func TestDialLateWinnerClosed(t *testing.T) {
	v6peers := make(chan net.Conn, 1)
	v4peers := make(chan net.Conn, 1)
	release := make(chan struct{})
	n := &fakeNet{
		hosts: map[string][]netip.Addr{"dual.example.com": addrs("2001:db8::1", "203.0.113.1")},
		dials: map[string]dialFunc{
			"[2001:db8::1]:80": func(ctx context.Context) (net.Conn, error) {
				<-release // ignores cancellation until released, then succeeds
				return pipe(v6peers)(ctx)
			},
			"203.0.113.1:80": pipe(v4peers),
		},
		started: make(chan string, 2),
	}
	clk := newFakeClock()
	r := testRoute(n, clk)

	res := dialAsync(r, context.Background(), "tcp", "dual.example.com:80")
	<-n.started
	clk.Advance(fallbackDelay)
	<-n.started
	out := <-res
	if out.err != nil {
		t.Fatalf("dial: %v", out.err)
	}
	v4 := <-v4peers
	close(release)
	v6 := <-v6peers
	// The late IPv6 connection is closed: its peer reads EOF.
	if _, err := v6.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("late IPv6 peer read = %v, want EOF", err)
	}
	_ = v6.Close()
	_ = out.conn.Close()
	_ = v4.Close()
}

func TestDialConnectTimeout(t *testing.T) {
	n := &fakeNet{
		hosts:   map[string][]netip.Addr{"slow.example.com": addrs("203.0.113.1")},
		dials:   map[string]dialFunc{"203.0.113.1:80": hang},
		started: make(chan string, 1),
	}
	clk := newFakeClock()
	r := testRoute(n, clk)

	res := dialAsync(r, context.Background(), "tcp", "slow.example.com:80")
	<-n.started
	clk.Advance(fallbackDelay) // a single address: nothing else to try
	clk.Advance(testUpstream.ConnectTimeout)
	out := <-res
	if !errors.Is(out.err, ErrConnectTimeout) {
		t.Fatalf("dial error = %v, want ErrConnectTimeout", out.err)
	}
	if got := Classify(out.err); got != ReasonConnect {
		t.Errorf("Classify = %q, want %q", got, ReasonConnect)
	}
	if got := ErrorStatus(out.err); got != 502 {
		t.Errorf("ErrorStatus = %d, want 502", got)
	}
}

func TestDialLookupHonoursConnectTimeout(t *testing.T) {
	started := make(chan struct{})
	n := &blockingLookupNet{started: started}
	clk := newFakeClock()
	r := testRoute(n, clk)

	res := dialAsync(r, context.Background(), "tcp", "slow-dns.example.com:80")
	<-started
	clk.Advance(testUpstream.ConnectTimeout)
	if out := <-res; !errors.Is(out.err, ErrConnectTimeout) {
		t.Fatalf("dial error = %v, want ErrConnectTimeout", out.err)
	}
}

// blockingLookupNet blocks every lookup until ctx is done; then it fails
// with ctx.Err(), wrapped in err if set.
type blockingLookupNet struct {
	started chan struct{}
	err     error
}

func (n *blockingLookupNet) LookupNetIP(ctx context.Context, _ string) ([]netip.Addr, error) {
	close(n.started)
	<-ctx.Done()
	if n.err != nil {
		return nil, fmt.Errorf("%w: %w", n.err, ctx.Err())
	}
	return nil, ctx.Err()
}

func (n *blockingLookupNet) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, errNoRoute
}

func TestDialParentCancel(t *testing.T) {
	n := &fakeNet{
		dials:   map[string]dialFunc{"203.0.113.1:80": hang},
		started: make(chan string, 1),
	}
	r := testRoute(n, newFakeClock())
	ctx, cancel := context.WithCancel(context.Background())

	res := dialAsync(r, ctx, "tcp", "203.0.113.1:80")
	<-n.started
	cancel()
	out := <-res
	if !errors.Is(out.err, context.Canceled) {
		t.Fatalf("dial error = %v, want context.Canceled", out.err)
	}
}

func TestInterleave(t *testing.T) {
	tests := []struct {
		in, want []string
	}{
		{nil, nil},
		{[]string{"203.0.113.1"}, []string{"203.0.113.1"}},
		{
			[]string{"203.0.113.1", "203.0.113.2", "2001:db8::1", "2001:db8::2"},
			[]string{"203.0.113.1", "2001:db8::1", "203.0.113.2", "2001:db8::2"},
		},
		{
			[]string{"2001:db8::1", "203.0.113.1", "203.0.113.2", "203.0.113.3"},
			[]string{"2001:db8::1", "203.0.113.1", "203.0.113.2", "203.0.113.3"},
		},
	}
	for _, tt := range tests {
		got := interleave(addrs(tt.in...))
		var gotS []string
		for _, a := range got {
			gotS = append(gotS, a.String())
		}
		if !slices.Equal(gotS, tt.want) {
			t.Errorf("interleave(%q) = %q, want %q", tt.in, gotS, tt.want)
		}
	}
}

// errNetworkDown stands for a network error that explains a connect
// timeout, like a tunnel whose start did not finish (tunnel.ErrUnavailable).
var errNetworkDown = errors.New("fake: network not started")

// TestDialConnectTimeoutKeepsNetworkError: when connect_timeout ends a
// lookup or dial that waited for the network, the error is still a
// connect timeout and also carries the network's own error, so the server
// can tell a tunnel that did not start (docs/clips.md 3.2).
func TestDialConnectTimeoutKeepsNetworkError(t *testing.T) {
	down := func(ctx context.Context) (net.Conn, error) {
		<-ctx.Done()
		return nil, fmt.Errorf("%w: start not finished: %w", errNetworkDown, ctx.Err())
	}
	tests := []struct {
		name string
		n    Network
		addr string
	}{
		{
			name: "lookup",
			n:    &blockingLookupNet{started: make(chan struct{}), err: errNetworkDown},
			addr: "slow-dns.example.com:80",
		},
		{
			name: "dial",
			n:    &fakeNet{dials: map[string]dialFunc{"203.0.113.1:80": down}, started: make(chan string, 1)},
			addr: "203.0.113.1:80",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk := newFakeClock()
			r := testRoute(tt.n, clk)
			res := dialAsync(r, context.Background(), "tcp", tt.addr)
			switch n := tt.n.(type) {
			case *blockingLookupNet:
				<-n.started
			case *fakeNet:
				<-n.started
			}
			clk.Advance(testUpstream.ConnectTimeout)
			out := <-res
			if !errors.Is(out.err, ErrConnectTimeout) || !errors.Is(out.err, errNetworkDown) {
				t.Fatalf("dial error = %v, want ErrConnectTimeout and the network's error", out.err)
			}
			if got := Classify(out.err); got != ReasonConnect {
				t.Errorf("Classify = %q, want %q", got, ReasonConnect)
			}
			if got := ErrorStatus(out.err); got != 502 {
				t.Errorf("ErrorStatus = %d, want 502", got)
			}
		})
	}
}
