// SPDX-License-Identifier: AGPL-3.0-or-later

// Package hls implements the stream rules of spec 3.1, 3.2, 4 and 4.1 as
// pure functions for the HTTP server: type detection, the stateless HLS
// playlist rewriter, signed resource URLs (/r/...), prefix-signed URLs
// (/p/...) and loading of the signing key. It does no I/O of its own
// beyond peeking at the start of a body the caller passes (Sniff);
// fetching, size limits on reads and HTTP status mapping belong to the
// caller.
package hls
