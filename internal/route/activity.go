// SPDX-License-Identifier: AGPL-3.0-or-later

package route

import "sync"

// Activity counts a route's upstream requests in flight; a tunnel uses
// the count for its idle close and drain (spec 2.1). *tunnel.Tunnel
// implements it.
type Activity interface {
	// Begin marks the start of a request; the returned func marks its
	// end.
	Begin() (end func())
}

// begin counts a request on the route's Activity, if any. The returned
// func ends it; calls after the first do nothing.
func (r *Route) begin() func() {
	if r.activity == nil {
		return func() {}
	}
	return sync.OnceFunc(r.activity.Begin())
}
