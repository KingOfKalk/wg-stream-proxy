// SPDX-License-Identifier: AGPL-3.0-or-later

// Package probe answers "does this URL work through this route?" for the
// probe command (spec 7.2): it fetches a URL through a route with the
// rules of serving (private-range guard, [upstream] timeouts, headers and
// size limits), reports what the proxy would do with the response and
// where the time went. For an HLS playlist it follows the first variant
// and the first segment, like a player would.
package probe

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/dash"
	"github.com/KingOfKalk/wg-stream-proxy/internal/hls"
	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel"
)

// Modes of a probe.
const (
	// ModeProxy: the running proxy made the requests, through its own
	// tunnel (POST /debug/probe, spec 7.1).
	ModeProxy = "proxy"
	// ModeStandalone: the command made the requests itself and started
	// the tunnel of the route itself.
	ModeStandalone = "standalone"
)

// Roles of a step.
const (
	// RoleURL is the URL that was asked for.
	RoleURL = "url"
	// RoleVariant is the media playlist of the first variant of a master
	// playlist.
	RoleVariant = "variant"
	// RoleSegment is the first segment of a media playlist.
	RoleSegment = "segment"
)

// Formats of a response, as reported in Step.Format.
const (
	FormatHLSMaster = "hls-master"
	FormatHLSMedia  = "hls-media"
	// FormatDASH is a DASH manifest (MPD) the proxy rewrites (spec 3.6).
	FormatDASH = "dash"
	FormatRaw  = "raw"
	// FormatICY is a raw stream with icy-* headers (Shoutcast radio).
	FormatICY = "icy"
	// FormatRefused is a playlist in a format the proxy does not rewrite
	// (spec 3.3); Step.Refused names it.
	FormatRefused = "refused"
)

// Classes of a failure besides the route.Reason values (spec 7.3).
const (
	// ClassStatus: the upstream answered with a status other than 2xx.
	ClassStatus = "status"
)

// maxSegmentPeek is how much of a segment is read to see that data
// flows.
const maxSegmentPeek = hls.SniffLen

// Options configure Run.
type Options struct {
	// Route is the route the URL is fetched through. Required.
	Route *route.Route
	// Header holds the upstream request headers, like the merged headers
	// of a channel (config.Channel.Header).
	Header http.Header
	// MaxPlaylistSize is the largest playlist body in bytes
	// ([upstream] max_playlist_size); 0 means hls.MaxPlaylistSize.
	MaxPlaylistSize int64
}

// Report is the result of a probe.
type Report struct {
	// Mode tells where the probe ran: ModeProxy or ModeStandalone.
	Mode string `json:"mode,omitempty"`
	// Route is the name of the route.
	Route string `json:"route"`
	// OK: the proxy would serve the URL.
	OK bool `json:"ok"`
	// Steps are the requests made, in order: the URL, then for HLS the
	// first variant and the first segment.
	Steps []Step `json:"steps"`
}

// Step is one upstream request of a probe.
type Step struct {
	// Role is RoleURL, RoleVariant or RoleSegment.
	Role string `json:"role"`
	// URL is the final URL after redirects in its logged form (spec 8:
	// scheme, host and file name only); the URL asked for if no
	// response came.
	URL    string `json:"url"`
	Status int    `json:"status,omitempty"`
	// ContentType is the upstream Content-Type, reduced to printable
	// characters.
	ContentType string `json:"content_type,omitempty"`
	// Size is the body size in bytes: the playlist as read, else the
	// Content-Length; nil if unknown. Streaming: the body has no known
	// end (raw streams).
	Size      *int64 `json:"size,omitempty"`
	Streaming bool   `json:"streaming,omitempty"`
	// Format is one of the Format constants; empty for a segment or if no
	// response was classified.
	Format string `json:"format,omitempty"`
	// Refused is the playlist format the proxy refuses (hls.Format*).
	Refused string `json:"refused,omitempty"`
	// Variants and Segments count the entries of an HLS playlist.
	Variants int `json:"variants,omitempty"`
	Segments int `json:"segments,omitempty"`
	// Periods and AdaptationSets count the elements of a DASH manifest.
	Periods        int `json:"periods,omitempty"`
	AdaptationSets int `json:"adaptation_sets,omitempty"`
	Redirects      int `json:"redirects,omitempty"`
	// OK: the proxy would serve this response.
	OK bool `json:"ok"`
	// Class is the failure class: a route.Reason, or ClassStatus.
	Class string `json:"class,omitempty"`
	// Error describes the failure, without query strings (spec 8).
	Error  string `json:"error,omitempty"`
	Timing Timing `json:"timing"`

	final *url.URL
	next  string // URI of the first variant or segment
}

// Timing is where the time of a step went, with the phases of
// wsp_upstream_phase_seconds (spec 7.3). A phase that did not happen is
// nil: no lookup for an IP literal or a pooled connection, no TLS for
// plain HTTP, no tunnel start for a running tunnel.
type Timing struct {
	TunnelStartMS *float64 `json:"tunnel_start_ms,omitempty"`
	DNSMS         *float64 `json:"dns_ms,omitempty"`
	ConnectMS     *float64 `json:"connect_ms,omitempty"`
	TLSMS         *float64 `json:"tls_ms,omitempty"`
	TTFBMS        *float64 `json:"ttfb_ms,omitempty"`
	TotalMS       float64  `json:"total_ms"`
	ConnReused    bool     `json:"conn_reused,omitempty"`
}

// Run probes rawURL through opts.Route.
func Run(ctx context.Context, rawURL string, opts Options) *Report {
	p := &prober{opts: opts}
	if p.opts.MaxPlaylistSize <= 0 {
		p.opts.MaxPlaylistSize = hls.MaxPlaylistSize
	}
	rep := &Report{Route: opts.Route.Name()}
	// Like a channel of type auto: an ICY status line is accepted.
	first := p.step(ctx, RoleURL, rawURL, true)
	rep.Steps = append(rep.Steps, *first)
	cur := first
	// Only HLS goes on: a master playlist to its first variant, a media
	// playlist to its first segment.
	for cur.OK && cur.next != "" {
		role := RoleSegment
		if cur.Format == FormatHLSMaster {
			role = RoleVariant
		}
		target, err := cur.final.Parse(cur.next)
		var s *Step
		switch {
		case err != nil || target.Scheme != "http" && target.Scheme != "https" || target.Host == "":
			// The URI is not logged: it may carry tokens.
			s = &Step{Role: role, URL: route.LogURL(cur.final.String()), Class: string(route.ReasonOther), Error: "first " + role + " is not an http(s) URL"}
		default:
			s = p.step(ctx, role, target.String(), false)
		}
		if role == RoleVariant && s.OK && s.Format != FormatHLSMedia {
			s.OK, s.Class, s.Error = false, string(route.ReasonOther), "variant playlist is not a media playlist (format "+s.Format+")"
		}
		rep.Steps = append(rep.Steps, *s)
		cur = s
		if role == RoleSegment {
			break
		}
	}
	rep.OK = true
	for _, s := range rep.Steps {
		rep.OK = rep.OK && s.OK
	}
	return rep
}

type prober struct {
	opts Options
}

// step fetches rawURL the way the proxy does and describes the result.
// icy selects the ICY transport, as for /c/ of a raw or auto channel.
func (p *prober) step(ctx context.Context, role, rawURL string, icy bool) *Step {
	s := &Step{Role: role, URL: route.LogURL(rawURL)}
	var trace route.Trace
	ctx = tunnel.WithStartTrace(ctx, trace.NetworkWait)
	start := time.Now()
	defer func() { s.Timing = timing(trace.Phases(), time.Since(start)) }()

	resp, err := p.opts.Route.Fetch(ctx, route.Request{URL: rawURL, Header: p.opts.Header, ICY: icy, Trace: &trace})
	if err != nil {
		s.fail(string(route.Classify(err)), err.Error())
		return s
	}
	defer func() { _ = resp.Body.Close() }()
	s.final = resp.Request.URL
	s.URL = route.LogURL(s.final.String())
	s.Status = resp.StatusCode
	s.ContentType = clean(resp.Header.Get("Content-Type"))
	s.Redirects = trace.Phases().Redirects
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.fail(ClassStatus, fmt.Sprintf("upstream status %d", resp.StatusCode))
		return s
	}

	body := bufio.NewReaderSize(resp.Body, hls.SniffLen)
	if role == RoleSegment {
		n, err := peek(body, maxSegmentPeek)
		if err != nil {
			s.fail(string(route.Classify(err)), "reading the segment: "+err.Error())
			return s
		}
		s.size(resp, n)
		s.OK = true
		return s
	}
	typ, refused, body, err := detect(resp.Header.Get("Content-Type"), body)
	switch {
	case err != nil:
		s.fail(string(route.Classify(err)), "reading the response: "+err.Error())
	case refused != "":
		s.Format, s.Refused = FormatRefused, refused
		s.fail(string(route.ReasonUnsupported), "unsupported playlist format "+refused+" (refused, spec 3.3)")
	case typ == hls.HLS:
		p.playlist(s, resp, body)
	case typ == hls.DASH:
		p.manifest(s, resp, body)
	default:
		s.Format = FormatRaw
		if hasICYHeader(resp.Header) {
			s.Format = FormatICY
		}
		s.size(resp, 0)
		s.OK = true
	}
	return s
}

// detect classifies a response like the server does (proxy.go respond):
// by Content-Type first, then by the start of the body. This is the one
// place that names the formats; refused is the format of a playlist the
// proxy does not rewrite.
func detect(contentType string, body *bufio.Reader) (typ hls.Type, refused string, rest *bufio.Reader, err error) {
	typ = hls.Detect(contentType, nil, hls.Auto)
	if typ != hls.Raw {
		return typ, "", body, nil
	}
	prefix, rest, err := hls.Sniff(body)
	if err != nil {
		return typ, "", rest, err
	}
	typ = hls.Detect(contentType, prefix, hls.Auto)
	if typ == hls.Raw {
		refused = hls.Unsupported(contentType, prefix, hls.Auto)
	}
	return typ, refused, rest, nil
}

// manifest reads and checks a DASH manifest like the proxy does before
// serving it: whole within the size limit and accepted by the rewriter
// (spec 3.6); a manifest with DRM fails with class drm.
func (p *prober) manifest(s *Step, resp *http.Response, body io.Reader) {
	s.Format = FormatDASH
	data, err := route.ReadPlaylist(body, p.opts.MaxPlaylistSize)
	if err != nil {
		s.fail(string(route.Classify(err)), err.Error())
		return
	}
	n := int64(len(data))
	s.Size = &n
	if resp.StatusCode == http.StatusPartialContent {
		s.fail(string(route.ReasonOther), "upstream answered a manifest with 206 without Range")
		return
	}
	// The rewriter errors never contain URLs from the manifest.
	_, err = dash.Rewrite(data, dash.Options{
		Final:     resp.Request.URL,
		PrefixURL: func(string, bool) (string, error) { return "http://probe.invalid/p/", nil },
		TimeURL:   "http://probe.invalid/time",
	})
	switch {
	case errors.Is(err, dash.ErrDRM):
		s.fail(string(route.ReasonDRM), err.Error())
		return
	case errors.Is(err, dash.ErrTooLarge):
		s.fail(string(route.ReasonSize), err.Error())
		return
	case err != nil:
		s.fail(string(route.ReasonOther), err.Error())
		return
	}
	s.Periods, s.AdaptationSets = dash.Count(data)
	s.OK = true
}

// playlist reads and checks an HLS playlist like the proxy does before
// serving it: whole within the size limit and accepted by the rewriter.
func (p *prober) playlist(s *Step, resp *http.Response, body io.Reader) {
	data, err := route.ReadPlaylist(body, p.opts.MaxPlaylistSize)
	if err != nil {
		s.fail(string(route.Classify(err)), err.Error())
		return
	}
	n := int64(len(data))
	s.Size = &n
	if resp.StatusCode == http.StatusPartialContent {
		s.fail(string(route.ReasonOther), "upstream answered a playlist with 206 without Range")
		return
	}
	// The rewriter errors never contain URIs from the playlist.
	if _, err := hls.Rewrite(data, resp.Request.URL, func(dst, _ []byte) []byte { return append(dst, 'x') }); err != nil {
		s.fail(string(route.ReasonOther), "playlist rejected: "+err.Error())
		return
	}
	info := parsePlaylist(data)
	s.Variants, s.Segments, s.next = info.variants, info.segments, info.first
	s.Format = FormatHLSMedia
	if info.master {
		s.Format = FormatHLSMaster
	}
	s.OK = true
}

// size sets the size of a response of which only the start was read:
// the Content-Length, else the body is streaming. peeked, if set, is the
// number of bytes read when there is no Content-Length and the body ended
// within them.
func (s *Step) size(resp *http.Response, peeked int) {
	switch {
	case resp.ContentLength >= 0:
		n := resp.ContentLength
		s.Size = &n
	case peeked > 0 && peeked < maxSegmentPeek:
		n := int64(peeked)
		s.Size = &n
	default:
		s.Streaming = true
	}
}

func (s *Step) fail(class, msg string) {
	s.OK = false
	s.Class = class
	s.Error = clean(msg)
}

// peek reads up to n bytes of the start of r and returns how many; a
// short body is no error.
func peek(r *bufio.Reader, n int) (int, error) {
	b, err := r.Peek(n)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	return len(b), nil
}

// hasICYHeader reports whether the response has an icy-* header.
func hasICYHeader(h http.Header) bool {
	for k := range h {
		if len(k) > 4 && strings.EqualFold(k[:4], "icy-") {
			return true
		}
	}
	return false
}

// timing converts the phases of a step.
func timing(p route.Phases, total time.Duration) Timing {
	t := Timing{TotalMS: ms(total), ConnReused: p.Reused}
	opt := func(ok bool, d time.Duration) *float64 {
		if !ok {
			return nil
		}
		v := ms(d)
		return &v
	}
	t.TunnelStartMS = opt(p.NetworkStarted, p.NetworkStart)
	t.DNSMS = opt(p.Lookups > 0, p.DNS)
	t.ConnectMS = opt(p.ConnectAttempts > 0, p.Connect)
	t.TLSMS = opt(p.TLSHandshakes > 0, p.TLS)
	t.TTFBMS = opt(p.GotFirstByte, p.FirstByte)
	return t
}

// ms returns d in milliseconds, to the microsecond.
func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// playlistInfo is what the probe needs of an HLS playlist.
type playlistInfo struct {
	master   bool
	variants int
	segments int
	// first is the first variant (master) or segment URI.
	first string
}

// parsePlaylist counts the variants and segments of a playlist that
// hls.Rewrite accepted and finds the first URI line after the first
// #EXT-X-STREAM-INF (master) or #EXTINF (media) tag.
func parsePlaylist(data []byte) playlistInfo {
	var info playlistInfo
	pending := false // a tag that announces a URI line was seen
	for _, line := range bytes.FieldsFunc(data, func(r rune) bool { return r == '\n' || r == '\r' }) {
		l := strings.TrimSpace(string(line))
		l = strings.TrimPrefix(l, "\ufeff")
		switch {
		case l == "":
		case strings.HasPrefix(l, "#EXT-X-STREAM-INF:"):
			info.master = true
			info.variants++
			pending = true
		case strings.HasPrefix(l, "#EXTINF:"):
			info.segments++
			pending = true
		case strings.HasPrefix(l, "#"):
		case pending && info.first == "":
			info.first = l
		}
	}
	if info.master {
		// A master playlist has variants, not segments.
		info.segments = 0
	}
	return info
}

// clean reduces foreign text to printable characters and a bounded
// length, so it cannot inject control sequences into a terminal.
func clean(s string) string {
	const maxLen = 200
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0 {
			return '?'
		}
		return r
	}, s)
	if len(s) > maxLen {
		s = s[:maxLen] + "…"
	}
	return s
}
