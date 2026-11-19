// SPDX-License-Identifier: AGPL-3.0-or-later

package clips

import (
	"bytes"
	"strings"
	"testing"
)

// TestRepeat plays every built-in segment as copies 0..k-1 of itself,
// concatenated: parseTS checks the continuity counters across the joins,
// and the timestamps must run on without gap or overlap from Offset.
func TestRepeat(t *testing.T) {
	const mod = int64(1) << 33
	for _, name := range Names() {
		t.Run(name, func(t *testing.T) {
			seg, _ := Segment(name)
			orig := bytes.Clone(seg)
			ref := parseTS(t, seg)
			for _, k := range []int{1, 2, 5, MaxPosition + 1} {
				var stream []byte
				for n := range k {
					out, err := Repeat(seg, n)
					if err != nil {
						t.Fatalf("Repeat(%d): %v", n, err)
					}
					if len(out) != len(seg) {
						t.Fatalf("Repeat(%d): %d bytes, want %d", n, len(out), len(seg))
					}
					stream = append(stream, out...)
				}
				if !bytes.Equal(seg, orig) {
					t.Fatal("Repeat modified its input")
				}
				p := parseTS(t, stream)
				if len(p.video) != k*videoFrames {
					t.Fatalf("k=%d: %d video PES, want %d", k, len(p.video), k*videoFrames)
				}
				for i, e := range p.video {
					if want := (Offset + int64(i)*frameTicks) % mod; e.pts != want || e.dts != want {
						t.Fatalf("k=%d: video PES %d: pts %d dts %d, want %d", k, i, e.pts, e.dts, want)
					}
				}
				perSeg := len(ref.audio)
				if len(p.audio) != k*perSeg {
					t.Fatalf("k=%d: %d audio PES, want %d", k, len(p.audio), k*perSeg)
				}
				for i, e := range p.audio {
					copyN, j := i/perSeg, i%perSeg
					if want := (ref.audio[j].pts + Offset + int64(copyN)*segmentTicks) % mod; e.pts != want {
						t.Fatalf("k=%d: audio PES %d: pts %d, want %d", k, i, e.pts, want)
					}
				}
				// Every PCR is shifted the same way and none wraps: the
				// offset is larger than the lead of the PCR.
				if len(p.pcrs) != k*len(ref.pcrs) {
					t.Fatalf("k=%d: %d PCRs, want %d", k, len(p.pcrs), k*len(ref.pcrs))
				}
				for i, pcr := range p.pcrs {
					copyN, j := i/len(ref.pcrs), i%len(ref.pcrs)
					want := (ref.pcrs[j] + Offset + int64(copyN)*segmentTicks) % mod
					if pcr != want {
						t.Fatalf("k=%d: PCR %d = %d, want %d", k, i, pcr, want)
					}
					if pcr > Offset+int64(k)*segmentTicks {
						t.Fatalf("k=%d: PCR %d = %d wrapped", k, i, pcr)
					}
					if i > 0 && pcr < p.pcrs[i-1] {
						t.Fatalf("k=%d: PCR %d = %d goes back from %d", k, i, pcr, p.pcrs[i-1])
					}
				}
			}
		})
	}
}

// TestRepeatChangesOnlyTimestampsAndCounters: Repeat(seg, 0) and the
// input differ only in PTS/DTS/PCR bytes and continuity counters, so the
// payload (pictures, sound, tables) is untouched.
func TestRepeatChangesOnlyTimestampsAndCounters(t *testing.T) {
	seg, _ := Segment(ChannelNotAvailable)
	out, err := Repeat(seg, 3)
	if err != nil {
		t.Fatal(err)
	}
	changed := 0
	for i := range seg {
		if seg[i] != out[i] {
			changed++
		}
	}
	if changed == 0 {
		t.Fatal("nothing changed")
	}
	// Upper bound: per packet the counter byte, plus a PCR (6 bytes) or
	// a PES header with PTS (5 bytes) per packet at most.
	if packets := len(seg) / tsPacketSize; changed > packets*12 {
		t.Fatalf("%d bytes changed in %d packets", changed, packets)
	}
	// The payload of a packet without a timestamp is byte for byte the
	// same: compare the packets past their header.
	for i := 0; i < len(seg); i += tsPacketSize {
		a, b := seg[i:i+tsPacketSize], out[i:i+tsPacketSize]
		if a[1]&0x40 == 0 && a[3]&0x20 == 0 && !bytes.Equal(a[4:], b[4:]) {
			t.Fatalf("packet %d: payload changed", i/tsPacketSize)
		}
		if a[3]&0xf0 != b[3]&0xf0 || !bytes.Equal(a[:3], b[:3]) {
			t.Fatalf("packet %d: header changed: % x -> % x", i/tsPacketSize, a[:4], b[:4])
		}
	}
}

func TestRepeatErrors(t *testing.T) {
	pkt := func(edit func(p []byte)) []byte {
		p := make([]byte, tsPacketSize)
		p[0] = 0x47
		p[1], p[2] = 0x01, 0x00 // PID 256
		p[3] = 0x10             // payload only
		edit(p)
		return p
	}
	seg, _ := Segment(Starting)
	tests := []struct {
		name string
		data []byte
		n    int
		want string
	}{
		{"negative position", seg, -1, "position"},
		{"partial packet", seg[:tsPacketSize+1], 0, "multiple of 188"},
		{"sync byte", pkt(func(p []byte) { p[0] = 0x46 }), 0, "sync byte"},
		{"adaptation field too long", pkt(func(p []byte) { p[3] = 0x30; p[4] = 184 }), 0, "adaptation field"},
		{"adaptation field only too short", pkt(func(p []byte) { p[3] = 0x20; p[4] = 100 }), 0, "adaptation field"},
		{"pcr flag without room", pkt(func(p []byte) { p[3] = 0x30; p[4] = 3; p[5] = 0x10 }), 0, "PCR"},
		{"opcr flag without room", pkt(func(p []byte) { p[3] = 0x30; p[4] = 7; p[5] = 0x18 }), 0, "OPCR"},
		{
			"pes header split", pkt(func(p []byte) {
				p[1] |= 0x40 // PES start
				p[3] = 0x30
				p[4] = 183 - 10 // 10 bytes of payload left
				copy(p[5+p[4]:], []byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0x80, 5, 0x21})
			}), 0, "PES header",
		},
		{
			"pts flag with short header", pkt(func(p []byte) {
				p[1] |= 0x40
				copy(p[4:], []byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0xc0, 5, 0x31})
			}), 0, "PES header",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := Repeat(tt.data, tt.n)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Repeat = %d bytes, %v; want error containing %q", len(out), err, tt.want)
			}
			if out != nil {
				t.Fatalf("output %d bytes with error", len(out))
			}
		})
	}
}

// TestRepeatEmpty: no packets, nothing to do.
func TestRepeatEmpty(t *testing.T) {
	out, err := Repeat(nil, 4)
	if err != nil || len(out) != 0 {
		t.Fatalf("Repeat(nil) = %v, %v", out, err)
	}
}

// TestShiftWraps: timestamps and counters wrap like the stream fields do.
func TestShiftWraps(t *testing.T) {
	const mod = int64(1) << 33
	// One PES start with PTS and DTS near the top, one PCR, counter 15.
	p := make([]byte, tsPacketSize)
	p[0], p[1], p[2], p[3] = 0x47, 0x41, 0x00, 0x3f // PES start, PID 256, AF + payload, CC 15
	p[4], p[5] = 7, 0x10                            // adaptation field with PCR
	putPCR(p[6:12], mod-1)
	pes := p[12:]
	copy(pes, []byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0xc0, 10})
	putTimestamp(pes[9:14], 0x3, mod-2)
	putTimestamp(pes[14:19], 0x1, mod-3)
	orig := bytes.Clone(p)
	out, err := shift(p, 5, func(int) int { return 3 })
	if err != nil {
		t.Fatal(err)
	}
	if got := readPCR(out[6:12]); got != 4 {
		t.Errorf("PCR %d, want 4", got)
	}
	if got := readTimestamp(out[12+9:]); got != 3 {
		t.Errorf("PTS %d, want 3", got)
	}
	if got := readTimestamp(out[12+14:]); got != 2 {
		t.Errorf("DTS %d, want 2", got)
	}
	if got := out[3] & 0x0f; got != 2 {
		t.Errorf("continuity counter %d, want 2", got)
	}
	// Prefix and marker bits are kept.
	if out[12+9]&0xf1 != orig[12+9]&0xf1 || out[12+14]&0xf1 != orig[12+14]&0xf1 {
		t.Errorf("timestamp prefix or marker bits changed: % x", out[12+9:12+19])
	}
	if out[10]&0x7f != orig[10]&0x7f || out[11] != orig[11] {
		t.Errorf("PCR reserved or extension bits changed: % x -> % x", orig[10:12], out[10:12])
	}
}

// putTimestamp writes a 33-bit PTS or DTS with the 4-bit prefix and the
// marker bits.
func putTimestamp(b []byte, prefix byte, ts int64) {
	b[0] = prefix<<4 | byte(ts>>29)&0x0e | 1
	b[1] = byte(ts >> 22)
	b[2] = byte(ts>>14)&0xfe | 1
	b[3] = byte(ts >> 7)
	b[4] = byte(ts<<1) | 1
}

// putPCR writes a PCR base with all reserved bits set and extension 0.
func putPCR(b []byte, base int64) {
	b[0] = byte(base >> 25)
	b[1] = byte(base >> 17)
	b[2] = byte(base >> 9)
	b[3] = byte(base >> 1)
	b[4] = byte(base<<7) | 0x7e
	b[5] = 0
}
