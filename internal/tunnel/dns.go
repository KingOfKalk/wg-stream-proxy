// SPDX-License-Identifier: AGPL-3.0-or-later

package tunnel

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel/netstack"
)

// dnsRetryDelays is the time from one attempt of a DNS query to the next
// (spec 2.1); the last delay repeats until the caller's ctx ends. The
// first attempt asks every server at once, and a lookup through a tunnel
// takes tens of milliseconds, so after 0.5s the first attempt counts as
// lost.
var dnsRetryDelays = []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second}

const (
	// dnsWarmupDelay is the time from one attempt to the next, each to
	// every server, until a server has answered since the tunnel started
	// (spec 2.1): the DNS servers of some VPN providers do not answer for
	// several seconds after the handshake of a key that was idle for a
	// while (#127), and the first answer should come soon after they do.
	dnsWarmupDelay = 250 * time.Millisecond
	// dnsWarmupRounds bounds the warm-up attempts of a query (8s); then
	// it goes on with the last of dnsRetryDelays.
	dnsWarmupRounds = 32

	// dnsUDPSize is the EDNS(0) UDP payload size offered to servers, as in
	// Go's resolver.
	dnsUDPSize = 1232
	// dnsReadSize is the UDP read buffer; larger than dnsUDPSize for
	// servers that ignore EDNS(0).
	dnsReadSize = 4096
	// maxCNAMEHops bounds the CNAME chain followed in an answer.
	maxCNAMEHops = 8

	// dnsMinTTL and dnsMaxTTL bound how long an answer stays in the cache
	// (spec 2.1), whatever its TTL. NXDOMAIN is cached for dnsMinTTL.
	dnsMinTTL = 10 * time.Second
	dnsMaxTTL = 5 * time.Minute
	// dnsCacheSize bounds the cache entries of one tunnel: one per name
	// and query type.
	dnsCacheSize = 256
)

// errStray: a DNS message is not the answer to the query it was read
// for. Such messages are ignored.
var errStray = errors.New("dns: stray message")

// dnsClient resolves names with the tunnel's DNS servers, over UDP and
// TCP inside the netstack, and caches the answers. It replaces the
// netstack resolver, which retries a lost query only after 5s. A client
// belongs to one running device: a start creates it empty, close at
// every stop drops it, so a cold tunnel starts with a cold cache.
type dnsClient struct {
	net     *netstack.Net
	servers []netip.Addr
	// types are the query types of every lookup: the address families
	// the tunnel has.
	types []dnsmessage.Type
	clock Clock
	// answered: a server answered a query of this client, so the DNS
	// path through the tunnel works. Until then queries retry with
	// dnsWarmupDelay.
	answered atomic.Bool

	// ctx ends when the client closes; it bounds every flight.
	ctx     context.Context
	cancel  context.CancelFunc
	flights sync.WaitGroup

	mu     sync.Mutex
	closed bool
	cache  dnsCache
	// pending are the flights in progress, by canonical name.
	pending map[string]*dnsFlight
}

func newDNSClient(n *netstack.Net, servers []netip.Addr, types []dnsmessage.Type, clock Clock) *dnsClient {
	ctx, cancel := context.WithCancel(context.Background())
	return &dnsClient{
		net:     n,
		servers: servers,
		types:   types,
		clock:   clock,
		ctx:     ctx,
		cancel:  cancel,
		cache:   dnsCache{entries: map[dnsKey]dnsEntry{}, size: dnsCacheSize},
		pending: map[string]*dnsFlight{},
	}
}

// close ends the flights in progress, waits for them and drops the
// cache. Lookups after close fail at once.
func (c *dnsClient) close() {
	c.mu.Lock()
	c.closed = true
	clear(c.cache.entries)
	c.mu.Unlock()
	c.cancel()
	c.flights.Wait()
}

// dnsFlight is the query of one name shared by all lookups that want it
// at the same time (singleflight). It runs while at least one of them
// still waits; the last one to give up cancels it.
type dnsFlight struct {
	cancel  context.CancelFunc
	waiters int // guarded by dnsClient.mu
	// done is closed when addrs and err are set.
	done  chan struct{}
	addrs []netip.Addr
	err   error
}

// lookup resolves host from the cache or with one query per type of the
// client, shared with concurrent lookups of the same name. It returns
// when ctx ends, even if the shared query goes on for others. If ctx ends
// before every type is answered, the addresses of the types that were
// are the result (as with Go's resolver); the error is only returned if
// there are none.
func (c *dnsClient) lookup(ctx context.Context, host string) ([]netip.Addr, error) {
	name, ok := queryName(host)
	if !ok {
		return nil, notFoundError(host)
	}
	key := canonicalName(host)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, stoppedError(host)
	}
	f := c.pending[key]
	if f == nil {
		cached, missing, notFound := c.cache.get(key, c.types, c.clock.Now())
		switch {
		case notFound:
			c.mu.Unlock()
			return nil, notFoundError(host)
		case len(missing) == 0:
			c.mu.Unlock()
			return flatten(c.types, cached), nil
		}
		fctx, cancel := context.WithCancel(c.ctx)
		f = &dnsFlight{cancel: cancel, done: make(chan struct{})}
		c.pending[key] = f
		c.flights.Add(1)
		go c.fly(fctx, f, key, host, name, cached, missing)
	}
	f.waiters++
	c.mu.Unlock()

	select {
	case <-f.done:
		return slices.Clone(f.addrs), f.err
	case <-ctx.Done():
		c.leave(key, f)
		// Every answered type is in the cache: the flight stores it as
		// soon as it arrives, so this holds for each waiter, whether it
		// was the last or not. Not for a tunnel that stopped or a name
		// that does not exist.
		if addrs := c.cachedAddrs(key); len(addrs) > 0 {
			return addrs, nil
		}
		return nil, fmt.Errorf("lookup %s: %w", host, ctx.Err())
	}
}

// cachedAddrs returns the cached addresses of the name key, whatever
// types are missing. It returns none after close or for a name cached as
// NXDOMAIN.
func (c *dnsClient) cachedAddrs(key string) []netip.Addr {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	cached, _, notFound := c.cache.get(key, c.types, c.clock.Now())
	if notFound {
		return nil
	}
	return flatten(c.types, cached)
}

// leave removes a lookup whose ctx ended from flight f. The last one
// cancels f and waits until it has stopped its timers and sockets.
func (c *dnsClient) leave(key string, f *dnsFlight) {
	c.mu.Lock()
	f.waiters--
	last := f.waiters == 0
	if last && c.pending[key] == f {
		delete(c.pending, key)
	}
	c.mu.Unlock()
	if last {
		f.cancel()
		<-f.done
	}
}

// fly runs flight f: it asks the missing types, caches each answer as it
// arrives and sets the result for all waiters. The flight ends early only
// when its last waiter gave up or the client closed; nobody reads the
// result of the former, and the answers it got stay cached.
func (c *dnsClient) fly(ctx context.Context, f *dnsFlight, key, host string, name dnsmessage.Name, cached map[dnsmessage.Type][]netip.Addr, missing []dnsmessage.Type) {
	defer c.flights.Done()
	defer f.cancel()
	queries, err := c.exchange(ctx, host, name, missing, func(q *dnsQuery) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if !c.closed {
			now := c.clock.Now()
			c.cache.put(dnsKey{key, q.q.Type}, dnsEntry{addrs: q.addrs, expires: now.Add(clampTTL(q.ttl))}, now)
		}
	})

	c.mu.Lock()
	if c.pending[key] == f {
		delete(c.pending, key)
	}
	now := c.clock.Now()
	switch {
	case c.closed:
	case errors.Is(err, errNXDOMAIN):
		for _, typ := range c.types {
			c.cache.put(dnsKey{key, typ}, dnsEntry{notFound: true, expires: now.Add(dnsMinTTL)}, now)
		}
	}
	c.mu.Unlock()

	switch {
	case errors.Is(err, errNXDOMAIN):
		f.err = notFoundError(host)
	case err != nil && c.ctx.Err() != nil:
		f.err = stoppedError(host)
	case err != nil:
		f.err = fmt.Errorf("lookup %s: %w", host, err)
	default:
		found := make(map[dnsmessage.Type][]netip.Addr, len(c.types))
		maps.Copy(found, cached)
		var qerr error
		for _, q := range queries {
			found[q.q.Type] = q.addrs
			if q.err != nil {
				qerr = q.err
			}
		}
		f.addrs = flatten(c.types, found)
		if len(f.addrs) == 0 && qerr != nil {
			f.addrs, f.err = nil, qerr
		}
	}
	close(f.done)
}

// flatten returns the addresses of byType in the order of types.
func flatten(types []dnsmessage.Type, byType map[dnsmessage.Type][]netip.Addr) []netip.Addr {
	var addrs []netip.Addr
	for _, typ := range types {
		addrs = append(addrs, byType[typ]...)
	}
	return addrs
}

func notFoundError(host string) error {
	return &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// stoppedError: the tunnel stopped while a lookup waited for its answer.
func stoppedError(host string) error {
	return fmt.Errorf("lookup %s: %w: tunnel stopped", host, ErrUnavailable)
}

// clampTTL bounds the time an answer stays in the cache.
func clampTTL(ttl time.Duration) time.Duration {
	return min(max(ttl, dnsMinTTL), dnsMaxTTL)
}

// dnsKey is a cache key: a canonical name and a query type.
type dnsKey struct {
	name string
	typ  dnsmessage.Type
}

// dnsEntry is a cached answer: the addresses (none for an answer without
// records of the type), or notFound for NXDOMAIN.
type dnsEntry struct {
	addrs    []netip.Addr
	notFound bool
	expires  time.Time
}

// dnsCache holds the answers of one tunnel, at most size entries. It is
// guarded by dnsClient.mu.
type dnsCache struct {
	entries map[dnsKey]dnsEntry
	size    int
}

// get returns the cached addresses of name by type and the types without
// a valid entry, or notFound if name is cached as NXDOMAIN. Expired
// entries are removed.
func (c *dnsCache) get(name string, types []dnsmessage.Type, now time.Time) (found map[dnsmessage.Type][]netip.Addr, missing []dnsmessage.Type, notFound bool) {
	for _, typ := range types {
		k := dnsKey{name, typ}
		e, ok := c.entries[k]
		if ok && !now.Before(e.expires) {
			delete(c.entries, k)
			ok = false
		}
		switch {
		case !ok:
			missing = append(missing, typ)
		case e.notFound:
			return nil, nil, true
		default:
			if found == nil {
				found = map[dnsmessage.Type][]netip.Addr{}
			}
			found[typ] = e.addrs
		}
	}
	return found, missing, false
}

// put stores e under k. If the cache is full, expired entries are removed
// first, then the entry that expires soonest.
func (c *dnsCache) put(k dnsKey, e dnsEntry, now time.Time) {
	if _, ok := c.entries[k]; !ok && len(c.entries) >= c.size {
		for k2, e2 := range c.entries {
			if !now.Before(e2.expires) {
				delete(c.entries, k2)
			}
		}
		if len(c.entries) >= c.size {
			var oldest dnsKey
			var first time.Time
			for k2, e2 := range c.entries {
				if first.IsZero() || e2.expires.Before(first) {
					oldest, first = k2, e2.expires
				}
			}
			delete(c.entries, oldest)
		}
	}
	c.entries[k] = e
}

// errNXDOMAIN: a server answered that the name does not exist.
var errNXDOMAIN = errors.New("dns: name does not exist")

// dnsQuery is one question of an exchange and its state. Only the
// exchange's own goroutine touches it.
type dnsQuery struct {
	q dnsmessage.Question
	// rounds counts the rounds sent: the first asks every server, each
	// retry the next server in turn.
	rounds int
	// backoff counts the rounds sent after a server had answered, the
	// index into dnsRetryDelays.
	backoff int
	// next is the index of the server for the next retry; failed marks
	// the servers that answered with an error.
	next     int
	failed   []bool
	failures int
	timer    Timer
	done     bool
	addrs    []netip.Addr
	ttl      time.Duration
	err      error
}

// dnsResult is the outcome of one attempt of a query.
type dnsResult struct {
	query, server, round int
	answer               dnsAnswer
	err                  error // transport or format error
}

// dnsRetry asks the exchange to send the next round of a query, unless a
// later round than the one numbered round was sent meanwhile.
type dnsRetry struct {
	query, round int
}

// dnsExchange is one exchange in progress.
type dnsExchange struct {
	c       *dnsClient
	ctx     context.Context
	queries []*dnsQuery
	results chan dnsResult
	retries chan dnsRetry
	conns   []net.Conn
	readers sync.WaitGroup
}

// exchange asks the questions name/types, in parallel. The first round
// of a query goes to every server at once; the first valid answer wins.
// A query is repeated on the next server after dnsRetryDelays (before
// the first answer since the start: see send), at once
// after an error answer to a retry, and over TCP after a truncated
// answer, until ctx ends. A server that answered with an error is not
// asked again; a query fails once every server did. It returns the
// queries with their answers, and errNXDOMAIN if a server said name does
// not exist, or ctx's error; then some queries may be done, others not.
// answered is called for every query that got an answer, as it does, so
// the caller can keep it even if ctx ends before the other queries are
// done.
func (c *dnsClient) exchange(ctx context.Context, host string, name dnsmessage.Name, types []dnsmessage.Type, answered func(*dnsQuery)) ([]*dnsQuery, error) {
	ctx, cancel := context.WithCancel(ctx)
	l := &dnsExchange{
		c:       c,
		ctx:     ctx,
		results: make(chan dnsResult),
		retries: make(chan dnsRetry),
	}
	defer l.close(cancel)
	for _, typ := range types {
		l.queries = append(l.queries, &dnsQuery{
			q:      dnsmessage.Question{Name: name, Type: typ, Class: dnsmessage.ClassINET},
			failed: make([]bool, len(c.servers)),
		})
	}
	for i := range l.queries {
		l.send(i)
	}

	for pending := len(l.queries); pending > 0; {
		select {
		case <-ctx.Done():
			return l.queries, ctx.Err()
		case r := <-l.retries:
			if q := l.queries[r.query]; !q.done && q.rounds == r.round+1 {
				l.send(r.query)
			}
		case r := <-l.results:
			if r.err == nil {
				// Any answer, also an error or a late one: the DNS path
				// works.
				c.answered.Store(true)
			}
			q := l.queries[r.query]
			if q.done {
				// A late answer to an earlier attempt.
				continue
			}
			switch {
			case r.err == nil && r.answer.h.RCode == dnsmessage.RCodeSuccess:
				q.addrs, q.ttl = r.answer.addrs, r.answer.ttl
			case r.err == nil && r.answer.h.RCode == dnsmessage.RCodeNameError:
				return l.queries, errNXDOMAIN
			default:
				if !q.failed[r.server] {
					q.failed[r.server] = true
					q.failures++
				}
				if q.failures < len(c.servers) {
					// The first round asked every server: wait for the
					// others, or the retry.
					if r.round > 0 {
						l.send(r.query)
					}
					continue
				}
				q.err = &net.DNSError{Err: "server misbehaving", Name: host, IsTemporary: true}
			}
			q.done = true
			q.timer.Stop()
			if q.err == nil {
				answered(q)
			}
			pending--
		}
	}
	return l.queries, nil
}

// queryName returns host as an absolute DNS name, if it is a valid one.
func queryName(host string) (dnsmessage.Name, bool) {
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return dnsmessage.Name{}, false
	}
	for label := range strings.SplitSeq(host, ".") {
		if label == "" || len(label) > 63 {
			return dnsmessage.Name{}, false
		}
	}
	name, err := dnsmessage.NewName(host + ".")
	return name, err == nil
}

// canonicalName returns the cache and flight key of a valid host name.
func canonicalName(host string) string {
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

// send sends the next round of query i and arms its retry timer: the
// first round to every server, later ones to the next server that has
// not answered with an error. Until a server has answered since the
// start, the first dnsWarmupRounds rounds go to every such server and
// retry after dnsWarmupDelay. Earlier attempts keep listening: a late
// answer still counts. An attempt that cannot be sent counts as lost.
func (l *dnsExchange) send(i int) {
	q := l.queries[i]
	round := q.rounds
	q.rounds++
	if q.timer != nil {
		q.timer.Stop()
	}
	answered := l.c.answered.Load()
	warmup := !answered && round < dnsWarmupRounds
	var delay time.Duration
	switch {
	case warmup:
		delay = dnsWarmupDelay
	case answered:
		delay = dnsRetryDelays[min(q.backoff, len(dnsRetryDelays)-1)]
		q.backoff++
	default:
		delay = dnsRetryDelays[len(dnsRetryDelays)-1]
	}
	q.timer = l.c.clock.AfterFunc(delay, func() {
		select {
		case l.retries <- dnsRetry{query: i, round: round}:
		case <-l.ctx.Done():
		}
	})
	if round == 0 || warmup {
		for s := range l.c.servers {
			if !q.failed[s] {
				l.sendTo(i, s, round)
			}
		}
		return
	}
	for range l.c.servers {
		s := q.next % len(l.c.servers)
		q.next++
		if !q.failed[s] {
			l.sendTo(i, s, round)
			return
		}
	}
}

// sendTo sends one attempt of query i to server s over UDP and starts its
// reader.
func (l *dnsExchange) sendTo(i, s, round int) {
	server := netip.AddrPortFrom(l.c.servers[s], 53)
	id := newQueryID()
	msg, err := buildQuery(id, l.queries[i].q)
	if err != nil {
		return
	}
	conn, err := l.c.net.DialUDPAddrPort(netip.AddrPort{}, server)
	if err != nil {
		return
	}
	l.conns = append(l.conns, conn)
	if _, err := conn.Write(msg[2:]); err != nil {
		return
	}
	l.readers.Go(func() { l.read(conn, dnsResult{query: i, server: s, round: round}, id, server) })
}

// read waits for the answer to one UDP attempt, ignoring stray messages,
// and repeats the query over TCP after a truncated answer.
func (l *dnsExchange) read(conn net.Conn, r dnsResult, id uint16, server netip.AddrPort) {
	q := l.queries[r.query].q
	buf := make([]byte, dnsReadSize)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			r.err = err
			l.post(r)
			return
		}
		a, err := parseResponse(buf[:n], id, q)
		if errors.Is(err, errStray) {
			continue
		}
		if err == nil && a.h.Truncated {
			a, err = l.exchangeTCP(server, q)
		}
		r.answer, r.err = a, err
		l.post(r)
		return
	}
}

// post hands a result to the exchange, unless the exchange is over.
func (l *dnsExchange) post(r dnsResult) {
	select {
	case l.results <- r:
	case <-l.ctx.Done():
	}
}

// exchangeTCP asks q over TCP (RFC 7766), for answers too large for UDP.
func (l *dnsExchange) exchangeTCP(server netip.AddrPort, q dnsmessage.Question) (dnsAnswer, error) {
	conn, err := l.c.net.DialContextTCPAddrPort(l.ctx, server)
	if err != nil {
		return dnsAnswer{}, err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(l.ctx, func() { _ = conn.Close() })
	defer stop()

	id := newQueryID()
	msg, err := buildQuery(id, q)
	if err != nil {
		return dnsAnswer{}, err
	}
	if _, err := conn.Write(msg); err != nil {
		return dnsAnswer{}, err
	}
	var size [2]byte
	if _, err := io.ReadFull(conn, size[:]); err != nil {
		return dnsAnswer{}, err
	}
	resp := make([]byte, binary.BigEndian.Uint16(size[:]))
	if _, err := io.ReadFull(conn, resp); err != nil {
		return dnsAnswer{}, err
	}
	return parseResponse(resp, id, q)
}

// close ends the exchange: it stops the retry timers, closes the sockets
// and waits for their readers.
func (l *dnsExchange) close(cancel context.CancelFunc) {
	cancel()
	for _, q := range l.queries {
		if q.timer != nil {
			q.timer.Stop()
		}
	}
	for _, c := range l.conns {
		_ = c.Close()
	}
	l.readers.Wait()
}

// newQueryID returns a random DNS message ID.
func newQueryID() uint16 {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint16(b[:])
}

// buildQuery returns the query message with the two-byte length prefix
// of TCP; UDP sends it without.
func buildQuery(id uint16, q dnsmessage.Question) ([]byte, error) {
	b := dnsmessage.NewBuilder(make([]byte, 2, 2+512), dnsmessage.Header{ID: id, RecursionDesired: true})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	if err := b.Question(q); err != nil {
		return nil, err
	}
	if err := b.StartAdditionals(); err != nil {
		return nil, err
	}
	var rh dnsmessage.ResourceHeader
	if err := rh.SetEDNS0(dnsUDPSize, dnsmessage.RCodeSuccess, false); err != nil {
		return nil, err
	}
	if err := b.OPTResource(rh, dnsmessage.OPTResource{}); err != nil {
		return nil, err
	}
	msg, err := b.Finish()
	if err != nil {
		return nil, err
	}
	binary.BigEndian.PutUint16(msg, uint16(len(msg)-2))
	return msg, nil
}

// dnsAnswer is a parsed answer to one question.
type dnsAnswer struct {
	h dnsmessage.Header
	// addrs are the addresses of the question's type that belong to its
	// name or its CNAME chain.
	addrs []netip.Addr
	// ttl is the smallest TTL of the records used: the address records
	// and the CNAME chain. An answer without addresses takes the
	// negative TTL of the SOA record in the authority section instead
	// (RFC 2308), if any. 0 if unknown.
	ttl time.Duration
}

// parseResponse checks that msg answers question q of the query with id
// and returns the answer. It returns errStray for a message that is not
// that answer, and another error for a malformed answer.
func parseResponse(msg []byte, id uint16, q dnsmessage.Question) (dnsAnswer, error) {
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	a := dnsAnswer{h: h}
	if err != nil || !h.Response || h.ID != id {
		return a, errStray
	}
	rq, err := p.Question()
	if err != nil || rq.Type != q.Type || rq.Class != q.Class || !strings.EqualFold(rq.Name.String(), q.Name.String()) {
		return a, errStray
	}
	if h.Truncated || h.RCode != dnsmessage.RCodeSuccess {
		return a, nil
	}
	if err := p.SkipAllQuestions(); err != nil {
		return a, err
	}

	type record struct {
		owner string
		addr  netip.Addr
		ttl   uint32
	}
	type alias struct {
		target string
		ttl    uint32
	}
	var records []record
	cnames := map[string]alias{}
	for {
		rh, err := p.AnswerHeader()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			break
		}
		if err != nil {
			return a, err
		}
		owner := strings.ToLower(rh.Name.String())
		switch {
		case rh.Class != dnsmessage.ClassINET:
			err = p.SkipAnswer()
		case rh.Type == dnsmessage.TypeCNAME:
			var r dnsmessage.CNAMEResource
			if r, err = p.CNAMEResource(); err == nil {
				cnames[owner] = alias{strings.ToLower(r.CNAME.String()), rh.TTL}
			}
		case rh.Type == q.Type && q.Type == dnsmessage.TypeA:
			var r dnsmessage.AResource
			if r, err = p.AResource(); err == nil {
				records = append(records, record{owner, netip.AddrFrom4(r.A), rh.TTL})
			}
		case rh.Type == q.Type && q.Type == dnsmessage.TypeAAAA:
			var r dnsmessage.AAAAResource
			if r, err = p.AAAAResource(); err == nil {
				records = append(records, record{owner, netip.AddrFrom16(r.AAAA), rh.TTL})
			}
		default:
			err = p.SkipAnswer()
		}
		if err != nil {
			return a, err
		}
	}

	// ttl is the smallest TTL seen; known reports whether there was one.
	var ttl uint32
	known := false
	seen := func(t uint32) {
		if !known || t < ttl {
			ttl, known = t, true
		}
	}
	// The names of the chain: q's name and up to maxCNAMEHops aliases.
	names := map[string]bool{}
	name := strings.ToLower(q.Name.String())
	for range maxCNAMEHops + 1 {
		if names[name] {
			break
		}
		names[name] = true
		next, ok := cnames[name]
		if !ok {
			break
		}
		seen(next.ttl)
		name = next.target
	}
	for _, r := range records {
		if names[r.owner] {
			a.addrs = append(a.addrs, r.addr)
			seen(r.ttl)
		}
	}
	if len(a.addrs) == 0 {
		if neg, ok := negativeTTL(&p); ok {
			seen(neg)
		}
	}
	if known {
		a.ttl = time.Duration(ttl) * time.Second
	}
	return a, nil
}

// negativeTTL returns the negative TTL of the SOA record in the authority
// section that p is at, the smaller of its TTL and MINIMUM (RFC 2308). A
// malformed authority section counts as none.
func negativeTTL(p *dnsmessage.Parser) (uint32, bool) {
	for {
		rh, err := p.AuthorityHeader()
		if err != nil {
			return 0, false
		}
		if rh.Type != dnsmessage.TypeSOA {
			if p.SkipAuthority() != nil {
				return 0, false
			}
			continue
		}
		soa, err := p.SOAResource()
		if err != nil {
			return 0, false
		}
		return min(rh.TTL, soa.MinTTL), true
	}
}
