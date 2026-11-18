// SPDX-License-Identifier: AGPL-3.0-or-later

package tunnel

import (
	"fmt"
	"slices"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config/wgquick"
)

// Reload (spec 5.6): Apply swaps the tunnel set. A tunnel whose parsed
// file is unchanged keeps its *Tunnel, so its device, pooled connections
// and idle state stay. A changed or removed tunnel is retired: its
// *Tunnel never starts again, a start in progress is cancelled, and it is
// drained in the background (in-flight requests get the drain timeout,
// then the device closes). Changed and added tunnels get a new *Tunnel
// that starts lazily, like every tunnel.

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

// Apply makes tunnels the manager's tunnel set (a reload). tunnels are
// checked like in New; on error nothing changes. Retired tunnels drain
// for at most drainTimeout (measured by Options.Clock) in the
// background; Close and Shutdown include them.
func (m *Manager) Apply(tunnels []*wgquick.Tunnel, drainTimeout time.Duration) (Changes, error) {
	if err := checkTunnels(tunnels); err != nil {
		return Changes{}, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Changes{}, ErrClosed
	}
	var ch Changes
	var retired []*Tunnel
	next := make(map[string]*Tunnel, len(tunnels))
	for _, cfg := range tunnels {
		old, ok := m.tunnels[cfg.Name]
		switch {
		case ok && old.cfg.Equal(cfg):
			next[cfg.Name] = old
			continue
		case ok:
			ch.Changed = append(ch.Changed, cfg.Name)
			retired = append(retired, old)
		default:
			ch.Added = append(ch.Added, cfg.Name)
		}
		next[cfg.Name] = newTunnel(m, cfg)
	}
	for name, old := range m.tunnels {
		if _, ok := next[name]; !ok {
			ch.Removed = append(ch.Removed, name)
			retired = append(retired, old)
		}
	}
	m.setTunnels(next)
	for _, t := range retired {
		t.retired = true
		close(t.retiring)
		m.draining[t] = struct{}{}
		m.drains.Add(1)
	}
	m.mu.Unlock()

	for _, t := range retired {
		m.opts.Logger.Debug("tunnel retired", "tunnel", t.cfg.Name, "running", t.Running(), "requests", t.inFlight())
		go m.drainRetired(t, drainTimeout)
	}
	slices.Sort(ch.Added)
	slices.Sort(ch.Changed)
	slices.Sort(ch.Removed)
	return ch, nil
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
