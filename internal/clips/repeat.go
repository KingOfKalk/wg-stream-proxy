// SPDX-License-Identifier: AGPL-3.0-or-later

package clips

import (
	"errors"
	"fmt"
)

// Offset is the timestamp shift of position 0, in 90 kHz ticks: 10 s.
// The segments' PCR starts 0.96 s before the first DTS, at a negative
// value that wraps modulo 2^33; with the offset no served PCR wraps.
const Offset = 10 * 90000

// MaxPosition is the highest position Repeat is meant for in a playlist
// (docs/clips.md 6.2: /clip/{name}/{n}.ts with n from 0 to 63).
const MaxPosition = 63

const (
	tsPacket = 188
	tsSync   = 0x47
	nullPID  = 0x1fff
	// timestampMod is the range of PTS, DTS and the PCR base: 33 bits.
	timestampMod = int64(1) << 33
)

// Repeat returns a copy of seg, an MPEG-TS segment, as position n (n >= 0)
// of a stream that plays seg again and again: every PTS, DTS, PCR and
// OPCR shifted by Offset + n × SegmentTicks (modulo 2^33), and the
// continuity counter of every PID continued from position n-1. Copies 0, 1,
// 2, … of a segment whose timestamps run from 0 to SegmentTicks and whose
// counters are continuous within the segment (the built-in clips) form
// one continuous stream.
//
// Only fixed header fields change; the payload is untouched. seg is not
// modified. Malformed packets give an error (and no output).
func Repeat(seg []byte, n int) ([]byte, error) {
	if n < 0 {
		return nil, fmt.Errorf("clips: negative position %d", n)
	}
	// Payload packets per PID: the continuity counter step of one copy.
	var count [nullPID + 1]int
	for i := 0; i+tsPacket <= len(seg); i += tsPacket {
		p := seg[i : i+tsPacket]
		if p[3]&0x10 != 0 {
			count[pidOf(p)]++
		}
	}
	m := int64(n) % timestampMod
	ticks := (Offset + m*SegmentTicks) % timestampMod
	return shift(seg, ticks, func(pid int) int { return n % 16 * count[pid] })
}

// shift returns a copy of ts, MPEG-TS packets, with every PTS, DTS, PCR
// base and OPCR base advanced by ticks modulo 2^33, and the continuity
// counter of every packet of PID p (null packets excepted) advanced by
// cc(p) modulo 16. Which bytes are fields depends only on bytes shift
// does not change, so shifting the result by the inverse amounts gives ts
// back.
func shift(ts []byte, ticks int64, cc func(pid int) int) ([]byte, error) {
	if len(ts)%tsPacket != 0 {
		return nil, fmt.Errorf("clips: length %d is not a multiple of %d", len(ts), tsPacket)
	}
	ticks = (ticks%timestampMod + timestampMod) % timestampMod
	out := make([]byte, len(ts))
	copy(out, ts)
	for i := 0; i < len(out); i += tsPacket {
		if err := shiftPacket(out[i:i+tsPacket], ticks, cc); err != nil {
			return nil, fmt.Errorf("clips: packet %d: %w", i/tsPacket, err)
		}
	}
	return out, nil
}

var (
	errSync      = errors.New("bad sync byte")
	errAdapt     = errors.New("adaptation field longer than the packet")
	errPCR       = errors.New("PCR flag without room for the PCR")
	errOPCR      = errors.New("OPCR flag without room for the OPCR")
	errPESHeader = errors.New("PES header with timestamps does not fit the packet")
)

// shiftPacket rewrites one 188-byte packet in place.
func shiftPacket(p []byte, ticks int64, cc func(pid int) int) error {
	if p[0] != tsSync {
		return errSync
	}
	pid := pidOf(p)
	if pid != nullPID {
		p[3] = p[3]&0xf0 | byte((int(p[3]&0x0f)+cc(pid))&0x0f)
	}
	afc := p[3] >> 4 & 3
	payload := 4
	if afc&2 != 0 {
		n := int(p[4])
		if 5+n > tsPacket || afc == 2 && n != tsPacket-5 {
			return errAdapt
		}
		if n > 0 {
			flags, pos := p[5], 6
			if flags&0x10 != 0 { // PCR
				if pos+6 > 5+n {
					return errPCR
				}
				shiftPCR(p[pos:pos+6], ticks)
				pos += 6
			}
			if flags&0x08 != 0 { // OPCR
				if pos+6 > 5+n {
					return errOPCR
				}
				shiftPCR(p[pos:pos+6], ticks)
			}
		}
		payload = 5 + n
	}
	if afc&1 == 0 || p[1]&0x40 == 0 {
		return nil
	}
	return shiftPES(p[payload:], ticks)
}

// shiftPES shifts PTS and DTS of a PES header at the start of b, the
// payload of a packet that starts a PES packet or a PSI section. b is
// left alone if it is not a PES header with the optional header fields.
func shiftPES(b []byte, ticks int64) error {
	if len(b) < 4 || b[0] != 0 || b[1] != 0 || b[2] != 1 || !hasPESHeader(b[3]) {
		return nil
	}
	if len(b) < 9 {
		return errPESHeader
	}
	if b[6]&0xc0 != 0x80 {
		return nil
	}
	hdr := int(b[8])
	switch b[7] >> 6 {
	case 2: // PTS
		if hdr < 5 || len(b) < 14 {
			return errPESHeader
		}
		shiftTimestamp(b[9:14], ticks)
	case 3: // PTS and DTS
		if hdr < 10 || len(b) < 19 {
			return errPESHeader
		}
		shiftTimestamp(b[9:14], ticks)
		shiftTimestamp(b[14:19], ticks)
	}
	return nil
}

// hasPESHeader reports whether a PES packet with this stream id has the
// optional header with PTS and DTS (ISO/IEC 13818-1, 2.4.3.7).
func hasPESHeader(streamID byte) bool {
	switch streamID {
	case 0xbc, 0xbe, 0xbf, 0xf0, 0xf1, 0xf2, 0xf8, 0xff:
		return false
	}
	return streamID >= 0xbc
}

func pidOf(p []byte) int { return int(p[1]&0x1f)<<8 | int(p[2]) }

// readTimestamp reads a 33-bit PTS or DTS from its 5 bytes.
func readTimestamp(b []byte) int64 {
	return int64(b[0]>>1&7)<<30 | int64(b[1])<<22 | int64(b[2]>>1)<<15 | int64(b[3])<<7 | int64(b[4]>>1)
}

// shiftTimestamp adds ticks to a PTS or DTS, keeping its prefix and marker
// bits.
func shiftTimestamp(b []byte, ticks int64) {
	ts := (readTimestamp(b) + ticks) % timestampMod
	b[0] = b[0]&0xf1 | byte(ts>>29)&0x0e
	b[1] = byte(ts >> 22)
	b[2] = b[2]&0x01 | byte(ts>>14)&0xfe
	b[3] = byte(ts >> 7)
	b[4] = b[4]&0x01 | byte(ts<<1)
}

// readPCR reads the 33-bit base of a PCR or OPCR from its 6 bytes.
func readPCR(b []byte) int64 {
	return int64(b[0])<<25 | int64(b[1])<<17 | int64(b[2])<<9 | int64(b[3])<<1 | int64(b[4]>>7)
}

// shiftPCR adds ticks to the base of a PCR or OPCR, keeping the reserved
// bits and the extension.
func shiftPCR(b []byte, ticks int64) {
	base := (readPCR(b) + ticks) % timestampMod
	b[0] = byte(base >> 25)
	b[1] = byte(base >> 17)
	b[2] = byte(base >> 9)
	b[3] = byte(base >> 1)
	b[4] = b[4]&0x7f | byte(base<<7)
}
