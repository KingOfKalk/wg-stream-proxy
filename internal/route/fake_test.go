// SPDX-License-Identifier: AGPL-3.0-or-later

package route

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"
)

// fakeClock is a manual Clock: timers fire only in Advance.
type fakeClock struct {
	mu     sync.Mutex
	cond   *sync.Cond
	now    time.Duration
	timers []*fakeTimer
}

type fakeTimer struct {
	c      *fakeClock
	when   time.Duration
	f      func()
	active bool
}

func newFakeClock() *fakeClock {
	c := &fakeClock{}
	c.cond = sync.NewCond(&c.mu)
	return c
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: c, when: c.now + d, f: f, active: true}
	c.timers = append(c.timers, t)
	c.cond.Broadcast()
	return t
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.active
	t.active = false
	return was
}

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.active
	t.active = true
	t.when = t.c.now + d
	t.c.cond.Broadcast()
	return was
}

// Advance moves the clock forward and runs the timers that are due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now += d
	var due []func()
	for _, t := range c.timers {
		if t.active && t.when <= c.now {
			t.active = false
			due = append(due, t.f)
		}
	}
	c.mu.Unlock()
	for _, f := range due {
		f()
	}
}

// WaitActive blocks until at least n timers are pending.
func (c *fakeClock) WaitActive(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		active := 0
		for _, t := range c.timers {
			if t.active {
				active++
			}
		}
		if active >= n {
			return
		}
		c.cond.Wait()
	}
}

// dialFunc answers one dial attempt of a fakeNet.
type dialFunc func(ctx context.Context) (net.Conn, error)

// fakeNet is an in-memory Network. Hosts resolve via hosts; dials are
// answered by dials[address], or by fallback if set.
type fakeNet struct {
	mu       sync.Mutex
	hosts    map[string][]netip.Addr
	dials    map[string]dialFunc
	fallback func(ctx context.Context, network, address string) (net.Conn, error)
	lookups  []string
	dialed   []string // "network address"
	started  chan string
}

var errNoRoute = errors.New("fake: no route to host")

func (n *fakeNet) LookupNetIP(ctx context.Context, host string) ([]netip.Addr, error) {
	n.mu.Lock()
	n.lookups = append(n.lookups, host)
	addrs, ok := n.hosts[host]
	n.mu.Unlock()
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return slices.Clone(addrs), nil
}

func (n *fakeNet) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	n.mu.Lock()
	n.dialed = append(n.dialed, network+" "+address)
	f := n.dials[address]
	n.mu.Unlock()
	if n.started != nil {
		n.started <- address
	}
	if f != nil {
		return f(ctx)
	}
	if n.fallback != nil {
		return n.fallback(ctx, network, address)
	}
	return nil, &net.OpError{Op: "dial", Net: network, Err: errNoRoute}
}

func (n *fakeNet) Lookups() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.lookups)
}

func (n *fakeNet) Dialed() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.dialed)
}

// hang blocks until ctx is done.
func hang(ctx context.Context) (net.Conn, error) {
	<-ctx.Done()
	return nil, &net.OpError{Op: "dial", Net: "tcp", Err: ctx.Err()}
}

// fail returns err at once.
func fail(err error) dialFunc {
	return func(context.Context) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: err}
	}
}

// pipe returns a dialFunc handing out the client end of a net.Pipe; the
// server end is sent on peer.
func pipe(peer chan<- net.Conn) dialFunc {
	return func(context.Context) (net.Conn, error) {
		c, s := net.Pipe()
		peer <- s
		return c, nil
	}
}
