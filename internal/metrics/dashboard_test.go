// SPDX-License-Identifier: AGPL-3.0-or-later

package metrics

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// dashboardDatasource is the import input every panel and query of the
// shipped dashboards refers to; no fixed datasource UID of a real instance.
const dashboardDatasource = "${DS_PROMETHEUS}"

// metricRef finds the metric names this package or the Go client exposes.
var metricRef = regexp.MustCompile(`\b(?:wsp|go|process)_[A-Za-z0-9_:]+`)

// registryTypes returns the metric families of a registry that has seen
// every kind of observation, as name -> type ("COUNTER", "GAUGE",
// "HISTOGRAM", "SUMMARY").
func registryTypes(t *testing.T) map[string]string {
	t.Helper()
	m := New(Options{Tunnels: func() []Tunnel {
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
		t.Fatalf("gather: %v", err)
	}
	types := make(map[string]string, len(mfs))
	for _, mf := range mfs {
		types[mf.GetName()] = mf.GetType().String()
	}
	return types
}

// resolve reports whether a metric name used in a query exists in types:
// a gauge or counter by its name, a histogram by _bucket, _sum or _count,
// a summary by _sum or _count.
func resolve(name string, types map[string]string) bool {
	if typ, ok := types[name]; ok {
		return typ == "COUNTER" || typ == "GAUGE"
	}
	for suffix, kinds := range map[string][]string{
		"_bucket": {"HISTOGRAM"},
		"_sum":    {"HISTOGRAM", "SUMMARY"},
		"_count":  {"HISTOGRAM", "SUMMARY"},
	} {
		base, ok := strings.CutSuffix(name, suffix)
		if !ok {
			continue
		}
		for _, k := range kinds {
			if types[base] == k {
				return true
			}
		}
	}
	return false
}

// checkDashboard checks that data is a JSON dashboard whose queries use
// only metrics in types and whose datasources are the import input.
func checkDashboard(data []byte, types map[string]string) error {
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	var problems []string
	exprs := 0
	var walk func(v any, key string)
	walk = func(v any, key string) {
		switch v := v.(type) {
		case map[string]any:
			// The built-in annotation datasource has type "grafana".
			if uid, _ := v["uid"].(string); key == "datasource" && v["type"] != "grafana" && uid != dashboardDatasource {
				problems = append(problems, fmt.Sprintf("datasource uid %q, want %q", uid, dashboardDatasource))
			}
			for k, e := range v {
				walk(e, k)
			}
		case []any:
			for _, e := range v {
				walk(e, key)
			}
		case string:
			if key != "expr" && key != "query" && key != "definition" {
				return
			}
			if key == "expr" {
				exprs++
			}
			for _, name := range metricRef.FindAllString(v, -1) {
				if strings.HasPrefix(name, "process_") && runtime.GOOS != "linux" {
					continue // the process collector reports nothing elsewhere
				}
				if !resolve(name, types) {
					problems = append(problems, "unknown metric "+name)
				}
			}
		}
	}
	walk(doc, "")
	if exprs == 0 {
		problems = append(problems, "no panel queries")
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("%s", strings.Join(problems, "; "))
}

func TestCheckDashboard(t *testing.T) {
	types := map[string]string{
		"wsp_tunnel_up":               "GAUGE",
		"wsp_upstream_requests_total": "COUNTER",
		"wsp_tunnel_start_seconds":    "HISTOGRAM",
		"go_gc_duration_seconds":      "SUMMARY",
	}
	ds := `{"type":"prometheus","uid":"${DS_PROMETHEUS}"}`
	tests := []struct {
		name    string
		doc     string
		wantErr string
	}{
		{
			name: "valid",
			doc: `{"panels":[{"datasource":` + ds + `,"targets":[{"datasource":` + ds + `,
				"expr":"sum(wsp_tunnel_up) / sum(rate(wsp_upstream_requests_total[1m]))"}]}]}`,
		},
		{
			name: "histogram suffixes and summary",
			doc:  `{"panels":[{"targets":[{"expr":"histogram_quantile(0.5, rate(wsp_tunnel_start_seconds_bucket[1h])) + rate(wsp_tunnel_start_seconds_sum[1h]) / rate(wsp_tunnel_start_seconds_count[1h]) + rate(go_gc_duration_seconds_sum[1m])"}]}]}`,
		},
		{
			name: "variable query",
			doc:  `{"templating":{"list":[{"query":"label_values(wsp_tunnel_up, tunnel)"}]},"panels":[{"targets":[{"expr":"wsp_tunnel_up"}]}]}`,
		},
		{
			name:    "invalid JSON",
			doc:     `{"panels":`,
			wantErr: "invalid JSON",
		},
		{
			name:    "unknown metric",
			doc:     `{"panels":[{"targets":[{"expr":"wsp_tunnel_dwn"}]}]}`,
			wantErr: "unknown metric wsp_tunnel_dwn",
		},
		{
			name:    "bucket of a gauge",
			doc:     `{"panels":[{"targets":[{"expr":"wsp_tunnel_up_bucket"}]}]}`,
			wantErr: "unknown metric wsp_tunnel_up_bucket",
		},
		{
			name:    "bare histogram name has no series",
			doc:     `{"panels":[{"targets":[{"expr":"wsp_tunnel_start_seconds"}]}]}`,
			wantErr: "unknown metric wsp_tunnel_start_seconds",
		},
		{
			name:    "unknown metric in a variable",
			doc:     `{"templating":{"list":[{"definition":"label_values(wsp_nope, x)"}]},"panels":[{"targets":[{"expr":"wsp_tunnel_up"}]}]}`,
			wantErr: "unknown metric wsp_nope",
		},
		{
			name: "built-in annotation datasource",
			doc:  `{"annotations":{"list":[{"datasource":{"type":"grafana","uid":"-- Grafana --"}}]},"panels":[{"targets":[{"expr":"wsp_tunnel_up"}]}]}`,
		},
		{
			name:    "fixed datasource",
			doc:     `{"panels":[{"datasource":{"type":"prometheus","uid":"abc123"},"targets":[{"expr":"wsp_tunnel_up"}]}]}`,
			wantErr: `datasource uid "abc123"`,
		},
		{
			name:    "no queries",
			doc:     `{"panels":[]}`,
			wantErr: "no panel queries",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkDashboard([]byte(tt.doc), types)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestDashboardsMatchRegistry keeps deploy/grafana/*.json in sync with the
// metrics: every wsp_, go_ and process_ name in a query exists.
func TestDashboardsMatchRegistry(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "deploy", "grafana", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no dashboards in deploy/grafana")
	}
	types := registryTypes(t)
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if err := checkDashboard(data, types); err != nil {
				t.Fatal(err)
			}
		})
	}
}
