// SPDX-License-Identifier: AGPL-3.0-or-later

package tunnel

import (
	"sync"
	"time"
)

// Lifecycle after the lazy start (spec 2.1): a running tunnel closes
// after Options.IdleTimeout without activity, and a reload removal or
// Cooldown gives in-flight requests a bounded time before the device
// closes.
//
// Activity is a request counted with Begin, plus the time of its Begin
// and end and of the latest lookup or dial outside any request
// (LastActivity). The idle time runs from the latest activity; the
// start of the device is none of its own. The idle timer runs only
// while no request is in flight. A background request (BeginBackground:
// an EPG fetch, spec 6.1) is in flight like a request but is not
// activity. Connections the caller keeps idle for reuse are not
// activity.

// Begin counts a request that uses the tunnel, from before its first
// lookup or dial until its response body is closed; the returned end
// marks its end and may be called more than once. While a request is in
// flight the tunnel is not closed for idleness, and a reload removal or
// Cooldown wait for it. Begin starts nothing: the first lookup or dial does.
func (t *Tunnel) Begin() (end func()) {
	t.touch()
	t.add()
	return sync.OnceFunc(func() { t.end(true) })
}

// BeginBackground counts a background request, like Begin, that is not
// activity: an EPG fetch (spec 2.1, 6.1). While it is in flight the
// tunnel is not closed for idleness, and a reload removal or Cooldown
// wait for it; neither it nor its lookups and dials restart the idle
// time. When it ends, the idle timer runs for what is left of the idle
// timeout since LastActivity; without activity in that time the tunnel
// closes at once.
func (t *Tunnel) BeginBackground() (end func()) {
	t.add()
	return sync.OnceFunc(func() { t.end(false) })
}

// add counts a request in flight and stops the idle timer.
func (t *Tunnel) add() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.inflight++
	if t.inflight == 1 {
		t.busy = make(chan struct{})
	}
	t.disarmIdle()
}

// end ends a request; activity reports whether Begin counted it.
func (t *Tunnel) end(activity bool) {
	if activity {
		t.touch()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.inflight--
	if t.inflight == 0 {
		close(t.busy)
		t.busy = nil
		t.armIdle()
	}
}

// inFlight returns the number of requests between Begin or
// BeginBackground and end.
func (t *Tunnel) inFlight() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.inflight
}

// OnStop sets f to be called each time the running device stops: idle
// close, reload, Cooldown or Close. f runs before the device closes; the
// route layer closes its pooled connections through the tunnel there.
// f replaces an earlier one; nil removes it.
func (t *Tunnel) OnStop(f func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onStop = f
}

// idleLeft returns the rest of the idle timeout since LastActivity; 0
// or less when it is over, also without any activity. The start of the
// device is no activity of its own: a tunnel that only background
// requests used closes when the last of them ends.
func (t *Tunnel) idleLeft() time.Duration {
	return t.m.idle() - t.m.opts.Clock.Now().Sub(t.LastActivity())
}

// armIdle starts the idle timer for the rest of the idle timeout since
// LastActivity, at once if it is over. t.mu is held.
func (t *Tunnel) armIdle() {
	t.armIdleIn(max(0, t.idleLeft()))
}

// armIdleIn starts the idle timer for d, replacing an armed one, if idle
// close is enabled and the device runs. t.mu is held.
func (t *Tunnel) armIdleIn(d time.Duration) {
	t.disarmIdle()
	if t.m.idle() == 0 || t.run.Load() == nil || isClosed(t.m.closing) {
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
	if rest := t.idleLeft(); rest > 0 {
		t.armIdleIn(rest)
		t.mu.Unlock()
		return
	}
	r, hook := t.detach()
	t.mu.Unlock()
	t.finishStop(r, hook, "idle")
}

// stop stops the running device, if any, and reports whether one ran. A
// later lookup or dial starts it again unless the manager is closed.
func (t *Tunnel) stop(reason string) bool {
	t.mu.Lock()
	r, hook := t.detach()
	t.mu.Unlock()
	t.finishStop(r, hook, reason)
	return r != nil
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
	t.m.opts.Logger.Info("tunnel closed", "tunnel", t.cfg.Name, "reason", reason, "uptime", t.m.opts.Clock.Now().Sub(r.started))
}

// wait blocks until no request on tunnels is in flight, timeout has
// passed since the call or the manager is closed. It logs the tunnels
// whose requests are cut by the timeout, and at debug level how long it
// waited for the others.
func (m *Manager) wait(tunnels []*Tunnel, timeout time.Duration) {
	var (
		expired chan struct{}
		timer   Timer
	)
	for _, t := range tunnels {
		t.mu.Lock()
		busy, requests := t.busy, t.inflight
		t.mu.Unlock()
		if busy == nil {
			continue
		}
		// Before the timer starts, so a test clock sees the wait begin.
		begin := m.opts.Clock.Now()
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
			m.opts.Logger.Debug("tunnel drained", "tunnel", t.cfg.Name, "requests", requests,
				"waited", m.opts.Clock.Now().Sub(begin))
		case <-expired:
			m.opts.Logger.Warn("drain timeout: closing tunnel with requests in flight",
				"tunnel", t.cfg.Name, "requests", t.inFlight(), "waited", m.opts.Clock.Now().Sub(begin))
		case <-m.closing:
		}
	}
	if timer != nil {
		timer.Stop()
	}
}
