// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import "testing"

func TestParseSize(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"8MiB", 8 << 20, true},
		{"512KiB", 512 << 10, true},
		{"2097152B", 2 << 20, true},
		{"32MiB", 32 << 20, true},
		{"0B", 0, true},
		{"007KiB", 7 << 10, true},
		{"", 0, false},
		{"8", 0, false},
		{"MiB", 0, false},
		{"8 MiB", 0, false},
		{" 8MiB", 0, false},
		{"8mib", 0, false},
		{"8MB", 0, false},
		{"8M", 0, false},
		{"8GiB", 0, false},
		{"-1MiB", 0, false},
		{"+1MiB", 0, false},
		{"1.5MiB", 0, false},
		{"1_000B", 0, false},
		{"99999999999999999999B", 0, false},
		{"9999999999999MiB", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, ok := parseSize(tt.in)
			if got != tt.want || ok != tt.ok {
				t.Errorf("parseSize(%q) = %d, %v, want %d, %v", tt.in, got, ok, tt.want, tt.ok)
			}
		})
	}
}
