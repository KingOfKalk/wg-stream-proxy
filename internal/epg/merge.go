// SPDX-License-Identifier: AGPL-3.0-or-later

package epg

import (
	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
)

// Want is a channel id the served guide should have (spec 5.4): a
// tvg_id of an enabled channel, and the EPG source named by its
// epg_source ("": the first source that has the id).
type Want struct {
	ID     string
	Source string
}

// Wants returns the guide ids of channels in config order: the tvg_id of
// every enabled channel, once, with the epg_source of the first channel
// of that id that sets one (config.Load refuses different ones).
func Wants(chs []config.Channel) []Want {
	var out []Want
	at := map[string]int{}
	for _, ch := range chs {
		id := ch.TVGID()
		if !ch.Enabled() || id == "" {
			continue
		}
		i, ok := at[id]
		if !ok {
			at[id] = len(out)
			out = append(out, Want{ID: id, Source: ch.EPGSource()})
			continue
		}
		if out[i].Source == "" {
			out[i].Source = ch.EPGSource()
		}
	}
	return out
}

// idsFor returns the ids a source is filtered for: the wants without a
// source of their own and those that name it.
func idsFor(source string, wants []Want) map[string]bool {
	ids := map[string]bool{}
	for _, w := range wants {
		if w.Source == "" || w.Source == source {
			ids[w.ID] = true
		}
	}
	return ids
}

// xmlHeader starts every served guide.
const xmlHeader = `<?xml version="1.0" encoding="UTF-8"?>` + "\n<tv>\n"

// Merge builds the served guide (spec 6.1): per want, the <channel>
// element and all programmes of one source, the one it names or else
// the first source in order whose copy has a <channel> element with the
// id; programmes are never mixed across sources. Ids no source has are
// left out. All channels come first, then the programmes (XMLTV DTD
// order), both in the order of wants. copies maps source names to their
// last good copy; sources without one are skipped.
func Merge(order []string, copies map[string]*Copy, wants []Want) (doc []byte, channels, programmes int) {
	var picked []*entry
	for _, w := range wants {
		var e *entry
		if w.Source != "" {
			if c := copies[w.Source]; c != nil && c.Has(w.ID) {
				e = c.chans[w.ID]
			}
		} else {
			for _, name := range order {
				if c := copies[name]; c != nil && c.Has(w.ID) {
					e = c.chans[w.ID]
					break
				}
			}
		}
		if e != nil {
			picked = append(picked, e)
		}
	}
	size := len(xmlHeader) + len("</tv>\n")
	for _, e := range picked {
		size += len(e.channel) + len(e.programmes)
	}
	doc = make([]byte, 0, size)
	doc = append(doc, xmlHeader...)
	for _, e := range picked {
		doc = append(doc, e.channel...)
	}
	for _, e := range picked {
		doc = append(doc, e.programmes...)
		programmes += e.count
	}
	doc = append(doc, "</tv>\n"...)
	return doc, len(picked), programmes
}
