// SPDX-License-Identifier: AGPL-3.0-or-later

package probe

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
)

// ErrInvalidURL: the URL to probe is not absolute with scheme http or
// https. The error never contains the URL: it may carry a token.
var ErrInvalidURL = errors.New("the URL must be absolute with scheme http or https")

// CheckURL returns ErrInvalidURL unless raw is an absolute http(s) URL.
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !config.IsHTTPURL(u) {
		return ErrInvalidURL
	}
	return nil
}

// SetHeader sets the upstream request header name to value in h, with the
// rules of the --header flag and of the headers of a channel (spec 5.4):
// the name is canonicalised and must be an HTTP token, the value has no
// control character (CR, LF, NUL, ...); a later value replaces an earlier
// one; an empty value removes the header, except for User-Agent, where it
// sends none.
func SetHeader(h http.Header, name, value string) error {
	switch {
	case !config.ValidHeaderName(name):
		return fmt.Errorf("invalid header name %q", name)
	case !config.ValidHeaderValue(value):
		return fmt.Errorf("invalid value for header %q", name)
	}
	name = http.CanonicalHeaderKey(name)
	if value == "" && name != "User-Agent" {
		delete(h, name)
		return nil
	}
	h[name] = []string{value}
	return nil
}
