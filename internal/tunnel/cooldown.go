// SPDX-License-Identifier: AGPL-3.0-or-later

package tunnel

import (
	"errors"
	"fmt"
	"time"
)

// ErrNoSuchTunnel: no tunnel of that name is configured.
var ErrNoSuchTunnel = errors.New("no such tunnel")

// State is the lifecycle state of a tunnel, as the debug endpoints show
// it (spec 7.1).
type State string

// The states of a tunnel.
const (
	// StateStopped: no device runs; the next request starts one.
	StateStopped State = "stopped"
	// StateStarting: a start is in progress (device up, first handshake).
	StateStarting State = "starting"
	// StateRunning: the device runs.
	StateRunning State = "running"
	// StateDraining: the device runs, and a drain or cool-down waits for
	// its requests before it stops.
	StateDraining State = "draining"
)

// Snapshot is the state of one tunnel at one moment.
type Snapshot struct {
	Name  string
	State State
	// StartedAt is when the running device completed its first
	// handshake, LastHandshake its latest handshake; both zero while no
	// device runs.
	StartedAt     time.Time
	LastHandshake time.Time
	// ActiveRequests counts the requests in flight (Begin until end).
	ActiveRequests int
}

// Snapshot returns the current state of t. Its fields are read one after
// the other, so a tunnel that changes state meanwhile may show a mix.
func (t *Tunnel) Snapshot() Snapshot {
	t.m.mu.Lock()
	starting := t.start != nil
	t.m.mu.Unlock()
	t.mu.Lock()
	active := t.inflight
	t.mu.Unlock()

	s := Snapshot{Name: t.cfg.Name, State: StateStopped, ActiveRequests: active}
	r := t.run.Load()
	switch {
	case r != nil && t.draining.Load() > 0:
		s.State = StateDraining
	case r != nil:
		s.State = StateRunning
	case starting:
		s.State = StateStarting
	}
	if r != nil {
		s.StartedAt = r.started
		s.LastHandshake = lastHandshake(r.dev)
	}
	return s
}

// Cooldown stops the tunnel name on demand (spec 7.1, the debug
// endpoints' cool-down): like a reload removal it waits until no request
// on it is in flight, at most timeout (measured by Options.Clock), then
// stops the device; connections still open break. The stop hook closes
// the route's pooled connections and the DNS cache is dropped, so the
// next request starts the tunnel cold. It reports whether a device was
// running; a stopped tunnel is left as it is.
func (m *Manager) Cooldown(name string, timeout time.Duration) (stopped bool, err error) {
	return m.drain(name, timeout, "cooldown")
}

// drain waits for the requests on the tunnel name, at most timeout, then
// stops its device with reason. It reports whether a device was running.
func (m *Manager) drain(name string, timeout time.Duration, reason string) (bool, error) {
	m.mu.Lock()
	closed := m.closed
	t, ok := m.tunnels[name]
	m.mu.Unlock()
	switch {
	case closed:
		return false, fmt.Errorf("tunnel %q: %w", name, ErrClosed)
	case !ok:
		return false, fmt.Errorf("tunnel %q: %w", name, ErrNoSuchTunnel)
	}
	t.draining.Add(1)
	defer t.draining.Add(-1)
	m.wait([]*Tunnel{t}, timeout)
	return t.stop(reason), nil
}
