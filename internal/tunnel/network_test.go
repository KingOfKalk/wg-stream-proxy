// SPDX-License-Identifier: AGPL-3.0-or-later

package tunnel_test

import (
	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel"
)

// A tunnel is a route's Network; the route layer defines the interface.
var _ route.Network = (*tunnel.Tunnel)(nil)
