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
}

// Metrics is a private registry with the wsp_ metrics. Its methods are
// safe for concurrent use.
type Metrics struct {
	reg      *prometheus.Registry
	active   *prometheus.GaugeVec
	requests *prometheus.CounterVec
	errors   *prometheus.CounterVec
}

// New returns Metrics with a new registry.
func New(opts Options) *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		active: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "wsp_active_requests",
			Help: "Requests in flight under /c/ and /r/, open raw streams included.",
		}, []string{"channel", "kind"}),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wsp_upstream_requests_total",
			Help: "Upstream requests by status code, or error if no response came.",
		}, []string{"channel", "kind", "code"}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wsp_upstream_errors_total",
			Help: "Upstream request failures by reason.",
		}, []string{"channel", "reason"}),
	}
	m.reg.MustRegister(
		m.active, m.requests, m.errors,
		tunnelCollector{tunnels: opts.Tunnels},
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
