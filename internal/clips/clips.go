// SPDX-License-Identifier: AGPL-3.0-or-later

// Package clips holds the built-in clip segments (docs/clips.md): one 0.96 s
// MPEG-TS segment per status or error text, rendered by generate.sh and
// embedded in the binary.
//
// Every segment is H.264 High 1920x1080 at 25 fps (24 frames, one IDR first,
// no B-frames) with a silent AAC-LC stereo 48 kHz track (45 AAC frames).
// Timestamps start at 0 and the segments share SPS, PPS and PIDs, so any
// sequence of them plays without a decoder reset. Callers that repeat a
// segment shift its timestamps and continue its continuity counters.
//
// Timestamp details for such a rewrite: PTS and DTS are equal (the PES
// headers carry only the PTS) and start at 0. The PCR is on the video PID
// and starts 0.96 s before the first DTS, so it is negative: stored modulo
// 2^33 (in 90 kHz units), it wraps within the first segment. Do the shift
// modulo 2^33 too.
package clips

import (
	"embed"
	"slices"
)

// The names of the built-in clips, the file names in segments/ without
// ".ts". Texts: docs/clips.md, decision 3.
const (
	// Starting is shown while the tunnel starts.
	Starting = "starting"
	// Connecting is shown during DNS, connect and TLS.
	Connecting = "connecting"
	// Waiting is shown until the real media playlist is in.
	Waiting = "waiting"
	// TunnelUnreachable is the error for a tunnel that cannot be reached.
	TunnelUnreachable = "tunnel-unreachable"
	// ChannelNotAvailable is the error for a channel that does not answer
	// or answers with an error.
	ChannelNotAvailable = "channel-not-available"
	// ChannelBlocked is the error for a channel the configuration blocks.
	ChannelBlocked = "channel-blocked"
)

// Segment format (docs/clips.md, 6.4).
const (
	// SegmentDuration is the duration of every segment in seconds, as
	// written in an #EXTINF tag.
	SegmentDuration = "0.960"
	// SegmentTicks is the duration of every segment in 90 kHz PTS ticks.
	SegmentTicks = 86400
)

//go:embed segments/*.ts
var files embed.FS

var names = []string{Starting, Connecting, Waiting, TunnelUnreachable, ChannelNotAvailable, ChannelBlocked}

// Names returns the names of the built-in clips: three phases, then three
// errors.
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

// Errors returns the names of the error clips (docs/clips.md 3.2).
func Errors() []string {
	return []string{TunnelUnreachable, ChannelNotAvailable, ChannelBlocked}
}
