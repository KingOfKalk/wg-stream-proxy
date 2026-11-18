// SPDX-License-Identifier: AGPL-3.0-or-later

package tunnel

import (
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel/tunneltest"
)

// trafficSize bounds the data each stress connection moves, so a
// connection never busy-loops on a tunnel that is gone; it takes
// seconds through the netstack, so it is still flowing at close.
const trafficSize = 256 << 10

// trafficHandler serves the stress traffic: /down sends trafficSize
// bytes and holds the connection open, /up reads the upload until the
// connection breaks.
var trafficHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/down":
		buf := []byte(strings.Repeat("d", 16<<10))
		for range trafficSize / len(buf) {
			if _, err := w.Write(buf); err != nil {
				return
			}
		}
		<-r.Context().Done()
	case "/up":
		// Hijacked: net/http would otherwise keep the connection
		// goroutine 500ms after the body broke (closeWriteAndWait), and
		// goleak would flag it.
		c, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = io.Copy(io.Discard, rw)
	}
})

// TestCloseWhileTrafficFlows is the regression test for #47: closing a
// netstack while gVisor still sends packets (TCP data, ACKs, window
// updates, retransmits and RSTs from TCP timers and processors, ICMP
// port unreachable for late UDP answers) must neither race nor panic
// with "send on closed channel". Stress it with -race -count=N.
func TestCloseWhileTrafficFlows(t *testing.T) {
	tests := []struct {
		name    string
		traffic func(t *testing.T, tun *Tunnel) (stop func())
		close   func(t *testing.T, s *closeSetup)
	}{
		{name: "late UDP answers, manager close", traffic: lateUDPAnswers, close: closeManager},
		{name: "TCP download, manager close", traffic: tcpTraffic("down"), close: closeManager},
		{name: "TCP upload, manager close", traffic: tcpTraffic("up"), close: closeManager},
		{name: "TCP download, idle close", traffic: tcpTraffic("down"), close: closeIdle},
		{name: "TCP upload, idle close", traffic: tcpTraffic("up"), close: closeIdle},
		{name: "TCP download, drain", traffic: tcpTraffic("down"), close: closeDrain},
		{name: "TCP download, peer close", traffic: tcpTraffic("down"), close: closePeer},
		{name: "TCP upload, peer close", traffic: tcpTraffic("up"), close: closePeer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newCloseSetup(t)
			stop := tt.traffic(t, s.tun)
			tt.close(t, s)
			stop()
			// Both sides close in any order; the other side ends with
			// traffic still in flight towards it.
			_ = s.m.Close()
			s.peer.Close()
		})
	}
}

// closeSetup is a started tunnel to a peer that serves trafficHandler.
type closeSetup struct {
	peer *tunneltest.Peer
	m    *Manager
	env  *testEnv
	tun  *Tunnel
}

func newCloseSetup(t *testing.T) *closeSetup {
	t.Helper()
	peer := tunneltest.Start(t, tunneltest.Options{Handler: trafficHandler})
	m, env := newManagerIdle(t, testIdle, peer.ClientTunnel(t, "gr"))
	s := &closeSetup{peer: peer, m: m, env: env, tun: mustTunnel(t, m, "gr")}
	if _, err := s.tun.LookupNetIP(t.Context(), tunneltest.ServerAddr4.String()); err != nil {
		t.Fatalf("start: %v", err)
	}
	return s
}

func closeManager(t *testing.T, s *closeSetup) {
	t.Helper()
	if err := s.m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// closeIdle closes the tunnel with its idle timer. The connections are
// not counted with Begin, like pooled connections of the route layer.
func closeIdle(t *testing.T, s *closeSetup) {
	t.Helper()
	s.env.clock.waitArmed(t, testIdle)
	s.env.clock.advance(testIdle)
	if s.tun.Running() {
		t.Fatal("tunnel still running after the idle timeout")
	}
}

func closeDrain(t *testing.T, s *closeSetup) {
	t.Helper()
	if err := s.m.Drain("gr", 0); err != nil {
		t.Fatalf("Drain: %v", err)
	}
}

func closePeer(t *testing.T, s *closeSetup) {
	t.Helper()
	s.peer.Close()
}

// tcpTraffic opens connections to the peer's HTTP server that download
// from /down or upload to /up. It returns once data flows on every
// connection; stop closes them and waits for their goroutines. The
// caller closes the tunnel or the peer in between.
func tcpTraffic(dir string) func(t *testing.T, tun *Tunnel) func() {
	return func(t *testing.T, tun *Tunnel) func() {
		t.Helper()
		const conns = 2
		var (
			wg      sync.WaitGroup
			flowing sync.WaitGroup
			cs      []net.Conn
		)
		for range conns {
			c, err := tun.DialContext(t.Context(), "tcp4", tunneltest.ServerAddr4.String()+":80")
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			cs = append(cs, c)
			flowing.Add(1)
			wg.Go(func() { pump(c, dir, flowing.Done) })
		}
		flowing.Wait()
		return func() {
			// After a peer close the connections would hang until
			// gVisor gives up retransmitting: close them here.
			for _, c := range cs {
				_ = c.Close()
			}
			wg.Wait()
		}
	}
}

// pump moves up to trafficSize bytes over c, then waits until the
// connection breaks. It calls flowing once the first data went through.
func pump(c net.Conn, dir string, flowing func()) {
	var once sync.Once
	defer once.Do(flowing)
	buf := make([]byte, 16<<10)
	switch dir {
	case "down":
		if _, err := io.WriteString(c, "GET /down HTTP/1.1\r\nHost: upstream.test\r\n\r\n"); err != nil {
			return
		}
		for {
			n, err := c.Read(buf)
			if n > 0 {
				once.Do(flowing)
			}
			if err != nil {
				return
			}
		}
	case "up":
		if _, err := io.WriteString(c, "POST /up HTTP/1.1\r\nHost: upstream.test\r\nTransfer-Encoding: chunked\r\n\r\n"); err != nil {
			return
		}
		chunk := []byte("4000\r\n" + strings.Repeat("u", 0x4000) + "\r\n")
		for range trafficSize / 0x4000 {
			if _, err := c.Write(chunk); err != nil {
				return
			}
			once.Do(flowing)
		}
		_, _ = io.Copy(io.Discard, c)
	}
}

// lateUDPAnswers sends DNS queries whose answers arrive at a closed
// socket while the tunnel closes: the netstack answers them with ICMP
// port unreachable.
func lateUDPAnswers(t *testing.T, tun *Tunnel) func() {
	t.Helper()
	msg, err := buildQuery(newQueryID(), dnsmessage.Question{
		Name: dnsmessage.MustNewName("late.test."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := tun.run.Load()
	if r == nil {
		t.Fatal("tunnel not running")
	}
	c, err := r.net.DialUDPAddrPort(netip.AddrPort{}, netip.AddrPortFrom(tunneltest.ServerAddr4, 53))
	if err != nil {
		t.Fatal(err)
	}
	for range 200 {
		if _, err := c.Write(msg[2:]); err != nil {
			t.Fatal(err)
		}
	}
	_ = c.Close()
	return func() {}
}
