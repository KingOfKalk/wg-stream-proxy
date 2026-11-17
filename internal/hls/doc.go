// SPDX-License-Identifier: AGPL-3.0-or-later

// Package hls implements the stream rules of spec 3.1, 3.2 and 4 as pure
// functions for the HTTP server: type detection, the stateless HLS
// playlist rewriter, signed resource URLs (/r/...) and loading of the
// signing key. It does no I/O of its own; fetching, size limits on reads
// and HTTP status mapping belong to the caller.
package hls
