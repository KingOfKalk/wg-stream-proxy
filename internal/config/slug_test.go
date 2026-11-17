// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"strings"
	"testing"
)

func TestSlug(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"ascii", "Das Erste", "das-erste"},
		{"ascii with digits", "ERT World", "ert-world"},
		{"greek upper", "ΣΚΑΪ", "skai"},
		{"greek accents removed first", "Θεσσαλονίκη", "thessaloniki"},
		{"greek digraphs", "Ψυχή Χαρά", "psychi-chara"},
		{"final sigma", "Κόσμος", "kosmos"},
		{"greek full table", "αβγδεζηθικλμνξοπρσςτυφχψω", "avgdezithiklmnxoprsstyfchpso"},
		{"greek with latin and digits", "ΕΡΤ1 HD", "ert1-hd"},
		{"latin accents", "Télé Ümlaut Ñ", "tele-umlaut-n"},
		{"nfkd ligature and fullwidth", "ﬁlm ＴＶ２", "film-tv2"},
		{"runs collapse and trim", "  --Sky // Sport++ 1--  ", "sky-sport-1"},
		{"only separators", "!!! ???", ""},
		{"empty", "", ""},
		{"non-latin script dropped", "日本 TV", "tv"},
		{"truncate to 48", strings.Repeat("a", 60), strings.Repeat("a", 48)},
		{"truncate then trim dash", strings.Repeat("a", 47) + " bcd", strings.Repeat("a", 47)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Slug(tt.in); got != tt.want {
				t.Errorf("Slug(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestValidName(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"gr", true},
		{"ert-world", true},
		{"a1-b2", true},
		{strings.Repeat("a", 48), true},
		{strings.Repeat("a", 49), false},
		{"", false},
		{"GR", false},
		{"gr_1", false},
		{"gr.1", false},
		{"ελ", false},
		{"a b", false},
	}
	for _, tt := range tests {
		if got := ValidName(tt.in); got != tt.want {
			t.Errorf("ValidName(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
