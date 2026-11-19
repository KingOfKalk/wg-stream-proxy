// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
)

// WatchOptions configure Watch.
type WatchOptions struct {
	// Interval is the time between two checks; it must be positive.
	Interval time.Duration
	// Clock schedules the checks; nil means the system clock.
	Clock route.Clock
	// Fingerprint returns the state of the watched files; see
	// config.Fingerprint. Equal values mean no change.
	Fingerprint func() string
	// Initial is the fingerprint of the files the running config was
	// loaded from; take it before loading, so a write in between is seen
	// as a change.
	Initial string
	// Reload reloads the configuration, normally Reloader.Reload (it
	// reports a rejected config itself).
	Reload func() error
	Log    *slog.Logger
}

// Watch polls the config files and calls o.Reload when they changed
// (spec 5.6), until ctx ends. A change counts once the fingerprint has
// stayed the same for one further interval, so an editor that writes a
// file in several steps causes one reload. A reload that fails is not
// repeated until the files change again.
func Watch(ctx context.Context, o WatchOptions) {
	clk := o.Clock
	if clk == nil {
		clk = route.SystemClock()
	}
	applied := o.Initial // what the running config was loaded from
	var (
		pending    string // last fingerprint seen that differs from applied
		hasPending bool
	)
	tick := make(chan struct{}, 1)
	for {
		timer := clk.AfterFunc(o.Interval, func() {
			select {
			case tick <- struct{}{}:
			default:
			}
		})
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-tick:
		}

		fp := o.Fingerprint()
		switch {
		case fp == applied:
			hasPending = false
		case !hasPending || fp != pending:
			// New or still changing: wait one more interval.
			pending, hasPending = fp, true
		default:
			o.Log.Info("config files changed: reloading")
			err := o.Reload()
			if errors.Is(err, ErrNotServing) {
				// Not serving yet (or anymore): try again next time.
				continue
			}
			// Also after a rejected reload: its errors were logged once,
			// and the next change reloads again.
			applied, hasPending = fp, false
		}
	}
}
