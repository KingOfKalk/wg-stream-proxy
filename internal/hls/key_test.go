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
		"/run/secrets/key":        key32 + "\n",
		"/run/secrets/padded":     " \t\r\n" + key32 + " \n\n",
		"/run/secrets/longer":     key32 + "xyz",
		"/run/secrets/short":      strings.Repeat("s", 31) + "\n",
		"/run/secrets/short-trim": " " + strings.Repeat("s", 31) + " ",
		"/run/secrets/empty":      "\n",
		"/run/secrets/blank":      strings.Repeat(" ", 40),
		"/run/secrets/binary":     "\x00\x01\x02" + strings.Repeat("\xff", 29),
		"/run/secrets/space-mid":  strings.Repeat("a", 16) + " " + strings.Repeat("b", 15),
	}
	readFile := func(name string) ([]byte, error) {
		if s, ok := files[name]; ok {
			return []byte(s), nil
		}
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	tests := []struct {
		name    string
		file    string
		want    string
		wantErr error
	}{
		{"file key", "/run/secrets/key", key32, nil},
		{"file key trimmed", "/run/secrets/padded", key32, nil},
		{"file key longer than minimum", "/run/secrets/longer", key32 + "xyz", nil},
		{"file key raw bytes", "/run/secrets/binary", files["/run/secrets/binary"], nil},
		{"inner whitespace kept", "/run/secrets/space-mid", files["/run/secrets/space-mid"], nil},
		{"file key short", "/run/secrets/short", "", ErrShortKey},
		{"file key short after trim", "/run/secrets/short-trim", "", ErrShortKey},
		{"file key empty", "/run/secrets/empty", "", ErrShortKey},
		{"file key whitespace only", "/run/secrets/blank", "", ErrShortKey},
		{"file missing", "/run/secrets/missing", "", fs.ErrNotExist},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(k string) string {
				if k == EnvSigningKeyFile {
					return tt.file
				}
				return ""
			}
			key, generated, err := LoadSigningKey(getenv, readFile)
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
	_, _, err := LoadSigningKey(func(string) string { return "/key" },
		func(string) ([]byte, error) { return []byte(secret), nil })
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("error = %v, want an error without the key", err)
	}
}

func TestLoadSigningKeyGeneratesRandomKey(t *testing.T) {
	noFile := func(name string) ([]byte, error) {
		t.Errorf("readFile(%q) called without a key file", name)
		return nil, fs.ErrNotExist
	}
	// An empty variable counts as unset.
	for _, unset := range []func(string) string{
		func(string) string { return "" },
	} {
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
}
