// SPDX-License-Identifier: AGPL-3.0-or-later

package epg

import (
	"slices"
	"strings"
)

// Index is the channel list of an EPG, as epg ids prints it. It is
// immutable.
type Index struct {
	chans []Channel // sorted by ID
}

// NewIndex builds an Index: channels are sorted by id, channels with the
// same id are merged and names deduplicated.
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
	x := &Index{}
	for _, c := range merged {
		x.chans = append(x.chans, *c)
	}
	slices.SortFunc(x.chans, func(a, b Channel) int { return strings.Compare(a.ID, b.ID) })
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
