// SPDX-License-Identifier: AGPL-3.0-or-later

package tunnel

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"runtime"
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
// fake clock. Starting it sends no DNS query; its lookups retry like
// those of a tunnel whose DNS servers answered before (no warm-up, see
// freshTunnel).
func startedTunnel(t *testing.T, peer *tunneltest.Peer) (*Tunnel, *testEnv) {
	t.Helper()
	tun, env := freshTunnel(t, peer)
	tun.run.Load().dns.answered.Store(true)
	return tun, env
}

// freshTunnel is startedTunnel before the first DNS answer since the
// start: its lookups retry with the warm-up schedule.
func freshTunnel(t *testing.T, peer *tunneltest.Peer) (*Tunnel, *testEnv) {
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

// waitWaiters blocks until n lookups of host wait for one shared query
// of the running tun. It fails t after 10s.
func waitWaiters(t *testing.T, tun *Tunnel, host string, n int) {
	t.Helper()
	c := tun.run.Load().dns
	giveUp := time.Now().Add(10 * time.Second)
	for {
		c.mu.Lock()
		got := 0
		if f := c.pending[host]; f != nil {
			got = f.waiters
		}
		c.mu.Unlock()
		if got >= n {
			return
		}
		if time.Now().After(giveUp) {
			t.Fatalf("%d lookups of %s wait after 10s, want %d", got, host, n)
		}
		runtime.Gosched()
	}
}

func TestLookupRetriesLostQueryAfterHalfASecond(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	peer.SetHost("upstream.test", upstream4)
	tun, env := startedTunnel(t, peer)
	peer.DropDNS(1)

	done := lookupAsync(t.Context(), tun, "upstream.test")
	env.clock.waitTimers(1)
	if got := env.clock.next(); got != 500*time.Millisecond {
		t.Fatalf("first retry in %v, want 0.5s", got)
	}
	env.clock.advance(500*time.Millisecond - time.Millisecond)
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
	// Retries after 0.5s, 1s, 2s, then every 4s, until the caller gives up.
	for _, want := range []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second} {
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

func TestLookupRetrySchedule(t *testing.T) {
	warmup := slices.Repeat([]time.Duration{250 * time.Millisecond}, 32)
	tests := []struct {
		name string
		// answered: a DNS server answered since the tunnel started.
		answered bool
		// delays are the times from one attempt to the next; sent[i] is
		// the number of UDP queries attempt i sends to the two servers.
		delays []time.Duration
		sent   []int
	}{
		{
			name:     "after the first answer",
			answered: true,
			delays:   []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second},
			sent:     []int{2, 1, 1, 1, 1, 1},
		},
		{
			// Every 0.25s to every server for 8s, then every 4s in turn.
			name:   "before the first answer",
			delays: append(slices.Clone(warmup), 4*time.Second, 4*time.Second),
			sent:   append(slices.Repeat([]int{2}, 32), 1, 1, 1),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			peer := tunneltest.Start(t, twoServers)
			peer.SetHost("upstream.test", upstream4)
			tun, env := freshTunnel(t, peer)
			if tt.answered {
				tun.run.Load().dns.answered.Store(true)
			}
			peer.DropDNS(-1)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := lookupAsync(ctx, tun, "upstream.test")
			total := 0
			for i, want := range tt.delays {
				total += tt.sent[i]
				peer.WaitDNS(t, total)
				env.clock.waitArmed(t, want)
				assertPending(t, done)
				env.clock.advance(want)
			}
			total += tt.sent[len(tt.delays)]
			peer.WaitDNS(t, total)
			env.clock.waitTimers(1)
			cancel()
			if r := <-done; !errors.Is(r.err, context.Canceled) {
				t.Errorf("err = %v, want context.Canceled", r.err)
			}
			if got := peer.DroppedDNS(); got != total {
				t.Errorf("queries sent = %d, want %d", got, total)
			}
		})
	}
}

func TestLookupWarmupEndsWithFirstAnswer(t *testing.T) {
	peer := tunneltest.Start(t, twoServers)
	peer.SetHost("upstream.test", upstream4)
	peer.SetHost("other.test", upstream4)
	tun, env := freshTunnel(t, peer)
	// The first attempt and the first retry, to both servers, are lost,
	// as while a VPN DNS server is silent after the handshake.
	peer.DropDNS(4)

	done := lookupAsync(t.Context(), tun, "upstream.test")
	for sent := 2; sent <= 4; sent += 2 {
		peer.WaitDNS(t, sent)
		env.clock.waitArmed(t, 250*time.Millisecond)
		assertPending(t, done)
		env.clock.advance(250 * time.Millisecond)
	}
	if r := <-done; r.err != nil || !slices.Equal(r.addrs, []netip.Addr{upstream4}) {
		t.Fatalf("LookupNetIP = %v, %v; want [%v]", r.addrs, r.err, upstream4)
	}

	// The DNS path works now: the next lookup retries after 0.5s.
	peer.WaitDNS(t, 6)
	peer.DropDNS(2)
	done = lookupAsync(t.Context(), tun, "other.test")
	peer.WaitDNS(t, 8)
	env.clock.waitArmed(t, 500*time.Millisecond)
	assertPending(t, done)
	env.clock.advance(500 * time.Millisecond)
	if r := <-done; r.err != nil || !slices.Equal(r.addrs, []netip.Addr{upstream4}) {
		t.Errorf("LookupNetIP = %v, %v; want [%v]", r.addrs, r.err, upstream4)
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

// twoServers is a peer with two DNS servers that both answer.
var twoServers = tunneltest.Options{
	ServerAddresses: []netip.Addr{tunneltest.ServerAddr4, secondDNS},
	DNS:             []netip.Addr{tunneltest.ServerAddr4, secondDNS},
}

func TestLookupFirstAttemptAsksEveryServer(t *testing.T) {
	tests := []struct {
		name string
		opts tunneltest.Options
		drop int
		// received is the number of UDP queries the peer gets: one per
		// live server.
		received int
	}{
		{name: "first server dead", opts: tunneltest.Options{DNS: []netip.Addr{deadDNS, tunneltest.ServerAddr4}}, received: 1},
		{name: "query to one server lost", opts: twoServers, drop: 1, received: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			peer := tunneltest.Start(t, tt.opts)
			peer.SetHost("upstream.test", upstream4)
			tun, env := startedTunnel(t, peer)
			peer.DropDNS(tt.drop)

			// No clock advance: the answer comes within the first attempt.
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			got, err := tun.LookupNetIP(ctx, "upstream.test")
			if err != nil {
				t.Fatalf("LookupNetIP: %v", err)
			}
			if !slices.Equal(got, []netip.Addr{upstream4}) {
				t.Errorf("LookupNetIP = %v, want [%v]", got, upstream4)
			}
			peer.WaitDNS(t, tt.received)
			if got := env.clock.pending(); got != 0 {
				t.Errorf("%d timers left armed", got)
			}
		})
	}
}

func TestLookupRetriesRotateThroughServers(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{DNS: []netip.Addr{deadDNS, tunneltest.ServerAddr4}})
	peer.SetHost("upstream.test", upstream4)
	tun, env := startedTunnel(t, peer)
	peer.DropDNS(1)

	done := lookupAsync(t.Context(), tun, "upstream.test")
	// First attempt: both servers; the live one's query is lost.
	peer.WaitDNS(t, 1)
	env.clock.waitArmed(t, 500*time.Millisecond)
	// First retry: the first server, dead.
	env.clock.advance(500 * time.Millisecond)
	env.clock.waitArmed(t, time.Second)
	assertPending(t, done)
	// Second retry: the second server, which answers.
	env.clock.advance(time.Second)
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
}

func TestLookupServerFailureTriesNextServerAtOnce(t *testing.T) {
	opts := twoServers
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
	t.Run("error answer to a retry", func(t *testing.T) {
		peer := tunneltest.Start(t, opts)
		peer.SetHost("upstream.test", upstream4)
		tun, env := startedTunnel(t, peer)
		peer.DropDNS(2)

		done := lookupAsync(t.Context(), tun, "upstream.test")
		peer.WaitDNS(t, 2)
		peer.FailDNS(1, dnsmessage.RCodeServerFailure)
		env.clock.waitArmed(t, 500*time.Millisecond)
		// The retry fails on the first server; the second is asked
		// without waiting for the next retry.
		env.clock.advance(500 * time.Millisecond)
		r := <-done
		if r.err != nil {
			t.Fatalf("LookupNetIP: %v", r.err)
		}
		if !slices.Equal(r.addrs, []netip.Addr{upstream4}) {
			t.Errorf("LookupNetIP = %v, want [%v]", r.addrs, upstream4)
		}
		if got := len(peer.Queries()); got != 2 {
			t.Errorf("answered queries = %d, want 2", got)
		}
	})
}

// dualStack is a peer whose client has an IPv4 and an IPv6 address, so
// lookups ask A and AAAA.
var dualStack = tunneltest.Options{ClientAddresses: []netip.Prefix{
	netip.PrefixFrom(tunneltest.ClientAddr4, 32), netip.PrefixFrom(tunneltest.ClientAddr6, 128),
}}

func TestLookupCache(t *testing.T) {
	tests := []struct {
		name string
		opts tunneltest.Options
		host string
		ttl  uint32
		// fresh is the last time after the first lookup at which the
		// answer is still cached; expired the first time it is not.
		fresh, expired time.Duration
		want           []netip.Addr // nil: not found
		// alias, if set, is the host of the lookups after the first.
		alias string
	}{
		{name: "record TTL", host: "upstream.test", ttl: 60, fresh: 59 * time.Second, expired: 60 * time.Second, want: []netip.Addr{upstream4}},
		{name: "TTL 0 raised to 10s", host: "upstream.test", ttl: 0, fresh: 9 * time.Second, expired: 10 * time.Second, want: []netip.Addr{upstream4}},
		{name: "TTL capped at 5min", host: "upstream.test", ttl: 86400, fresh: 5*time.Minute - time.Second, expired: 5 * time.Minute, want: []netip.Addr{upstream4}},
		{name: "both families", opts: dualStack, host: "dual.test", ttl: 60, fresh: 59 * time.Second, expired: 60 * time.Second, want: []netip.Addr{upstream4, upstream6}},
		{name: "no AAAA record: SOA minimum", opts: dualStack, host: "upstream.test", ttl: 120, fresh: 119 * time.Second, expired: 120 * time.Second, want: []netip.Addr{upstream4}},
		{name: "NXDOMAIN 10s", host: "missing.test", ttl: 3600, fresh: 9 * time.Second, expired: 10 * time.Second},
		{name: "name case and trailing dot", host: "upstream.test", alias: "UpStream.Test.", ttl: 60, fresh: 59 * time.Second, expired: 60 * time.Second, want: []netip.Addr{upstream4}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			peer := tunneltest.Start(t, tt.opts)
			peer.SetHost("upstream.test", upstream4)
			peer.SetHost("dual.test", upstream6, upstream4)
			peer.SetTTL(tt.ttl)
			tun, env := startedTunnel(t, peer)

			lookup := func(step, host string) {
				t.Helper()
				got, err := tun.LookupNetIP(t.Context(), host)
				if tt.want == nil {
					var de *net.DNSError
					if !errors.As(err, &de) || !de.IsNotFound {
						t.Fatalf("%s: err = %v, want not-found *net.DNSError", step, err)
					}
					return
				}
				if err != nil {
					t.Fatalf("%s: LookupNetIP: %v", step, err)
				}
				if !slices.Equal(got, tt.want) {
					t.Errorf("%s: LookupNetIP = %v, want %v", step, got, tt.want)
				}
			}
			again := cmp.Or(tt.alias, tt.host)
			lookup("first", tt.host)
			sent := len(peer.Queries())
			if sent == 0 {
				t.Fatal("first lookup sent no query")
			}
			lookup("cached", again)
			env.clock.advance(tt.fresh)
			lookup("still cached", again)
			if got := len(peer.Queries()); got != sent {
				t.Errorf("queries after cached lookups = %d, want %d", got, sent)
			}
			env.clock.advance(tt.expired - tt.fresh)
			lookup("expired", again)
			if got := len(peer.Queries()); got != 2*sent {
				t.Errorf("queries after expiry = %d, want %d", got, 2*sent)
			}
		})
	}
}

func TestLookupCacheClearedOnStop(t *testing.T) {
	tests := map[string]func(t *testing.T, m *Manager, env *testEnv){
		"drain": func(t *testing.T, m *Manager, _ *testEnv) {
			if err := drain(m, "gr", 0); err != nil {
				t.Fatalf("Drain: %v", err)
			}
		},
		"idle close": func(t *testing.T, _ *Manager, env *testEnv) {
			env.clock.waitArmed(t, testIdle)
			env.clock.advance(testIdle)
		},
	}
	for name, stop := range tests {
		t.Run(name, func(t *testing.T) {
			peer := tunneltest.Start(t, tunneltest.Options{})
			peer.SetHost("upstream.test", upstream4)
			m, env := newManagerIdle(t, testIdle, peer.ClientTunnel(t, "gr"))
			tun := mustTunnel(t, m, "gr")
			for range 2 {
				if _, err := tun.LookupNetIP(t.Context(), "upstream.test"); err != nil {
					t.Fatalf("LookupNetIP: %v", err)
				}
			}
			if got := len(peer.Queries()); got != 1 {
				t.Fatalf("queries before stop = %d, want 1", got)
			}
			stop(t, m, env)
			if tun.Running() {
				t.Fatal("tunnel still running")
			}
			if _, err := tun.LookupNetIP(t.Context(), "upstream.test"); err != nil {
				t.Fatalf("LookupNetIP after stop: %v", err)
			}
			if got := len(peer.Queries()); got != 2 {
				t.Errorf("queries = %d, want a new query after the restart", got)
			}
			if got := tun.starts.Load(); got != 2 {
				t.Errorf("starts = %d, want 2", got)
			}
		})
	}
}

func TestLookupSharesOneQuery(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	peer.SetHost("upstream.test", upstream4)
	tun, env := startedTunnel(t, peer)
	peer.DropDNS(1)

	const callers = 8
	var done []<-chan lookupResult
	for range callers {
		done = append(done, lookupAsync(t.Context(), tun, "upstream.test"))
	}
	waitWaiters(t, tun, "upstream.test", callers)
	env.clock.waitArmed(t, 500*time.Millisecond)
	env.clock.advance(500 * time.Millisecond)
	for _, ch := range done {
		r := <-ch
		if r.err != nil || !slices.Equal(r.addrs, []netip.Addr{upstream4}) {
			t.Errorf("LookupNetIP = %v, %v; want [%v]", r.addrs, r.err, upstream4)
		}
	}
	if got, dropped := len(peer.Queries()), peer.DroppedDNS(); got != 1 || dropped != 1 {
		t.Errorf("answered %d and dropped %d queries, want 1 and 1", got, dropped)
	}
}

func TestLookupCancelledCallerLeavesSharedQuery(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	peer.SetHost("upstream.test", upstream4)
	tun, env := startedTunnel(t, peer)
	peer.DropDNS(-1)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	first := lookupAsync(ctx, tun, "upstream.test")
	second := lookupAsync(t.Context(), tun, "upstream.test")
	waitWaiters(t, tun, "upstream.test", 2)
	cancel()
	if r := <-first; !errors.Is(r.err, context.Canceled) {
		t.Fatalf("cancelled caller: err = %v, want context.Canceled", r.err)
	}
	assertPending(t, second)

	peer.DropDNS(0)
	env.clock.waitArmed(t, 500*time.Millisecond)
	env.clock.advance(500 * time.Millisecond)
	r := <-second
	if r.err != nil || !slices.Equal(r.addrs, []netip.Addr{upstream4}) {
		t.Errorf("LookupNetIP = %v, %v; want [%v]", r.addrs, r.err, upstream4)
	}
	if got := env.clock.pending(); got != 0 {
		t.Errorf("%d timers left armed", got)
	}
}

func TestLookupEndsWhenTunnelStops(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	peer.SetHost("upstream.test", upstream4)
	m, env := newManager(t, peer.ClientTunnel(t, "gr"))
	tun := mustTunnel(t, m, "gr")
	if _, err := tun.LookupNetIP(t.Context(), tunneltest.ServerAddr4.String()); err != nil {
		t.Fatalf("start: %v", err)
	}
	peer.DropDNS(-1)

	done := lookupAsync(t.Context(), tun, "upstream.test")
	waitWaiters(t, tun, "upstream.test", 1)
	if err := drain(m, "gr", 0); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if r := <-done; !errors.Is(r.err, ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", r.err)
	}
	if got := env.clock.pending(); got != 0 {
		t.Errorf("%d timers left armed", got)
	}
}

func TestDNSCacheBound(t *testing.T) {
	now := time.Now()
	key := func(name string) dnsKey { return dnsKey{name, dnsmessage.TypeA} }
	entry := func(expiresIn time.Duration) dnsEntry {
		return dnsEntry{addrs: []netip.Addr{upstream4}, expires: now.Add(expiresIn)}
	}
	tests := []struct {
		name  string
		have  map[string]time.Duration // name: expires in
		put   string
		after []string // names left, sorted
	}{
		{name: "room left", have: map[string]time.Duration{"a": time.Minute, "b": time.Minute}, put: "c", after: []string{"a", "b", "c"}},
		{name: "full: soonest expiry goes", have: map[string]time.Duration{"a": 2 * time.Minute, "b": time.Minute, "c": 3 * time.Minute}, put: "d", after: []string{"a", "c", "d"}},
		{name: "full: expired entries go first", have: map[string]time.Duration{"a": -time.Second, "b": 0, "c": time.Minute}, put: "d", after: []string{"c", "d"}},
		{name: "full: replacing an entry evicts nothing", have: map[string]time.Duration{"a": time.Minute, "b": time.Minute, "c": time.Minute}, put: "b", after: []string{"a", "b", "c"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := dnsCache{entries: map[dnsKey]dnsEntry{}, size: 3}
			for name, in := range tt.have {
				c.entries[key(name)] = entry(in)
			}
			c.put(key(tt.put), entry(time.Hour), now)
			var got []string
			for k := range c.entries {
				got = append(got, k.name)
			}
			slices.Sort(got)
			if !slices.Equal(got, tt.after) {
				t.Errorf("entries = %v, want %v", got, tt.after)
			}
		})
	}
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
			ans, err := parseResponse(tt.msg, 7, q)
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
			if got := ans.addrs; !slices.Equal(got, tt.want) {
				t.Errorf("addrs = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseResponseTTL(t *testing.T) {
	type rec func(b *dnsmessage.Builder) error
	hdr := func(n string, typ dnsmessage.Type, ttl uint32) dnsmessage.ResourceHeader {
		return dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(n), Type: typ, Class: dnsmessage.ClassINET, TTL: ttl}
	}
	a := func(n string, ttl uint32) rec {
		return func(b *dnsmessage.Builder) error {
			return b.AResource(hdr(n, dnsmessage.TypeA, ttl), dnsmessage.AResource{A: upstream4.As4()})
		}
	}
	cname := func(n, target string, ttl uint32) rec {
		return func(b *dnsmessage.Builder) error {
			return b.CNAMEResource(hdr(n, dnsmessage.TypeCNAME, ttl), dnsmessage.CNAMEResource{CNAME: dnsmessage.MustNewName(target)})
		}
	}
	soa := func(ttl, minimum uint32) rec {
		return func(b *dnsmessage.Builder) error {
			if err := b.StartAuthorities(); err != nil {
				return err
			}
			return b.SOAResource(hdr("test.", dnsmessage.TypeSOA, ttl), dnsmessage.SOAResource{
				NS: dnsmessage.MustNewName("ns.test."), MBox: dnsmessage.MustNewName("hostmaster.test."), MinTTL: minimum,
			})
		}
	}
	q := dnsmessage.Question{Name: dnsmessage.MustNewName("upstream.test."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}
	tests := []struct {
		name    string
		records []rec
		want    time.Duration
	}{
		{name: "smallest address TTL", records: []rec{a("upstream.test.", 300), a("upstream.test.", 120)}, want: 120 * time.Second},
		{name: "CNAME chain counts", records: []rec{cname("upstream.test.", "cdn.test.", 30), a("cdn.test.", 300)}, want: 30 * time.Second},
		{name: "records of other names ignored", records: []rec{a("other.test.", 5), a("upstream.test.", 300)}, want: 300 * time.Second},
		{name: "no record: SOA MINIMUM", records: []rec{soa(900, 60)}, want: 60 * time.Second},
		{name: "no record: SOA TTL if smaller", records: []rec{soa(20, 60)}, want: 20 * time.Second},
		{name: "no record, no SOA", want: 0},
		{name: "SOA ignored when there are records", records: []rec{a("upstream.test.", 300), soa(5, 5)}, want: 300 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := response(t, dnsmessage.Header{ID: 7}, q, func(b *dnsmessage.Builder) error {
				for _, r := range tt.records {
					if err := r(b); err != nil {
						return err
					}
				}
				return nil
			})
			ans, err := parseResponse(msg, 7, q)
			if err != nil {
				t.Fatalf("parseResponse: %v", err)
			}
			if ans.ttl != tt.want {
				t.Errorf("ttl = %v, want %v", ans.ttl, tt.want)
			}
		})
	}
}

// TestLookupOutsideAllowedIPs: a name whose addresses all lie outside
// AllowedIPs fails like an unknown name and wraps ErrNotAllowed, so the
// server can tell the viewer the configuration blocks it (spec 3.4).
func TestLookupOutsideAllowedIPs(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{
		AllowedIPs: []netip.Prefix{netip.PrefixFrom(tunneltest.ServerAddr4, 32)},
	})
	peer.SetHost("outside.test", netip.MustParseAddr("198.51.100.1"))
	tun, _ := startedTunnel(t, peer)
	for _, host := range []string{"outside.test", "198.51.100.1"} {
		_, err := tun.LookupNetIP(t.Context(), host)
		var de *net.DNSError
		if !errors.As(err, &de) || !de.IsNotFound || !errors.Is(err, ErrNotAllowed) {
			t.Errorf("LookupNetIP(%q): err = %v, want not-found *net.DNSError wrapping ErrNotAllowed", host, err)
		}
	}
	// A name the DNS server does not know is not a configuration matter.
	if _, err := tun.LookupNetIP(t.Context(), "missing.test"); errors.Is(err, ErrNotAllowed) {
		t.Errorf("unknown name: err = %v wraps ErrNotAllowed", err)
	}
}

// waitDNSAnswered blocks until a DNS server has answered a query of the
// running tun (the client of a freshTunnel starts without an answer). The
// exchange records that answer before it looks at its context again.
// It fails t after 10s.
func waitDNSAnswered(t *testing.T, tun *Tunnel) {
	t.Helper()
	c := tun.run.Load().dns
	giveUp := time.Now().Add(10 * time.Second)
	for !c.answered.Load() {
		if time.Now().After(giveUp) {
			t.Fatal("no DNS answer after 10s")
		}
		runtime.Gosched()
	}
}

func TestLookupPartialAnswerWhenCallerEnds(t *testing.T) {
	peer := tunneltest.Start(t, dualStack)
	peer.SetHost("dual.test", upstream6, upstream4)
	tun, env := freshTunnel(t, peer)
	peer.DropDNSType(dnsmessage.TypeAAAA, true)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := lookupAsync(ctx, tun, "dual.test")
	waitDNSAnswered(t, tun)
	assertPending(t, done)
	cancel()
	r := <-done
	if r.err != nil || !slices.Equal(r.addrs, []netip.Addr{upstream4}) {
		t.Fatalf("LookupNetIP = %v, %v; want [%v]", r.addrs, r.err, upstream4)
	}
	if got := env.clock.pending(); got != 0 {
		t.Errorf("%d timers left armed", got)
	}

	// The unanswered type is not cached as missing: once the server
	// answers it, the next lookup has both. The A answer is cached.
	peer.DropDNSType(dnsmessage.TypeAAAA, false)
	got, err := tun.LookupNetIP(t.Context(), "dual.test")
	if err != nil || !slices.Equal(got, []netip.Addr{upstream4, upstream6}) {
		t.Errorf("second LookupNetIP = %v, %v; want [%v %v]", got, err, upstream4, upstream6)
	}
	if n := countQueries(peer.Queries(), "dual.test A"); n != 1 {
		t.Errorf("A queries = %d, want 1 (cached)", n)
	}
}

func TestLookupPartialAnswerWhenSharedCallersEnd(t *testing.T) {
	peer := tunneltest.Start(t, dualStack)
	peer.SetHost("dual.test", upstream6, upstream4)
	tun, _ := freshTunnel(t, peer)
	peer.DropDNSType(dnsmessage.TypeAAAA, true)

	ctx1, cancel1 := context.WithCancel(t.Context())
	defer cancel1()
	ctx2, cancel2 := context.WithCancel(t.Context())
	defer cancel2()
	first := lookupAsync(ctx1, tun, "dual.test")
	second := lookupAsync(ctx2, tun, "dual.test")
	waitWaiters(t, tun, "dual.test", 2)
	waitDNSAnswered(t, tun)

	// The first caller's deadline ends while the second still waits.
	cancel1()
	if r := <-first; r.err != nil || !slices.Equal(r.addrs, []netip.Addr{upstream4}) {
		t.Errorf("first: LookupNetIP = %v, %v; want [%v]", r.addrs, r.err, upstream4)
	}
	assertPending(t, second)
	cancel2()
	if r := <-second; r.err != nil || !slices.Equal(r.addrs, []netip.Addr{upstream4}) {
		t.Errorf("second: LookupNetIP = %v, %v; want [%v]", r.addrs, r.err, upstream4)
	}
}

func TestLookupNothingAnsweredStillFails(t *testing.T) {
	peer := tunneltest.Start(t, dualStack)
	peer.SetHost("dual.test", upstream6, upstream4)
	tun, _ := startedTunnel(t, peer)
	peer.DropDNSType(dnsmessage.TypeA, true)
	peer.DropDNSType(dnsmessage.TypeAAAA, true)

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	got, err := tun.LookupNetIP(ctx, "dual.test")
	if !errors.Is(err, context.DeadlineExceeded) || len(got) != 0 {
		t.Errorf("LookupNetIP = %v, %v; want context.DeadlineExceeded", got, err)
	}
}

func TestLookupNoAddressesAnsweredStillFails(t *testing.T) {
	// The A query is answered, without records; AAAA is lost.
	peer := tunneltest.Start(t, dualStack)
	peer.SetHost("v6only.test", upstream6)
	tun, _ := freshTunnel(t, peer)
	peer.DropDNSType(dnsmessage.TypeAAAA, true)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := lookupAsync(ctx, tun, "v6only.test")
	waitDNSAnswered(t, tun)
	cancel()
	r := <-done
	if !errors.Is(r.err, context.Canceled) || len(r.addrs) != 0 {
		t.Errorf("LookupNetIP = %v, %v; want context.Canceled", r.addrs, r.err)
	}
}

func TestLookupNXDOMAINWithOtherTypeUnanswered(t *testing.T) {
	peer := tunneltest.Start(t, dualStack)
	tun, _ := startedTunnel(t, peer)
	peer.DropDNSType(dnsmessage.TypeAAAA, true)

	_, err := tun.LookupNetIP(t.Context(), "missing.test")
	var de *net.DNSError
	if !errors.As(err, &de) || !de.IsNotFound {
		t.Errorf("err = %v, want not-found *net.DNSError", err)
	}
}

func TestLookupPartialAnswerEndsWhenTunnelStops(t *testing.T) {
	peer := tunneltest.Start(t, dualStack)
	peer.SetHost("dual.test", upstream6, upstream4)
	m, _ := newManager(t, peer.ClientTunnel(t, "gr"))
	tun := mustTunnel(t, m, "gr")
	if _, err := tun.LookupNetIP(t.Context(), tunneltest.ServerAddr4.String()); err != nil {
		t.Fatalf("start: %v", err)
	}
	peer.DropDNSType(dnsmessage.TypeAAAA, true)

	done := lookupAsync(t.Context(), tun, "dual.test")
	waitWaiters(t, tun, "dual.test", 1)
	waitDNSAnswered(t, tun)
	if err := drain(m, "gr", 0); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if r := <-done; !errors.Is(r.err, ErrUnavailable) {
		t.Errorf("LookupNetIP = %v, %v; want ErrUnavailable", r.addrs, r.err)
	}
}

// countQueries returns how often q is in queries.
func countQueries(queries []string, q string) int {
	n := 0
	for _, s := range queries {
		if s == q {
			n++
		}
	}
	return n
}

// resolutionDelay is the time a lookup waits for the other types once one
// is answered with addresses.
const resolutionDelay = 50 * time.Millisecond

// backgroundDone blocks until the client of tun has no flight in
// progress. It fails t after 10s.
func backgroundDone(t *testing.T, tun *Tunnel) {
	t.Helper()
	c := tun.run.Load().dns
	giveUp := time.Now().Add(10 * time.Second)
	for {
		c.mu.Lock()
		n := len(c.pending)
		c.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(giveUp) {
			t.Fatalf("%d flights still running after 10s", n)
		}
		runtime.Gosched()
	}
}

// A lookup returns the answered addresses dnsResolutionDelay after the
// answer, without its ctx ending; the unanswered type keeps resolving in
// the background and lands in the cache.
func TestLookupResolutionDelay(t *testing.T) {
	peer := tunneltest.Start(t, dualStack)
	peer.SetHost("dual.test", upstream6, upstream4)
	tun, env := startedTunnel(t, peer)
	peer.DropDNSType(dnsmessage.TypeAAAA, true)

	done := lookupAsync(t.Context(), tun, "dual.test")
	env.clock.waitArmed(t, resolutionDelay)
	env.clock.advance(resolutionDelay - time.Millisecond)
	assertPending(t, done)
	env.clock.advance(time.Millisecond)
	r := <-done
	if r.err != nil || !slices.Equal(r.addrs, []netip.Addr{upstream4}) {
		t.Fatalf("LookupNetIP = %v, %v; want [%v]", r.addrs, r.err, upstream4)
	}

	// The flight goes on: a lookup that joins it gets the cached A at once.
	again, err := tun.LookupNetIP(t.Context(), "dual.test")
	if err != nil || !slices.Equal(again, []netip.Addr{upstream4}) {
		t.Errorf("second LookupNetIP = %v, %v; want [%v]", again, err, upstream4)
	}

	// The server answers AAAA after all; the background flight's retry
	// (0.5s) gets it and the flight ends.
	peer.DropDNSType(dnsmessage.TypeAAAA, false)
	env.clock.waitArmed(t, 500*time.Millisecond-resolutionDelay)
	env.clock.advance(500*time.Millisecond - resolutionDelay)
	backgroundDone(t, tun)
	if got := env.clock.pending(); got != 0 {
		t.Errorf("%d timers left armed", got)
	}
	both, err := tun.LookupNetIP(t.Context(), "dual.test")
	if err != nil || !slices.Equal(both, []netip.Addr{upstream4, upstream6}) {
		t.Errorf("third LookupNetIP = %v, %v; want [%v %v]", both, err, upstream4, upstream6)
	}
	if n := countQueries(peer.Queries(), "dual.test A"); n != 1 {
		t.Errorf("A queries = %d, want 1", n)
	}
}

func TestLookupResolutionDelayForEverySharedCaller(t *testing.T) {
	peer := tunneltest.Start(t, dualStack)
	peer.SetHost("dual.test", upstream6, upstream4)
	tun, env := startedTunnel(t, peer)
	peer.DropDNSType(dnsmessage.TypeAAAA, true)

	const callers = 4
	var done []<-chan lookupResult
	for range callers {
		done = append(done, lookupAsync(t.Context(), tun, "dual.test"))
	}
	waitWaiters(t, tun, "dual.test", callers)
	env.clock.waitArmed(t, resolutionDelay)
	env.clock.advance(resolutionDelay)
	for i, ch := range done {
		if r := <-ch; r.err != nil || !slices.Equal(r.addrs, []netip.Addr{upstream4}) {
			t.Errorf("caller %d: LookupNetIP = %v, %v; want [%v]", i, r.addrs, r.err, upstream4)
		}
	}
	if n := countQueries(peer.Queries(), "dual.test A"); n != 1 {
		t.Errorf("A queries = %d, want 1 (one shared flight)", n)
	}
}

// A background flight is bounded: connect_timeout after its start it
// ends, and the next lookup starts a new one that asks the missing type
// only, waiting the resolution delay for it.
func TestLookupBackgroundFlightIsBounded(t *testing.T) {
	peer := tunneltest.Start(t, dualStack)
	peer.SetHost("dual.test", upstream6, upstream4)
	peer.SetTTL(300)
	tun, env := startedTunnel(t, peer)
	peer.DropDNSType(dnsmessage.TypeAAAA, true)

	done := lookupAsync(t.Context(), tun, "dual.test")
	env.clock.waitArmed(t, resolutionDelay)
	env.clock.advance(resolutionDelay)
	if r := <-done; r.err != nil || !slices.Equal(r.addrs, []netip.Addr{upstream4}) {
		t.Fatalf("first LookupNetIP = %v, %v; want [%v]", r.addrs, r.err, upstream4)
	}
	// The bound is armed for the rest of connect_timeout.
	env.clock.waitArmed(t, testConnectTimeout-resolutionDelay)
	env.clock.advance(testConnectTimeout - resolutionDelay)
	backgroundDone(t, tun)
	if got := env.clock.pending(); got != 0 {
		t.Errorf("%d timers left armed after the bound", got)
	}

	// A is still cached; the new flight asks AAAA only, and the delay
	// starts with it.
	second := lookupAsync(t.Context(), tun, "dual.test")
	env.clock.waitArmed(t, resolutionDelay)
	assertPending(t, second)
	env.clock.advance(resolutionDelay)
	if r := <-second; r.err != nil || !slices.Equal(r.addrs, []netip.Addr{upstream4}) {
		t.Errorf("second LookupNetIP = %v, %v; want [%v]", r.addrs, r.err, upstream4)
	}
	if n := countQueries(peer.Queries(), "dual.test A"); n != 1 {
		t.Errorf("A queries = %d, want 1 (cached)", n)
	}
}

// The tunnel stopping ends a background flight.
func TestLookupBackgroundFlightEndsWhenTunnelStops(t *testing.T) {
	peer := tunneltest.Start(t, dualStack)
	peer.SetHost("dual.test", upstream6, upstream4)
	m, env := newManager(t, peer.ClientTunnel(t, "gr"))
	tun := mustTunnel(t, m, "gr")
	if _, err := tun.LookupNetIP(t.Context(), tunneltest.ServerAddr4.String()); err != nil {
		t.Fatalf("start: %v", err)
	}
	peer.DropDNSType(dnsmessage.TypeAAAA, true)

	done := lookupAsync(t.Context(), tun, "dual.test")
	env.clock.waitArmed(t, resolutionDelay)
	env.clock.advance(resolutionDelay)
	if r := <-done; r.err != nil || !slices.Equal(r.addrs, []netip.Addr{upstream4}) {
		t.Fatalf("LookupNetIP = %v, %v; want [%v]", r.addrs, r.err, upstream4)
	}
	if err := drain(m, "gr", 0); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if got := env.clock.pending(); got != 0 {
		t.Errorf("%d timers left armed", got)
	}
}
