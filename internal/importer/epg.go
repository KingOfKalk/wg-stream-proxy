// SPDX-License-Identifier: AGPL-3.0-or-later

package importer

import (
	"strings"

	"github.com/KingOfKalk/wg-stream-proxy/internal/epg"
)

// matchEPG applies --match-epg (spec 7.2) to the channels: a tvg_id that
// exists in the EPG is kept; any other is dropped with a warning. A
// channel without a kept tvg_id gets its best EPG suggestion as a comment
// (tvgHint), if there is one.
func matchEPG(chs []channel, x *epg.Index) []Warning {
	var warns []Warning
	for i := range chs {
		ch := &chs[i]
		if ch.tvgID != "" {
			if x.Has(ch.tvgID) {
				continue
			}
			warns = append(warns, Warning{Line: ch.line, Msg: "tvg-id " + quote(ch.tvgID) + " is not in the EPG, dropped"})
			ch.tvgID = ""
		}
		if c, ok := x.Best(ch.name); ok {
			// EPG names come from a foreign file: keep the comment on one line.
			name := strings.Join(strings.Fields(c.Name), " ")
			ch.tvgHint = "tvg_id = " + quote(c.ID) + "  # EPG: " + name
		}
	}
	return warns
}
