// SPDX-License-Identifier: AGPL-3.0-or-later

package epg

import (
	"cmp"
	"slices"
	"strings"
	"unicode"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
)

// MinScore is the lowest similarity that counts as a suggestion (spec 7.2).
const MinScore = 0.6

// Index is the channel list of an EPG with the lookups epg ids, epg match
// and import --match-epg need. It is immutable.
type Index struct {
	chans []Channel // sorted by ID
	byID  map[string]struct{}
	names []nameEntry
}

type nameEntry struct {
	id, name string
	norm     []rune
}

// NewIndex builds an Index: channels are sorted by id, channels with the
// same id are merged and names deduplicated. A channel without display
// name is matched by its id.
func NewIndex(chans []Channel) *Index {
	merged := map[string]*Channel{}
	for _, c := range chans {
		m := merged[c.ID]
		if m == nil {
			m = &Channel{ID: c.ID}
			merged[c.ID] = m
		}
		for _, n := range c.Names {
			if !slices.Contains(m.Names, n) {
				m.Names = append(m.Names, n)
			}
		}
	}
	x := &Index{byID: make(map[string]struct{}, len(merged))}
	for _, c := range merged {
		x.chans = append(x.chans, *c)
		x.byID[c.ID] = struct{}{}
	}
	slices.SortFunc(x.chans, func(a, b Channel) int { return strings.Compare(a.ID, b.ID) })
	for _, c := range x.chans {
		names := c.Names
		if len(names) == 0 {
			names = []string{c.ID}
		}
		for _, n := range names {
			x.names = append(x.names, nameEntry{id: c.ID, name: n, norm: []rune(Normalize(n))})
		}
	}
	return x
}

// Channels returns the channels sorted by id.
func (x *Index) Channels() []Channel {
	out := make([]Channel, len(x.chans))
	for i, c := range x.chans {
		out[i] = Channel{ID: c.ID, Names: slices.Clone(c.Names)}
	}
	return out
}

// Has reports whether the EPG has a channel with exactly this id.
func (x *Index) Has(id string) bool {
	_, ok := x.byID[id]
	return ok
}

// Candidate is an EPG channel suggested for a name.
type Candidate struct {
	ID    string  // EPG channel id
	Name  string  // the display name that matched best
	Score float64 // similarity in (0, 1]; 1 is an exact normalised match
}

// Match returns up to n EPG channels for a channel name, best first.
// Every EPG channel is scored by its best display name; candidates below
// MinScore are left out. Equal scores order by EPG id.
func (x *Index) Match(name string, n int) []Candidate {
	want := []rune(Normalize(name))
	if len(want) == 0 || n <= 0 {
		return nil
	}
	best := map[string]Candidate{}
	for _, e := range x.names {
		s := similarity(want, e.norm)
		if s < MinScore {
			continue
		}
		if c, ok := best[e.id]; !ok || s > c.Score {
			best[e.id] = Candidate{ID: e.id, Name: e.name, Score: s}
		}
	}
	out := make([]Candidate, 0, len(best))
	for _, c := range best {
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b Candidate) int {
		return cmp.Or(cmp.Compare(b.Score, a.Score), strings.Compare(a.ID, b.ID))
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// Best returns the best candidate for a channel name, if any.
func (x *Index) Best(name string) (Candidate, bool) {
	c := x.Match(name, 1)
	if len(c) == 0 {
		return Candidate{}, false
	}
	return c[0], true
}

// Normalize prepares a name for matching (spec 7.2): steps 1 and 2 of the
// id slug (spec 5.2, config.Fold), then only letters and digits are kept,
// so case, spacing, punctuation, accents and Greek versus Latin spelling
// do not matter.
func Normalize(name string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return -1
	}, config.Fold(name))
}

// similarity is the normalised Levenshtein similarity of two normalised
// names: 1 - distance / length of the longer one. Two empty names score 0.
func similarity(a, b []rune) float64 {
	m := max(len(a), len(b))
	if m == 0 {
		return 0
	}
	return 1 - float64(levenshtein(a, b))/float64(m)
}

// levenshtein is the edit distance (insert, delete, replace) of a and b.
func levenshtein(a, b []rune) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
