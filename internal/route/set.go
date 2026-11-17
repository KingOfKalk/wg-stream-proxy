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
	if direct == nil {
		return nil, errors.New("route: no direct network")
	}
	s := &Set{routes: map[string]*Route{config.RouteDirect: newRoute(config.RouteDirect, direct, opts)}}
	for _, name := range slices.Sorted(maps.Keys(tunnels)) {
		switch n := tunnels[name]; {
		case name == config.RouteDirect || !config.ValidName(name):
			return nil, fmt.Errorf("route: invalid tunnel name %q", name)
		case n == nil:
			return nil, fmt.Errorf("route: tunnel %q has no network", name)
		default:
			s.routes[name] = newRoute(name, n, opts)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(opts.AllowPrivate)) {
		if _, ok := s.routes[name]; !ok {
			return nil, fmt.Errorf("route: allow_private for unknown route %q", name)
		}
	}
	return s, nil
}

// Route returns the route with the given name (tunnel name or
// config.RouteDirect).
func (s *Set) Route(name string) (*Route, bool) {
	r, ok := s.routes[name]
	return r, ok
}

// CloseIdleConnections closes the idle connections of all routes.
func (s *Set) CloseIdleConnections() {
	for _, r := range s.routes {
		r.CloseIdleConnections()
	}
}
