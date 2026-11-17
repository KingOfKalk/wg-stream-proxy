// SPDX-License-Identifier: AGPL-3.0-or-later

package importer

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzImport checks that Import never panics and that every successful
// import loads with internal/config and yields the parsed channels.
func FuzzImport(f *testing.F) {
	inputs, err := filepath.Glob(filepath.Join("testdata", "*.m3u"))
	if err != nil {
		f.Fatal(err)
	}
	for _, in := range inputs {
		b, err := os.ReadFile(in)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	for _, s := range []string{
		"#EXTINF:-1,A\nhttps://a.example.com/|\n",
		"#EXTINF:-1 tvg-id=\"\x00\",\"\nhttp://[::1]:80/\n",
		"#EXTINF:-1,\u2028\nhttp://a/%zz\n#EXTINF:-1,-\nhttp://a/\n",
		"#X:\"=\\\n#EXTINF:,a\rhttps://a.example.com/x|A=%ff&B=%\n",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		opts := Options{Tunnel: testTunnel}
		res, err := Import(data, opts)
		if err != nil {
			if res.TOML != nil {
				t.Fatalf("output returned with error %v", err)
			}
			return
		}
		checkLoads(t, data, opts, res.TOML)
	})
}
