// SPDX-License-Identifier: AGPL-3.0-or-later

package tunnel

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config/wgquick"
)

// Reload (spec 5.6) has two steps, so that no tunnel is retired while a
// published route can still hand it to a new request. Apply makes the new
// tunnel set current and returns an Update; the caller builds its routes
// over the new set and publishes them, then calls Commit, which retires
// the replaced tunnels. If the caller fails in between, Rollback restores
// the old set.
//
// A tunnel whose parsed file is unchanged keeps its *Tunnel, so its
// device, pooled connections and idle state stay. A changed or removed
// tunnel is retired at Commit: its *Tunnel never starts again, a start
// in progress is cancelled, and it is drained in the background
// (in-flight requests get the drain timeout, then the device closes).
// Until Commit it works as before. Changed and added tunnels get a new
// *Tunnel that starts lazily, like every tunnel.

// Changes lists the tunnel names an Apply added, changed and removed,
// each sorted.
type Changes struct {
	Added, Changed, Removed []string
}

// errRetired is the error of a start on a retired tunnel, or of a start
// a reload cancelled. Callers answer 502 like for any unavailable tunnel.
func errRetired(name string) error {
	return fmt.Errorf("tunnel %q: %w: replaced by a reload", name, ErrUnavailable)
}

// errUpdating: Apply was called before the previous Update was committed
// or rolled back.
var errUpdating = errors.New("tunnel: a reload is in progress")

// Update is a tunnel set Apply made current, until Commit or Rollback
// finishes it. Only one Update is pending at a time.
type Update struct {
	// Changes are the names Apply added, changed and removed.
	Changes

	m *Manager
	// prev is the tunnel set before Apply.
	prev map[string]*Tunnel
	// created are the tunnels Apply created (added and changed), replaced
	// the ones the new set no longer has (changed and removed).
	created, replaced []*Tunnel
	// finished: Commit or Rollback ran. Guarded by m.mu.
	finished bool
}

// Apply makes tunnels the manager's tunnel set, the first step of a
// reload. tunnels are checked like in New; on error nothing changes. The
// tunnels the set no longer has keep working until the returned Update
// is committed. Apply fails while another Update is pending.
func (m *Manager) Apply(tunnels []*wgquick.Tunnel) (*Update, error) {
	if err := checkTunnels(tunnels); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.closed:
		return nil, ErrClosed
	case m.update != nil:
		return nil, errUpdating
	}
	u := &Update{m: m, prev: m.tunnels}
	next := make(map[string]*Tunnel, len(tunnels))
	for _, cfg := range tunnels {
		old, ok := m.tunnels[cfg.Name]
		switch {
		case ok && old.cfg.Equal(cfg):
			next[cfg.Name] = old
			continue
		case ok:
			u.Changed = append(u.Changed, cfg.Name)
			u.replaced = append(u.replaced, old)
		default:
			u.Added = append(u.Added, cfg.Name)
		}
		t := newTunnel(m, cfg)
		next[cfg.Name] = t
		u.created = append(u.created, t)
	}
	for name, old := range m.tunnels {
		if _, ok := next[name]; !ok {
			u.Removed = append(u.Removed, name)
			u.replaced = append(u.replaced, old)
		}
	}
	m.setTunnels(next)
	m.update = u
	slices.Sort(u.Added)
	slices.Sort(u.Changed)
	slices.Sort(u.Removed)
	return u, nil
}

// Commit retires the tunnels the update replaced, the last step of a
// reload: they start no more, a start in progress is cancelled, and each
// drains for at most drainTimeout (measured by Options.Clock) in the
// background; Close includes them. Call it once routes over the new set
// serve, after every request that may still use a replaced tunnel counts
// on it (Begin). Commit after Commit or Rollback does nothing.
func (u *Update) Commit(drainTimeout time.Duration) {
	m := u.m
	m.mu.Lock()
	if u.finished {
		m.mu.Unlock()
		return
	}
	u.finished = true
	m.update = nil
	closed := m.closed
	retire(u.replaced)
	if !closed {
		for _, t := range u.replaced {
			m.draining[t] = struct{}{}
			m.drains.Add(1)
		}
	}
	m.mu.Unlock()

	for _, t := range u.replaced {
		m.opts.Logger.Debug("tunnel retired", "tunnel", t.cfg.Name, "running", t.Running(), "requests", t.inFlight())
		if closed {
			// Close stops them; no drain starts after it waited for the
			// drains.
			t.stop("shutdown")
			continue
		}
		go m.drainRetired(t, drainTimeout)
	}
}

// Rollback restores the tunnel set from before Apply, for a reload that
// failed after it: the replaced tunnels stay as they are, the tunnels
// Apply created are retired and never start. Rollback after Commit or
// Rollback does nothing.
func (u *Update) Rollback() {
	m := u.m
	m.mu.Lock()
	if u.finished {
		m.mu.Unlock()
		return
	}
	u.finished = true
	m.update = nil
	m.setTunnels(u.prev)
	retire(u.created)
	m.mu.Unlock()
	// Nothing routes to them; a start someone began is cancelled.
	for _, t := range u.created {
		t.stop("reload")
	}
}

// retire marks tunnels as replaced by a reload and cancels their starts
// in progress. m.mu is held.
func retire(tunnels []*Tunnel) {
	for _, t := range tunnels {
		t.retired = true
		close(t.retiring)
	}
}

// drainRetired waits until no request on the retired tunnel t is in
// flight, at most timeout, then closes its device.
func (m *Manager) drainRetired(t *Tunnel, timeout time.Duration) {
	defer m.drains.Done()
	t.draining.Add(1)
	defer t.draining.Add(-1)
	m.wait([]*Tunnel{t}, timeout)
	t.stop("reload")
	m.mu.Lock()
	delete(m.draining, t)
	m.mu.Unlock()
}

// SetTimeouts changes the connect timeout of later starts and the idle
// timeout (a reload of [upstream] connect_timeout and [tunnels]
// idle_timeout). A running tunnel without requests in flight re-arms its
// idle timer for the rest of the new idle timeout since its last
// activity; 0 disables idle close.
func (m *Manager) SetTimeouts(connect, idle time.Duration) error {
	if err := checkTimeouts(connect, idle); err != nil {
		return err
	}
	m.connectTimeout.Store(int64(connect))
	if time.Duration(m.idleTimeout.Swap(int64(idle))) == idle {
		return nil
	}
	now := m.opts.Clock.Now()
	for _, t := range m.Tunnels() {
		t.mu.Lock()
		if t.inflight == 0 {
			t.armIdleIn(max(0, idle-now.Sub(t.LastActivity())))
		}
		t.mu.Unlock()
	}
	return nil
}
