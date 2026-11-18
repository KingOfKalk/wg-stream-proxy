// SPDX-License-Identifier: AGPL-3.0-or-later

package route

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"time"
)

// fallbackDelay is the Happy-Eyeballs delay before the next address is
// tried while the previous attempt is still pending (RFC 8305: 250ms
// recommended; a little more for tunnels).
const fallbackDelay = 300 * time.Millisecond

var (
	// ErrConnectTimeout: no connection within connect_timeout, lookup
	// included.
	ErrConnectTimeout = errors.New("route: connect timeout")

	errNoAddress      = errors.New("route: no address for network")
	errInvalidAddress = errors.New("route: invalid dial address")
)

// dialError marks errors of the connect phase (spec 7.3 reason connect,
// unless the guard blocked the address).
type dialError struct{ err error }

func (e *dialError) Error() string { return "dial: " + e.err.Error() }
func (e *dialError) Unwrap() error { return e.err }

// dial is the DialContext of the route's transport. It resolves the host
// inside the route's network (IP literals skip the lookup), checks every
// candidate with the guard and connects Happy-Eyeballs style, all within
// connect_timeout. Every connection of the route, redirects included,
// goes through here, so the guard sees each resolved IP at connect time.
func (r *Route) dial(ctx context.Context, network, address string) (net.Conn, error) {
	c, err := r.dialAddr(ctx, network, address)
	if err != nil {
		return nil, &dialError{err}
	}
	return c, nil
}

func (r *Route) dialAddr(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("%w: network %q", errInvalidAddress, network)
	}
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errInvalidAddress, err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("%w: port %q", errInvalidAddress, portStr)
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	timer := r.clock.AfterFunc(r.upstream.ConnectTimeout, func() { cancel(ErrConnectTimeout) })
	defer timer.Stop()

	tr := traceFrom(ctx)
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else {
		m := tr.mark()
		ips, err = r.network.LookupNetIP(ctx, host)
		tr.lookupDone(m)
		if err != nil {
			return nil, timeoutCause(ctx, err)
		}
	}

	var candidates []netip.AddrPort
	var blockErr error
	for _, ip := range ips {
		ip = ip.Unmap()
		if network == "tcp4" && !ip.Is4() || network == "tcp6" && !ip.Is6() {
			continue
		}
		if err := r.guard.Check(ip); err != nil {
			if blockErr == nil {
				blockErr = err
			}
			continue
		}
		candidates = append(candidates, netip.AddrPortFrom(ip, uint16(port)))
	}
	if len(candidates) == 0 {
		if blockErr != nil {
			return nil, blockErr
		}
		return nil, fmt.Errorf("%w %s: %s", errNoAddress, network, host)
	}

	m := tr.mark()
	c, attempts, err := r.dialParallel(ctx, interleaveAddrPorts(candidates))
	tr.connectDone(m, attempts)
	if err != nil {
		return nil, timeoutCause(ctx, err)
	}
	return c, nil
}

// timeoutCause returns ErrConnectTimeout if ctx ended because of it, else
// err.
func timeoutCause(ctx context.Context, err error) error {
	if errors.Is(context.Cause(ctx), ErrConnectTimeout) {
		return ErrConnectTimeout
	}
	return err
}

// dialParallel connects to the first reachable candidate (RFC 8305,
// simplified): the next attempt starts after fallbackDelay or as soon as
// the previous one failed; the first success wins, the other attempts
// are cancelled and connections that still succeed are closed. It also
// returns the number of addresses tried.
func (r *Route) dialParallel(ctx context.Context, cands []netip.AddrPort) (net.Conn, int, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		c   net.Conn
		err error
	}
	results := make(chan result, len(cands)) // never blocks a dialer
	next := make(chan struct{}, 1)
	// Registered before the first attempt starts, so a test clock sees it.
	stagger := r.clock.AfterFunc(fallbackDelay, func() {
		select {
		case next <- struct{}{}:
		default:
		}
	})
	defer stagger.Stop()

	started, finished := 0, 0
	defer func() {
		// Close connections of attempts that are still running.
		if pending := started - finished; pending > 0 {
			go func() {
				for range pending {
					if res := <-results; res.c != nil {
						_ = res.c.Close()
					}
				}
			}()
		}
	}()
	start := func() {
		ap := cands[started]
		started++
		network := "tcp4"
		if ap.Addr().Is6() {
			network = "tcp6"
		}
		go func() {
			c, err := r.network.DialContext(ctx, network, ap.String())
			results <- result{c, err}
		}()
	}
	startNext := func() {
		start()
		select {
		case <-next: // drop a pending tick, the delay starts over
		default:
		}
		stagger.Reset(fallbackDelay)
	}

	start()
	var firstErr error
	for {
		select {
		case <-next:
			if started < len(cands) {
				startNext()
			}
		case res := <-results:
			finished++
			if res.err == nil {
				return res.c, started, nil
			}
			if firstErr == nil {
				firstErr = res.err
			}
			switch {
			case started < len(cands):
				startNext()
			case finished == started:
				return nil, started, firstErr
			}
		case <-ctx.Done():
			return nil, started, context.Cause(ctx)
		}
	}
}

// interleaveAddrPorts orders candidates by alternating address families,
// starting with the family of the first one (RFC 8305 section 4).
func interleaveAddrPorts(aps []netip.AddrPort) []netip.AddrPort {
	ips := make([]netip.Addr, len(aps))
	for i, ap := range aps {
		ips[i] = ap.Addr()
	}
	ports := aps[0].Port()
	out := make([]netip.AddrPort, 0, len(aps))
	for _, ip := range interleave(ips) {
		out = append(out, netip.AddrPortFrom(ip, ports))
	}
	return out
}

// interleave alternates address families, starting with the family of
// the first address; order within a family is kept.
func interleave(ips []netip.Addr) []netip.Addr {
	if len(ips) == 0 {
		return nil
	}
	var first, second []netip.Addr
	for _, ip := range ips {
		if ip.Is4() == ips[0].Is4() {
			first = append(first, ip)
		} else {
			second = append(second, ip)
		}
	}
	out := make([]netip.Addr, 0, len(ips))
	for i := 0; i < len(first) || i < len(second); i++ {
		if i < len(first) {
			out = append(out, first[i])
		}
		if i < len(second) {
			out = append(out, second[i])
		}
	}
	return out
}
