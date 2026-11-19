// SPDX-License-Identifier: AGPL-3.0-or-later

package epg

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
)

// source returns an XMLTV document of source tag with a channel and one
// programme per id; the programme title names the source.
func source(tag string, ids ...string) string {
	var b strings.Builder
	b.WriteString("<tv>")
	for _, id := range ids {
		b.WriteString(`<channel id="` + id + `"><display-name>` + tag + `</display-name></channel>`)
	}
	for _, id := range ids {
		b.WriteString(`<programme channel="` + id + `" stop="20261120000000"><title>` + tag + " " + id + `</title></programme>`)
	}
	b.WriteString("</tv>")
	return b.String()
}

// copyOf filters doc for every want.
func copyOf(t *testing.T, doc string, wants []Want) *Copy {
	t.Helper()
	ids := map[string]bool{}
	for _, w := range wants {
		ids[w.ID] = true
	}
	c, err := Filter(strings.NewReader(doc), ids, filterNow)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestMerge(t *testing.T) {
	gr := source("gr", "a", "b", "c")
	de := source("de", "b", "c", "d")
	// Programmes of "e" in gr without a channel element: gr does not have it.
	orphan := `<tv><programme channel="e" stop="20261120000000"><title>gr e</title></programme></tv>`
	wants := []Want{{ID: "d"}, {ID: "b"}, {ID: "c", Source: "de"}, {ID: "a", Source: "de"}, {ID: "e"}, {ID: "x"}}
	copies := map[string]*Copy{"gr": copyOf(t, gr, wants), "de": copyOf(t, de, wants), "orphan": copyOf(t, orphan, wants)}
	doc, channels, programmes := Merge([]string{"orphan", "gr", "de"}, copies, wants)
	want := xmlHeader +
		`<channel id="d"><display-name>de</display-name></channel>` + "\n" +
		`<channel id="b"><display-name>gr</display-name></channel>` + "\n" +
		`<channel id="c"><display-name>de</display-name></channel>` + "\n" +
		`<programme channel="d" stop="20261120000000"><title>de d</title></programme>` + "\n" +
		`<programme channel="b" stop="20261120000000"><title>gr b</title></programme>` + "\n" +
		`<programme channel="c" stop="20261120000000"><title>de c</title></programme>` + "\n" +
		"</tv>\n"
	if string(doc) != want {
		t.Errorf("got\n%s\nwant\n%s", doc, want)
	}
	if channels != 3 || programmes != 3 {
		t.Errorf("channels %d programmes %d, want 3 and 3", channels, programmes)
	}

	// A source without a copy (never fetched) is skipped; a named one
	// is not replaced by another source.
	delete(copies, "de")
	doc, _, _ = Merge([]string{"gr", "de"}, copies, wants)
	if s := string(doc); !strings.Contains(s, `<title>gr b</title>`) || strings.Contains(s, `id="c"`) || strings.Contains(s, `id="a"`) {
		t.Errorf("without de:\n%s", s)
	}
}

func TestWants(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"config.toml": `[[epg.source]]
name = "gr"
url = "https://epg.example.org/gr.xml.gz"
[[epg.source]]
name = "de"
url = "https://epg.example.net/de.xml"
`,
		"channels.d/tv.toml": `tunnel = "direct"
[[channel]]
name = "A"
url = "https://live.example.com/a.m3u8"
playlist.tvg_id = "a.example"
[[channel]]
name = "A HD"
url = "https://live.example.com/a-hd.m3u8"
playlist = { tvg_id = "a.example", epg_source = "de" }
[[channel]]
name = "No id"
url = "https://live.example.com/n.m3u8"
[[channel]]
name = "Off"
url = "https://live.example.com/off.m3u8"
enabled = false
playlist.tvg_id = "off.example"
[[channel]]
name = "B"
url = "https://live.example.com/b.m3u8"
playlist = { tvg_id = "b.example", epg_source = "gr" }
`,
	}
	writeFiles(t, dir, files)
	cfg, err := config.Load(config.Options{Paths: config.Paths{ConfigDir: dir, TunnelsDir: filepath.Join(dir, "tunnels")}})
	if err != nil {
		t.Fatal(err)
	}
	got := Wants(cfg.Channels())
	want := []Want{{ID: "a.example", Source: "de"}, {ID: "b.example", Source: "gr"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Wants = %v, want %v", got, want)
	}
	if ids := idsFor("gr", want); !reflect.DeepEqual(ids, map[string]bool{"b.example": true}) {
		t.Errorf("idsFor(gr) = %v", ids)
	}
}
