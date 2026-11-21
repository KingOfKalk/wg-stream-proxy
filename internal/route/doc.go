// SPDX-License-Identifier: AGPL-3.0-or-later

// Package route implements the upstream side of spec 2.1, 3.4 and 3.5:
// one Route per tunnel plus "direct", each with its own HTTP transports
// (normal and ICY) that resolve and dial through a Network, the
// private-range guard checked on every dial, and the upstream HTTP rules
// (request and response headers, redirects, limits, timeouts, status
// mapping and error classification) as functions for the HTTP server.
package route
