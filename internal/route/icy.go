// SPDX-License-Identifier: AGPL-3.0-or-later

package route

import (
	"context"
	"errors"
	"net"
	"sync"
)

// Shoutcast v1 servers answer "ICY 200 OK" instead of an HTTP status
// line; Go's HTTP parser accepts it as "HTTP/1.0 200 OK" (spec 3.3).
const (
	icyPrefix  = "ICY "
	icyReplace = "HTTP/1.0 "
)

// errICYConnReused: a request was written on a connection that carried
// an ICY answer. Nothing is written, so the transport retries the request
// on a new connection.
var errICYConnReused = errors.New("route: request on a connection that carried an ICY answer")

// dialICY is the DialContext of the route's ICY transport: the route's
// dial with every connection wrapped by newICYConn.
func (r *Route) dialICY(ctx context.Context, network, address string) (net.Conn, error) {
	c, err := r.dial(ctx, network, address)
	if err != nil {
		return nil, err
	}
	return newICYConn(c), nil
}

// icyConn rewrites a leading "ICY " of a response on its connection to
// "HTTP/1.0 ". It checks the first bytes read on the connection and, if
// they started a plain HTTP response ("H"), the first bytes read after
// each later request, so every response of a kept-alive connection may
// be an ICY one: a Shoutcast v1 server may answer one port with HTTP or
// ICY per path (#63). Anything else passes through unchanged; a TLS
// connection (its first byte is a TLS record type) is checked only once.
//
// The transport writes a request only after the previous response was
// read to its end, and may already wait for the next response while it
// writes, so a write arms the check for the next bytes read, even by a
// Read that is already waiting.
//
// A connection that carried an ICY answer takes no further request: such
// an answer has no length and ends with its connection. Writes fail with
// errICYConnReused, even if the server claimed keep-alive.
//
// Reads are not safe for concurrent use, like reads of the transport's
// connections; a Write may run concurrently with a Read.
type icyConn struct {
	net.Conn

	mu sync.Mutex
	// armed: the next bytes read start a response and are checked.
	armed bool
	// plain: the first response started with "H"; later responses are
	// checked too.
	plain bool
	// checked: a response was checked since the check was last armed, so
	// the next write starts a new request.
	checked bool
	// icy: an ICY status line was rewritten.
	icy bool

	// head holds bytes to return before reading on: the rewritten start
	// of a response, or the bytes read while checking it.
	head []byte
	// err is a read error met while checking, returned once after head.
	err error
}

func newICYConn(c net.Conn) net.Conn { return &icyConn{Conn: c, armed: true} }

func (c *icyConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
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
	n, err := c.Conn.Read(p)
	if n == 0 || !c.disarm() {
		return n, err
	}
	return c.check(p, n, err)
}

// disarm reports whether the check was armed and clears it.
func (c *icyConn) disarm() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	armed := c.armed
	c.armed = false
	return armed
}

// check looks at p[:n], the first bytes of a response, read with err.
// While they are a proper prefix of "ICY " it reads on, up to
// len(icyPrefix) bytes; a read error ends the check with the bytes read
// so far. It returns like Read.
func (c *icyConn) check(p []byte, n int, err error) (int, error) {
	got := p[:n]
	if n < len(icyPrefix) && err == nil && string(got) == icyPrefix[:n] {
		buf := make([]byte, n, len(icyPrefix))
		copy(buf, got)
		for len(buf) < len(icyPrefix) && err == nil && string(buf) == icyPrefix[:len(buf)] {
			var k int
			k, err = c.Conn.Read(buf[len(buf):len(icyPrefix)])
			buf = buf[:len(buf)+k]
		}
		got = buf
	}
	icy := len(got) >= len(icyPrefix) && string(got[:len(icyPrefix)]) == icyPrefix

	c.mu.Lock()
	if !c.plain && !c.checked {
		// The first response of the connection: checked is reset only
		// on plain connections.
		c.plain = got[0] == 'H'
	}
	c.checked = true
	c.icy = c.icy || icy
	c.mu.Unlock()

	switch {
	case icy:
		c.head = append([]byte(icyReplace), got[len(icyPrefix):]...)
	case len(got) > n:
		c.head = got
	default:
		return n, err
	}
	c.err = err
	return c.Read(p)
}

// Write arms the check for the next response if a request follows a
// checked response on a plain HTTP connection; after an ICY answer it
// fails without writing.
func (c *icyConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	icy := c.icy
	if c.plain && c.checked {
		c.armed = true
		c.checked = false
	}
	c.mu.Unlock()
	if icy {
		return 0, errICYConnReused
	}
	return c.Conn.Write(b)
}
