// SPDX-License-Identifier: AGPL-3.0-or-later

// Package playertest holds the player tests of docs/testing.md (L4) and
// their fixture server. Fixtures serves the media fixtures of
// testdata/media like broadcasters do: HLS as VOD and as a live window
// that advances with time, DASH as VOD, raw MPEG-TS, and endless
// Icecast-like and Shoutcast v1 (ICY) radio streams.
//
// The player tests run ffmpeg and only run with WSP_PLAYER_TESTS=1; the
// fixture server's own tests always run.
package playertest

import (
	"bytes"
	"io"
	"io/fs"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Defaults of Options.
const (
	DefaultWindow  = 3
	DefaultMetaInt = 8192
)

// icyTitle is the metadata of the radio streams.
const icyTitle = "StreamTitle='Test tone';"

// Options configure Fixtures.
type Options struct {
	// Now is the clock of the live windows; nil means time.Now.
	Now func() time.Time
	// Window is the number of segments in a live media playlist; 0 means
	// DefaultWindow.
	Window int
	// MetaInt is the icy-metaint of Icecast-like streams; 0 means
	// DefaultMetaInt.
	MetaInt int
}

// Fixtures is an http.Handler serving the fixtures in its file system,
// one directory per fixture (testdata/media):
//
//   - /vod/{fixture}/{file}: the file as is; media playlists are VOD
//     playlists with ENDLIST.
//   - /live/{fixture}/{file}: media playlists (with #EXTINF) become a
//     live window of Window segments. The first request of any file of the
//     fixture starts its clock; one more segment is published whenever
//     the duration of the next segment has passed, and ENDLIST is added
//     once the last segment is out. Other files are served as is.
//   - /redirect/{file}: a 302 redirect to /vod/{file}, like a channel
//     URL that redirects to its CDN.
//   - /raw/{fixture}: the segments of {fixture}/index.m3u8 in one
//     response, like a raw MPEG-TS channel.
//   - /icecast/{file}: the file in an endless loop with icy-* headers,
//     and with icy-metaint metadata if the request has Icy-MetaData: 1.
//   - /shoutcast/{file}: the same loop after an "ICY 200 OK" status line,
//     like a Shoutcast v1 server.
//
// Files are served with Range support.
type Fixtures struct {
	fsys    fs.FS
	now     func() time.Time
	window  int
	metaInt int

	mu       sync.Mutex
	origins  map[string]time.Time
	requests []string
	conns    map[net.Conn]struct{}
	closed   bool
	done     chan struct{}
}

// New returns Fixtures serving fsys.
func New(fsys fs.FS, opts Options) *Fixtures {
	f := &Fixtures{
		fsys:    fsys,
		now:     opts.Now,
		window:  opts.Window,
		metaInt: opts.MetaInt,
		origins: map[string]time.Time{},
		conns:   map[net.Conn]struct{}{},
		done:    make(chan struct{}),
	}
	if f.now == nil {
		f.now = time.Now
	}
	if f.window <= 0 {
		f.window = DefaultWindow
	}
	if f.metaInt <= 0 {
		f.metaInt = DefaultMetaInt
	}
	return f
}

// Close ends all endless streams, including hijacked Shoutcast
// connections, which http.Server.Close does not close. Later stream
// requests end at once. It is safe to call more than once.
func (f *Fixtures) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.closed = true
	close(f.done)
	for c := range f.conns {
		_ = c.Close()
	}
}

// Requests returns the paths of all requests so far, in order.
func (f *Fixtures) Requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// ServeHTTP implements http.Handler.
func (f *Fixtures) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.URL.Path)
	f.mu.Unlock()

	mode, name, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	switch mode {
	case "vod":
		f.serveFile(w, r, name)
	case "live":
		f.serveLive(w, r, name)
	case "redirect":
		http.Redirect(w, r, "/vod/"+name, http.StatusFound)
	case "raw":
		f.serveRaw(w, r, name)
	case "icecast":
		f.serveIcecast(w, r, name)
	case "shoutcast":
		f.serveShoutcast(w, r, name)
	default:
		http.NotFound(w, r)
	}
}

// contentTypes maps file extensions to the Content-Type of the fixture
// server.
var contentTypes = map[string]string{
	".m3u8": "application/vnd.apple.mpegurl",
	".mpd":  "application/dash+xml",
	".ts":   "video/mp2t",
	".m4s":  "video/iso.segment",
	".mp4":  "video/mp4",
	".mp3":  "audio/mpeg",
}

func contentType(name string) string {
	if ct, ok := contentTypes[path.Ext(name)]; ok {
		return ct
	}
	return "application/octet-stream"
}

// readFile returns a regular file of the fixtures.
func (f *Fixtures) readFile(name string) ([]byte, bool) {
	st, err := fs.Stat(f.fsys, name)
	if err != nil || !st.Mode().IsRegular() {
		return nil, false
	}
	b, err := fs.ReadFile(f.fsys, name)
	return b, err == nil
}

// serveFile serves a file as is, with Range support.
func (f *Fixtures) serveFile(w http.ResponseWriter, r *http.Request, name string) {
	b, ok := f.readFile(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType(name))
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
}

// serveLive serves media playlists as a live window, everything else as
// is.
func (f *Fixtures) serveLive(w http.ResponseWriter, r *http.Request, name string) {
	fixture, _, _ := strings.Cut(name, "/")
	now := f.now()
	f.mu.Lock()
	origin, ok := f.origins[fixture]
	if !ok {
		origin = now
		f.origins[fixture] = now
	}
	f.mu.Unlock()

	if path.Ext(name) != ".m3u8" {
		f.serveFile(w, r, name)
		return
	}
	b, ok := f.readFile(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	p := parseMedia(b)
	if len(p.segments) == 0 {
		// A master playlist.
		f.serveFile(w, r, name)
		return
	}
	out := p.live(f.window, now.Sub(origin))
	w.Header().Set("Content-Type", contentType(name))
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	_, _ = w.Write(out)
}

// serveRaw serves the segments of a fixture's index.m3u8 as one stream.
func (f *Fixtures) serveRaw(w http.ResponseWriter, r *http.Request, fixture string) {
	b, ok := f.readFile(path.Join(fixture, "index.m3u8"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	var segs [][]byte
	for _, s := range parseMedia(b).segments {
		seg, ok := f.readFile(path.Join(fixture, s.uri))
		if !ok {
			http.NotFound(w, r)
			return
		}
		segs = append(segs, seg)
	}
	w.Header().Set("Content-Type", "video/mp2t")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	for _, seg := range segs {
		if _, err := w.Write(seg); err != nil {
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}

// serveIcecast serves a file in an endless loop like an Icecast server.
func (f *Fixtures) serveIcecast(w http.ResponseWriter, r *http.Request, name string) {
	b, ok := f.readFile(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", contentType(name))
	h.Set("Cache-Control", "no-cache")
	h.Set("Icy-Name", "Test tone")
	h.Set("Icy-Genre", "Test")
	h.Set("Icy-Br", "32")
	h.Set("Icy-Pub", "0")
	var out io.Writer = w
	if r.Header.Get("Icy-MetaData") == "1" {
		h.Set("Icy-Metaint", strconv.Itoa(f.metaInt))
		out = &icyWriter{w: w, every: f.metaInt, meta: icyBlock(icyTitle)}
	}
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	for f.open(r) {
		if _, err := out.Write(b); err != nil {
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}

// serveShoutcast serves a file in an endless loop like a Shoutcast v1
// server: the status line is "ICY 200 OK", not HTTP.
func (f *Fixtures) serveShoutcast(w http.ResponseWriter, r *http.Request, name string) {
	b, ok := f.readFile(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijacking", http.StatusInternalServerError)
		return
	}
	c, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer func() { _ = c.Close() }()
	if !f.track(c) {
		return
	}
	defer f.untrack(c)
	_, _ = brw.WriteString("ICY 200 OK\r\nicy-name:Test tone\r\nicy-genre:Test\r\nicy-br:32\r\nicy-pub:0\r\n" +
		"content-type:" + contentType(name) + "\r\n\r\n")
	if brw.Flush() != nil {
		return
	}
	for f.open(r) {
		if _, err := c.Write(b); err != nil {
			return
		}
	}
}

// open reports whether an endless stream may go on.
func (f *Fixtures) open(r *http.Request) bool {
	select {
	case <-f.done:
		return false
	case <-r.Context().Done():
		return false
	default:
		return true
	}
}

// track registers a hijacked connection for Close; false if Close
// already ran.
func (f *Fixtures) track(c net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return false
	}
	f.conns[c] = struct{}{}
	return true
}

func (f *Fixtures) untrack(c net.Conn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.conns, c)
}

// icyBlock returns an ICY metadata block: a length byte in units of 16
// bytes, then the text padded with NUL bytes.
func icyBlock(text string) []byte {
	n := (len(text) + 15) / 16
	b := make([]byte, 1+n*16)
	b[0] = byte(n)
	copy(b[1:], text)
	return b
}

// icyWriter inserts a metadata block after every `every` bytes of audio.
type icyWriter struct {
	w     io.Writer
	every int
	meta  []byte
	n     int // audio bytes since the last metadata block
}

func (iw *icyWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		k := min(len(p), iw.every-iw.n)
		if _, err := iw.w.Write(p[:k]); err != nil {
			return written, err
		}
		written += k
		iw.n += k
		p = p[k:]
		if iw.n == iw.every {
			if _, err := iw.w.Write(iw.meta); err != nil {
				return written, err
			}
			iw.n = 0
		}
	}
	return written, nil
}

// mediaPlaylist is a parsed VOD media playlist.
type mediaPlaylist struct {
	// header holds the lines before the first segment, without
	// EXT-X-MEDIA-SEQUENCE, EXT-X-PLAYLIST-TYPE and EXT-X-ENDLIST.
	header []string
	// seqAt is the index in header where EXT-X-MEDIA-SEQUENCE goes.
	seqAt    int
	segments []segment
}

// segment is one media segment: its tag lines and the URI line.
type segment struct {
	lines []string
	uri   string
	dur   time.Duration
}

// parseMedia parses a media playlist as ffmpeg's hls muxer writes it.
// Tags before the first #EXTINF belong to the header; tags after a URI
// belong to the next segment.
func parseMedia(b []byte) mediaPlaylist {
	var p mediaPlaylist
	seqSeen := false
	inHeader := true
	var cur segment
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case line == "#EXT-X-ENDLIST", strings.HasPrefix(line, "#EXT-X-PLAYLIST-TYPE:"):
			continue
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			if !seqSeen {
				p.seqAt = len(p.header)
				seqSeen = true
			}
			continue
		case strings.HasPrefix(line, "#EXTINF:"):
			inHeader = false
			v, _, _ := strings.Cut(strings.TrimPrefix(line, "#EXTINF:"), ",")
			sec, _ := strconv.ParseFloat(v, 64)
			cur.dur = time.Duration(sec * float64(time.Second))
		case inHeader:
			p.header = append(p.header, line)
			continue
		}
		cur.lines = append(cur.lines, line)
		if line != "" && !strings.HasPrefix(line, "#") {
			cur.uri = line
			p.segments = append(p.segments, cur)
			cur = segment{}
		}
	}
	if !seqSeen {
		p.seqAt = len(p.header)
	}
	return p
}

// live returns the playlist as a live window of up to window segments
// after elapsed: the first window segments are there from the start, the
// next one is published when its duration has passed, and so on. Once
// the last segment is out, ENDLIST ends the playlist.
func (p mediaPlaylist) live(window int, elapsed time.Duration) []byte {
	n := len(p.segments)
	avail := min(window, n)
	var acc time.Duration
	for avail < n {
		acc += p.segments[avail].dur
		if acc > elapsed {
			break
		}
		avail++
	}
	start := max(0, avail-window)

	var b bytes.Buffer
	for i, line := range p.header {
		if i == p.seqAt {
			b.WriteString("#EXT-X-MEDIA-SEQUENCE:" + strconv.Itoa(start) + "\n")
		}
		b.WriteString(line + "\n")
	}
	if p.seqAt == len(p.header) {
		b.WriteString("#EXT-X-MEDIA-SEQUENCE:" + strconv.Itoa(start) + "\n")
	}
	for _, s := range p.segments[start:avail] {
		for _, line := range s.lines {
			b.WriteString(line + "\n")
		}
	}
	if avail == n {
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	return b.Bytes()
}
