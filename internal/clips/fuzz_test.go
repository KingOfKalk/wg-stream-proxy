// SPDX-License-Identifier: AGPL-3.0-or-later

package clips

import (
	"bytes"
	"testing"
)

// FuzzShift is the L2 target of the timestamp and continuity counter
// rewrite (docs/testing.md): for any input and shift, shift does not
// panic, keeps the length, leaves its input alone, and shifting back by
// the inverse amounts gives the input again, byte for byte.
func FuzzShift(f *testing.F) {
	for _, name := range Names() {
		seg, _ := Segment(name)
		f.Add(seg, int64(Offset), uint8(1))
	}
	seg, _ := Segment(Starting)
	f.Add(seg[:3*tsPacketSize], int64(-1), uint8(15))
	f.Add([]byte{}, int64(0), uint8(0))
	f.Add(bytes.Repeat([]byte{0x47}, tsPacketSize), int64(1)<<40, uint8(7))
	f.Fuzz(func(t *testing.T, data []byte, ticks int64, step uint8) {
		orig := bytes.Clone(data)
		cc := func(pid int) int { return (int(step) + pid) & 0xf }
		out, err := shift(data, ticks, cc)
		if !bytes.Equal(data, orig) {
			t.Fatal("shift modified its input")
		}
		if err != nil {
			if out != nil {
				t.Fatalf("output of %d bytes with error %v", len(out), err)
			}
			return
		}
		if len(out) != len(data) {
			t.Fatalf("%d bytes out of %d", len(out), len(data))
		}
		const mod = int64(1) << 33
		back, err := shift(out, mod-((ticks%mod)+mod)%mod, func(pid int) int { return 16 - cc(pid) })
		if err != nil {
			t.Fatalf("shifting back: %v", err)
		}
		if !bytes.Equal(back, data) {
			t.Fatal("shifting back does not give the input")
		}
	})
}
