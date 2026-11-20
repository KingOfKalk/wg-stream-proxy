// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/clips"
	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/metrics"
	"github.com/KingOfKalk/wg-stream-proxy/internal/playlist"
	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel"
)

// Error clips (spec 3.4, design: docs/clips.md 6.2): a GET of /c/ of an
// HLS channel answers a failure with a short VOD playlist of an error
// clip instead of an HTTP error, so the player shows a text instead of a
// failure dialog. Its segments are served under /clip/ from the embedded
// clips and reach no upstream.

// errorClipSegments is how often an error playlist lists its segment:
// 5 × 0.96 s.
const errorClipSegments = 5

// kindClip is the kind of /clip/ requests in the debug line.
// It is never a metric label: /clip/ requests are not channel requests (spec 7.3).
const kindClip metrics.Kind = "clip"

// clipContentType is the Content-Type of clip segments.
const clipContentType = "video/mp2t"

// errorClips reports whether a request to /c/ of ch answers failures with
// an error clip: GET, an HLS channel by the rule of the channel list
// (spec 6) and [clips] error on.
func errorClips(r *http.Request, cfg *config.Config, ch config.Channel) bool {
	return r.Method == http.MethodGet && cfg.Clips().Error &&
		strings.HasSuffix(playlist.ChannelPath(ch), ".m3u8")
}

// errorClip returns the error clip for a failure (docs/clips.md 3.2): the
// tunnel did not start, the configuration blocks the address (guard or
// AllowedIPs), or else the channel is not available. err is nil for an
// upstream status or a playlist the proxy refused.
func errorClip(err error) string {
	switch {
	case errors.Is(err, tunnel.ErrUnavailable):
		return clips.TunnelUnreachable
	case errors.Is(err, route.ErrBlocked), errors.Is(err, tunnel.ErrNotAllowed):
		return clips.ChannelBlocked
	}
	return clips.ChannelNotAvailable
}

// clip returns the error clip that answers a failure of a request with
// fetch f, or "" if it gets an HTTP error.
func (f fetch) clip(err error) string {
	if !f.errorClips {
		return ""
	}
	return errorClip(err)
}

// withClip appends the clip field to the arguments of a log line if a
// clip answers the failure.
func withClip(args []any, clip string) []any {
	if clip == "" {
		return args
	}
	return append(args, "clip", clip)
}

// answerFailure answers a failed request: with the error playlist of clip,
// or with the HTTP status code if clip is "". Headers set so far are
// dropped.
func (s *Server) answerFailure(w http.ResponseWriter, r *http.Request, g *generation, code int, clip string) {
	clearHeader(w.Header())
	if clip == "" {
		http.Error(w, http.StatusText(code), code)
		return
	}
	body := errorPlaylist(baseURL(g.cfg.PublicBaseURL(), r), clip)
	h := w.Header()
	h.Set("Content-Type", playlistContentType)
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	s.metrics.ErrorClip(clip)
}

// errorPlaylist returns the VOD media playlist of an error clip: its
// segment errorClipSegments times, at positions 0, 1, ….
func errorPlaylist(base, clip string) []byte {
	b := []byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n")
	for n := range errorClipSegments {
		b = append(b, "#EXTINF:"+clips.SegmentDuration+",\n"...)
		b = append(b, base+"/clip/"+clip+"/"...)
		b = strconv.AppendInt(b, int64(n), 10)
		b = append(b, ".ts\n"...)
	}
	return append(b, "#EXT-X-ENDLIST\n"...)
}

// handleClip serves /clip/{name}/{n}.ts: the built-in clip name at
// position n (0 to clips.MaxPosition), timestamps and continuity counters
// rewritten by clips.Repeat. Anything else is 404. No upstream is asked.
func (s *Server) handleClip(w http.ResponseWriter, r *http.Request) {
	rec := &requestRecord{ResponseWriter: w, id: s.requestIDs.Add(1), start: time.Now()}
	defer func() {
		s.requestFinished(finishedRequest{
			ID: rec.id, Start: rec.start, Total: time.Since(rec.start), Method: r.Method, Kind: kindClip,
			Status: rec.status, Bytes: rec.bytes, ClientGone: r.Context().Err() != nil,
			Phases: route.Phases{URL: r.URL.Path},
		})
	}()
	name := r.PathValue("name")
	num, ok := strings.CutSuffix(r.PathValue("file"), ".ts")
	n, err := strconv.Atoi(num)
	seg, known := clips.Segment(name)
	if !known || !ok || err != nil || strconv.Itoa(n) != num || n < 0 || n > clips.MaxPosition {
		http.NotFound(rec, r)
		return
	}
	body, err := clips.Repeat(seg, n)
	if err != nil {
		// The embedded segments are checked by the clips tests.
		s.log.Error("clip segment", "clip", name, "error", err)
		http.Error(rec, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	h := rec.Header()
	h.Set("Content-Type", clipContentType)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	rec.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = rec.Write(body)
}
