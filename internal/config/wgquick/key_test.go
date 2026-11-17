// SPDX-License-Identifier: AGPL-3.0-or-later

package wgquick

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestKeyIsRedactedEverywhere(t *testing.T) {
	tun, _, err := Parse("gr", strings.NewReader(readFixture(t, "full.conf")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	values := map[string]any{
		"key":            tun.PrivateKey,
		"key pointer":    &tun.PrivateKey,
		"tunnel":         *tun,
		"tunnel pointer": tun,
		"peer":           tun.Peer,
	}
	verbs := []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%o", "%b", "%T"}
	for name, v := range values {
		for _, verb := range verbs {
			assertNoSecrets(t, name+" "+verb, fmt.Sprintf(verb, v))
		}
	}
	if got := fmt.Sprint(tun.PrivateKey); got != redacted {
		t.Errorf("fmt.Sprint(key) = %q, want %q", got, redacted)
	}
	if got := fmt.Sprintf("%d", tun.PrivateKey); got != redacted {
		t.Errorf(`fmt.Sprintf("%%d", key) = %q, want %q`, got, redacted)
	}

	for _, h := range []func(*bytes.Buffer) slog.Handler{
		func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
		func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
	} {
		var buf bytes.Buffer
		slog.New(h(&buf)).Info("tunnel", "key", tun.PrivateKey, "tunnel", tun, "peer", tun.Peer)
		assertNoSecrets(t, "slog", buf.String())
		if !strings.Contains(buf.String(), redacted) {
			t.Errorf("slog output %q lacks %q", buf.String(), redacted)
		}
	}

	b, err := json.Marshal(tun)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	assertNoSecrets(t, "json", string(b))
}

func TestKeyIsZero(t *testing.T) {
	if !(Key{}).IsZero() {
		t.Error("zero Key: IsZero() = false")
	}
	if mustKey(testPSK).IsZero() {
		t.Error("PSK: IsZero() = true")
	}
}
