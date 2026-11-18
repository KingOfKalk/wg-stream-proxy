// SPDX-License-Identifier: AGPL-3.0-or-later

package tunnel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
	"strings"
	"time"

	"golang.zx2c4.com/wireguard/device"
)

// Debug log (spec 8): the start wait of a request, and wireguard-go's own
// events without keys.

type startTraceKey struct{}

// WithStartTrace returns a copy of ctx with f: a LookupNetIP or
// DialContext with that context that waits for the tunnel to start calls
// f with the time it waited (measured with Options.Clock), whether the
// start succeeded or not. Calls on a running tunnel do not call f. The
// server uses it for the debug line of a request.
func WithStartTrace(ctx context.Context, f func(wait time.Duration)) context.Context {
	return context.WithValue(ctx, startTraceKey{}, f)
}

// startTrace returns the function of WithStartTrace in ctx, or nil.
func startTrace(ctx context.Context) func(time.Duration) {
	f, _ := ctx.Value(startTraceKey{}).(func(time.Duration))
	return f
}

var (
	// wgPeer matches wireguard-go's name of a peer, "peer(" plus base64
	// fragments of its public key.
	wgPeer = regexp.MustCompile(`peer\([^)]*\)`)
	// wgFragment matches such fragments without the "peer(" around them.
	wgFragment = regexp.MustCompile(`[A-Za-z0-9+/=]{4}…[A-Za-z0-9+/=]{4}`)
	// wgKey matches base64 and hex text as long as a key or longer.
	wgKey = regexp.MustCompile(`[A-Za-z0-9+/]{40,}={0,2}`)
)

// redacted replaces key-like text in wireguard-go's log output.
const redacted = "[redacted]"

// wgLog passes wireguard-go's log output of one tunnel on at debug level.
type wgLog struct {
	log  *slog.Logger
	name string
	// peer replaces wireguard-go's name of the peer.
	peer string
}

// wgLogger returns the wireguard-go device logger of tunnel name. At
// debug level it passes wireguard-go's events (handshakes, keepalives,
// retries) on as "wireguard" lines; above debug it formats nothing, so
// a reload of log_level applies at once. Lines about its goroutines
// ("Routine: ...", a few dozen per start and stop) are dropped.
//
// wireguard-go names a peer by fragments of its public key, and its UAPI
// errors may quote the configuration. So the peer is shown as
// "peer(<tunnel>)", lines with a UAPI error are dropped, byte arrays and
// slices (key types) are redacted, and any key-like text left (a
// fragment, or a base64 or hex run of key length) is redacted too.
func wgLogger(log *slog.Logger, name string) *device.Logger {
	l := &wgLog{log: log, name: name, peer: "peer(" + name + ")"}
	return &device.Logger{
		Verbosef: func(format string, args ...any) { l.write("wireguard", format, args) },
		Errorf:   func(format string, args ...any) { l.write("wireguard error", format, args) },
	}
}

func (l *wgLog) write(msg, format string, args []any) {
	ctx := context.Background()
	if !l.log.Enabled(ctx, slog.LevelDebug) || strings.Contains(format, "Routine: ") {
		return
	}
	event, ok := l.sanitize(format, args)
	if !ok {
		return
	}
	l.log.LogAttrs(ctx, slog.LevelDebug, msg, slog.String("tunnel", l.name), slog.String("event", event))
}

// sanitize formats a wireguard-go log line without keys. It reports false
// for a line that must be dropped.
func (l *wgLog) sanitize(format string, args []any) (string, bool) {
	safe := make([]any, len(args))
	for i, arg := range args {
		var ipcErr *device.IPCError
		switch v := arg.(type) {
		case *device.Peer:
			safe[i] = l.peer
			continue
		case error:
			if errors.As(v, &ipcErr) {
				return "", false
			}
		}
		if isBytes(arg) {
			safe[i] = redacted
			continue
		}
		safe[i] = arg
	}
	s := fmt.Sprintf(format, safe...)
	s = wgPeer.ReplaceAllLiteralString(s, l.peer)
	s = wgFragment.ReplaceAllLiteralString(s, redacted)
	s = wgKey.ReplaceAllLiteralString(s, redacted)
	return s, true
}

// isBytes reports whether v is a byte array or slice, like the key types
// of wireguard-go.
func isBytes(v any) bool {
	t := reflect.TypeOf(v)
	if t == nil {
		return false
	}
	switch t.Kind() {
	case reflect.Array, reflect.Slice:
		return t.Elem().Kind() == reflect.Uint8
	case reflect.Pointer:
		e := t.Elem()
		return (e.Kind() == reflect.Array || e.Kind() == reflect.Slice) && e.Elem().Kind() == reflect.Uint8
	}
	return false
}
