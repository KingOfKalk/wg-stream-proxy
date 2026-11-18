// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"log/slog"
)

// logRequest writes the debug line of a finished request (spec 8): what
// was asked and answered, and where the time went (route.Phases). The URL
// is the last upstream URL, reduced by route.LogURL; tunnel_start appears
// only if the request waited for a tunnel start. Above debug level it
// costs a level check.
func (s *Server) logRequest(fr finishedRequest) {
	ctx := context.Background()
	if !s.log.Enabled(ctx, slog.LevelDebug) {
		return
	}
	p := fr.Phases
	attrs := make([]slog.Attr, 0, 21)
	attrs = append(attrs,
		slog.Uint64("id", fr.ID),
		slog.String("method", fr.Method),
		slog.String("channel", fr.Channel),
		slog.String("route", fr.Route),
		slog.String("kind", string(fr.Kind)),
		slog.Int("status", fr.Status),
		slog.Int64("bytes", fr.Bytes),
		slog.Bool("client_gone", fr.ClientGone),
	)
	if fr.UpstreamStatus != 0 {
		attrs = append(attrs, slog.Int("upstream_status", fr.UpstreamStatus))
	}
	if fr.Reason != "" {
		attrs = append(attrs, slog.String("reason", string(fr.Reason)))
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
		slog.Duration("total", fr.Total),
		slog.Bool("conn_reused", p.Reused),
		slog.Int("redirects", p.Redirects),
		slog.String("url", p.URL),
	)
	s.log.LogAttrs(ctx, slog.LevelDebug, "request", attrs...)
}
