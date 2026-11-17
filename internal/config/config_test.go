// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import "testing"

func TestLogoKindString(t *testing.T) {
	tests := []struct {
		k    LogoKind
		want string
	}{
		{LogoNone, "none"},
		{LogoURL, "url"},
		{LogoLocal, "local"},
		{LogoRaw, "raw"},
		{LogoKind(42), "LogoKind(42)"},
	}
	for _, tt := range tests {
		if got := tt.k.String(); got != tt.want {
			t.Errorf("LogoKind(%d).String() = %q, want %q", int(tt.k), got, tt.want)
		}
	}
}
