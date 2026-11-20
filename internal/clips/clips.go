// SPDX-License-Identifier: AGPL-3.0-or-later

// Package clips holds the built-in error clips (docs/clips.md): one 4.8 s
// MPEG-TS segment per error text, rendered by generate.sh and embedded in
// the binary.
//
// Every segment is H.264 High 1920x1080 at 25 fps (120 frames, one IDR
// first, no B-frames) with a silent AAC-LC stereo 48 kHz track (225 AAC
// frames). Timestamps start at 10 s, so that the PCR, which the muxer
// starts 0.96 s before the first DTS, never wraps below zero.
package clips

import (
	"embed"
	"slices"
)

// The names of the built-in clips, the file names in segments/ without
// ".ts". Texts: docs/clips.md, decision 3.
const (
	// TunnelUnreachable is the error for a tunnel that cannot be reached.
	TunnelUnreachable = "tunnel-unreachable"
	// ChannelNotAvailable is the error for a channel that does not answer
	// or answers with an error.
	ChannelNotAvailable = "channel-not-available"
	// ChannelBlocked is the error for a channel the configuration blocks.
	ChannelBlocked = "channel-blocked"
)

// SegmentDuration is the duration of every segment in seconds, as written
// in an #EXTINF tag (docs/clips.md, 6.4).
const SegmentDuration = "4.800"

//go:embed segments/*.ts
var files embed.FS

var names = []string{TunnelUnreachable, ChannelNotAvailable, ChannelBlocked}

// Names returns the names of the built-in clips.
func Names() []string {
	return slices.Clone(names)
}

// Segment returns a copy of the MPEG-TS segment of the named clip, and false
// for an unknown name.
func Segment(name string) ([]byte, bool) {
	if !slices.Contains(names, name) {
		return nil, false
	}
	b, err := files.ReadFile("segments/" + name + ".ts")
	if err != nil {
		// Unreachable: the embed pattern and names are checked by tests.
		return nil, false
	}
	return b, true
}
