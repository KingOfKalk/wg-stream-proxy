// SPDX-License-Identifier: AGPL-3.0-or-later

package netstack

import (
	"bytes"
	"errors"
	"net/netip"
	"os"
	"runtime"
	"testing"
	"time"

	"go.uber.org/goleak"
	"golang.zx2c4.com/wireguard/tun"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

var (
	localAddr  = netip.MustParseAddr("192.0.2.2")
	remoteAddr = netip.AddrPortFrom(netip.MustParseAddr("192.0.2.1"), 53)
)

// TestCloseWithBlockedCalls is the regression test for #47: Close while
// another goroutine is inside WriteNotify (gVisor sending a packet that
// nobody reads anymore) or Read must neither panic with "send on closed
// channel" nor leave the goroutine blocked.
func TestCloseWithBlockedCalls(t *testing.T) {
	tests := []struct {
		name string
		// call blocks in the function named blockedIn until Close.
		call      func(dev tun.Device, tnet *Net) error
		blockedIn string
		wantErr   error
	}{
		{
			name: "gVisor writes a packet nobody reads",
			call: func(_ tun.Device, tnet *Net) error {
				c, err := tnet.DialUDPAddrPort(netip.AddrPort{}, remoteAddr)
				if err != nil {
					return err
				}
				defer func() { _ = c.Close() }()
				// gVisor hands the packet to WriteNotify in this goroutine.
				_, err = c.Write([]byte("late"))
				return err
			},
			blockedIn: "(*netTun).WriteNotify",
		},
		{
			name: "device reads while nothing is sent",
			call: func(dev tun.Device, _ *Net) error {
				_, err := dev.Read([][]byte{make([]byte, 1500)}, []int{0}, 0)
				return err
			},
			blockedIn: "(*netTun).Read",
			wantErr:   os.ErrClosed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dev, tnet, err := CreateNetTUN([]netip.Addr{localAddr}, nil, 1420)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- tt.call(dev, tnet) }()
			waitBlocked(t, tt.blockedIn)
			if err := dev.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("err = %v, want %v", err, tt.wantErr)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("still blocked in %s 10s after Close", tt.blockedIn)
			}
		})
	}
}

// TestPacketsFlowBeforeClose checks the normal path the fork must keep:
// a packet gVisor sends reaches Read.
func TestPacketsFlowBeforeClose(t *testing.T) {
	dev, tnet, err := CreateNetTUN([]netip.Addr{localAddr}, nil, 1420)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dev.Close() }()
	c, err := tnet.DialUDPAddrPort(netip.AddrPort{}, remoteAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	sent := make(chan error, 1)
	go func() {
		_, err := c.Write([]byte("payload"))
		sent <- err
	}()
	buf := make([]byte, 1500)
	sizes := []int{0}
	n, err := dev.Read([][]byte{buf}, sizes, 0)
	if err != nil || n != 1 {
		t.Fatalf("Read = %d, %v; want 1 packet", n, err)
	}
	if err := <-sent; err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !bytes.HasSuffix(buf[:sizes[0]], []byte("payload")) {
		t.Errorf("packet %x does not carry the payload", buf[:sizes[0]])
	}
}

// waitBlocked waits until a goroutine is inside fn, the netTun method
// that blocks, so Close is guaranteed to hit it there. It fails tb after
// 10s.
func waitBlocked(tb testing.TB, fn string) {
	tb.Helper()
	giveUp := time.Now().Add(10 * time.Second)
	buf := make([]byte, 1<<20)
	for time.Now().Before(giveUp) {
		n := runtime.Stack(buf, true)
		if bytes.Contains(buf[:n], []byte("netstack."+fn+"(")) {
			return
		}
		runtime.Gosched()
	}
	tb.Fatalf("no goroutine blocked in %s", fn)
}
