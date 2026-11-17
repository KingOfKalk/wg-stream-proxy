// SPDX-License-Identifier: AGPL-3.0-or-later

package route

import "time"

// Clock schedules the timers of a route: connect, header and read
// timeouts and the Happy-Eyeballs fallback delay. Tests inject a manual
// clock instead of sleeping (spec 9).
type Clock interface {
	// AfterFunc calls f in its own goroutine after d, like time.AfterFunc.
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is a timer created by Clock.AfterFunc; *time.Timer implements it.
type Timer interface {
	Stop() bool
	Reset(d time.Duration) bool
}

// SystemClock returns the clock backed by package time.
func SystemClock() Clock { return systemClock{} }

type systemClock struct{}

func (systemClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }
