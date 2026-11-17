// SPDX-License-Identifier: AGPL-3.0-or-later

package wgquick

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
)

func TestCheckUniquePrivateKeys(t *testing.T) {
	a := &Tunnel{Name: "gr", PrivateKey: mustKey(testPrivA)}
	b := &Tunnel{Name: "de", PrivateKey: mustKey(testPrivB)}
	c := &Tunnel{Name: "cy", PrivateKey: mustKey(testPrivA)}
	tests := []struct {
		name    string
		tunnels []*Tunnel
		wantMsg string
	}{
		{"none", nil, ""},
		{"one", []*Tunnel{a}, ""},
		{"distinct", []*Tunnel{a, b}, ""},
		{"duplicate", []*Tunnel{a, b, c}, `same PrivateKey as tunnel "gr"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckUniquePrivateKeys(tt.tunnels)
			if tt.wantMsg == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			var pe *Error
			if !errors.As(err, &pe) {
				t.Fatalf("err = %v (%T), want *Error", err, err)
			}
			if pe.Tunnel != "cy" || pe.Msg != tt.wantMsg {
				t.Errorf("err = %+v, want tunnel cy, msg %q", *pe, tt.wantMsg)
			}
			assertNoSecrets(t, "error", err.Error())
		})
	}
}

func TestModeWarning(t *testing.T) {
	tests := []struct {
		mode fs.FileMode
		want bool
	}{
		{0o600, false},
		{0o640, false},
		{0o400, false},
		{0o644, true},
		{0o604, true},
		{0o666, true},
		{0o602, false},
	}
	for _, tt := range tests {
		w, got := ModeWarning("gr", tt.mode)
		if got != tt.want {
			t.Errorf("ModeWarning(%v) = %v, want %v", tt.mode, got, tt.want)
			continue
		}
		if got && (w.Tunnel != "gr" || !strings.Contains(w.Msg, "world-readable")) {
			t.Errorf("ModeWarning(%v) = %+v, want world-readable warning for gr", tt.mode, w)
		}
		if !got && w != (Warning{}) {
			t.Errorf("ModeWarning(%v) = %+v, want zero Warning", tt.mode, w)
		}
	}
}
