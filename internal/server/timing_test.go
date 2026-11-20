// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/metrics"
	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel/tunneltest"
)

// histogramCounts returns the sample count of every series of the
// histogram name in the exposition body of /metrics, keyed by its labels
// without braces, e.g. `phase="dns",route="gr"`.
func histogramCounts(body, name string) map[string]int {
	out := map[string]int{}
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `_count\{([^}]*)\} (\d+)$`)
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		n, _ := strconv.Atoi(m[2])
		out[m[1]] = n
	}
	return out
}

// eventually polls cond until it holds. The finished-request hook runs
// after the response is sent, so a client can be ahead of it. It fails tb
// after 10s.
func eventually(tb testing.TB, what string, cond func() bool) {
	tb.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			tb.Fatalf("%s: not true after 10s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestObservePhases: a phase is observed only if it happened; a fast phase
// (0s) is observed, a missing one is not.
func TestObservePhases(t *testing.T) {
	tests := []struct {
		name   string
		phases route.Phases
		want   map[string]int
	}{
		{"nothing happened", route.Phases{}, map[string]int{}},
		{
			"cold https",
			route.Phases{Lookups: 1, ConnectAttempts: 1, TLSHandshakes: 1, GotFirstByte: true, FirstByte: time.Second},
			map[string]int{`phase="dns",route="gr"`: 1, `phase="connect",route="gr"`: 1, `phase="tls",route="gr"`: 1, `phase="first_byte",route="gr"`: 1},
		},
		{
			"pooled connection",
			route.Phases{Reused: true, GotFirstByte: true, FirstByte: time.Millisecond},
			map[string]int{`phase="first_byte",route="gr"`: 1},
		},
		{
			"ip literal, plain http",
			route.Phases{ConnectAttempts: 1, GotFirstByte: true},
			map[string]int{`phase="connect",route="gr"`: 1, `phase="first_byte",route="gr"`: 1},
		},
		{
			"connect failed",
			route.Phases{Lookups: 1, ConnectAttempts: 2},
			map[string]int{`phase="dns",route="gr"`: 1, `phase="connect",route="gr"`: 1},
		},
		{
			"tls failed",
			route.Phases{ConnectAttempts: 1, TLSHandshakes: 1},
			map[string]int{`phase="connect",route="gr"`: 1, `phase="tls",route="gr"`: 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := metrics.New(metrics.Options{})
			s := &Server{metrics: m, log: slog.New(slog.DiscardHandler)}
			s.requestFinished(finishedRequest{Route: "gr", Phases: tt.phases})
			rec := httptest.NewRecorder()
			m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
			got := histogramCounts(rec.Body.String(), "wsp_upstream_phase_seconds")
			if len(got) != len(tt.want) {
				t.Errorf("series = %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("%s = %d, want %d (all: %v)", k, got[k], v, got)
				}
			}
		})
	}
}

// TestPhaseMetricsDirect: requests through the direct route observe the
// phases that happened, per route and never per channel. The fixture is an
// IP literal over plain HTTP: no lookup, no TLS.
func TestPhaseMetricsDirect(t *testing.T) {
	e := newDirectEnv(t, allowLoopback, channelsTOML)
	e.up.add("/radio", file{ctype: "audio/mpeg", body: "radio"})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().String()
	_ = ln.Close()

	if resp, _ := e.get(t, "/c/radio", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /c/radio: status %d", resp.StatusCode)
	}
	// Nothing listens there: the connect fails, no response comes.
	if resp, _ := e.get(t, e.signer.Path("news", "http://"+closed+"/x.ts"), nil); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("GET closed port: status %d, want 502", resp.StatusCode)
	}
	e.client.CloseIdleConnections()
	e.proxy.Close() // waits for the handlers, so the hook has run

	rec := httptest.NewRecorder()
	e.metrics.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	got := histogramCounts(rec.Body.String(), "wsp_upstream_phase_seconds")
	want := map[string]int{`phase="connect",route="direct"`: 2, `phase="first_byte",route="direct"`: 1}
	if len(got) != len(want) || got[`phase="connect",route="direct"`] != 2 || got[`phase="first_byte",route="direct"`] != 1 {
		t.Errorf("phase series = %v, want %v", got, want)
	}
	if n := histogramCounts(rec.Body.String(), "wsp_tunnel_start_seconds"); len(n) != 0 {
		t.Errorf("tunnel start series without a tunnel: %v", n)
	}
}

// TestPhaseMetricsTunnel: a cold request through a tunnel observes one
// tunnel start and the phases under the tunnel's name, a second one no
// further start; at info level, where no debug line is written.
func TestPhaseMetricsTunnel(t *testing.T) {
	up := newUpstream()
	up.add("/seg.ts", file{ctype: "video/mp2t", body: "segment"})
	peer := tunneltest.Start(t, tunneltest.Options{Handler: up})
	peer.SetHost("upstream.test", tunneltest.ServerAddr4)
	dir := writeConfig(t, map[string]string{
		"config.toml":     "",
		"tunnels/gr.conf": peer.ClientConfig(),
		"channels.d/gr.toml": `
[[channel]]
name = "Seg"
url = "http://upstream.test/seg.ts"
`,
	})
	logs := &syncBuffer{}
	e := startRun(t, dir, 0, func(o *RunOptions) {
		o.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	})
	scrape := func() string {
		_, body := do(t, e.client, http.MethodGet, e.url+"/metrics", nil)
		return body
	}
	phase := func(p string) func() bool {
		return func() bool {
			return histogramCounts(scrape(), "wsp_upstream_phase_seconds")[`phase="`+p+`",route="gr"`] >= 2
		}
	}

	for range 2 {
		if resp, body := do(t, e.client, http.MethodGet, e.url+"/c/seg", nil); resp.StatusCode != http.StatusOK || body != "segment" {
			t.Fatalf("GET /c/seg: status %d, body %q", resp.StatusCode, body)
		}
	}
	eventually(t, "two first_byte observations", phase("first_byte"))
	body := scrape()
	got := histogramCounts(body, "wsp_upstream_phase_seconds")
	// The second request reuses the pooled connection of the first (ICY
	// transport, #131): one lookup and one connect.
	want := map[string]int{`phase="dns",route="gr"`: 1, `phase="connect",route="gr"`: 1, `phase="first_byte",route="gr"`: 2}
	if len(got) != len(want) {
		t.Errorf("phase series = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %d, want %d", k, got[k], v)
		}
	}
	if got := histogramCounts(body, "wsp_tunnel_start_seconds"); len(got) != 1 || got[`tunnel="gr"`] != 1 {
		t.Errorf("tunnel start series = %v, want one start of gr", got)
	}
	if strings.Contains(logs.String(), "msg=request") {
		t.Errorf("request line above debug level:\n%s", logs.String())
	}
	if strings.Contains(body, "channel=\"seg\"") && strings.Contains(body, "wsp_upstream_phase_seconds{channel") {
		t.Error("phase histogram carries a channel label")
	}
}
