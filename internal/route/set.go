// SPDX-License-Identifier: AGPL-3.0-or-later

package route

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
)

// Set holds one Route per tunnel plus the direct route (spec 2.1).
type Set struct {
	routes map[string]*Route
}

// NewSet builds the direct route over direct (normally Host()) and one
// route per tunnel over its Network. Every route gets its own
// http.Transport and guard with opts.AllowPrivate[name] as exceptions.
func NewSet(direct Network, tunnels map[string]Network, opts Options) (*Set, error) {
	s, _, err := build(nil, direct, tunnels, opts)
	return s, err
}

// Next builds the set of a reloaded configuration like NewSet (spec 5.6).
// A route of s whose network, activity, guard exceptions and settings are
// the same is kept, with its pooled connections; the others are new.
// dropped are the routes of s that are not part of the next set; s and
// its routes stay usable for requests that started before. The caller
// closes their idle connections when they are no longer used.
func (s *Set) Next(direct Network, tunnels map[string]Network, opts Options) (next *Set, dropped []*Route, err error) {
	return build(s, direct, tunnels, opts)
}

// build returns a new set, reusing the unchanged routes of prev (which
// may be nil), and the routes of prev it does not reuse.
func build(prev *Set, direct Network, tunnels map[string]Network, opts Options) (*Set, []*Route, error) {
	if direct == nil {
		return nil, nil, errors.New("route: no direct network")
	}
	nets := map[string]Network{config.RouteDirect: direct}
	for _, name := range slices.Sorted(maps.Keys(tunnels)) {
		switch n := tunnels[name]; {
		case name == config.RouteDirect || !config.ValidName(name):
			return nil, nil, fmt.Errorf("route: invalid tunnel name %q", name)
		case n == nil:
			return nil, nil, fmt.Errorf("route: tunnel %q has no network", name)
		default:
			nets[name] = n
		}
	}
	for _, name := range slices.Sorted(maps.Keys(opts.AllowPrivate)) {
		if _, ok := nets[name]; !ok {
			return nil, nil, fmt.Errorf("route: allow_private for unknown route %q", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(opts.Activity)) {
		if _, ok := nets[name]; !ok {
			return nil, nil, fmt.Errorf("route: activity for unknown route %q", name)
		}
	}

	s := &Set{routes: make(map[string]*Route, len(nets))}
	var dropped []*Route
	for name, n := range nets {
		if old, ok := prev.route(name); ok && old.same(routeSettings(name, n, opts)) {
			s.routes[name] = old
			continue
		}
		s.routes[name] = newRoute(name, n, opts)
	}
	if prev != nil {
		for _, name := range slices.Sorted(maps.Keys(prev.routes)) {
			if old := prev.routes[name]; s.routes[name] != old {
				dropped = append(dropped, old)
			}
		}
	}
	return s, dropped, nil
}

// route returns the route name of s; a nil s has none.
func (s *Set) route(name string) (*Route, bool) {
	if s == nil {
		return nil, false
	}
	r, ok := s.routes[name]
	return r, ok
}

// Route returns the route with the given name (tunnel name or
// config.RouteDirect).
func (s *Set) Route(name string) (*Route, bool) {
	return s.route(name)
}

// CloseIdleConnections closes the idle connections of all routes.
func (s *Set) CloseIdleConnections() {
	for _, r := range s.routes {
		r.CloseIdleConnections()
	}
}
