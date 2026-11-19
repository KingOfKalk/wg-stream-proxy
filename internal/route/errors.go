// SPDX-License-Identifier: AGPL-3.0-or-later

package route

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"strings"
)

// Reason classifies an upstream error for the metric label
// upstream_errors_total{reason} (spec 7.3).
type Reason string

// Reasons (spec 7.3).
const (
	ReasonConnect   Reason = "connect"
	ReasonTimeout   Reason = "timeout"
	ReasonGuard     Reason = "guard"
	ReasonTLS       Reason = "tls"
	ReasonRedirects Reason = "redirects"
	ReasonSize      Reason = "size"
	ReasonOther     Reason = "other"
	// ReasonDRM is a DASH manifest with ContentProtection, refused
	// (spec 3.6, 1.2). Classify never returns it.
	ReasonDRM Reason = "drm"
	// ReasonUnsupported is a playlist in a format the proxy does not
	// rewrite, refused instead of piped (spec 3.3). Classify never
	// returns it.
	ReasonUnsupported Reason = "unsupported"
)

// Classify returns the reason of an upstream error, or "" for nil:
//   - guard: the private-range guard refused every address;
//   - redirects: more than MaxRedirects redirects, or a redirect
//     Request.Redirect refused;
//   - size: an upstream URL or a playlist is too long;
//   - timeout: header_timeout, read_timeout or another deadline;
//   - connect: lookup or connect failed, connect_timeout included;
//   - tls: certificate or handshake errors;
//   - other: everything else, e.g. a client disconnect.
func Classify(err error) Reason {
	var de *dialError
	var ne net.Error
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrBlocked):
		return ReasonGuard
	case errors.Is(err, ErrTooManyRedirects), errors.Is(err, ErrRedirectRefused):
		return ReasonRedirects
	case errors.Is(err, ErrURLTooLong), errors.Is(err, ErrTooLarge):
		return ReasonSize
	case errors.Is(err, ErrHeaderTimeout), errors.Is(err, ErrReadTimeout):
		return ReasonTimeout
	case errors.As(err, &de), errors.Is(err, ErrConnectTimeout):
		return ReasonConnect
	case isTLSError(err):
		return ReasonTLS
	case errors.Is(err, context.DeadlineExceeded),
		errors.As(err, &ne) && ne.Timeout():
		return ReasonTimeout
	}
	return ReasonOther
}

// ErrorStatus returns the client status for an upstream error (spec
// 3.4): 504 for timeouts, 502 for everything else, connection errors and
// guard refusals included.
func ErrorStatus(err error) int {
	if Classify(err) == ReasonTimeout {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

// isTLSError reports whether err comes from certificate verification or
// the TLS handshake. Many handshake errors are untyped; they are
// recognised by their "tls: " prefix anywhere in the chain.
func isTLSError(err error) bool {
	var (
		cve *tls.CertificateVerificationError
		rhe tls.RecordHeaderError
		ae  tls.AlertError
		uae x509.UnknownAuthorityError
		he  x509.HostnameError
		cie x509.CertificateInvalidError
	)
	if errors.As(err, &cve) || errors.As(err, &rhe) || errors.As(err, &ae) ||
		errors.As(err, &uae) || errors.As(err, &he) || errors.As(err, &cie) {
		return true
	}
	for e := err; e != nil; e = errors.Unwrap(e) {
		if strings.HasPrefix(e.Error(), "tls: ") {
			return true
		}
	}
	return false
}
