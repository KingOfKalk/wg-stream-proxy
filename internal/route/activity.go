// SPDX-License-Identifier: AGPL-3.0-or-later

package route

import (
	"context"
	"sync"
)

// Activity counts a route's upstream requests in flight; a tunnel uses
// the count for its idle close and drain (spec 2.1). *tunnel.Tunnel
// implements it.
type Activity interface {
	// Begin marks the start of a request; the returned func marks its
	// end.
	Begin() (end func())
}

// countedKey is the context key of Begin: the value is the route the
// request is counted on.
type countedKey struct{}

// Begin counts a request on the route's Activity until end is called;
// calls of end after the first do nothing. A Fetch on this route with
// the returned context does not count again. A caller that fetches more
// than once for one request (a refetch, the probe) uses it, so the
// tunnel does not see the request end in between; so does a caller that
// must count before its first fetch (a reload drains a retired tunnel
// only for the requests counted on it, spec 5.6).
func (r *Route) Begin(ctx context.Context) (context.Context, func()) {
	return context.WithValue(ctx, countedKey{}, r), r.begin()
}

// begin counts a request on the route's Activity, if any. The returned
// func ends it; calls after the first do nothing.
func (r *Route) begin() func() {
	if r.activity == nil {
		return func() {}
	}
	return sync.OnceFunc(r.activity.Begin())
}

// beginFetch is begin for a Fetch with ctx: nothing if Begin already
// counts the request on this route.
func (r *Route) beginFetch(ctx context.Context) func() {
	if ctx.Value(countedKey{}) == r {
		return func() {}
	}
	return r.begin()
}
