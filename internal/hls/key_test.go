// SPDX-License-Identifier: AGPL-3.0-or-later

package hls

import (
	"bytes"
	"errors"
	"io/fs"
	"strings"
	"testing"
)

func TestLoadSigningKey(t *testing.T) {
	key32 := strings.Repeat("k", 32)
	files := map[string]string{
		"/run/secrets/key":       key32 + "\n",
		"/run/secrets/padded":    " \t\r\n" + key32 + " \n\n",
		"/run/secrets/short":     strings.Repeat("s", 31) + "\n",
		"/run/secrets/empty":     "\n",
		"/run/secrets/binary":    "\x00\x01\x02" + strings.Repeat("\xff", 29),
		"/run/secrets/space-mid": strings.Repeat("a", 16) + " " + strings.Repeat("b", 15),
	}
	readFile := func(name string) ([]byte, error) {
		if s, ok := files[name]; ok {
			return []byte(s), nil
		}
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	tests := []struct {
		name    string
		env     map[string]string
		want    string
		wantErr error
	}{
		{"env key", map[string]string{"WG_STREAM_PROXY_SIGNING_KEY": key32}, key32, nil},
		{"env key trimmed", map[string]string{"WG_STREAM_PROXY_SIGNING_KEY": "  " + key32 + "\n"}, key32, nil},
		{"env key longer than minimum", map[string]string{"WG_STREAM_PROXY_SIGNING_KEY": key32 + "xyz"}, key32 + "xyz", nil},
		{"env key short", map[string]string{"WG_STREAM_PROXY_SIGNING_KEY": strings.Repeat("s", 31)}, "", ErrShortKey},
		{"env key short after trim", map[string]string{"WG_STREAM_PROXY_SIGNING_KEY": " " + strings.Repeat("s", 31) + " "}, "", ErrShortKey},
		{"env key whitespace only", map[string]string{"WG_STREAM_PROXY_SIGNING_KEY": strings.Repeat(" ", 40)}, "", ErrShortKey},
		{"file key", map[string]string{"WG_STREAM_PROXY_SIGNING_KEY_FILE": "/run/secrets/key"}, key32, nil},
		{"file key trimmed", map[string]string{"WG_STREAM_PROXY_SIGNING_KEY_FILE": "/run/secrets/padded"}, key32, nil},
		{"file key raw bytes", map[string]string{"WG_STREAM_PROXY_SIGNING_KEY_FILE": "/run/secrets/binary"}, files["/run/secrets/binary"], nil},
		{"inner whitespace kept", map[string]string{"WG_STREAM_PROXY_SIGNING_KEY_FILE": "/run/secrets/space-mid"}, files["/run/secrets/space-mid"], nil},
		{"file key short", map[string]string{"WG_STREAM_PROXY_SIGNING_KEY_FILE": "/run/secrets/short"}, "", ErrShortKey},
		{"file key empty", map[string]string{"WG_STREAM_PROXY_SIGNING_KEY_FILE": "/run/secrets/empty"}, "", ErrShortKey},
		{"file missing", map[string]string{"WG_STREAM_PROXY_SIGNING_KEY_FILE": "/run/secrets/missing"}, "", fs.ErrNotExist},
		{"both set", map[string]string{"WG_STREAM_PROXY_SIGNING_KEY": key32, "WG_STREAM_PROXY_SIGNING_KEY_FILE": "/run/secrets/key"}, "", ErrKeyConflict},
		{"empty env means unset", map[string]string{"WG_STREAM_PROXY_SIGNING_KEY": "", "WG_STREAM_PROXY_SIGNING_KEY_FILE": "/run/secrets/key"}, key32, nil},
		{"empty file env means unset", map[string]string{"WG_STREAM_PROXY_SIGNING_KEY": key32, "WG_STREAM_PROXY_SIGNING_KEY_FILE": ""}, key32, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, generated, err := LoadSigningKey(func(k string) string { return tt.env[k] }, readFile)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if generated {
				t.Error("generated = true for a configured key")
			}
			if string(key) != tt.want {
				t.Errorf("key = %q, want %q", key, tt.want)
			}
		})
	}
}

func TestLoadSigningKeyErrorsDoNotContainKey(t *testing.T) {
	secret := "short-secret-value"
	_, _, err := LoadSigningKey(func(k string) string {
		if k == "WG_STREAM_PROXY_SIGNING_KEY" {
			return secret
		}
		return ""
	}, nil)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("error = %v, want an error without the key", err)
	}
}

func TestLoadSigningKeyGeneratesRandomKey(t *testing.T) {
	unset := func(string) string { return "" }
	noFile := func(name string) ([]byte, error) {
		t.Errorf("readFile(%q) called without a key file", name)
		return nil, fs.ErrNotExist
	}
	key1, generated, err := LoadSigningKey(unset, noFile)
	if err != nil || !generated {
		t.Fatalf("LoadSigningKey = (_, %v, %v), want generated key", generated, err)
	}
	key2, _, _ := LoadSigningKey(unset, noFile)
	if len(key1) != MinKeyLen || len(key2) != MinKeyLen {
		t.Errorf("generated key lengths %d, %d, want %d", len(key1), len(key2), MinKeyLen)
	}
	if bytes.Equal(key1, key2) {
		t.Error("two generated keys are equal")
	}
	if _, err := NewSigner(key1); err != nil {
		t.Errorf("generated key rejected: %v", err)
	}
}
