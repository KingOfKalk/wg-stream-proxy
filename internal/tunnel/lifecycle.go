// SPDX-License-Identifier: AGPL-3.0-or-later

package tunnel

import (
	"fmt"
	"sync"
	"time"
)

// Lifecycle after the lazy start (spec 2.1): a running tunnel closes
// after Options.IdleTimeout without activity, and Drain and Shutdown
// give in-flight requests a bounded time before the device closes.
//
// Activity is a request counted with Begin, plus the time of the latest
// lookup, dial, Begin or end (LastActivity). The idle timer runs only
// while no request is in flight; connections the caller keeps idle for
// reuse are not activity.

// Begin counts a request that uses the tunnel, from before its first
// lookup or dial until its response body is closed; the returned end
// marks its end and may be called more than once. While a request is in
// flight the tunnel is not closed for idleness, and Drain and Shutdown
// wait for it. Begin starts nothing: the first lookup or dial does.
func (t *Tunnel) Begin() (end func()) {
	t.touch()
	t.mu.Lock()
	t.inflight++
	if t.inflight == 1 {
		t.busy = make(chan struct{})
	}
	t.disarmIdle()
	t.mu.Unlock()
	var once sync.Once
	return func() { once.Do(t.end) }
}

func (t *Tunnel) end() {
	t.touch()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.inflight--
	if t.inflight == 0 {
		close(t.busy)
		t.busy = nil
		t.armIdle()
	}
}

// inFlight returns the number of requests between Begin and end.
func (t *Tunnel) inFlight() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.inflight
}

// OnStop sets f to be called each time the running device stops: idle
// close, Drain, Shutdown or Close. f runs before the device closes; the
// route layer closes its pooled connections through the tunnel there.
// f replaces an earlier one; nil removes it.
func (t *Tunnel) OnStop(f func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onStop = f
}

// armIdle starts the idle timer for the full idle timeout. t.mu is held.
func (t *Tunnel) armIdle() {
	t.armIdleIn(t.m.opts.IdleTimeout)
}

// armIdleIn starts the idle timer for d, replacing an armed one, if idle
// close is enabled and the device runs. t.mu is held.
func (t *Tunnel) armIdleIn(d time.Duration) {
	t.disarmIdle()
	if t.m.opts.IdleTimeout == 0 || t.run.Load() == nil || isClosed(t.m.closing) {
		return
	}
	gen := t.idleGen
	t.idle = t.m.opts.Clock.AfterFunc(d, func() { t.idleFired(gen) })
}

// disarmIdle stops the idle timer. t.mu is held.
func (t *Tunnel) disarmIdle() {
	t.idleGen++
	if t.idle != nil {
		t.idle.Stop()
		t.idle = nil
	}
}

// idleFired closes the tunnel if it is still idle: no request in flight
// and no activity for the idle timeout. After later activity it waits
// for the rest of the timeout.
func (t *Tunnel) idleFired(gen uint64) {
	t.mu.Lock()
	if gen != t.idleGen || t.inflight > 0 {
		t.mu.Unlock()
		return
	}
	t.idle = nil
	if rest := t.m.opts.IdleTimeout - t.m.opts.Clock.Now().Sub(t.LastActivity()); rest > 0 {
		t.armIdleIn(rest)
		t.mu.Unlock()
		return
	}
	r, hook := t.detach()
	t.mu.Unlock()
	t.finishStop(r, hook, "idle")
}

// stop stops the running device, if any. A later lookup or dial starts
// it again unless the manager is closed.
func (t *Tunnel) stop(reason string) {
	t.mu.Lock()
	r, hook := t.detach()
	t.mu.Unlock()
	t.finishStop(r, hook, reason)
}

// detach disarms the idle timer and takes the running device. t.mu is
// held.
func (t *Tunnel) detach() (*running, func()) {
	t.disarmIdle()
	return t.run.Swap(nil), t.onStop
}

// finishStop runs the stop hook and closes r, outside t.mu.
func (t *Tunnel) finishStop(r *running, hook func(), reason string) {
	if r == nil {
		return
	}
	if hook != nil {
		hook()
	}
	r.close()
	t.m.opts.Logger.Info("tunnel closed", "tunnel", t.cfg.Name, "reason", reason)
}

// Drain stops the tunnel name gracefully (spec 2.1, reload removal): it
// waits until no request on it is in flight, at most timeout (measured
// by Options.Clock), then closes the device; connections still open
// break. The tunnel stays usable: a later request starts it again.
func (m *Manager) Drain(name string, timeout time.Duration) error {
	t, ok := m.tunnels[name]
	if !ok {
		return fmt.Errorf("tunnel %q: no such tunnel", name)
	}
	m.wait([]*Tunnel{t}, timeout)
	t.stop("drain")
	return nil
}

// Shutdown drains every tunnel with one shared timeout, like Drain, then
// closes the manager like Close (spec 2.1, shutdown).
func (m *Manager) Shutdown(timeout time.Duration) error {
	m.wait(m.sorted, timeout)
	return m.Close()
}

// wait blocks until no request on tunnels is in flight or timeout has
// passed since the call. It logs the tunnels whose requests are cut.
func (m *Manager) wait(tunnels []*Tunnel, timeout time.Duration) {
	var (
		expired chan struct{}
		timer   Timer
	)
	for _, t := range tunnels {
		t.mu.Lock()
		busy := t.busy
		t.mu.Unlock()
		if busy == nil {
			continue
		}
		if expired == nil {
			// The timer starts only when there is something to wait for.
			expired = make(chan struct{})
			if timeout > 0 {
				ch := expired
				timer = m.opts.Clock.AfterFunc(timeout, func() { close(ch) })
			} else {
				close(expired)
			}
		}
		select {
		case <-busy:
		case <-expired:
			m.opts.Logger.Warn("drain timeout: closing tunnel with requests in flight",
				"tunnel", t.cfg.Name, "requests", t.inFlight())
		}
	}
	if timer != nil {
		timer.Stop()
	}
}
