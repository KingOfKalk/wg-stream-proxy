// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/metrics"
	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
)

// requestLog collects the debug line of one /c/ or /r/ request (spec 8).
// It exists only at debug level: it wraps the response writer to record
// the status and bytes sent to the client.
type requestLog struct {
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
func (l *requestLog) WriteHeader(code int) {
	if l.status == 0 {
		l.status = code
	}
	l.ResponseWriter.WriteHeader(code)
}

// Write implements http.ResponseWriter.
func (l *requestLog) Write(p []byte) (int, error) {
	if l.status == 0 {
		l.status = http.StatusOK
	}
	n, err := l.ResponseWriter.Write(p)
	l.bytes += int64(n)
	return n, err
}

// Unwrap returns the wrapped writer, for http.ResponseController.
func (l *requestLog) Unwrap() http.ResponseWriter { return l.ResponseWriter }

// debugLog returns the requestLog of w, or nil below debug level.
func debugLog(w http.ResponseWriter) *requestLog {
	l, _ := w.(*requestLog)
	return l
}

// upstream records the status of an upstream response.
func (l *requestLog) upstream(status int) {
	if l != nil {
		l.upstreamStatus = status
	}
}

// failed records the reason of an upstream error.
func (l *requestLog) failed(reason route.Reason) {
	if l != nil {
		l.reason = reason
	}
}

// logRequest writes the debug line of a request when it ends: what was
// asked and answered, and where the time went (route.Phases). The URL is
// the last upstream URL, reduced by route.LogURL; tunnel_start appears only if the
// request waited for a tunnel start.
func (s *Server) logRequest(r *http.Request, ch config.Channel, rq *metrics.Request, l *requestLog) {
	p := l.trace.Phases()
	attrs := make([]slog.Attr, 0, 21)
	attrs = append(attrs,
		slog.Uint64("id", l.id),
		slog.String("method", r.Method),
		slog.String("channel", ch.ID()),
		slog.String("route", ch.Route()),
		slog.String("kind", string(rq.Kind())),
		slog.Int("status", l.status),
		slog.Int64("bytes", l.bytes),
		slog.Bool("client_gone", r.Context().Err() != nil),
	)
	if l.upstreamStatus != 0 {
		attrs = append(attrs, slog.Int("upstream_status", l.upstreamStatus))
	}
	if l.reason != "" {
		attrs = append(attrs, slog.String("reason", string(l.reason)))
	}
	if p.NetworkStarted {
		attrs = append(attrs, slog.Duration("tunnel_start", p.NetworkStart))
	}
	attrs = append(attrs,
		slog.Duration("dns", p.DNS),
		slog.Duration("connect", p.Connect),
		slog.Int("connect_attempts", p.ConnectAttempts),
		slog.Duration("tls", p.TLS),
		slog.Duration("ttfb", p.FirstByte),
		slog.Duration("total", time.Since(l.start)),
		slog.Bool("conn_reused", p.Reused),
		slog.Int("redirects", p.Redirects),
		slog.String("url", p.URL),
	)
	s.log.LogAttrs(r.Context(), slog.LevelDebug, "request", attrs...)
}
