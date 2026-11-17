// SPDX-License-Identifier: AGPL-3.0-or-later

package capacitytest

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// playlistType is the Content-Type of the fixture's playlists.
const playlistType = "application/vnd.apple.mpegurl"

// live is a live HLS upstream for any number of channels: for every
// channel name ch it serves /ch/master.m3u8 with one variant,
// /ch/index.m3u8 with a sliding window of the newest segments and
// /ch/seg{n}.ts. The caller publishes segments with advance, so the test
// drives the live edge; now stamps the publish times.
type live struct {
	segDur time.Duration
	window int
	size   int
	now    func() time.Time

	mu sync.Mutex
	// published holds the publish time of every segment, by sequence
	// number.
	published []time.Time
}

func newLive(segDur time.Duration, window, size int, now func() time.Time) *live {
	return &live{segDur: segDur, window: window, size: size, now: now}
}

// advance publishes the next segment of every channel.
func (l *live) advance() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.published = append(l.published, l.now())
}

// newest returns the sequence number of the newest segment, -1 before the
// first.
func (l *live) newest() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.published) - 1
}

// publishedAt returns the publish time of segment seq.
func (l *live) publishedAt(seq int) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if seq < 0 || seq >= len(l.published) {
		return time.Time{}, false
	}
	return l.published[seq], true
}

func (l *live) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ch, name, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if !ok || ch == "" {
		http.NotFound(w, r)
		return
	}
	var body []byte
	switch {
	case name == "master.m3u8":
		w.Header().Set("Content-Type", playlistType)
		body = []byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-STREAM-INF:BANDWIDTH=2000000\nindex.m3u8\n")
	case name == "index.m3u8":
		w.Header().Set("Content-Type", playlistType)
		body = []byte(l.playlist())
	case strings.HasPrefix(name, "seg") && strings.HasSuffix(name, ".ts"):
		seq, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "seg"), ".ts"))
		if err != nil || seq < 0 || seq > l.newest() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "video/mp2t")
		body = segment(ch, seq, l.size)
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}

// playlist returns the media playlist with the newest window segments.
func (l *live) playlist() string {
	newest := l.newest()
	first := max(0, newest-l.window+1)
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", int(math.Ceil(l.segDur.Seconds())))
	fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", first)
	for seq := first; seq <= newest; seq++ {
		fmt.Fprintf(&b, "#EXTINF:%.3f,\nseg%d.ts\n", l.segDur.Seconds(), seq)
	}
	return b.String()
}

// segment returns the content of segment seq of channel ch: size bytes
// that differ for every channel and sequence number.
func segment(ch string, seq, size int) []byte {
	line := fmt.Sprintf("%s segment %d\n", ch, seq)
	b := make([]byte, size)
	for i := 0; i < size; {
		i += copy(b[i:], line)
	}
	return b
}
