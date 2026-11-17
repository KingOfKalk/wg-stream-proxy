// SPDX-License-Identifier: AGPL-3.0-or-later

package wgquick

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
)

// redacted replaces a key in every text representation.
const redacted = "[redacted]"

// Key is a 32-byte WireGuard key (private, public or preshared).
//
// A Key never reveals its value: fmt (every verb), slog and encoding/json
// all print a redacted placeholder. Only UAPIConfig writes the bytes.
type Key struct {
	b [32]byte
}

// IsZero reports whether k is all zeros. A zero PresharedKey means none,
// as in WireGuard itself.
func (k Key) IsZero() bool {
	return k == Key{}
}

// Format implements fmt.Formatter, so no verb prints the key bytes.
func (Key) Format(f fmt.State, _ rune) {
	_, _ = f.Write([]byte(redacted))
}

// LogValue implements slog.LogValuer.
func (Key) LogValue() slog.Value {
	return slog.StringValue(redacted)
}

// MarshalJSON implements json.Marshaler.
func (Key) MarshalJSON() ([]byte, error) {
	return []byte(`"` + redacted + `"`), nil
}

// hex returns the lowercase hex form used by the UAPI.
func (k Key) hex() string {
	return hex.EncodeToString(k.b[:])
}

var errInvalidKey = errors.New("invalid key")

// parseKey decodes a standard base64 key of exactly 32 bytes. The error
// never contains the input.
func parseKey(s string) (Key, error) {
	var k Key
	if base64.StdEncoding.EncodedLen(len(k.b)) != len(s) {
		return Key{}, errInvalidKey
	}
	var buf [33]byte // DecodedLen(44) is 33
	n, err := base64.StdEncoding.Strict().Decode(buf[:], []byte(s))
	if err != nil || n != len(k.b) {
		return Key{}, errInvalidKey
	}
	copy(k.b[:], buf[:n])
	clear(buf[:])
	return k, nil
}
