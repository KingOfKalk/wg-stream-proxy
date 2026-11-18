// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"net/http"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/metrics"
	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
)

// requestRecord follows one /c/ or /r/ request at every log level: it
// wraps the response writer to see the status and bytes the client got,
// carries the route.Trace of the upstream requests, and collects what the
// handlers learn on the way (upstream status, error reason). When the
// request ends, finish turns it into the finishedRequest that
// Server.requestFinished hands on.
type requestRecord struct {
	http.ResponseWriter
	id    uint64
	start time.Time
	trace route.Trace
	// status and bytes: what the client got; status 0 if nothing.
	status int
	bytes  int64
	// upstreamStatus is the status of the last upstream response, reason
	// the upstream error counted for the request.
	upstreamStatus int
	reason         route.Reason
}

// WriteHeader implements http.ResponseWriter.
func (l *requestRecord) WriteHeader(code int) {
	if l.status == 0 {
		l.status = code
	}
	l.ResponseWriter.WriteHeader(code)
}

// Write implements http.ResponseWriter.
func (l *requestRecord) Write(p []byte) (int, error) {
	if l.status == 0 {
		l.status = http.StatusOK
	}
	n, err := l.ResponseWriter.Write(p)
	l.bytes += int64(n)
	return n, err
}

// Unwrap returns the wrapped writer, for http.ResponseController.
func (l *requestRecord) Unwrap() http.ResponseWriter { return l.ResponseWriter }

// recordOf returns the requestRecord of w, or nil for a writer that is
// not one (a request that is not recorded).
func recordOf(w http.ResponseWriter) *requestRecord {
	l, _ := w.(*requestRecord)
	return l
}

// upstream records the status of an upstream response.
func (l *requestRecord) upstream(status int) {
	if l != nil {
		l.upstreamStatus = status
	}
}

// failed records the reason of an upstream error.
func (l *requestRecord) failed(reason route.Reason) {
	if l != nil {
		l.reason = reason
	}
}

// finishedRequest is everything the server knows about a /c/ or /r/
// request when it ends. It holds no query string, no secret and no
// upstream path beyond what route.LogURL leaves (Phases.URL), so every
// consumer of requestFinished may keep or show it.
type finishedRequest struct {
	// ID numbers the requests of this process.
	ID uint64
	// Start is when the request reached the handler, Total how long it
	// took until it ended (a stream: until it closed).
	Start time.Time
	Total time.Duration
	// Method is the client's HTTP method.
	Method string
	// Channel is the channel id, Route its route (a tunnel name or
	// "direct"), Kind the metrics kind of the request.
	Channel string
	Route   string
	Kind    metrics.Kind
	// Status and Bytes are what the client got; Status is 0 if nothing.
	Status int
	Bytes  int64
	// ClientGone: the client went away before the request ended.
	ClientGone bool
	// UpstreamStatus is the status of the last upstream response, 0
	// without one. Reason is the upstream error counted for the request,
	// "" without one.
	UpstreamStatus int
	Reason         route.Reason
	// Phases is where the time of the upstream requests went.
	Phases route.Phases
}

// finish returns the finishedRequest of l, called when the request ends.
func (l *requestRecord) finish(r *http.Request, ch config.Channel, rq *metrics.Request) finishedRequest {
	return finishedRequest{
		ID:             l.id,
		Start:          l.start,
		Total:          time.Since(l.start),
		Method:         r.Method,
		Channel:        ch.ID(),
		Route:          ch.Route(),
		Kind:           rq.Kind(),
		Status:         l.status,
		Bytes:          l.bytes,
		ClientGone:     r.Context().Err() != nil,
		UpstreamStatus: l.upstreamStatus,
		Reason:         l.reason,
		Phases:         l.trace.Phases(),
	}
}

// requestFinished is the one place where a finished /c/ or /r/ request is
// handed on, at every log level and for every outcome. Its consumers are
// the timing metrics, the debug line and, while the debug listener is on,
// the buffer of recent requests (/debug/requests). Consumers must not keep
// references into fr that they do not own, and must be cheap: the call
// runs in the request's goroutine.
func (s *Server) requestFinished(fr finishedRequest) {
	s.observePhases(fr)
	s.logRequest(fr)
	if s.recent != nil {
		s.recent.add(fr)
	}
}

// observePhases records the phases of an upstream request that happened:
// a lookup, a connection, a TLS handshake, a response byte. A phase that
// did not happen (pooled connection, IP literal, plain HTTP, no response)
// is not observed.
func (s *Server) observePhases(fr finishedRequest) {
	p := fr.Phases
	if p.Lookups > 0 {
		s.metrics.ObserveUpstreamPhase(fr.Route, metrics.PhaseDNS, p.DNS)
	}
	if p.ConnectAttempts > 0 {
		s.metrics.ObserveUpstreamPhase(fr.Route, metrics.PhaseConnect, p.Connect)
	}
	if p.TLSHandshakes > 0 {
		s.metrics.ObserveUpstreamPhase(fr.Route, metrics.PhaseTLS, p.TLS)
	}
	if p.GotFirstByte {
		s.metrics.ObserveUpstreamPhase(fr.Route, metrics.PhaseFirstByte, p.FirstByte)
	}
}
