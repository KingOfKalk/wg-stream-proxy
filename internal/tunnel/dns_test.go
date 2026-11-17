// SPDX-License-Identifier: AGPL-3.0-or-later

package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel/tunneltest"
)

var (
	upstream4 = netip.MustParseAddr("198.51.100.7")
	upstream6 = netip.MustParseAddr("2001:db8:1::7")
	// deadDNS is inside AllowedIPs, but no peer address: queries vanish.
	deadDNS = netip.MustParseAddr("192.0.2.99")
	// secondDNS is a second address of the peer, with its own DNS server.
	secondDNS = netip.MustParseAddr("192.0.2.53")
)

// startedTunnel returns a running tunnel to peer whose lookups use the
// fake clock. Starting it sends no DNS query.
func startedTunnel(t *testing.T, peer *tunneltest.Peer) (*Tunnel, *testEnv) {
	t.Helper()
	m, env := newManager(t, peer.ClientTunnel(t, "gr"))
	tun := mustTunnel(t, m, "gr")
	if _, err := tun.LookupNetIP(t.Context(), tunneltest.ServerAddr4.String()); err != nil {
		t.Fatalf("start: %v", err)
	}
	return tun, env
}

type lookupResult struct {
	addrs []netip.Addr
	err   error
}

func lookupAsync(ctx context.Context, tun *Tunnel, host string) <-chan lookupResult {
	ch := make(chan lookupResult, 1)
	go func() {
		addrs, err := tun.LookupNetIP(ctx, host)
		ch <- lookupResult{addrs, err}
	}()
	return ch
}

func assertPending(t *testing.T, ch <-chan lookupResult) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("lookup returned early: %v, %v", r.addrs, r.err)
	default:
	}
}

func TestLookupRetriesLostQueryAfterOneSecond(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	peer.SetHost("upstream.test", upstream4)
	tun, env := startedTunnel(t, peer)
	peer.DropDNS(1)

	done := lookupAsync(t.Context(), tun, "upstream.test")
	env.clock.waitTimers(1)
	if got := env.clock.next(); got != time.Second {
		t.Fatalf("first retry in %v, want 1s", got)
	}
	env.clock.advance(time.Second - time.Millisecond)
	assertPending(t, done)
	env.clock.advance(time.Millisecond)

	r := <-done
	if r.err != nil {
		t.Fatalf("LookupNetIP: %v", r.err)
	}
	if !slices.Equal(r.addrs, []netip.Addr{upstream4}) {
		t.Errorf("LookupNetIP = %v, want [%v]", r.addrs, upstream4)
	}
	if got := peer.DroppedDNS(); got != 1 {
		t.Errorf("dropped queries = %d, want 1", got)
	}
	if got := env.clock.pending(); got != 0 {
		t.Errorf("%d timers left armed", got)
	}
}

func TestLookupAllQueriesLost(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	peer.SetHost("upstream.test", upstream4)
	tun, env := startedTunnel(t, peer)
	peer.DropDNS(-1)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := lookupAsync(ctx, tun, "upstream.test")
	// Retries after 1s, 2s, then every 4s, until the caller gives up.
	for _, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second} {
		env.clock.waitTimers(1)
		if got := env.clock.next(); got != want {
			t.Fatalf("next retry in %v, want %v", got, want)
		}
		assertPending(t, done)
		env.clock.advance(want)
	}
	env.clock.waitTimers(1)
	cancel()
	r := <-done
	if !errors.Is(r.err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", r.err)
	}
	if got := env.clock.pending(); got != 0 {
		t.Errorf("%d timers left armed", got)
	}
	if len(peer.Queries()) != 0 {
		t.Errorf("answered queries: %v", peer.Queries())
	}
}

func TestLookupAllQueriesLostEndsAtDeadline(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	peer.SetHost("upstream.test", upstream4)
	tun, _ := startedTunnel(t, peer)
	peer.DropDNS(-1)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := tun.LookupNetIP(ctx, "upstream.test")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestLookupTruncatedAnswerFallsBackToTCP(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{ClientAddresses: []netip.Prefix{
		netip.PrefixFrom(tunneltest.ClientAddr4, 32), netip.PrefixFrom(tunneltest.ClientAddr6, 128),
	}})
	peer.SetHost("upstream.test", upstream6, upstream4)
	peer.SetTruncate(true)
	tun, _ := startedTunnel(t, peer)

	got, err := tun.LookupNetIP(t.Context(), "upstream.test")
	if err != nil {
		t.Fatalf("LookupNetIP: %v", err)
	}
	if want := []netip.Addr{upstream4, upstream6}; !slices.Equal(got, want) {
		t.Errorf("LookupNetIP = %v, want %v", got, want)
	}
	tcp := peer.TCPQueries()
	slices.Sort(tcp)
	if want := []string{"upstream.test A", "upstream.test AAAA"}; !slices.Equal(tcp, want) {
		t.Errorf("TCP queries = %v, want %v", tcp, want)
	}
}

func TestLookupFirstServerDeadSecondAnswersAfterRetry(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{DNS: []netip.Addr{deadDNS, tunneltest.ServerAddr4}})
	peer.SetHost("upstream.test", upstream4)
	tun, env := startedTunnel(t, peer)

	done := lookupAsync(t.Context(), tun, "upstream.test")
	env.clock.waitTimers(1)
	assertPending(t, done)
	env.clock.advance(time.Second)
	r := <-done
	if r.err != nil {
		t.Fatalf("LookupNetIP: %v", r.err)
	}
	if !slices.Equal(r.addrs, []netip.Addr{upstream4}) {
		t.Errorf("LookupNetIP = %v, want [%v]", r.addrs, upstream4)
	}
}

func TestLookupServerFailureTriesNextServerAtOnce(t *testing.T) {
	opts := tunneltest.Options{
		ServerAddresses: []netip.Addr{tunneltest.ServerAddr4, secondDNS},
		DNS:             []netip.Addr{tunneltest.ServerAddr4, secondDNS},
	}
	t.Run("second server answers", func(t *testing.T) {
		peer := tunneltest.Start(t, opts)
		peer.SetHost("upstream.test", upstream4)
		peer.FailDNS(1, dnsmessage.RCodeServerFailure)
		tun, _ := startedTunnel(t, peer)
		// No clock advance: the next server is asked at once.
		got, err := tun.LookupNetIP(t.Context(), "upstream.test")
		if err != nil {
			t.Fatalf("LookupNetIP: %v", err)
		}
		if !slices.Equal(got, []netip.Addr{upstream4}) {
			t.Errorf("LookupNetIP = %v, want [%v]", got, upstream4)
		}
	})
	t.Run("every server fails", func(t *testing.T) {
		peer := tunneltest.Start(t, opts)
		peer.SetHost("upstream.test", upstream4)
		peer.FailDNS(-1, dnsmessage.RCodeServerFailure)
		tun, _ := startedTunnel(t, peer)
		_, err := tun.LookupNetIP(t.Context(), "upstream.test")
		var de *net.DNSError
		if !errors.As(err, &de) || !de.IsTemporary || de.IsNotFound {
			t.Fatalf("err = %v, want temporary *net.DNSError", err)
		}
		if got := len(peer.Queries()); got != 2 {
			t.Errorf("queries = %d, want one per server", got)
		}
	})
}

func TestLookupFollowsCNAMEChain(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	peer.SetCNAME("alias.test", "mid.test")
	peer.SetCNAME("mid.test", "cdn.test")
	peer.SetHost("cdn.test", upstream4)
	tun, _ := startedTunnel(t, peer)

	got, err := tun.LookupNetIP(t.Context(), "alias.test")
	if err != nil {
		t.Fatalf("LookupNetIP: %v", err)
	}
	if !slices.Equal(got, []netip.Addr{upstream4}) {
		t.Errorf("LookupNetIP = %v, want [%v]", got, upstream4)
	}
}

func TestLookupNXDOMAINIsNotRetried(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{ClientAddresses: []netip.Prefix{
		netip.PrefixFrom(tunneltest.ClientAddr4, 32), netip.PrefixFrom(tunneltest.ClientAddr6, 128),
	}})
	tun, env := startedTunnel(t, peer)

	_, err := tun.LookupNetIP(t.Context(), "missing.test")
	var de *net.DNSError
	if !errors.As(err, &de) || !de.IsNotFound {
		t.Fatalf("err = %v, want not-found *net.DNSError", err)
	}
	if got := env.clock.pending(); got != 0 {
		t.Errorf("%d retry timers left armed", got)
	}
	if q := peer.Queries(); len(q) > 2 {
		t.Errorf("queries = %v, want at most one per family", q)
	}
}

func TestLookupQueriesOnlyTunnelFamilies(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	peer.SetHost("upstream.test", upstream4, upstream6)
	tun, _ := startedTunnel(t, peer)
	if _, err := tun.LookupNetIP(t.Context(), "upstream.test"); err != nil {
		t.Fatalf("LookupNetIP: %v", err)
	}
	if q := peer.Queries(); !slices.Equal(q, []string{"upstream.test A"}) {
		t.Errorf("queries = %v, want only A on an IPv4-only tunnel", q)
	}
}

func TestLookupInvalidName(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	tun, _ := startedTunnel(t, peer)
	long := ""
	for range 64 {
		long += "a"
	}
	for _, host := range []string{"", ".", long + ".test", "a..test"} {
		_, err := tun.LookupNetIP(t.Context(), host)
		var de *net.DNSError
		if !errors.As(err, &de) || !de.IsNotFound {
			t.Errorf("LookupNetIP(%q): err = %v, want not-found *net.DNSError", host, err)
		}
	}
	if q := peer.Queries(); len(q) != 0 {
		t.Errorf("queries sent for invalid names: %v", q)
	}
}

func TestConcurrentLookups(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	const hosts = 4
	for i := range hosts {
		peer.SetHost(fmt.Sprintf("h%d.test", i), netip.AddrFrom4([4]byte{198, 51, 100, byte(i + 1)}))
	}
	tun, _ := startedTunnel(t, peer)

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			h := i % hosts
			got, err := tun.LookupNetIP(t.Context(), fmt.Sprintf("h%d.test", h))
			want := netip.AddrFrom4([4]byte{198, 51, 100, byte(h + 1)})
			if err != nil || !slices.Equal(got, []netip.Addr{want}) {
				t.Errorf("h%d.test: %v, %v; want [%v]", h, got, err, want)
			}
		})
	}
	wg.Wait()
}

// response builds a DNS answer for parseResponse tests.
func response(t *testing.T, h dnsmessage.Header, q dnsmessage.Question, answers func(b *dnsmessage.Builder) error) []byte {
	t.Helper()
	h.Response = true
	b := dnsmessage.NewBuilder(nil, h)
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := b.Question(q); err != nil {
		t.Fatal(err)
	}
	if err := b.StartAnswers(); err != nil {
		t.Fatal(err)
	}
	if answers != nil {
		if err := answers(&b); err != nil {
			t.Fatal(err)
		}
	}
	msg, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func TestParseResponse(t *testing.T) {
	name := dnsmessage.MustNewName("Upstream.Test.")
	q := dnsmessage.Question{Name: dnsmessage.MustNewName("upstream.test."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}
	rr := func(n string, typ dnsmessage.Type) dnsmessage.ResourceHeader {
		return dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(n), Type: typ, Class: dnsmessage.ClassINET, TTL: 60}
	}
	a := func(n string, ip netip.Addr) func(*dnsmessage.Builder) error {
		return func(b *dnsmessage.Builder) error {
			return b.AResource(rr(n, dnsmessage.TypeA), dnsmessage.AResource{A: ip.As4()})
		}
	}
	cname := func(n, target string) func(*dnsmessage.Builder) error {
		return func(b *dnsmessage.Builder) error {
			return b.CNAMEResource(rr(n, dnsmessage.TypeCNAME), dnsmessage.CNAMEResource{CNAME: dnsmessage.MustNewName(target)})
		}
	}
	all := func(fs ...func(*dnsmessage.Builder) error) func(*dnsmessage.Builder) error {
		return func(b *dnsmessage.Builder) error {
			for _, f := range fs {
				if err := f(b); err != nil {
					return err
				}
			}
			return nil
		}
	}
	other := netip.MustParseAddr("203.0.113.9")
	tests := []struct {
		name    string
		msg     []byte
		want    []netip.Addr
		stray   bool
		wantErr bool
	}{
		{name: "answer, name case-insensitive", msg: response(t, dnsmessage.Header{ID: 7}, dnsmessage.Question{Name: name, Type: q.Type, Class: q.Class}, a("upstream.test.", upstream4)), want: []netip.Addr{upstream4}},
		{name: "wrong ID", msg: response(t, dnsmessage.Header{ID: 8}, q, a("upstream.test.", upstream4)), stray: true},
		{name: "wrong question name", msg: response(t, dnsmessage.Header{ID: 7}, dnsmessage.Question{Name: dnsmessage.MustNewName("other.test."), Type: q.Type, Class: q.Class}, nil), stray: true},
		{name: "wrong question type", msg: response(t, dnsmessage.Header{ID: 7}, dnsmessage.Question{Name: q.Name, Type: dnsmessage.TypeAAAA, Class: q.Class}, nil), stray: true},
		{name: "garbage", msg: []byte{0, 7, 1}, stray: true},
		{name: "records of other names ignored", msg: response(t, dnsmessage.Header{ID: 7}, q, all(a("other.test.", other), a("upstream.test.", upstream4))), want: []netip.Addr{upstream4}},
		{name: "CNAME chain out of order", msg: response(t, dnsmessage.Header{ID: 7}, q, all(a("cdn.test.", upstream4), cname("mid.test.", "cdn.test."), cname("upstream.test.", "mid.test."))), want: []netip.Addr{upstream4}},
		{name: "CNAME loop", msg: response(t, dnsmessage.Header{ID: 7}, q, all(cname("upstream.test.", "loop.test."), cname("loop.test.", "upstream.test."))), want: nil},
		{name: "truncated answer section", msg: response(t, dnsmessage.Header{ID: 7}, q, a("upstream.test.", upstream4))[:40], wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, got, err := parseResponse(tt.msg, 7, q)
			switch {
			case tt.stray:
				if !errors.Is(err, errStray) {
					t.Errorf("err = %v, want errStray", err)
				}
				return
			case tt.wantErr:
				if err == nil || errors.Is(err, errStray) {
					t.Errorf("err = %v, want parse error", err)
				}
				return
			case err != nil:
				t.Fatalf("parseResponse: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("addrs = %v, want %v", got, tt.want)
			}
		})
	}
}
