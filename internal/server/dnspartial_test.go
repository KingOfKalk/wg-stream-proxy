// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"net/http"
	"net/netip"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel/tunneltest"
)

// TestRequestWithUnansweredAAAA: on a dual-stack tunnel whose DNS server
// drops AAAA queries, a request succeeds soon after the A answer (the
// resolution delay, spec 2.1), not at connect_timeout (#181).
func TestRequestWithUnansweredAAAA(t *testing.T) {
	up := newUpstream()
	up.add("/seg.ts", file{ctype: "video/mp2t", body: "segment"})
	peer := tunneltest.Start(t, tunneltest.Options{
		Handler: up,
		ClientAddresses: []netip.Prefix{
			netip.PrefixFrom(tunneltest.ClientAddr4, 32), netip.PrefixFrom(tunneltest.ClientAddr6, 128),
		},
	})
	peer.SetHost("upstream.test", tunneltest.ServerAddr4)
	peer.DropDNSType(dnsmessage.TypeAAAA, true)
	dir := writeConfig(t, map[string]string{
		"config.toml":     "[upstream]\nconnect_timeout = \"5s\"\n",
		"tunnels/gr.conf": peer.ClientConfig(),
		"channels.d/gr.toml": `
[[channel]]
name = "Seg"
url = "http://upstream.test/seg.ts"
`,
	})
	e := startRun(t, dir, 0)

	start := time.Now()
	resp, body := do(t, e.client, http.MethodGet, e.url+"/c/seg", nil)
	if resp.StatusCode != http.StatusOK || body != "segment" {
		t.Fatalf("GET /c/seg: status %d, body %q", resp.StatusCode, body)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("request took %v, want well within connect_timeout (5s)", took)
	}
	if peer.DroppedDNS() == 0 {
		t.Error("no AAAA query was dropped: the test did not exercise the unanswered type")
	}
}
