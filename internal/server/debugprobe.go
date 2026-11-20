// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/probe"
	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
)

const (
	// maxProbeBody bounds the request body of POST /debug/probe.
	maxProbeBody = 16 << 10
	// probeTimeout bounds one probe: a tunnel start and up to three
	// upstream requests, each within the [upstream] timeouts.
	probeTimeout = 60 * time.Second
)

// probeRequest is the body of POST /debug/probe. The URL is in the body,
// never in the query string, so that it does not reach access logs.
type probeRequest struct {
	// Route is direct or a tunnel name; empty means direct.
	Route string `json:"route"`
	URL   string `json:"url"`
	// Headers are the upstream request headers, like upstream.headers of
	// a channel.
	Headers map[string]string `json:"headers"`
}

// probeError is the JSON body of a failed POST /debug/probe.
type probeError struct {
	Error string `json:"error"`
}

// handleProbe runs the probe command (spec 7.2) through the running
// server: with the current generation's route, so a tunnel that is up is
// used and no second device starts, and with its guard, [upstream]
// settings and limits. The answer is the JSON of probe --json. The probe
// is no channel request: it counts in no metric; it logs one
// line with the route and the URL in its logged form.
func (d *debugHandler) handleProbe(w http.ResponseWriter, r *http.Request) {
	fail := func(code int, msg string) { writeJSON(w, code, probeError{msg}) }
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxProbeBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			fail(http.StatusRequestEntityTooLarge, "request body larger than 16 KiB")
			return
		}
		fail(http.StatusBadRequest, "cannot read the request body")
		return
	}
	var req probeRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.More() {
		// The decoder error may quote body text: not repeated.
		fail(http.StatusBadRequest, `the body must be one JSON object {"route", "url", "headers"}`)
		return
	}
	if err := probe.CheckURL(req.URL); err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	header := http.Header{}
	keys := make([]string, 0, len(req.Headers))
	for k := range req.Headers {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if err := probe.SetHeader(header, k, req.Headers[k]); err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
	}

	g := d.srv.active()
	name := req.Route
	if name == "" {
		name = config.RouteDirect
	}
	rt, ok := g.routes.Route(name)
	if !ok {
		fail(http.StatusNotFound, "unknown route "+quote(name))
		return
	}
	d.srv.log.Info("probe", "route", name, "url", route.LogURL(req.URL))

	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	rep := probe.Run(ctx, req.URL, probe.Options{Route: rt, Header: header, MaxPlaylistSize: g.cfg.Upstream().MaxPlaylistSize})
	rep.Mode = probe.ModeProxy
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = rep.WriteJSON(w)
}

// quote returns s as a JSON string, for messages.
func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
