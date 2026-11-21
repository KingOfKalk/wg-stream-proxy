// SPDX-License-Identifier: AGPL-3.0-or-later

// Package playertest holds the player tests of docs/testing.md (L4) and
// their fixture server. Fixtures serves the media fixtures of
// testdata/media like broadcasters do: HLS as VOD and as a live window
// that advances with time, DASH as VOD, raw MPEG-TS, and endless
// Icecast-like and Shoutcast v1 (ICY) radio streams.
//
// The player tests run ffmpeg and only run with WSP_PLAYER_TESTS=1. The
// package has no code outside its tests.
package playertest
