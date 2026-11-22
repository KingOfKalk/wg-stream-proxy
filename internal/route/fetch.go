// SPDX-License-Identifier: AGPL-3.0-or-later

package route

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
)

// Limits (spec 3.4).
const (
	// MaxRedirects is the number of redirects followed per request.
	MaxRedirects = 10
	// MaxURLLen is the longest upstream URL in bytes, redirect targets
	// included.
	MaxURLLen = 4096
	// redirectDrainLen is how much of a redirect body is read so the
	// connection can be reused.
	redirectDrainLen = 2 << 10
)

var (
	// ErrTooManyRedirects: more than MaxRedirects redirects.
	ErrTooManyRedirects = errors.New("route: too many redirects")
	// ErrURLTooLong: an upstream URL or redirect target is longer than
	// MaxURLLen.
	ErrURLTooLong = errors.New("route: upstream URL too long")
	// ErrHeaderTimeout: no final response headers within header_timeout.
	ErrHeaderTimeout = errors.New("route: response header timeout")
	// ErrReadTimeout: no body data within read_timeout.
	ErrReadTimeout = errors.New("route: body read timeout")
	// ErrRedirectRefused: Request.Redirect refused a redirect target.
	ErrRedirectRefused = errors.New("route: redirect target not allowed")

	errInvalidURL  = errors.New("route: invalid upstream URL")
	errBadRedirect = errors.New("route: invalid redirect location")
)

// clientHeaders are the only client request headers forwarded upstream
// (spec 3.4), in canonical form.
var clientHeaders = []string{"Range", "If-Range", "Icy-Metadata"}

// Request is an upstream request.
type Request struct {
	// URL is the absolute http(s) upstream URL.
	URL string
	// Header holds the merged upstream.headers of the channel
	// (config.Channel.Header).
	Header http.Header
	// Client holds the headers of the player's request; only Range,
	// If-Range and Icy-MetaData are used.
	Client http.Header
	// ICY sends the request through the route's ICY transport, whose
	// connections rewrite a leading "ICY " status line to "HTTP/1.0 "
	// (spec 3.3). HTTP responses pass unchanged; only the connection pool
	// differs.
	ICY bool
	// Trace, if set, records where the time of the request goes (debug
	// log, spec 8); nil records nothing.
	Trace *Trace
	// Redirect, if set, reports whether a redirect to the resolved
	// target is followed; if not, Fetch fails with ErrRedirectRefused.
	// Prefix-signed resources (spec 4.1) only follow redirects below
	// their prefix. nil follows every redirect.
	Redirect func(*url.URL) bool
}

// Error is an upstream request error. Its URL has no query string,
// fragment or user info (spec 8).
type Error struct {
	URL string
	Err error
}

func (e *Error) Error() string { return "upstream " + e.URL + ": " + e.Err.Error() }

// Unwrap returns the underlying error.
func (e *Error) Unwrap() error { return e.Err }

// Fetch sends GET for req through the route and returns the final
// response after at most MaxRedirects redirects; resp.Request.URL is the
// final URL. The upstream method is always GET: for a client HEAD the
// caller relays the headers and closes the body (spec 3.4).
//
// ctx should be the client request's context, so a client disconnect
// cancels the upstream request. header_timeout runs from the start until
// the final response headers, across redirects; read_timeout limits each
// body Read, so a slow client does not count against it. The caller must
// close the body. Errors are *Error values; see Classify and ErrorStatus.
//
// The request counts as activity of the route (Options.Activity) from the
// start until the body is closed or the error is returned, so an open
// stream keeps a tunnel up and a pooled idle connection does not. With a
// context from Begin on this route, Begin counts it instead.
func (r *Route) Fetch(ctx context.Context, req Request) (*http.Response, error) {
	u, err := parseUpstreamURL(req.URL)
	if err != nil {
		return nil, &Error{URL: LogURL(req.URL), Err: err}
	}

	end := r.beginFetch(ctx)
	if req.Trace != nil {
		ctx = req.Trace.begin(ctx)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	headerTimer := r.clock.AfterFunc(r.upstream.HeaderTimeout, func() { cancel(ErrHeaderTimeout) })
	t := r.transport
	if req.ICY {
		t = r.icyTransport
	}
	resp, u, err := r.follow(ctx, t, u, req)
	req.Trace.setURL(u)
	if !headerTimer.Stop() && err == nil {
		// The timer fired while the headers arrived.
		_ = resp.Body.Close()
		err = ErrHeaderTimeout
	}
	if err != nil {
		if errors.Is(context.Cause(ctx), ErrHeaderTimeout) {
			err = ErrHeaderTimeout
		}
		cancel(nil)
		end()
		return nil, &Error{URL: LogURL(u.String()), Err: err}
	}
	resp.Body = newTimedBody(ctx, cancel, resp.Body, r.clock, r.upstream.ReadTimeout, end)
	return resp, nil
}

// follow sends the request through t and follows redirects. It returns
// the last URL tried, for errors.
func (r *Route) follow(ctx context.Context, t http.RoundTripper, u *url.URL, req Request) (*http.Response, *url.URL, error) {
	for hop := 0; ; hop++ {
		hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			// The parse error quotes the whole URL; it is not passed on
			// (spec 8).
			return nil, u, errInvalidURL
		}
		hreq.Header = requestHeader(u, req.Header, req.Client, r.upstream.UserAgent)
		resp, err := t.RoundTrip(hreq)
		if err != nil {
			return nil, u, err
		}
		loc := resp.Header.Get("Location")
		if !isRedirect(resp.StatusCode) || loc == "" {
			return resp, u, nil
		}
		_, _ = io.CopyN(io.Discard, resp.Body, redirectDrainLen)
		_ = resp.Body.Close()
		if hop == MaxRedirects {
			return nil, u, ErrTooManyRedirects
		}
		next, err := u.Parse(loc)
		switch {
		case err != nil:
			// The Location value may carry tokens; it is not logged.
			return nil, u, errBadRedirect
		case !config.IsHTTPURL(next):
			return nil, u, fmt.Errorf("%w: scheme %q", errBadRedirect, next.Scheme)
		}
		next.User = redirectUser(u, next)
		// A scheme-relative target is parsed without a scheme, where the
		// authority is checked less strictly, and may print as an http URL
		// that does not parse again ("//::/x"). The target is checked like
		// a configured upstream URL, so the next hop requests what was
		// checked and no parse error quotes it.
		next, err = parseUpstreamURL(next.String())
		switch {
		case errors.Is(err, ErrURLTooLong):
			return nil, u, ErrURLTooLong
		case err != nil:
			return nil, u, errBadRedirect
		case req.Redirect != nil && !req.Redirect(next):
			// The target is not logged: it may carry tokens.
			return nil, u, ErrRedirectRefused
		}
		req.Trace.redirected()
		u = next
	}
}

// isRedirect reports whether code is a redirect Fetch follows.
func isRedirect(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// redirectUser returns the user info for the redirect from u to next
// (spec 3.4): next's own, else u's if next has the same scheme, host and
// port, else none, so credentials never reach another host. A relative
// Location already carries u's.
func redirectUser(u, next *url.URL) *url.Userinfo {
	if next.User != nil {
		return next.User
	}
	if u.User != nil && sameOrigin(u, next) {
		return u.User
	}
	return nil
}

// sameOrigin reports whether a and b have the same scheme, host
// (case-insensitive) and port (an explicit default port counts as none).
func sameOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && strings.EqualFold(a.Hostname(), b.Hostname()) && effectivePort(a) == effectivePort(b)
}

// effectivePort returns the port of the http(s) URL u, the scheme's
// default if it has none.
func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

// parseUpstreamURL checks the length and form of an upstream URL.
func parseUpstreamURL(s string) (*url.URL, error) {
	if len(s) > MaxURLLen {
		return nil, ErrURLTooLong
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, errInvalidURL
	}
	if !config.IsHTTPURL(u) {
		return nil, errInvalidURL
	}
	return u, nil
}

// requestHeader builds the upstream request headers for u (spec 3.4): the
// merged channel headers, User-Agent from them or else userAgent, and
// Range, If-Range and Icy-MetaData from the client; no Range for
// playlist paths; Authorization: Basic from the user info of u unless the
// merged headers set Authorization (http.Transport ignores URL.User). It
// is applied on every redirect hop, so Go adds no Referer of its own and
// an empty User-Agent sends none.
func requestHeader(u *url.URL, merged, client http.Header, userAgent string) http.Header {
	h := make(http.Header, len(merged)+len(clientHeaders)+1)
	for k, v := range merged {
		h[http.CanonicalHeaderKey(k)] = slices.Clone(v)
	}
	if _, ok := h["User-Agent"]; !ok {
		h["User-Agent"] = []string{userAgent}
	}
	for _, k := range clientHeaders {
		if v := client.Values(k); len(v) > 0 {
			h[k] = slices.Clone(v)
		}
	}
	if IsPlaylistPath(u.Path) {
		h.Del("Range")
	}
	if _, ok := h["Authorization"]; !ok && u.User != nil {
		pw, _ := u.User.Password()
		h["Authorization"] = []string{"Basic " + base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+pw))}
	}
	return h
}

// IsPlaylistPath reports whether path ends in .m3u8, .m3u or .mpd,
// case-insensitively.
func IsPlaylistPath(path string) bool {
	p := strings.ToLower(path)
	return strings.HasSuffix(p, ".m3u8") || strings.HasSuffix(p, ".m3u") || strings.HasSuffix(p, ".mpd")
}

// timedBody is a response body with the resettable read timer: the timer
// runs only while a Read waits for data and cancels the request when it
// fires (spec 3.4 read_timeout). Close ends the request's activity.
type timedBody struct {
	rc     io.ReadCloser
	ctx    context.Context
	cancel context.CancelCauseFunc
	timer  Timer
	d      time.Duration
	end    func()
}

func newTimedBody(ctx context.Context, cancel context.CancelCauseFunc, rc io.ReadCloser, clk Clock, d time.Duration, end func()) *timedBody {
	t := clk.AfterFunc(d, func() { cancel(ErrReadTimeout) })
	t.Stop()
	return &timedBody{rc: rc, ctx: ctx, cancel: cancel, timer: t, d: d, end: end}
}

func (b *timedBody) Read(p []byte) (int, error) {
	b.timer.Reset(b.d)
	n, err := b.rc.Read(p)
	b.timer.Stop()
	if err != nil && err != io.EOF && errors.Is(context.Cause(b.ctx), ErrReadTimeout) {
		err = ErrReadTimeout
	}
	return n, err
}

func (b *timedBody) Close() error {
	b.timer.Stop()
	err := b.rc.Close()
	b.cancel(nil)
	b.end()
	return err
}
