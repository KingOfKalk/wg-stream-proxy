// SPDX-License-Identifier: AGPL-3.0-or-later

// Package metrics holds the Prometheus metrics of wg-stream-proxy (spec
// 7.3) in a private registry: the wsp_ metrics plus the standard Go and
// process metrics. It knows nothing about HTTP handling or tunnels; the
// server reports requests and hands in the tunnels.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Kind is the kind label of the request metrics (spec 7.3).
type Kind string

// Kinds.
const (
	// KindPlaylist: the response is a playlist (rewritten or rejected).
	KindPlaylist Kind = "playlist"
	// KindSegment: a resource under /r/ that is not a playlist
	// (segments, keys, init segments, subtitles).
	KindSegment Kind = "segment"
	// KindRaw: a channel under /c/ that is not a playlist (raw stream).
	KindRaw Kind = "raw"
)

// Phase is the phase label of wsp_upstream_phase_seconds (spec 7.3).
type Phase string

// Phases.
const (
	// PhaseDNS: host lookups of the request.
	PhaseDNS Phase = "dns"
	// PhaseConnect: connection setup, Happy-Eyeballs fallback included.
	PhaseConnect Phase = "connect"
	// PhaseTLS: TLS handshakes.
	PhaseTLS Phase = "tls"
	// PhaseFirstByte: from the start of the upstream request to the first
	// byte of the response.
	PhaseFirstByte Phase = "first_byte"
)

// TimingBuckets are the bucket bounds, in seconds, of the timing
// histograms: 10ms up to the default connect timeout.
var TimingBuckets = []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 15}

// CodeError is the code label of an upstream request without a response.
const CodeError = "error"

// Tunnel is the state of one tunnel; *tunnel.Tunnel implements it.
type Tunnel interface {
	Name() string
	// Up reports whether the tunnel runs and its latest handshake is
	// younger than 180s.
	Up() bool
	// LastHandshake returns the time of the latest handshake of the
	// running tunnel, or the zero time.
	LastHandshake() time.Time
}

// Options configure Metrics.
type Options struct {
	// Tunnels returns the tunnels to report; it is called once per
	// scrape. nil reports none.
	Tunnels func() []Tunnel
	// EPG returns the EPG sources to report (spec 7.3); it is called once
	// per scrape. nil reports none.
	EPG func() []EPGSource
}

// Metrics is a private registry with the wsp_ metrics. Its methods are
// safe for concurrent use.
type Metrics struct {
	reg         *prometheus.Registry
	active      *prometheus.GaugeVec
	requests    *prometheus.CounterVec
	errors      *prometheus.CounterVec
	tunnelStart *prometheus.HistogramVec
	phases      *prometheus.HistogramVec
	errorClips  *prometheus.CounterVec
}

// New returns Metrics with a new registry.
func New(opts Options) *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		active: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "wsp_active_requests",
			Help: "Requests in flight under /c/, /r/ and /p/, open raw streams included.",
		}, []string{"channel", "kind"}),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wsp_upstream_requests_total",
			Help: "Upstream requests by status code, or error if no response came.",
		}, []string{"channel", "kind", "code"}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wsp_upstream_errors_total",
			Help: "Upstream request failures by reason.",
		}, []string{"channel", "reason"}),
		tunnelStart: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "wsp_tunnel_start_seconds",
			Help:    "Time of a successful tunnel start: device up including the first handshake.",
			Buckets: TimingBuckets,
		}, []string{"tunnel"}),
		phases: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "wsp_upstream_phase_seconds",
			Help:    "Time of the phases of upstream requests by route: DNS lookup, connect, TLS handshake, first response byte.",
			Buckets: TimingBuckets,
		}, []string{"route", "phase"}),
		errorClips: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wsp_error_clips_total",
			Help: "Error clips served instead of an HTTP error, per clip.",
		}, []string{"clip"}),
	}
	m.reg.MustRegister(
		m.active, m.requests, m.errors, m.tunnelStart, m.phases, m.errorClips,
		tunnelCollector{tunnels: opts.Tunnels},
		epgCollector{sources: opts.EPG},
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Handler serves the registry in the Prometheus exposition formats.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// Gatherer returns the registry.
func (m *Metrics) Gatherer() prometheus.Gatherer { return m.reg }

// ObserveTunnelStart records the duration of one successful start of
// tunnel.
func (m *Metrics) ObserveTunnelStart(tunnel string, d time.Duration) {
	m.tunnelStart.WithLabelValues(tunnel).Observe(d.Seconds())
}

// ObserveUpstreamPhase records the duration of one phase of an upstream
// request of the route (the direct route or a tunnel name, never a
// channel or URL).
func (m *Metrics) ObserveUpstreamPhase(route string, phase Phase, d time.Duration) {
	m.phases.WithLabelValues(route, string(phase)).Observe(d.Seconds())
}

// DeclareErrorClips creates the series of wsp_error_clips_total for the
// clips at 0, so a rate sees their first increase.
func (m *Metrics) DeclareErrorClips(clips ...string) {
	for _, c := range clips {
		m.errorClips.WithLabelValues(c)
	}
}

// ErrorClip counts an error clip served instead of an HTTP error; clip is
// the name of a built-in clip, never a channel or URL.
func (m *Metrics) ErrorClip(clip string) {
	m.errorClips.WithLabelValues(clip).Inc()
}

// Begin counts a request of channel as active with kind until End. The
// returned Request is used by one goroutine.
func (m *Metrics) Begin(channel string, kind Kind) *Request {
	m.active.WithLabelValues(channel, string(kind)).Inc()
	return &Request{m: m, channel: channel, kind: kind}
}

// Request is one request of a channel under /c/ or /r/.
type Request struct {
	m       *Metrics
	channel string
	kind    Kind
	ended   bool
}

// Kind returns the current kind.
func (r *Request) Kind() Kind { return r.kind }

// SetKind changes the kind once the response type is known; an active
// request moves to the new kind.
func (r *Request) SetKind(k Kind) {
	if k == r.kind {
		return
	}
	if !r.ended {
		r.m.active.WithLabelValues(r.channel, string(r.kind)).Dec()
		r.m.active.WithLabelValues(r.channel, string(k)).Inc()
	}
	r.kind = k
}

// Response counts an upstream request answered with status.
func (r *Request) Response(status int) {
	r.m.requests.WithLabelValues(r.channel, string(r.kind), strconv.Itoa(status)).Inc()
}

// FetchError counts an upstream request that failed without a response.
func (r *Request) FetchError(reason string) {
	r.m.requests.WithLabelValues(r.channel, string(r.kind), CodeError).Inc()
	r.Error(reason)
}

// Error counts an upstream failure, e.g. after the response headers.
func (r *Request) Error(reason string) {
	r.m.errors.WithLabelValues(r.channel, reason).Inc()
}

// End ends the request; later calls do nothing.
func (r *Request) End() {
	if r.ended {
		return
	}
	r.ended = true
	r.m.active.WithLabelValues(r.channel, string(r.kind)).Dec()
}

var (
	tunnelUpDesc = prometheus.NewDesc("wsp_tunnel_up",
		"1 while the tunnel runs and its latest handshake is younger than 180s.",
		[]string{"tunnel"}, nil)
	tunnelHandshakeDesc = prometheus.NewDesc("wsp_tunnel_last_handshake_timestamp_seconds",
		"Time of the latest completed WireGuard handshake of the running tunnel, 0 if none.",
		[]string{"tunnel"}, nil)
)

// tunnelCollector reads the tunnel state at scrape time, so no goroutine
// polls the devices.
type tunnelCollector struct {
	tunnels func() []Tunnel
}

func (c tunnelCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- tunnelUpDesc
	ch <- tunnelHandshakeDesc
}

func (c tunnelCollector) Collect(ch chan<- prometheus.Metric) {
	if c.tunnels == nil {
		return
	}
	for _, t := range c.tunnels() {
		up := 0.0
		if t.Up() {
			up = 1
		}
		hs := 0.0
		if h := t.LastHandshake(); !h.IsZero() {
			hs = float64(h.UnixNano()) / 1e9
		}
		ch <- prometheus.MustNewConstMetric(tunnelUpDesc, prometheus.GaugeValue, up, t.Name())
		ch <- prometheus.MustNewConstMetric(tunnelHandshakeDesc, prometheus.GaugeValue, hs, t.Name())
	}
}

// EPGSource is the state of one EPG source (spec 7.3).
type EPGSource struct {
	Name string
	// Fetches counts the fetches by result.
	Fetches map[string]uint64
	// Duration is the time of the last fetch.
	Duration time.Duration
	// Bytes is the XML size of the copy in use before filtering,
	// FilteredBytes after it; 0 without a copy.
	Bytes, FilteredBytes int64
	// Updated is when the copy in use was fetched; zero without one.
	Updated time.Time
}

// EPGResults are the result label values of wsp_epg_fetches_total; every
// source reports all of them, so a rate sees the first increase.
var EPGResults = []string{"ok", "connect", "timeout", "guard", "tls", "redirects", "size", "status", "invalid", "other"}

var (
	epgFetchesDesc = prometheus.NewDesc("wsp_epg_fetches_total",
		"Fetches of an EPG source by result: ok or why it failed.",
		[]string{"source", "result"}, nil)
	epgDurationDesc = prometheus.NewDesc("wsp_epg_fetch_duration_seconds",
		"Time of the last fetch of an EPG source, download and filter.",
		[]string{"source"}, nil)
	epgBytesDesc = prometheus.NewDesc("wsp_epg_source_bytes",
		"XML size of the EPG source copy in use, before filtering (after decompression); 0 without a copy.",
		[]string{"source"}, nil)
	epgFilteredDesc = prometheus.NewDesc("wsp_epg_filtered_bytes",
		"Size of what the proxy keeps of the EPG source copy in use; 0 without a copy.",
		[]string{"source"}, nil)
	epgUpdatedDesc = prometheus.NewDesc("wsp_epg_last_success_timestamp_seconds",
		"Time the EPG source copy in use was fetched, 0 without a copy.",
		[]string{"source"}, nil)
)

// epgCollector reads the EPG sources at scrape time.
type epgCollector struct {
	sources func() []EPGSource
}

func (c epgCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- epgFetchesDesc
	ch <- epgDurationDesc
	ch <- epgBytesDesc
	ch <- epgFilteredDesc
	ch <- epgUpdatedDesc
}

func (c epgCollector) Collect(ch chan<- prometheus.Metric) {
	if c.sources == nil {
		return
	}
	for _, s := range c.sources() {
		for _, r := range EPGResults {
			ch <- prometheus.MustNewConstMetric(epgFetchesDesc, prometheus.CounterValue, float64(s.Fetches[r]), s.Name, r)
		}
		updated := 0.0
		if !s.Updated.IsZero() {
			updated = float64(s.Updated.UnixNano()) / 1e9
		}
		ch <- prometheus.MustNewConstMetric(epgDurationDesc, prometheus.GaugeValue, s.Duration.Seconds(), s.Name)
		ch <- prometheus.MustNewConstMetric(epgBytesDesc, prometheus.GaugeValue, float64(s.Bytes), s.Name)
		ch <- prometheus.MustNewConstMetric(epgFilteredDesc, prometheus.GaugeValue, float64(s.FilteredBytes), s.Name)
		ch <- prometheus.MustNewConstMetric(epgUpdatedDesc, prometheus.GaugeValue, updated, s.Name)
	}
}
