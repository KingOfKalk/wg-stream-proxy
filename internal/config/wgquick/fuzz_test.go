// SPDX-License-Identifier: AGPL-3.0-or-later

package wgquick

import (
	"encoding/base64"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// base64Run matches anything long enough to be (part of) a key.
var base64Run = regexp.MustCompile(`[A-Za-z0-9+/]{25,}`)

func FuzzParse(f *testing.F) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.conf"))
	if err != nil {
		f.Fatal(err)
	}
	for _, name := range files {
		b, err := os.ReadFile(name)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(string(b))
	}
	f.Add(conf(baseIface+"\nListenPort = 1\nTable = off\nSaveConfig = true\nMTU = 1280", basePeer+"\nPersistentKeepalive = off"))
	f.Add(conf("PrivateKey = "+testPrivA+"\nAddress = 2001:db8::2\nDNS = 2001:db8::53, home.example",
		"PublicKey = "+testPubSrv+"\nEndpoint = [2001:db8::1]:51820\nAllowedIPs = ::/0"))
	f.Add(conf(baseIface+"\nPostUp = true", basePeer))
	f.Add("[" + testPrivA + "]\n")

	f.Fuzz(func(t *testing.T, input string) {
		tun, warns, err := Parse("fuzz", strings.NewReader(input))
		for _, w := range warns {
			if base64Run.MatchString(w.String()) {
				t.Fatalf("warning may echo key material: %q", w.String())
			}
		}
		if err != nil {
			if tun != nil {
				t.Fatalf("tunnel %+v returned with error %v", tun, err)
			}
			if base64Run.MatchString(err.Error()) {
				t.Fatalf("error may echo key material: %q", err.Error())
			}
			return
		}
		if err := tun.validate(); err != nil {
			t.Fatalf("accepted tunnel is invalid: %v", err)
		}
		if _, err := tun.UAPIConfig(netip.MustParseAddr("192.0.2.1")); err != nil {
			t.Fatalf("UAPIConfig of accepted tunnel: %v", err)
		}
		again, _, err := Parse("fuzz", strings.NewReader(format(tun)))
		if err != nil {
			t.Fatalf("re-parsing the formatted tunnel: %v", err)
		}
		if !reflect.DeepEqual(again, tun) {
			t.Fatalf("round trip changed the tunnel\n got %+v\nwant %+v", again, tun)
		}
	})
}

// format writes t back in wg-quick format, for round-trip checks.
func format(t *Tunnel) string {
	var b strings.Builder
	b.WriteString("[Interface]\n")
	fmt.Fprintf(&b, "PrivateKey = %s\n", base64.StdEncoding.EncodeToString(t.PrivateKey.b[:]))
	for _, a := range t.Addresses {
		fmt.Fprintf(&b, "Address = %s\n", a)
	}
	for _, d := range t.DNS {
		fmt.Fprintf(&b, "DNS = %s\n", d)
	}
	fmt.Fprintf(&b, "MTU = %d\n", t.MTU)
	b.WriteString("[Peer]\n")
	fmt.Fprintf(&b, "PublicKey = %s\n", base64.StdEncoding.EncodeToString(t.Peer.PublicKey.b[:]))
	if !t.Peer.PresharedKey.IsZero() {
		fmt.Fprintf(&b, "PresharedKey = %s\n", base64.StdEncoding.EncodeToString(t.Peer.PresharedKey.b[:]))
	}
	fmt.Fprintf(&b, "Endpoint = %s\n", t.Peer.Endpoint)
	for _, a := range t.Peer.AllowedIPs {
		fmt.Fprintf(&b, "AllowedIPs = %s\n", a)
	}
	fmt.Fprintf(&b, "PersistentKeepalive = %d\n", t.Peer.PersistentKeepalive)
	return b.String()
}
