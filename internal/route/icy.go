// SPDX-License-Identifier: AGPL-3.0-or-later

package route

import (
	"fmt"
	"context"
	"net"
	"sync"
)

// Shoutcast v1 servers answer "ICY 200 OK" instead of an HTTP status
// line; Go's HTTP parser accepts it as "HTTP/1.0 200 OK" (spec 3.3).
const (
	icyPrefix  = "ICY "
	icyReplace = "HTTP/1.0 "
)

// dialICY is the DialContext of the route's ICY transport: the route's
// dial with every connection wrapped by newICYConn.
func (r *Route) dialICY(ctx context.Context, network, address string) (net.Conn, error) {
	c, err := r.dial(ctx, network, address)
	if err != nil {
		return nil, err
	}
	return newICYConn(c), nil
}

// icyConn rewrites a leading "ICY " on its connection to "HTTP/1.0 ".
// Only the first bytes read on the connection are looked at, so only the
// first response of a connection can be rewritten; a Shoutcast v1
// response has no length and ends with the connection anyway. Anything
// that does not start with "ICY " (an HTTP status line, a TLS record)
// passes through unchanged.
//
// Reads are not safe for concurrent use, like reads of the transport's
// connections; all other methods are those of the wrapped connection.
type icyConn struct {
	net.Conn
	decided bool
	buf     [len(icyPrefix)]byte
	// head holds bytes to return before reading on: the bytes read while
	// deciding, or icyReplace.
	head []byte
	// err is a read error met while deciding, returned once after head.
	err error
	dbg []string
	mu  sync.Mutex
}

func newICYConn(c net.Conn) net.Conn { return &icyConn{Conn: c} }

func (c *icyConn) Close() error {
	c.mu.Lock()
	println("ICYDEBUG close", fmt.Sprint(c.dbg))
	c.mu.Unlock()
	return c.Conn.Close()
}

func (c *icyConn) Read(p []byte) (int, error) {
	n, err := c.read(p)
	c.mu.Lock()
	if len(c.dbg) < 6 {
		c.dbg = append(c.dbg, fmt.Sprintf("read(len %d)=%d %q %v", len(p), n, p[:min(n, 12)], err))
	}
	c.mu.Unlock()
	return n, err
}

func (c *icyConn) read(p []byte) (int, error) {
	if !c.decided {
		if len(p) == 0 {
			return 0, nil
		}
		c.decide()
	}
	if len(c.head) > 0 {
		n := copy(p, c.head)
		c.head = c.head[n:]
		return n, nil
	}
	if c.err != nil {
		err := c.err
		c.err = nil
		return 0, err
	}
	return c.Conn.Read(p)
}

// decide reads until the first bytes either differ from "ICY " or match
// it entirely, at most len(icyPrefix) bytes, and sets head. Short reads
// and a partial prefix are handled by reading on; a read error ends the
// decision with the bytes read so far.
func (c *icyConn) decide() {
	c.decided = true
	n := 0
	for n < len(icyPrefix) && string(c.buf[:n]) == icyPrefix[:n] {
		k, err := c.Conn.Read(c.buf[n:])
		n += k
		if err != nil {
			c.err = err
			break
		}
	}
	if string(c.buf[:n]) == icyPrefix {
		c.head = []byte(icyReplace)
		return
	}
	c.head = c.buf[:n]
}
