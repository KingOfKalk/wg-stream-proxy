// SPDX-License-Identifier: AGPL-3.0-or-later

package epg

import (
	"bytes"
	"io"
	"testing"
)

// FuzzParse checks that Open and Parse never panic, and that a parsed
// index is consistent: sorted unique ids, no empty ids or names, and
// matching any name never fails.
func FuzzParse(f *testing.F) {
	plain, err := readFixture()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(plain)
	f.Add([]byte(`<tv><channel id="a"><display-name>A</display-name></channel><programme/></tv>`))
	f.Add([]byte("<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><tv><channel id=\"\xe9\"/></tv>"))
	f.Add([]byte{0x1f, 0x8b, 8, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := Open(bytes.NewReader(data), 1<<20)
		if err != nil {
			return
		}
		x, err := Parse(io.LimitReader(r, 1<<20))
		if err != nil {
			return
		}
		prev := ""
		for _, c := range x.Channels() {
			if c.ID == "" || c.ID <= prev {
				t.Fatalf("ids not sorted and unique: %q after %q", c.ID, prev)
			}
			prev = c.ID
			for _, n := range c.Names {
				if n == "" {
					t.Fatalf("empty name in %q", c.ID)
				}
				x.Match(n, 3)
			}
		}
	})
}
