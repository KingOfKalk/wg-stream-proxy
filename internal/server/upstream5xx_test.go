// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// TestUpstreamErrorStatusLogged: every upstream 5xx is answered with the
// proxy's own 502 and writes the "upstream error status" warn line, also
// an upstream 502, whose status does not change (spec 3.4); an upstream
// 4xx without a clip is relayed and writes none.
func TestUpstreamErrorStatusLogged(t *testing.T) {
	const marker = "upstream-error-body"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/s/"))
		http.Error(w, marker, code)
	}))
	t.Cleanup(upstream.Close)

	// Channels listed without .m3u8: no error clips.
	dir := writeConfig(t, map[string]string{
		"config.toml": allowLoopback,
		"channels.d/de.toml": `
tunnel = "direct"
[[channel]]
name = "S502"
url = "` + upstream.URL + `/s/502"
[[channel]]
name = "S503"
url = "` + upstream.URL + `/s/503"
[[channel]]
name = "S404"
url = "` + upstream.URL + `/s/404"
`,
	})
	e := startRun(t, dir, 0)
	for i, tt := range []struct {
		id     string
		status int
		warn   bool
		body   bool // the upstream body reaches the client
	}{
		{"s502", http.StatusBadGateway, true, false},
		{"s503", http.StatusBadGateway, true, false},
		{"s404", http.StatusNotFound, false, true},
	} {
		resp, body := do(t, e.client, http.MethodGet, e.url+"/c/"+tt.id, nil)
		if resp.StatusCode != tt.status {
			t.Errorf("%s: status %d, want %d", tt.id, resp.StatusCode, tt.status)
		}
		if got := strings.Contains(body, marker); got != tt.body {
			t.Errorf("%s: upstream body relayed %v, want %v: %q", tt.id, got, tt.body, body)
		}
		e.logs.waitFor(t, " msg=request ", i+1)
		upstreamStatus := strings.TrimPrefix(tt.id, "s")
		want := `msg="upstream error status" channel=` + tt.id + ` route=direct`
		line := ""
		for l := range strings.SplitSeq(e.logs.String(), "\n") {
			if strings.Contains(l, want) {
				line = l
			}
		}
		switch {
		case tt.warn && line == "":
			t.Errorf("%s: no warn line %q:\n%s", tt.id, want, e.logs.String())
		case tt.warn && !strings.Contains(line, "upstream_status="+upstreamStatus+" status=502"):
			t.Errorf("%s: warn line %q lacks upstream_status=%s status=502", tt.id, line, upstreamStatus)
		case !tt.warn && line != "":
			t.Errorf("%s: unexpected warn line %q", tt.id, line)
		}
	}
}
