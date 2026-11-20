// SPDX-License-Identifier: AGPL-3.0-or-later

package hls

import (
	"bytes"
	"crypto/rand"
	"fmt"
)

// EnvSigningKeyFile names the file with the signing key (spec 4).
const EnvSigningKeyFile = "WG_STREAM_PROXY_SIGNING_KEY_FILE"

// LoadSigningKey returns the resource URL signing key (spec 4): the contents
// of the file named by WG_STREAM_PROXY_SIGNING_KEY_FILE, as raw bytes with
// leading and trailing ASCII whitespace removed. An empty variable counts as
// unset. A key shorter than MinKeyLen is an error (ErrShortKey).
//
// If the variable is not set, it returns a random MinKeyLen-byte key and
// generated = true; the caller logs a warning that resource URLs do not
// survive restarts. getenv and readFile are os.Getenv and os.ReadFile
// outside tests. Errors never contain the key.
func LoadSigningKey(getenv func(string) string, readFile func(string) ([]byte, error)) (key []byte, generated bool, err error) {
	file := getenv(EnvSigningKeyFile)
	if file == "" {
		key = make([]byte, MinKeyLen)
		_, _ = rand.Read(key) // never fails, see crypto/rand.Read
		return key, true, nil
	}
	key, err = readFile(file)
	if err != nil {
		return nil, false, fmt.Errorf("hls: read %s: %w", EnvSigningKeyFile, err)
	}
	key = bytes.Trim(key, " \t\r\n\v\f")
	if len(key) < MinKeyLen {
		return nil, false, ErrShortKey
	}
	return key, false, nil
}
