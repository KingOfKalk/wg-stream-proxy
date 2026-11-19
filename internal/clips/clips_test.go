// SPDX-License-Identifier: AGPL-3.0-or-later

package clips

import (
	"bytes"
	"encoding/binary"
	"io/fs"
	"slices"
	"strings"
	"testing"
)

// The tests parse the embedded segments without ffmpeg and check the format
// of docs/clips.md 6.4: MPEG-TS, 0.96 s = 24 frames of 25 fps video = 45
// AAC frames, one IDR first, timestamps from 0.

const (
	tsPacketSize = 188
	ticksPerSec  = 90000 // PTS/DTS clock
	frameTicks   = 3600  // one video frame at 25 fps
	aacTicks     = 1920  // one AAC frame of 1024 samples at 48 kHz
	segmentTicks = 86400 // 0.96 s
	videoFrames  = 24
	aacFrames    = 45

	streamTypeH264 = 0x1b
	streamTypeAAC  = 0x0f // ADTS
	pidPAT         = 0
	pidSDT         = 0x11
)

// pes is one reassembled PES packet.
type pes struct {
	streamID byte
	pts, dts int64 // -1 when absent
	payload  []byte
}

// parsed is the result of parseTS.
type parsed struct {
	pmtPID, videoPID, audioPID int
	video, audio               []pes
	pids                       map[int]int // packets per PID
	firstPCR                   int64       // 90 kHz ticks, -1 when none
}

func ptsAt(b []byte) int64 {
	return int64(b[0]>>1&7)<<30 | int64(b[1])<<22 | int64(b[2]>>1)<<15 | int64(b[3])<<7 | int64(b[4]>>1)
}

// parseTS validates the packet layer and reassembles PAT, PMT and the PES
// packets of the video and the audio PID.
func parseTS(t *testing.T, data []byte) parsed {
	t.Helper()
	if len(data) == 0 || len(data)%tsPacketSize != 0 {
		t.Fatalf("length %d is not a multiple of %d", len(data), tsPacketSize)
	}
	p := parsed{pmtPID: -1, videoPID: -1, audioPID: -1, pids: map[int]int{}, firstPCR: -1}
	counters := map[int]int{}
	sections := map[int][]byte{} // PAT and PMT payloads
	pesBufs := map[int][]byte{}
	flush := func(pid int) {
		buf := pesBufs[pid]
		if len(buf) == 0 {
			return
		}
		e := parsePES(t, buf)
		switch pid {
		case p.videoPID:
			p.video = append(p.video, e)
		case p.audioPID:
			p.audio = append(p.audio, e)
		}
		pesBufs[pid] = nil
	}
	for i := 0; i < len(data); i += tsPacketSize {
		pkt := data[i : i+tsPacketSize]
		if pkt[0] != 0x47 {
			t.Fatalf("packet %d: sync byte %#x", i/tsPacketSize, pkt[0])
		}
		if pkt[1]&0x80 != 0 {
			t.Fatalf("packet %d: transport error indicator", i/tsPacketSize)
		}
		pusi := pkt[1]&0x40 != 0
		pid := int(pkt[1]&0x1f)<<8 | int(pkt[2])
		afc := pkt[3] >> 4 & 3
		cc := int(pkt[3] & 0xf)
		p.pids[pid]++
		payload := pkt[4:]
		if afc&2 != 0 { // adaptation field
			n := int(pkt[4])
			if n > tsPacketSize-5 {
				t.Fatalf("packet %d: adaptation field length %d", i/tsPacketSize, n)
			}
			if n > 0 && pkt[5]&0x10 != 0 && p.firstPCR < 0 && pid == p.videoPID {
				base := int64(binary.BigEndian.Uint32(pkt[6:10]))<<1 | int64(pkt[10]>>7)
				p.firstPCR = base
			}
			payload = pkt[5+n:]
		}
		if afc&1 == 0 {
			continue
		}
		// Continuity counters count packets with a payload, per PID.
		if last, ok := counters[pid]; ok && cc != (last+1)%16 {
			t.Fatalf("PID %d: continuity counter %d after %d", pid, cc, last)
		}
		counters[pid] = cc

		switch pid {
		case pidPAT, p.pmtPID:
			if pusi {
				payload = payload[1+int(payload[0]):] // pointer field
				sections[pid] = nil
			}
			sections[pid] = append(sections[pid], payload...)
			if pid == pidPAT && p.pmtPID < 0 {
				s := sections[pid]
				// table_id 0, 5 header bytes after the length, then
				// program_number(2) + PMT PID(2) entries.
				if len(s) < 12 || s[0] != 0 {
					t.Fatalf("bad PAT: % x", s[:min(len(s), 12)])
				}
				p.pmtPID = int(s[10]&0x1f)<<8 | int(s[11])
			}
			if pid == p.pmtPID && p.videoPID < 0 {
				p.videoPID, p.audioPID = parsePMT(t, sections[pid])
			}
		case p.videoPID, p.audioPID:
			if pusi {
				flush(pid)
			} else if len(pesBufs[pid]) == 0 {
				t.Fatalf("PID %d: payload before the first PES start", pid)
			}
			pesBufs[pid] = append(pesBufs[pid], payload...)
		}
	}
	flush(p.videoPID)
	flush(p.audioPID)
	return p
}

// parsePMT returns the PIDs of the H.264 and the AAC stream.
func parsePMT(t *testing.T, s []byte) (video, audio int) {
	t.Helper()
	if len(s) < 12 || s[0] != 2 {
		t.Fatalf("bad PMT: % x", s[:min(len(s), 12)])
	}
	end := 3 + (int(s[1]&0xf)<<8 | int(s[2])) - 4 // without CRC
	if end > len(s) {
		t.Fatalf("PMT section of %d bytes in a payload of %d", end, len(s))
	}
	video, audio = -1, -1
	pos := 12 + (int(s[10]&0xf)<<8 | int(s[11])) // after the program info
	for pos+5 <= end {
		typ := s[pos]
		pid := int(s[pos+1]&0x1f)<<8 | int(s[pos+2])
		switch typ {
		case streamTypeH264:
			video = pid
		case streamTypeAAC:
			audio = pid
		default:
			t.Fatalf("unexpected stream type %#x", typ)
		}
		pos += 5 + (int(s[pos+3]&0xf)<<8 | int(s[pos+4]))
	}
	if video < 0 || audio < 0 {
		t.Fatalf("PMT without H.264 and AAC: video %d, audio %d", video, audio)
	}
	return video, audio
}

func parsePES(t *testing.T, b []byte) pes {
	t.Helper()
	if len(b) < 9 || b[0] != 0 || b[1] != 0 || b[2] != 1 {
		t.Fatalf("bad PES start: % x", b[:min(len(b), 9)])
	}
	e := pes{streamID: b[3], pts: -1, dts: -1}
	flags := b[7] >> 6
	hdr := 9 + int(b[8])
	if flags&2 != 0 {
		e.pts = ptsAt(b[9:14])
	}
	if flags&1 != 0 {
		e.dts = ptsAt(b[14:19])
	} else {
		e.dts = e.pts // the PES carries only the PTS when both are equal
	}
	// A video PES has length 0 (unbounded); an audio PES a real one.
	if n := int(binary.BigEndian.Uint16(b[4:6])); n != 0 && 6+n != len(b) {
		t.Fatalf("PES length %d, have %d bytes", 6+n, len(b))
	}
	e.payload = b[hdr:]
	return e
}

// nalTypes returns the types of the NAL units of an Annex B stream, in order.
func nalTypes(b []byte) []byte {
	var types []byte
	for i := 0; i+3 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			types = append(types, b[i+3]&0x1f)
			i += 3
		}
	}
	return types
}

// nalUnit returns the first NAL unit of the type, without start code.
func nalUnit(b []byte, typ byte) []byte {
	for i := 0; i+3 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 && b[i+3]&0x1f == typ {
			rest := b[i+3:]
			if end := bytes.Index(rest, []byte{0, 0, 1}); end >= 0 {
				rest = rest[:end]
			}
			return bytes.TrimRight(rest, "\x00")
		}
	}
	return nil
}

// adtsFrames walks the ADTS frames of b and returns their count; it fails on
// anything but AAC-LC, 48 kHz, stereo.
func adtsFrames(t *testing.T, b []byte) int {
	t.Helper()
	n := 0
	for len(b) > 0 {
		if len(b) < 7 || b[0] != 0xff || b[1]&0xf6 != 0xf0 {
			t.Fatalf("ADTS frame %d: bad header % x", n, b[:min(len(b), 7)])
		}
		profile := b[2] >> 6 // 1 = AAC-LC
		rate := b[2] >> 2 & 0xf
		channels := b[2]&1<<2 | b[3]>>6
		if profile != 1 || rate != 3 || channels != 2 {
			t.Fatalf("ADTS frame %d: profile %d, rate index %d, channels %d; want 1, 3 (48 kHz), 2", n, profile, rate, channels)
		}
		length := int(b[3]&3)<<11 | int(b[4])<<3 | int(b[5]>>5)
		if length < 7 || length > len(b) {
			t.Fatalf("ADTS frame %d: length %d of %d bytes left", n, length, len(b))
		}
		b = b[length:]
		n++
	}
	return n
}

func TestNames(t *testing.T) {
	want := []string{Starting, Connecting, Waiting, TunnelUnreachable, ChannelNotAvailable, ChannelBlocked}
	if got := Names(); !slices.Equal(got, want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	if len(want) != 6 {
		t.Fatalf("%d clips, want the six of docs/clips.md decision 3", len(want))
	}
	// No stray or missing file in segments/.
	entries, err := fs.ReadDir(files, "segments")
	if err != nil {
		t.Fatal(err)
	}
	var onDisk []string
	for _, e := range entries {
		onDisk = append(onDisk, strings.TrimSuffix(e.Name(), ".ts"))
	}
	slices.Sort(onDisk)
	sorted := slices.Sorted(slices.Values(want))
	if !slices.Equal(onDisk, sorted) {
		t.Fatalf("files in segments/ %v, want %v", onDisk, sorted)
	}
}

func TestSegmentUnknown(t *testing.T) {
	for _, name := range []string{"", "nope", "../clips_test", "starting.ts", "Starting"} {
		if b, ok := Segment(name); ok || b != nil {
			t.Errorf("Segment(%q) = %d bytes, %v; want nothing", name, len(b), ok)
		}
	}
}

func TestSegmentIsACopy(t *testing.T) {
	a, _ := Segment(Starting)
	a[0] = 0
	if b, _ := Segment(Starting); b[0] != 0x47 {
		t.Fatal("modifying a returned segment changed the embedded one")
	}
}

func TestSegments(t *testing.T) {
	var firstParams [2][]byte // SPS and PPS of the first segment
	var firstPIDs [3]int
	for i, name := range Names() {
		t.Run(name, func(t *testing.T) {
			data, ok := Segment(name)
			if !ok {
				t.Fatal("Segment not found")
			}
			if len(data) < 5000 || len(data) > 20000 {
				t.Errorf("%d bytes, docs/clips.md 6.4 says 12-16 KB per text", len(data))
			}
			p := parseTS(t, data)

			// PIDs: only PAT, PMT, SDT, video and audio; the same in every file.
			for pid := range p.pids {
				if pid != pidPAT && pid != pidSDT && pid != p.pmtPID && pid != p.videoPID && pid != p.audioPID {
					t.Errorf("unexpected PID %d", pid)
				}
			}
			pids := [3]int{p.pmtPID, p.videoPID, p.audioPID}
			if i == 0 {
				firstPIDs = pids
			} else if pids != firstPIDs {
				t.Errorf("PIDs (PMT, video, audio) %v, first segment has %v", pids, firstPIDs)
			}

			// Video: 24 frames, PTS = DTS (no B-frames), 0 to 0.96 s.
			if len(p.video) != videoFrames {
				t.Fatalf("%d video PES, want %d", len(p.video), videoFrames)
			}
			for n, e := range p.video {
				if e.streamID != 0xe0 {
					t.Errorf("video PES %d: stream id %#x", n, e.streamID)
				}
				if want := int64(n * frameTicks); e.pts != want || e.dts != want {
					t.Errorf("video PES %d: pts %d dts %d, want both %d", n, e.pts, e.dts, want)
				}
			}
			if last := p.video[videoFrames-1]; last.pts+frameTicks != segmentTicks {
				t.Errorf("video ends at %d ticks, want %d (0.96 s)", last.pts+frameTicks, segmentTicks)
			}

			// IDR (NAL type 5) in the first video PES together with SPS (7)
			// and PPS (8); no IDR and no parameter sets in the others.
			types := nalTypes(p.video[0].payload)
			for _, want := range []byte{7, 8, 5} {
				if !slices.Contains(types, want) {
					t.Errorf("first video PES has NAL types %v, missing %d", types, want)
				}
			}
			if i7, i8, i5 := slices.Index(types, 7), slices.Index(types, 8), slices.Index(types, 5); i7 >= i8 || i8 >= i5 {
				t.Errorf("first video PES NAL types %v: want SPS, PPS, IDR slice in this order", types)
			}
			for n, e := range p.video[1:] {
				for _, typ := range nalTypes(e.payload) {
					if typ == 5 || typ == 7 || typ == 8 {
						t.Errorf("video PES %d has NAL type %d", n+1, typ)
					}
				}
			}

			// SPS and PPS identical across the six files.
			params := [2][]byte{nalUnit(p.video[0].payload, 7), nalUnit(p.video[0].payload, 8)}
			if i == 0 {
				firstParams = params
			} else if !bytes.Equal(params[0], firstParams[0]) || !bytes.Equal(params[1], firstParams[1]) {
				t.Errorf("SPS/PPS differ from the first segment")
			}
			// High profile (100), level 4.0 (40).
			if sps := params[0]; len(sps) < 4 || sps[1] != 100 || sps[3] != 40 {
				t.Errorf("SPS % x: want profile 100 (High) and level 40", sps[:min(len(sps), 4)])
			}

			// Audio: exactly 45 ADTS frames (AAC-LC, 48 kHz, stereo), the
			// PES timestamps from 0 in steps of whole frames.
			if len(p.audio) == 0 {
				t.Fatal("no audio PES")
			}
			frames := 0
			for n, e := range p.audio {
				if e.streamID != 0xc0 {
					t.Errorf("audio PES %d: stream id %#x", n, e.streamID)
				}
				if want := int64(frames * aacTicks); e.pts != want {
					t.Errorf("audio PES %d: pts %d, want %d", n, e.pts, want)
				}
				frames += adtsFrames(t, e.payload)
			}
			if frames != aacFrames {
				t.Errorf("%d AAC frames, want %d", frames, aacFrames)
			}
			if frames*aacTicks != segmentTicks {
				t.Errorf("audio is %d ticks, video %d", frames*aacTicks, segmentTicks)
			}

			// The PCR is on the video PID and leads the first DTS by at most 1 s.
			// ffmpeg's muxer starts it 0.96 s before the first DTS, so it wraps
			// modulo 2^33 right at the start of the segment (documented in clips.go).
			if p.firstPCR < 0 {
				t.Error("no PCR on the video PID")
			} else if lead := (p.video[0].dts - p.firstPCR) & (1<<33 - 1); lead != segmentTicks {
				t.Errorf("first PCR leads the first DTS by %d ticks, want %d", lead, segmentTicks)
			}
		})
	}
}
