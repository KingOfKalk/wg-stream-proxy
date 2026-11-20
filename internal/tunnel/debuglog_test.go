// SPDX-License-Identifier: AGPL-3.0-or-later

package tunnel

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config/wgquick"
	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel/netstack"
	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel/tunneltest"
)

// lockedBuffer is a goroutine-safe log sink.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// testLogger logs at level into a buffer, without time, so the output
// holds only what the code under test wrote.
func testLogger(level slog.Level) (*slog.Logger, *lockedBuffer) {
	buf := &lockedBuffer{}
	h := slog.NewTextHandler(buf, &slog.HandlerOptions{Level: level, ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
		if len(groups) == 0 && a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}})
	return slog.New(h), buf
}

// testKey returns a test-only key derived from seed.
func testKey(seed string) [32]byte {
	return sha256.Sum256([]byte("wg-stream-proxy test-only key " + seed))
}

// publicKey returns the X25519 public key of the clamped private key priv.
func publicKey(t *testing.T, priv [32]byte) [32]byte {
	t.Helper()
	k, err := ecdh.X25519().NewPrivateKey(priv[:])
	if err != nil {
		t.Fatal(err)
	}
	var pub [32]byte
	copy(pub[:], k.PublicKey().Bytes())
	return pub
}

// keyForms returns the text forms of key a log line could leak: base64
// and hex, and the base64 fragments wireguard-go prints for a peer
// ("peer(AAAA…BBBB)").
func keyForms(key [32]byte) []string {
	b := base64.StdEncoding.EncodeToString(key[:])
	return []string{b, hex.EncodeToString(key[:]), b[0:4] + "…", "…" + b[39:43]}
}

// assertNoKeys fails t if out contains any form of keys; with fragments,
// also the bare four-character base64 fragments.
func assertNoKeys(t *testing.T, out string, fragments bool, keys map[string][32]byte) {
	t.Helper()
	for name, key := range keys {
		forms := keyForms(key)
		if fragments {
			b := base64.StdEncoding.EncodeToString(key[:])
			// The decimal form of a byte array, as %v prints it.
			forms = append(forms, b[0:4], b[39:43], fmt.Sprintf("[%d %d %d %d", key[0], key[1], key[2], key[3]))
		}
		for _, f := range forms {
			if strings.Contains(out, f) {
				t.Errorf("log contains %q of %s:\n%s", f, name, out)
			}
		}
	}
}

// TestWireGuardLogRedactsKeys feeds the device logger wireguard-go's own
// output (configuration, a real peer) and its log formats with that
// peer, and checks that no key or key fragment gets through.
func TestWireGuardLogRedactsKeys(t *testing.T) {
	priv := testKey("private")
	priv[0] &= 248
	priv[31] = (priv[31] & 127) | 64
	peerPriv := testKey("peer")
	peerPriv[0] &= 248
	peerPriv[31] = (peerPriv[31] & 127) | 64
	psk := testKey("psk")
	keys := map[string][32]byte{
		"private key":      priv,
		"public key":       publicKey(t, priv),
		"peer private key": peerPriv,
		"peer public key":  publicKey(t, peerPriv),
		"preshared key":    psk,
	}
	peerPub := keys["peer public key"]
	uapi := fmt.Sprintf("private_key=%s\npublic_key=%s\npreshared_key=%s\nendpoint=127.0.0.1:9\nallowed_ip=0.0.0.0/0\n",
		hex.EncodeToString(priv[:]), hex.EncodeToString(peerPub[:]), hex.EncodeToString(psk[:]))

	for _, level := range []slog.Level{slog.LevelDebug, slog.LevelInfo} {
		t.Run(level.String(), func(t *testing.T) {
			log, buf := testLogger(level)
			wl := wgLogger(log, "gr")
			tun, _, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("10.0.0.2")}, nil, 1420)
			if err != nil {
				t.Fatal(err)
			}
			dev := device.NewDevice(tun, conn.NewDefaultBind(), wl)
			defer dev.Close()
			if err := dev.IpcSet(uapi); err != nil {
				t.Fatalf("IpcSet: %v", err)
			}
			// A UAPI error quoting a key: wireguard-go logs it itself.
			if err := dev.IpcSet(hex.EncodeToString(priv[:]) + "\n"); err == nil {
				t.Fatal("IpcSet of an invalid line succeeded")
			}
			peer := dev.LookupPeer(device.NoisePublicKey(peerPub))
			if peer == nil {
				t.Fatal("peer not configured")
			}
			// Formats of wireguard-go's device package.
			wl.Verbosef("%v - Sending handshake initiation", peer)
			wl.Verbosef("%v - Received handshake response", peer)
			wl.Verbosef("%v - Receiving keepalive packet", peer)
			wl.Verbosef("%s - Handshake did not complete after %d seconds, retrying (try %d)", peer, 5, 2)
			wl.Errorf("%v - Failed to send handshake initiation: %v", peer, fmt.Errorf("send: %w", errNoRouteForTest))
			// Defense in depth: key text in places the adapter cannot type.
			b64 := base64.StdEncoding.EncodeToString(peerPub[:])
			wl.Verbosef("stray peer(" + b64[0:4] + "…" + b64[39:43] + ") text")
			wl.Verbosef("key %s in a message", b64)
			wl.Errorf("%v", fmt.Errorf("bad key %s", hex.EncodeToString(psk[:])))
			wl.Verbosef("raw %v", peerPub)
			wl.Verbosef("raw %x", psk[:])

			out := buf.String()
			assertNoKeys(t, out, true, keys)
			if level > slog.LevelDebug {
				if out != "" {
					t.Errorf("level %s: wireguard-go output logged:\n%s", level, out)
				}
				return
			}
			for _, want := range []string{
				`msg=wireguard tunnel=gr event="peer(gr) - Sending handshake initiation"`,
				`event="peer(gr) - Handshake did not complete after 5 seconds, retrying (try 2)"`,
				`msg="wireguard error" tunnel=gr event="peer(gr) - Failed to send handshake initiation: send: no route"`,
				`event="peer(gr) - UAPI: Created"`,
			} {
				if !strings.Contains(out, want) {
					t.Errorf("log lacks %s:\n%s", want, out)
				}
			}
			if strings.Contains(out, "failed to parse line") {
				t.Errorf("UAPI error logged:\n%s", out)
			}
			if strings.Contains(out, "Routine:") {
				t.Errorf("goroutine lines logged:\n%s", out)
			}
		})
	}
}

var errNoRouteForTest = fmt.Errorf("no route")

// TestWireGuardLogThroughTunnel runs a request through a real tunnel at
// debug level: wireguard-go's handshake events are logged under the
// tunnel name, its keys never.
func TestWireGuardLogThroughTunnel(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	log, buf := testLogger(slog.LevelDebug)
	m, err := New([]*wgquick.Tunnel{peer.ClientTunnel(t, "gr")}, Options{ConnectTimeout: testConnectTimeout, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	tun := mustTunnel(t, m, "gr")
	if _, err := get(t, tun, "http://"+tunneltest.ServerAddr4.String()+"/"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	keys := map[string][32]byte{}
	for line := range strings.SplitSeq(peer.ClientConfig(), "\n") {
		k, v, ok := strings.Cut(line, " = ")
		if !ok || !strings.HasSuffix(k, "Key") {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil || len(b) != 32 {
			t.Fatalf("%s: %v", k, err)
		}
		keys[k] = [32]byte(b)
	}
	keys["client PublicKey"] = publicKey(t, keys["PrivateKey"])
	assertNoKeys(t, out, false, keys)
	for _, want := range []string{
		`msg=wireguard tunnel=gr event="peer(gr) - Sending handshake initiation"`,
		`msg=wireguard tunnel=gr event="peer(gr) - Received handshake response"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %s:\n%s", want, out)
		}
	}
}

// TestStartTrace checks that a lookup or dial that waits for the start
// reports the wait, and one on a running tunnel does not.
func TestStartTrace(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	m, _ := newManager(t, peer.ClientTunnel(t, "gr"))
	tun := mustTunnel(t, m, "gr")
	var mu sync.Mutex
	var waits []time.Duration
	ctx := WithStartTrace(t.Context(), func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		waits = append(waits, d)
	})
	reported := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(waits)
	}

	if _, err := tun.LookupNetIP(ctx, tunneltest.ServerAddr4.String()); err != nil {
		t.Fatalf("cold lookup: %v", err)
	}
	if n := reported(); n != 1 {
		t.Fatalf("cold lookup reported %d waits, want 1", n)
	}
	c, err := tun.DialContext(ctx, "tcp4", tunneltest.ServerAddr4.String()+":80")
	if err != nil {
		t.Fatalf("warm dial: %v", err)
	}
	_ = c.Close()
	if _, err := tun.LookupNetIP(context.Background(), tunneltest.ServerAddr4.String()); err != nil {
		t.Fatalf("lookup without trace: %v", err)
	}
	if n := reported(); n != 1 {
		t.Errorf("calls on a running tunnel reported %d waits, want none after the first", n-1)
	}
}

// TestLifecycleLogDurations checks the durations of the start and stop
// lines, measured with the tunnel clock.
func TestLifecycleLogDurations(t *testing.T) {
	peer := tunneltest.Start(t, tunneltest.Options{})
	log, buf := testLogger(slog.LevelDebug)
	clk := newFakeClock()
	m, err := New([]*wgquick.Tunnel{peer.ClientTunnel(t, "gr")}, Options{
		ConnectTimeout: testConnectTimeout, IdleTimeout: testIdle, Logger: log, Clock: clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	tun := mustTunnel(t, m, "gr")

	end := tun.Begin()
	if _, err := get(t, tun, "http://"+tunneltest.ServerAddr4.String()+"/"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, `msg="tunnel started" tunnel=gr duration=0s handshake=0s`) {
		t.Errorf("start line lacks durations:\n%s", out)
	}
	clk.advance(time.Minute)
	done := make(chan error, 1)
	go func() { done <- drain(m, "gr", testDrain) }()
	clk.waitArmed(t, testDrain)
	clk.advance(2 * time.Second)
	end()
	if err := <-done; err != nil {
		t.Fatalf("Drain: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`msg="tunnel drained" tunnel=gr requests=1 waited=2s`,
		`msg="tunnel closed" tunnel=gr reason=cooldown uptime=1m2s`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %s:\n%s", want, out)
		}
	}
}
