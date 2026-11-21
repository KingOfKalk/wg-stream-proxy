// SPDX-License-Identifier: AGPL-3.0-or-later

package metrics

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// wspRef finds the metric names of this package in a dashboard.
var wspRef = regexp.MustCompile(`\bwsp_[A-Za-z0-9_]+`)

// TestDashboardsUseExistingMetrics keeps deploy/grafana/*.json in sync
// with the metrics: every wsp_* name in a dashboard is a metric family of
// a registry that has seen every kind of observation, or a _bucket, _sum
// or _count series of one.
func TestDashboardsUseExistingMetrics(t *testing.T) {
	m := New(Options{EPG: func() []EPGSource { return []EPGSource{{Name: "gr"}} }, Tunnels: func() []Tunnel {
		return []Tunnel{fakeTunnel{name: "gr", up: true, hs: time.Unix(1700000000, 0)}}
	}})
	m.ObserveTunnelStart("gr", time.Second)
	m.ObserveUpstreamPhase("direct", PhaseFirstByte, time.Second)
	r := m.Begin("ch", KindPlaylist)
	r.Response(200)
	r.FetchError("connect")
	m.ErrorClip("channel-not-available")
	mfs, err := m.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	families := map[string]bool{}
	for _, mf := range mfs {
		families[mf.GetName()] = true
	}
	files, err := filepath.Glob(filepath.Join("..", "..", "deploy", "grafana", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no dashboards in deploy/grafana (err %v)", err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range wspRef.FindAllString(string(data), -1) {
			base := name
			for _, suffix := range []string{"_bucket", "_sum", "_count"} {
				if b, ok := strings.CutSuffix(name, suffix); ok && !families[name] {
					base = b
				}
			}
			if !families[base] {
				t.Errorf("%s: unknown metric %s", filepath.Base(f), name)
			}
		}
	}
}
