// SPDX-License-Identifier: AGPL-3.0-or-later

package importer

import (
	"flag"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/epg"
)

var update = flag.Bool("update", false, "rewrite golden files")

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// testTunnel is the tunnel the golden cases import with, except no-tunnel.
const testTunnel = "gr"

// caseTunnel returns the --tunnel value for a golden case.
func caseTunnel(name string) string {
	if name == "no-tunnel" {
		return ""
	}
	return testTunnel
}

// golden renders an import result as one comparable text.
func golden(res Result, err error) string {
	var b strings.Builder
	if err != nil {
		b.WriteString("== error ==\n" + err.Error() + "\n")
	} else {
		b.WriteString("== toml ==\n")
		b.Write(res.TOML)
	}
	b.WriteString("== warnings ==\n")
	for _, w := range res.Warnings {
		b.WriteString(w.String() + "\n")
	}
	return b.String()
}

// TestImportGolden imports every testdata/*.m3u and compares TOML output,
// warnings and errors with testdata/*.golden. Successful imports must
// load with internal/config and yield the parsed channels.
func TestImportGolden(t *testing.T) {
	inputs, err := filepath.Glob(filepath.Join("testdata", "*.m3u"))
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) == 0 {
		t.Fatal("no golden inputs")
	}
	for _, in := range inputs {
		name := strings.TrimSuffix(filepath.Base(in), ".m3u")
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(in)
			if err != nil {
				t.Fatal(err)
			}
			opts := Options{Tunnel: caseTunnel(name)}
			res, err := Import(data, opts)
			got := golden(res, err)

			path := strings.TrimSuffix(in, ".m3u") + ".golden"
			if *update {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, rerr := os.ReadFile(path)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if got != string(want) {
				t.Errorf("import %s:\ngot:\n%s\nwant:\n%s", in, got, want)
			}
			if err == nil {
				checkLoads(t, data, opts, res.TOML)
			}
		})
	}
}

// checkLoads writes out as channels.d/gr.toml next to a tunnel file gr.conf
// and checks that config.Load yields the channels parsed from data.
func checkLoads(t *testing.T, data []byte, opts Options, out []byte) {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"tunnels", "channels.d"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Tunnel files are only listed by config.Load, not parsed.
	if err := os.WriteFile(filepath.Join(dir, "tunnels", testTunnel+".conf"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if opts.Tunnel != "" && opts.Tunnel != testTunnel && opts.Tunnel != config.RouteDirect {
		if err := os.WriteFile(filepath.Join(dir, "tunnels", opts.Tunnel+".conf"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "channels.d", testTunnel+".toml"), out, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.Options{Paths: config.Paths{
		ConfigDir: dir, TunnelsDir: filepath.Join(dir, "tunnels"),
	}})
	if err != nil {
		t.Fatalf("output does not load: %v\noutput:\n%s", err, out)
	}

	doc, _ := parse(data)
	assignIDs(doc.channels)
	if opts.EPG != nil {
		matchEPG(doc.channels, opts.EPG)
	}
	got := cfg.Channels()
	if len(got) != len(doc.channels) {
		t.Fatalf("loaded %d channels, want %d\noutput:\n%s", len(got), len(doc.channels), out)
	}
	route := opts.Tunnel
	if route == "" {
		route = testTunnel // tunnel named like the file stem (spec 5.2)
	}
	for i, ch := range doc.channels {
		g := got[i]
		id := ch.id
		if id == "" {
			id = config.Slug(ch.name)
		}
		group := ch.group
		if group == "" {
			group = testTunnel // file stem
		}
		header := map[string][]string{"User-Agent": {"Mozilla/5.0 (X11; Linux x86_64) Firefox/132.0"}}
		// An empty value removes the key (spec 5.4); a removed User-Agent
		// stays empty, as [upstream] user_agent is set.
		for k, v := range ch.headers {
			if v != "" || k == "User-Agent" {
				header[k] = []string{v}
			}
		}
		var attrs []config.Attr
		for _, k := range slices.Sorted(maps.Keys(ch.attrs)) {
			if ch.attrs[k] != "" {
				attrs = append(attrs, config.Attr{Key: k, Value: ch.attrs[k]})
			}
		}
		var extra []config.Extra
		for _, d := range slices.Sorted(maps.Keys(ch.extra)) {
			for _, k := range slices.Sorted(maps.Keys(ch.extra[d])) {
				if ch.extra[d][k] != "" {
					extra = append(extra, config.Extra{Directive: d, Key: k, Value: ch.extra[d][k]})
				}
			}
		}
		checks := []struct {
			field     string
			got, want any
		}{
			{"id", g.ID(), id},
			{"name", g.Name(), ch.name},
			{"url", g.URL(), ch.url},
			{"route", g.Route(), route},
			{"group", g.Group(), group},
			{"enabled", g.Enabled(), ch.enabled},
			{"type", g.Type(), config.TypeAuto},
			{"header", map[string][]string(g.Header()), header},
			{"tvg_id", g.TVGID(), ch.tvgID},
			{"logo", g.Logo().Value, ch.logo},
			{"attrs", g.Attrs(), attrs},
			{"extra", g.Extra(), extra},
		}
		for _, c := range checks {
			if !reflect.DeepEqual(c.got, c.want) && (!isEmpty(c.got) || !isEmpty(c.want)) {
				t.Errorf("channel %d (%q) %s = %#v, want %#v\noutput:\n%s", i+1, ch.name, c.field, c.got, c.want, out)
			}
		}
	}
}

// isEmpty reports whether v is a nil or empty slice.
func isEmpty(v any) bool {
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Slice && rv.Len() == 0
}

func TestImportOptions(t *testing.T) {
	data := []byte("#EXTINF:-1,A\nhttps://a.example.com/\n")
	tests := []struct {
		name    string
		tunnel  string
		wantErr string
		want    string
	}{
		{"direct", "direct", "", "tunnel = \"direct\"\n"},
		{"tunnel", "de-1", "", "tunnel = \"de-1\"\n"},
		{"invalid", "Not Valid", `invalid tunnel name "Not Valid"`, ""},
		{"too long", strings.Repeat("a", 49), "invalid tunnel name", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := Import(data, Options{Tunnel: tt.tunnel})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(res.TOML), tt.want) {
				t.Errorf("output lacks %q:\n%s", tt.want, res.TOML)
			}
			checkLoads(t, data, Options{Tunnel: tt.tunnel}, res.TOML)
		})
	}
}

func TestParseEXTINF(t *testing.T) {
	tests := []struct {
		line      string
		wantName  string
		wantAttrs []attr
		wantOK    bool
	}{
		{`#EXTINF:-1,Plain`, "Plain", nil, true},
		{`#EXTINF:-1 a="1" b="x,y",Name, with comma`, "Name, with comma", []attr{{"a", "1"}, {"b", "x,y"}}, true},
		{`#EXTINF:-1 a=1 b=2,N`, "N", []attr{{"a", "1"}, {"b", "2"}}, true},
		{"#EXTINF:-1\ta=\"1\"\t,  N  ", "N", []attr{{"a", "1"}}, true},
		{`#EXTINF:-1 a="1" junk,Rest, of name`, "Rest, of name", []attr{{"a", "1"}}, false},
		{`#EXTINF:-1 a="1 b="2",N`, "N", nil, false},
		{`#EXTINF:-1 a="1"`, "", []attr{{"a", "1"}}, true},
		{`#EXTINF:`, "", nil, true},
		{`#EXTINF:-1 ="x",N`, "N", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			name, attrs, ok := parseEXTINF(tt.line)
			if name != tt.wantName || ok != tt.wantOK || !reflect.DeepEqual(attrs, tt.wantAttrs) {
				t.Errorf("parseEXTINF = %q, %v, %v; want %q, %v, %v", name, attrs, ok, tt.wantName, tt.wantAttrs, tt.wantOK)
			}
		})
	}
}

func TestQuote(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain", `"plain"`},
		{`a"b\c`, `"a\"b\\c"`},
		{"tab\there", `"tab\there"`},
		{"nul\x00del\x7f", `"nul\u0000del\u007F"`},
		{"Ελληνική", `"Ελληνική"`},
	}
	for _, tt := range tests {
		if got := quote(tt.in); got != tt.want {
			t.Errorf("quote(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func TestKey(t *testing.T) {
	tests := []struct{ in, want string }{
		{"User-Agent", "User-Agent"},
		{"parent_code-1", "parent_code-1"},
		{"a.b", `"a.b"`},
		{"", `""`},
		{"ü", `"ü"`},
	}
	for _, tt := range tests {
		if got := key(tt.in); got != tt.want {
			t.Errorf("key(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

// testEPG is the EPG of the --match-epg golden cases (made-up channels).
func testEPG() *epg.Index {
	return epg.NewIndex([]epg.Channel{
		{ID: "ertworld.example", Names: []string{"ERT World"}},
		{ID: "ertworld-hd.example", Names: []string{"ERT World HD"}},
		{ID: "skai.example", Names: []string{"ΣΚΑΪ"}},
		{ID: "daserste.example", Names: []string{"Das Erste", "ARD Das Erste"}},
		{ID: "radio-one.example", Names: []string{"Radio One"}},
	})
}

// TestImportMatchEPGGolden imports every testdata/match-epg/*.m3u with
// Options.EPG set; the output must load with internal/config.
func TestImportMatchEPGGolden(t *testing.T) {
	inputs, err := filepath.Glob(filepath.Join("testdata", "match-epg", "*.m3u"))
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) == 0 {
		t.Fatal("no golden inputs")
	}
	for _, in := range inputs {
		name := strings.TrimSuffix(filepath.Base(in), ".m3u")
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(in)
			if err != nil {
				t.Fatal(err)
			}
			opts := Options{Tunnel: testTunnel, EPG: testEPG()}
			res, err := Import(data, opts)
			got := golden(res, err)

			path := strings.TrimSuffix(in, ".m3u") + ".golden"
			if *update {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, rerr := os.ReadFile(path)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if got != string(want) {
				t.Errorf("import %s:\ngot:\n%s\nwant:\n%s", in, got, want)
			}
			if err == nil {
				checkLoads(t, data, opts, res.TOML)
			}
		})
	}
}

// TestImportWithoutEPGUnchanged: Options.EPG nil leaves tvg_id as it is
// and writes no suggestion.
func TestImportWithoutEPGUnchanged(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "match-epg", "mixed.m3u"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := Import(data, Options{Tunnel: testTunnel})
	if err != nil {
		t.Fatal(err)
	}
	out := string(res.TOML)
	if !strings.Contains(out, `tvg_id = "skai-old"`) || strings.Contains(out, "# tvg_id") {
		t.Errorf("output changed without EPG:\n%s", out)
	}
}
