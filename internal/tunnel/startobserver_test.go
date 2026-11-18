// SPDX-License-Identifier: AGPL-3.0-or-later

package tunnel

import (
	"errors"
	"io"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config/wgquick"
	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel/tunneltest"
)

// startRecorder records the calls of Options.OnStart.
type startRecorder struct {
	mu    sync.Mutex
	names []string
	took  []time.Duration
}

func (r *startRecorder) record(name string, took time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.names = append(r.names, name)
	r.took = append(r.took, took)
}

func (r *startRecorder) snapshot() ([]string, []time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.names), slices.Clone(r.took)
}

// TestOnStart checks that OnStart is called once per successful start, with
// the tunnel name and the start duration measured with the tunnel clock,
// not for a failed start and not for the requests that wait for a start or
// use the running tunnel.
func TestOnStart(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})})
	peer.SetReachable(false)
	rec := &startRecorder{}
	clk := newFakeClock()
	m, err := New([]*wgquick.Tunnel{peer.ClientTunnel(t, "gr")}, Options{
		ConnectTimeout: testConnectTimeout, Clock: clk, OnStart: rec.record,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	tun := mustTunnel(t, m, "gr")
	target := "http://" + tunneltest.ServerAddr4.String() + "/"

	// A failed start (no handshake within the connect timeout) is not
	// observed.
	done := make(chan error, 1)
	go func() {
		_, err := get(t, tun, target)
		done <- err
	}()
	clk.waitTimers(1)
	peer.WaitPackets(t, 1)
	clk.advance(testConnectTimeout)
	if err := <-done; !errors.Is(err, ErrUnavailable) {
		t.Fatalf("get without handshake: %v, want ErrUnavailable", err)
	}
	if names, _ := rec.snapshot(); len(names) != 0 {
		t.Fatalf("OnStart called for a failed start: %v", names)
	}

	// Concurrent requests share one start: one call.
	peer.SetReachable(true)
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := get(t, tun, target); err != nil {
				t.Errorf("get: %v", err)
			}
		}()
	}
	wg.Wait()
	// The running tunnel serves another request without a start.
	if _, err := get(t, tun, target); err != nil {
		t.Fatalf("get on running tunnel: %v", err)
	}
	names, took := rec.snapshot()
	if !slices.Equal(names, []string{"gr"}) {
		t.Fatalf("OnStart calls = %v, want one for gr", names)
	}
	// The fake clock did not move during the start.
	if took[0] != 0 {
		t.Errorf("start duration = %s, want 0s on the fake clock", took[0])
	}
}
