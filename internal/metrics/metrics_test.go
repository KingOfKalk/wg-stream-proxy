// SPDX-License-Identifier: AGPL-3.0-or-later

package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// fakeTunnel is a Tunnel with fixed state.
type fakeTunnel struct {
	name string
	up   bool
	hs   time.Time
}

func (f fakeTunnel) Name() string             { return f.name }
func (f fakeTunnel) Up() bool                 { return f.up }
func (f fakeTunnel) LastHandshake() time.Time { return f.hs }

func TestTunnelCollector(t *testing.T) {
	hs := time.Unix(1700000000, 500_000_000)
	tests := []struct {
		name    string
		tunnels []Tunnel
		want    string
	}{
		{
			name: "none",
			want: "",
		},
		{
			name: "never started, up and stale",
			tunnels: []Tunnel{
				fakeTunnel{name: "de"},
				fakeTunnel{name: "gr", up: true, hs: hs},
				fakeTunnel{name: "uk", hs: hs},
			},
			want: `
# HELP wsp_tunnel_last_handshake_timestamp_seconds Time of the latest completed WireGuard handshake of the running tunnel, 0 if none.
# TYPE wsp_tunnel_last_handshake_timestamp_seconds gauge
wsp_tunnel_last_handshake_timestamp_seconds{tunnel="de"} 0
wsp_tunnel_last_handshake_timestamp_seconds{tunnel="gr"} 1.7000000005e+09
wsp_tunnel_last_handshake_timestamp_seconds{tunnel="uk"} 1.7000000005e+09
# HELP wsp_tunnel_up 1 while the tunnel runs and its latest handshake is younger than 180s.
# TYPE wsp_tunnel_up gauge
wsp_tunnel_up{tunnel="de"} 0
wsp_tunnel_up{tunnel="gr"} 1
wsp_tunnel_up{tunnel="uk"} 0
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			m := New(Options{Tunnels: func() []Tunnel { calls++; return tt.tunnels }})
			if calls != 0 {
				t.Fatalf("tunnels read %d times before a scrape", calls)
			}
			err := testutil.GatherAndCompare(m.Gatherer(), strings.NewReader(tt.want),
				"wsp_tunnel_up", "wsp_tunnel_last_handshake_timestamp_seconds")
			if err != nil {
				t.Error(err)
			}
			if calls != 1 {
				t.Errorf("tunnels read %d times per scrape, want 1", calls)
			}
		})
	}
}

// TestTunnelsAtScrapeTime: the collector reports the state at each
// scrape, so a changed tunnel set (reload) shows without re-registering.
func TestTunnelsAtScrapeTime(t *testing.T) {
	tunnels := []Tunnel{fakeTunnel{name: "gr"}}
	m := New(Options{Tunnels: func() []Tunnel { return tunnels }})
	const head = "# HELP wsp_tunnel_up 1 while the tunnel runs and its latest handshake is younger than 180s.\n# TYPE wsp_tunnel_up gauge\n"
	if err := testutil.GatherAndCompare(m.Gatherer(), strings.NewReader(head+`wsp_tunnel_up{tunnel="gr"} 0`+"\n"), "wsp_tunnel_up"); err != nil {
		t.Error(err)
	}
	tunnels = []Tunnel{fakeTunnel{name: "de", up: true, hs: time.Unix(1, 0)}}
	if err := testutil.GatherAndCompare(m.Gatherer(), strings.NewReader(head+`wsp_tunnel_up{tunnel="de"} 1`+"\n"), "wsp_tunnel_up"); err != nil {
		t.Error(err)
	}
}

func TestRequest(t *testing.T) {
	m := New(Options{})
	a := m.Begin("news", KindRaw)
	b := m.Begin("news", KindSegment)
	c := m.Begin("radio", KindRaw)
	a.SetKind(KindPlaylist)
	a.SetKind(KindPlaylist) // no change
	if a.Kind() != KindPlaylist {
		t.Errorf("Kind() = %q, want playlist", a.Kind())
	}
	a.Response(206)
	a.Response(200)
	b.Response(404)
	c.FetchError("connect")
	b.Error("timeout")
	c.End()
	c.End() // only the first End counts

	const want = `
# HELP wsp_active_requests Requests in flight under /c/ and /r/, open raw streams included.
# TYPE wsp_active_requests gauge
wsp_active_requests{channel="news",kind="playlist"} 1
wsp_active_requests{channel="news",kind="raw"} 0
wsp_active_requests{channel="news",kind="segment"} 1
wsp_active_requests{channel="radio",kind="raw"} 0
# HELP wsp_upstream_errors_total Upstream request failures by reason.
# TYPE wsp_upstream_errors_total counter
wsp_upstream_errors_total{channel="news",reason="timeout"} 1
wsp_upstream_errors_total{channel="radio",reason="connect"} 1
# HELP wsp_upstream_requests_total Upstream requests by status code, or error if no response came.
# TYPE wsp_upstream_requests_total counter
wsp_upstream_requests_total{channel="news",code="200",kind="playlist"} 1
wsp_upstream_requests_total{channel="news",code="206",kind="playlist"} 1
wsp_upstream_requests_total{channel="news",code="404",kind="segment"} 1
wsp_upstream_requests_total{channel="radio",code="error",kind="raw"} 1
`
	if err := testutil.GatherAndCompare(m.Gatherer(), strings.NewReader(want),
		"wsp_active_requests", "wsp_upstream_requests_total", "wsp_upstream_errors_total"); err != nil {
		t.Error(err)
	}
	a.End()
	b.End()
	for _, k := range []Kind{KindPlaylist, KindSegment, KindRaw} {
		if v := testutil.ToFloat64(m.active.WithLabelValues("news", string(k))); v != 0 {
			t.Errorf("active_requests{news,%s} = %v after End, want 0", k, v)
		}
	}
}

func TestHandler(t *testing.T) {
	m := New(Options{Tunnels: func() []Tunnel { return []Tunnel{fakeTunnel{name: "gr"}} }})
	m.Begin("news", KindPlaylist).Response(200)
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	want := []string{
		`wsp_upstream_requests_total{channel="news",code="200",kind="playlist"} 1`,
		`wsp_tunnel_up{tunnel="gr"} 0`,
		"go_goroutines ",
	}
	if runtime.GOOS == "linux" {
		want = append(want, "process_start_time_seconds ")
	}
	for _, w := range want {
		if !strings.Contains(string(body), w) {
			t.Errorf("metrics lack %q:\n%s", w, body)
		}
	}
}
