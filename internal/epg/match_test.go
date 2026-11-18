// SPDX-License-Identifier: AGPL-3.0-or-later

package epg

import (
	"math"
	"reflect"
	"testing"
)

func TestNormalize(t *testing.T) {
	tests := []struct{ in, want string }{
		{"ERT World", "ertworld"},
		{"ΕΡΤ1 HD", "ert1hd"},
		{"ΣΚΑΪ", "skai"},
		{"Das  Erste!", "daserste"},
		{"BBC One (UK)", "bbconeuk"},
		{"日本 TV", "日本tv"},
		{"---", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := Normalize(tt.in); got != tt.want {
			t.Errorf("Normalize(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSimilarity(t *testing.T) {
	tests := []struct {
		a, b string
		want float64
	}{
		{"ertworld", "ertworld", 1},
		{"ertworld", "ertworldhd", 0.8},
		{"abc", "xyz", 0},
		{"ert1", "ert2", 0.75},
		{"", "", 0},
		{"a", "", 0},
		{"kitten", "sitting", 1 - 3.0/7},
	}
	for _, tt := range tests {
		if got := similarity([]rune(tt.a), []rune(tt.b)); math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("similarity(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestMatchRanking(t *testing.T) {
	x := NewIndex(fixtureChannels)
	tests := []struct {
		name string
		in   string
		n    int
		want []Candidate
	}{
		{
			name: "exact normalised match first",
			in:   "ERT World",
			n:    3,
			want: []Candidate{
				{ID: "ertworld.example", Name: "ERT World", Score: 1},
				{ID: "ertworld-hd.example", Name: "ERT World HD", Score: 0.8},
			},
		},
		{
			name: "greek name against greek display name",
			in:   "ΕΡΤ1",
			n:    3,
			want: []Candidate{{ID: "ert1.example", Name: "ΕΡΤ1", Score: 1}},
		},
		{
			name: "best display name wins",
			in:   "ARD Das Erste",
			n:    3,
			want: []Candidate{{ID: "daserste.example", Name: "ARD Das Erste", Score: 1}},
		},
		{
			name: "punctuation and case ignored",
			in:   "das-erste",
			n:    1,
			want: []Candidate{{ID: "daserste.example", Name: "Das Erste", Score: 1}},
		},
		{
			name: "close but not exact",
			in:   "ERT1",
			n:    2,
			want: []Candidate{{ID: "ert1.example", Name: "ΕΡΤ1", Score: 1}},
		},
		{
			name: "limit",
			in:   "ERT World",
			n:    1,
			want: []Candidate{{ID: "ertworld.example", Name: "ERT World", Score: 1}},
		},
		{name: "below threshold", in: "Completely Different", n: 3},
		{name: "empty after normalising", in: "!!!", n: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := x.Match(tt.in, tt.n)
			if len(got) != len(tt.want) {
				t.Fatalf("Match(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
			for i := range got {
				if got[i].ID != tt.want[i].ID || got[i].Name != tt.want[i].Name ||
					math.Abs(got[i].Score-tt.want[i].Score) > 1e-9 {
					t.Errorf("Match(%q)[%d] = %+v, want %+v", tt.in, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestMatchTieBreakByID(t *testing.T) {
	x := NewIndex([]Channel{{ID: "b", Names: []string{"Same"}}, {ID: "a", Names: []string{"Same"}}})
	got := x.Match("same", 5)
	if ids := []string{got[0].ID, got[1].ID}; !reflect.DeepEqual(ids, []string{"a", "b"}) {
		t.Errorf("ids = %v, want [a b]", ids)
	}
}

func TestMatchNoNameFallsBackToID(t *testing.T) {
	x := NewIndex([]Channel{{ID: "ertworld"}})
	got := x.Match("ERT World", 1)
	if len(got) != 1 || got[0].ID != "ertworld" || got[0].Name != "ertworld" {
		t.Errorf("got %+v", got)
	}
}

func TestHas(t *testing.T) {
	x := NewIndex(fixtureChannels)
	if !x.Has("skai.example") || x.Has("SKAI.example") || x.Has("") {
		t.Error("Has must be an exact, case-sensitive id lookup")
	}
}

func TestBest(t *testing.T) {
	x := NewIndex(fixtureChannels)
	if c, ok := x.Best("ERT World HD"); !ok || c.ID != "ertworld-hd.example" {
		t.Errorf("Best = %+v, %v", c, ok)
	}
	if _, ok := x.Best("Nothing Alike"); ok {
		t.Error("Best found a match below the threshold")
	}
}
