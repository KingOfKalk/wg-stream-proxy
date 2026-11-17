// SPDX-License-Identifier: AGPL-3.0-or-later

package tunnel

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// dnsRetryDelays is the time from one attempt of a DNS query to the next
// (spec 2.1); the last delay repeats until the caller's ctx ends. Lookups
// through a tunnel take tens of milliseconds, so after 1s a query counts
// as lost.
var dnsRetryDelays = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

const (
	// dnsUDPSize is the EDNS(0) UDP payload size offered to servers, as in
	// Go's resolver.
	dnsUDPSize = 1232
	// dnsReadSize is the UDP read buffer; larger than dnsUDPSize for
	// servers that ignore EDNS(0).
	dnsReadSize = 4096
	// maxCNAMEHops bounds the CNAME chain followed in an answer.
	maxCNAMEHops = 8
)

// errStray: a DNS message is not the answer to the query it was read
// for. Such messages are ignored.
var errStray = errors.New("dns: stray message")

// dnsClient resolves names with the tunnel's DNS servers, over UDP and
// TCP inside the netstack. It replaces the netstack resolver, which
// retries a lost query only after 5s.
type dnsClient struct {
	net     *netstack.Net
	servers []netip.Addr
	clock   Clock
}

// dnsQuery is one question of a lookup and its state. Only the lookup's
// own goroutine touches it.
type dnsQuery struct {
	q        dnsmessage.Question
	attempts int // attempts sent
	failures int // error answers
	timer    Timer
	done     bool
	addrs    []netip.Addr
	err      error
}

// dnsResult is the outcome of one attempt of a query.
type dnsResult struct {
	query int
	h     dnsmessage.Header
	addrs []netip.Addr
	err   error // transport or format error
}

// dnsRetry asks the lookup to send the next attempt of a query, unless a
// later attempt than the one numbered attempt was sent meanwhile.
type dnsRetry struct {
	query, attempt int
}

// dnsLookup is one lookup in progress.
type dnsLookup struct {
	c       *dnsClient
	ctx     context.Context
	queries []*dnsQuery
	results chan dnsResult
	retries chan dnsRetry
	conns   []net.Conn
	readers sync.WaitGroup
}

// lookup resolves host with one query per type, in parallel. Each query
// is retried on the next server after dnsRetryDelays, at once after an
// error answer, and over TCP after a truncated answer, until ctx ends.
// NXDOMAIN ends the lookup at once. A query fails once it got as many
// error answers as there are servers; the lookup then returns the
// addresses of the other queries, if any.
func (c *dnsClient) lookup(ctx context.Context, host string, types []dnsmessage.Type) ([]netip.Addr, error) {
	notFound := &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	name, ok := queryName(host)
	if !ok {
		return nil, notFound
	}
	ctx, cancel := context.WithCancel(ctx)
	l := &dnsLookup{
		c:       c,
		ctx:     ctx,
		results: make(chan dnsResult),
		retries: make(chan dnsRetry),
	}
	defer l.close(cancel)
	for _, typ := range types {
		l.queries = append(l.queries, &dnsQuery{q: dnsmessage.Question{Name: name, Type: typ, Class: dnsmessage.ClassINET}})
	}
	for i := range l.queries {
		l.send(i)
	}

	for pending := len(l.queries); pending > 0; {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("lookup %s: %w", host, ctx.Err())
		case r := <-l.retries:
			if q := l.queries[r.query]; !q.done && q.attempts == r.attempt+1 {
				l.send(r.query)
			}
		case r := <-l.results:
			q := l.queries[r.query]
			if q.done {
				// A late answer to an earlier attempt.
				continue
			}
			switch {
			case r.err == nil && r.h.RCode == dnsmessage.RCodeSuccess:
				q.addrs = r.addrs
			case r.err == nil && r.h.RCode == dnsmessage.RCodeNameError:
				return nil, notFound
			default:
				q.failures++
				if q.failures < len(c.servers) {
					l.send(r.query)
					continue
				}
				q.err = &net.DNSError{Err: "server misbehaving", Name: host, IsTemporary: true}
			}
			q.done = true
			q.timer.Stop()
			pending--
		}
	}

	var addrs []netip.Addr
	var err error
	for _, q := range l.queries {
		addrs = append(addrs, q.addrs...)
		if q.err != nil {
			err = q.err
		}
	}
	if len(addrs) == 0 && err != nil {
		return nil, err
	}
	return addrs, nil
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

// send sends the next attempt of query i to the next server and arms its
// retry timer. Earlier attempts keep listening: a late answer still
// counts. An attempt that cannot be sent counts as lost.
func (l *dnsLookup) send(i int) {
	q := l.queries[i]
	attempt := q.attempts
	q.attempts++
	server := netip.AddrPortFrom(l.c.servers[attempt%len(l.c.servers)], 53)
	if q.timer != nil {
		q.timer.Stop()
	}
	delay := dnsRetryDelays[min(attempt, len(dnsRetryDelays)-1)]
	q.timer = l.c.clock.AfterFunc(delay, func() {
		select {
		case l.retries <- dnsRetry{query: i, attempt: attempt}:
		case <-l.ctx.Done():
		}
	})

	id := newQueryID()
	msg, err := buildQuery(id, q.q)
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
	l.readers.Go(func() { l.read(conn, i, id, server) })
}

// read waits for the answer to one UDP attempt, ignoring stray messages,
// and repeats the query over TCP after a truncated answer.
func (l *dnsLookup) read(conn net.Conn, i int, id uint16, server netip.AddrPort) {
	q := l.queries[i].q
	buf := make([]byte, dnsReadSize)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			l.post(dnsResult{query: i, err: err})
			return
		}
		h, addrs, err := parseResponse(buf[:n], id, q)
		if errors.Is(err, errStray) {
			continue
		}
		if err == nil && h.Truncated {
			h, addrs, err = l.exchangeTCP(server, q)
		}
		l.post(dnsResult{query: i, h: h, addrs: addrs, err: err})
		return
	}
}

// post hands a result to the lookup, unless the lookup is over.
func (l *dnsLookup) post(r dnsResult) {
	select {
	case l.results <- r:
	case <-l.ctx.Done():
	}
}

// exchangeTCP asks q over TCP (RFC 7766), for answers too large for UDP.
func (l *dnsLookup) exchangeTCP(server netip.AddrPort, q dnsmessage.Question) (dnsmessage.Header, []netip.Addr, error) {
	conn, err := l.c.net.DialContextTCPAddrPort(l.ctx, server)
	if err != nil {
		return dnsmessage.Header{}, nil, err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(l.ctx, func() { _ = conn.Close() })
	defer stop()

	id := newQueryID()
	msg, err := buildQuery(id, q)
	if err != nil {
		return dnsmessage.Header{}, nil, err
	}
	if _, err := conn.Write(msg); err != nil {
		return dnsmessage.Header{}, nil, err
	}
	var size [2]byte
	if _, err := io.ReadFull(conn, size[:]); err != nil {
		return dnsmessage.Header{}, nil, err
	}
	resp := make([]byte, binary.BigEndian.Uint16(size[:]))
	if _, err := io.ReadFull(conn, resp); err != nil {
		return dnsmessage.Header{}, nil, err
	}
	return parseResponse(resp, id, q)
}

// close ends the lookup: it stops the retry timers, closes the sockets
// and waits for their readers.
func (l *dnsLookup) close(cancel context.CancelFunc) {
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

// parseResponse checks that msg answers question q of the query with id
// and returns its header and the addresses of q's type that belong to
// q's name or its CNAME chain. It returns errStray for a message that is
// not that answer, and another error for a malformed answer.
func parseResponse(msg []byte, id uint16, q dnsmessage.Question) (dnsmessage.Header, []netip.Addr, error) {
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil || !h.Response || h.ID != id {
		return h, nil, errStray
	}
	rq, err := p.Question()
	if err != nil || rq.Type != q.Type || rq.Class != q.Class || !strings.EqualFold(rq.Name.String(), q.Name.String()) {
		return h, nil, errStray
	}
	if h.Truncated || h.RCode != dnsmessage.RCodeSuccess {
		return h, nil, nil
	}
	if err := p.SkipAllQuestions(); err != nil {
		return h, nil, err
	}

	type record struct {
		owner string
		addr  netip.Addr
	}
	var records []record
	cnames := map[string]string{}
	for {
		rh, err := p.AnswerHeader()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			break
		}
		if err != nil {
			return h, nil, err
		}
		owner := strings.ToLower(rh.Name.String())
		switch {
		case rh.Class != dnsmessage.ClassINET:
			err = p.SkipAnswer()
		case rh.Type == dnsmessage.TypeCNAME:
			var r dnsmessage.CNAMEResource
			if r, err = p.CNAMEResource(); err == nil {
				cnames[owner] = strings.ToLower(r.CNAME.String())
			}
		case rh.Type == q.Type && q.Type == dnsmessage.TypeA:
			var r dnsmessage.AResource
			if r, err = p.AResource(); err == nil {
				records = append(records, record{owner, netip.AddrFrom4(r.A)})
			}
		case rh.Type == q.Type && q.Type == dnsmessage.TypeAAAA:
			var r dnsmessage.AAAAResource
			if r, err = p.AAAAResource(); err == nil {
				records = append(records, record{owner, netip.AddrFrom16(r.AAAA)})
			}
		default:
			err = p.SkipAnswer()
		}
		if err != nil {
			return h, nil, err
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
		name = next
	}
	var addrs []netip.Addr
	for _, r := range records {
		if names[r.owner] {
			addrs = append(addrs, r.addr)
		}
	}
	return h, addrs, nil
}
