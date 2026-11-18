// SPDX-License-Identifier: AGPL-3.0-or-later

package hls

import (
	"bytes"
	"fmt"
	"testing"
	"time"
)

// Shape of the large live media playlists some public broadcasters serve
// with long DVR windows (issue #76): about 5500 segments and 1.4 MB.
const (
	largeSegments = 5500
	largeFinal    = "https://cdn.example.com/live/channel/1080p/index.m3u8?session=0123456789abcdef"
	largeBase     = "http://192.0.2.10:2349"
	largeChannel  = "news-hd"
	largeSession  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	largeSig      = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

// largeMediaPlaylist returns a synthetic live media playlist of n segments:
// a #EXTINF line per segment, a #EXT-X-PROGRAM-DATE-TIME line on every
// second segment and relative segment URIs with long numeric names and a
// query string. Every eighth URI points to another directory, so the
// resolver has dot segments to remove.
func largeMediaPlaylist(n int) []byte {
	var b bytes.Buffer
	const seq = 1731931200
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:6\n#EXT-X-MEDIA-SEQUENCE:%d\n", seq)
	b.WriteString("#EXT-X-DISCONTINUITY-SEQUENCE:0\n")
	for i := range n {
		fmt.Fprintf(&b, "#EXTINF:6.000000,\n")
		if i%2 == 0 {
			fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n",
				start.Add(time.Duration(i)*6*time.Second).Format("2006-01-02T15:04:05.000-07:00"))
		}
		dir := ""
		if i%8 == 7 {
			dir = "../1080p-b/"
		}
		fmt.Fprintf(&b, "%smedia_b5000000_t1080p_lang-eng_%019d_%010d.ts?session=%s&seg=%d&sig=%s\n",
			dir, int64(seq+i)*1000003, seq+i, largeSession, seq+i, largeSig)
	}
	return b.Bytes()
}

func TestLargeMediaPlaylist(t *testing.T) {
	in := largeMediaPlaylist(largeSegments)
	if n := len(in); n < 1300_000 || n > 1500_000 {
		t.Errorf("fixture is %d bytes, want about 1.4 MB", n)
	}
	s := mustSigner(t, testKey)
	final := mustParse(t, largeFinal)
	n := 0
	out, err := Rewrite(in, final, func(dst, abs []byte) []byte {
		n++
		return append(dst, largeBase+s.Path(largeChannel, string(abs))...)
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != largeSegments {
		t.Errorf("%d URIs rewritten, want %d", n, largeSegments)
	}
	t.Logf("%d bytes in, %d bytes out", len(in), len(out))
	checkRewrite(t, in, bytes.ReplaceAll(out, []byte(largeBase), nil))
}

func BenchmarkSignerPath(b *testing.B) {
	s := mustSigner(b, testKey)
	const upstream = "https://cdn.example.com/live/channel/1080p/media_b5000000_t1080p_1731937186127600_1731931200.ts?session=0123456789abcdef0123456789abcdef&seg=1731931200"
	b.Run("Path", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = s.Path(largeChannel, upstream)
		}
	})
	b.Run("PathBuilder", func(b *testing.B) {
		pb := s.PathBuilder(largeChannel)
		var dst []byte
		b.ReportAllocs()
		for b.Loop() {
			dst = pb.AppendPath(dst[:0], []byte(upstream))
		}
	})
}

// BenchmarkRewrite rewrites the large playlist the way the server does:
// one PathBuilder per playlist response.
func BenchmarkRewrite(b *testing.B) {
	in := largeMediaPlaylist(largeSegments)
	s := mustSigner(b, testKey)
	final := mustParse(b, largeFinal)
	b.SetBytes(int64(len(in)))
	b.ReportAllocs()
	for b.Loop() {
		pb := s.PathBuilder(largeChannel)
		if _, err := Rewrite(in, final, func(dst, abs []byte) []byte {
			return pb.AppendPath(append(dst, largeBase...), abs)
		}); err != nil {
			b.Fatal(err)
		}
	}
}
