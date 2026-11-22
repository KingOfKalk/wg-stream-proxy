// SPDX-License-Identifier: AGPL-3.0-or-later

package wgquick

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TEST-ONLY keys, generated for this repository's tests (see
// testdata/README.md). They protect nothing.
const (
	testPrivA  = "ARGJGraXPR1iFd9oTshCi8s+VCr7xPbEgUKeEJrcT+8="
	testPrivB  = "3c5cA96yyFer1TFMR3GG8mRNCIk9WFGzuIr1NXIRKIo="
	testPubSrv = "sYmVRDkaaT2HdjPTtmB0/zieyemri/zhQCM0mJzY/1k="
	testPSK    = "sA8D0Ytk1cR1F0PaUhqmxgwrVvPnmCSZj1O+winRHSc="
)

// testSecrets lists every key in base64 and hex, plus prefixes that would
// reveal a partial key, for leak checks.
var testSecrets = func() []string {
	var s []string
	for _, k := range []string{testPrivA, testPrivB, testPubSrv, testPSK} {
		s = append(s, k, k[:12], mustKey(k).hex(), mustKey(k).hex()[:16])
	}
	return s
}()

func mustKey(b64 string) Key {
	k, err := parseKey(b64)
	if err != nil {
		panic(err)
	}
	return k
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// conf builds a tunnel file from an [Interface] body and a [Peer] body.
func conf(iface, peer string) string {
	return "[Interface]\n" + iface + "\n[Peer]\n" + peer + "\n"
}

const (
	baseIface = "PrivateKey = " + testPrivA + "\nAddress = 192.0.2.2/32\nDNS = 192.0.2.53"
	basePeer  = "PublicKey = " + testPubSrv + "\nEndpoint = vpn.example.com:51820\nAllowedIPs = 0.0.0.0/0"
)

func assertNoSecrets(t *testing.T, what, s string) {
	t.Helper()
	for _, secret := range testSecrets {
		if strings.Contains(s, secret) {
			t.Errorf("%s leaks key material %q: %q", what, secret, s)
		}
	}
}

func TestParseFull(t *testing.T) {
	tun, warns, err := Parse("gr", strings.NewReader(readFixture(t, "full.conf")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := &Tunnel{
		Name:       "gr",
		PrivateKey: mustKey(testPrivA),
		Addresses: []netip.Prefix{
			netip.MustParsePrefix("192.0.2.2/32"),
			netip.MustParsePrefix("2001:db8::2/128"),
		},
		DNS: []netip.Addr{
			netip.MustParseAddr("192.0.2.53"),
			netip.MustParseAddr("2001:db8::53"),
		},
		MTU: 1380,
		Peer: Peer{
			PublicKey:    mustKey(testPubSrv),
			PresharedKey: mustKey(testPSK),
			Endpoint:     Endpoint{Host: "vpn.example.com", Port: 51820},
			AllowedIPs: []netip.Prefix{
				netip.MustParsePrefix("0.0.0.0/0"),
				netip.MustParsePrefix("::/0"),
			},
			PersistentKeepalive: 25,
		},
	}
	if !reflect.DeepEqual(tun, want) {
		t.Errorf("tunnel mismatch\n got %+v\nwant %+v", tun, want)
	}
	wantWarns := []Warning{{Tunnel: "gr", Line: 7, Msg: "DNS search domain ignored"}}
	if !reflect.DeepEqual(warns, wantWarns) {
		t.Errorf("warnings = %v, want %v", warns, wantWarns)
	}
}

func TestParseMinimalDefaults(t *testing.T) {
	tun, warns, err := Parse("min", strings.NewReader(readFixture(t, "minimal.conf")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("warnings = %v, want none", warns)
	}
	if tun.MTU != DefaultMTU || DefaultMTU != 1420 {
		t.Errorf("MTU = %d, want 1420", tun.MTU)
	}
	if !tun.Peer.PresharedKey.IsZero() {
		t.Error("PresharedKey set, want zero")
	}
	if tun.Peer.PersistentKeepalive != 0 {
		t.Errorf("PersistentKeepalive = %d, want 0", tun.Peer.PersistentKeepalive)
	}
	if got, want := tun.Peer.Endpoint, (Endpoint{Host: "192.0.2.1", Port: 51820}); got != want {
		t.Errorf("Endpoint = %v, want %v", got, want)
	}
}

// TestParseSyntax covers the format details the spec leaves open.
func TestParseSyntax(t *testing.T) {
	tests := []struct {
		name  string
		input string
		check func(t *testing.T, tun *Tunnel)
	}{
		{
			name: "lowercase sections and keys",
			input: "[interface]\nprivatekey = " + testPrivA + "\naddress = 192.0.2.2/32\ndns = 192.0.2.53\n" +
				"[PEER]\nPUBLICKEY = " + testPubSrv + "\nendpoint = vpn.example.com:51820\nallowedips = 0.0.0.0/0\n",
			check: func(t *testing.T, tun *Tunnel) {
				if tun.PrivateKey != mustKey(testPrivA) {
					t.Error("PrivateKey not parsed")
				}
			},
		},
		{
			name: "comments, blank lines, CRLF, BOM and whitespace",
			input: "\ufeff# header comment\r\n\r\n  [ Interface ]  # trailing\r\n\tPrivateKey=" + testPrivA + "   # key\r\n" +
				"Address =192.0.2.2/32\r\nDNS= 192.0.2.53\r\n[Peer]\r\n" + strings.ReplaceAll(basePeer, "\n", "\r\n") + "\r\n",
			check: func(t *testing.T, tun *Tunnel) {
				if len(tun.Addresses) != 1 || tun.Addresses[0] != netip.MustParsePrefix("192.0.2.2/32") {
					t.Errorf("Addresses = %v", tun.Addresses)
				}
			},
		},
		{
			name:  "repeated list keys append, separators comma and space",
			input: conf(baseIface+"\nAddress = 2001:db8::2/128 , 192.0.2.4/32\nDNS = 2001:db8::53 192.0.2.54", basePeer+"\nAllowedIPs = ::/0"),
			check: func(t *testing.T, tun *Tunnel) {
				wantAddr := []netip.Prefix{
					netip.MustParsePrefix("192.0.2.2/32"),
					netip.MustParsePrefix("2001:db8::2/128"),
					netip.MustParsePrefix("192.0.2.4/32"),
				}
				if !reflect.DeepEqual(tun.Addresses, wantAddr) {
					t.Errorf("Addresses = %v, want %v", tun.Addresses, wantAddr)
				}
				wantDNS := []netip.Addr{
					netip.MustParseAddr("192.0.2.53"),
					netip.MustParseAddr("2001:db8::53"),
					netip.MustParseAddr("192.0.2.54"),
				}
				if !reflect.DeepEqual(tun.DNS, wantDNS) {
					t.Errorf("DNS = %v, want %v", tun.DNS, wantDNS)
				}
				if len(tun.Peer.AllowedIPs) != 2 {
					t.Errorf("AllowedIPs = %v, want 2 entries", tun.Peer.AllowedIPs)
				}
			},
		},
		{
			name:  "address without prefix length is a host prefix",
			input: conf("PrivateKey = "+testPrivA+"\nAddress = 192.0.2.2, 2001:db8::2\nDNS = 192.0.2.53", basePeer),
			check: func(t *testing.T, tun *Tunnel) {
				want := []netip.Prefix{netip.MustParsePrefix("192.0.2.2/32"), netip.MustParsePrefix("2001:db8::2/128")}
				if !reflect.DeepEqual(tun.Addresses, want) {
					t.Errorf("Addresses = %v, want %v", tun.Addresses, want)
				}
			},
		},
		{
			name:  "address keeps host bits",
			input: conf("PrivateKey = "+testPrivA+"\nAddress = 192.0.2.2/24\nDNS = 192.0.2.53", basePeer),
			check: func(t *testing.T, tun *Tunnel) {
				if tun.Addresses[0] != netip.MustParsePrefix("192.0.2.2/24") {
					t.Errorf("Addresses = %v", tun.Addresses)
				}
			},
		},
		{
			name:  "allowed IPs are masked, bare IPs become host prefixes",
			input: conf(baseIface, "PublicKey = "+testPubSrv+"\nEndpoint = vpn.example.com:51820\nAllowedIPs = 198.51.100.7/24, 203.0.113.9"),
			check: func(t *testing.T, tun *Tunnel) {
				want := []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.9/32")}
				if !reflect.DeepEqual(tun.Peer.AllowedIPs, want) {
					t.Errorf("AllowedIPs = %v, want %v", tun.Peer.AllowedIPs, want)
				}
			},
		},
		{
			name:  "IPv4-mapped addresses are unmapped",
			input: conf("PrivateKey = "+testPrivA+"\nAddress = ::ffff:192.0.2.2/128\nDNS = ::ffff:192.0.2.53", basePeer),
			check: func(t *testing.T, tun *Tunnel) {
				if tun.Addresses[0] != netip.MustParsePrefix("192.0.2.2/32") || tun.DNS[0] != netip.MustParseAddr("192.0.2.53") {
					t.Errorf("Addresses = %v, DNS = %v", tun.Addresses, tun.DNS)
				}
			},
		},
		{
			name:  "IPv6 endpoint in brackets",
			input: conf(baseIface, "PublicKey = "+testPubSrv+"\nEndpoint = [2001:db8::1]:51820\nAllowedIPs = 0.0.0.0/0"),
			check: func(t *testing.T, tun *Tunnel) {
				if got, want := tun.Peer.Endpoint, (Endpoint{Host: "2001:db8::1", Port: 51820}); got != want {
					t.Errorf("Endpoint = %v, want %v", got, want)
				}
				if got := tun.Peer.Endpoint.String(); got != "[2001:db8::1]:51820" {
					t.Errorf("Endpoint.String() = %q", got)
				}
			},
		},
		{
			name:  "persistent keepalive off",
			input: conf(baseIface, basePeer+"\nPersistentKeepalive = off"),
			check: func(t *testing.T, tun *Tunnel) {
				if tun.Peer.PersistentKeepalive != 0 {
					t.Errorf("PersistentKeepalive = %d, want 0", tun.Peer.PersistentKeepalive)
				}
			},
		},
		{
			name:  "MTU bounds",
			input: conf(baseIface+"\nMTU = 1280", basePeer),
			check: func(t *testing.T, tun *Tunnel) {
				if tun.MTU != 1280 {
					t.Errorf("MTU = %d, want 1280", tun.MTU)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tun, _, err := Parse("t", strings.NewReader(tt.input))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			tt.check(t, tun)
		})
	}
}

func TestParseWarnings(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []Warning
	}{
		{
			name:  "ignored interface keys",
			input: conf(baseIface+"\nListenPort = 51820\nTable = off\nSaveConfig = true", basePeer),
			want: []Warning{
				{Tunnel: "t", Line: 5, Msg: "ListenPort ignored"},
				{Tunnel: "t", Line: 6, Msg: "Table ignored"},
				{Tunnel: "t", Line: 7, Msg: "SaveConfig ignored"},
			},
		},
		{
			name:  "DNS search domains",
			input: conf("PrivateKey = "+testPrivA+"\nAddress = 192.0.2.2/32\nDNS = 192.0.2.53, home.example, corp.example", basePeer),
			want: []Warning{
				{Tunnel: "t", Line: 4, Msg: "DNS search domain ignored"},
				{Tunnel: "t", Line: 4, Msg: "DNS search domain ignored"},
			},
		},
		{
			name:  "no DNS server",
			input: conf("PrivateKey = "+testPrivA+"\nAddress = 192.0.2.2/32", basePeer),
			want: []Warning{
				{Tunnel: "t", Msg: "no DNS server: upstream host names cannot be resolved"},
			},
		},
		{
			name:  "DNS outside AllowedIPs",
			input: conf("PrivateKey = "+testPrivA+"\nAddress = 192.0.2.2/32\nDNS = 192.0.2.53", "PublicKey = "+testPubSrv+"\nEndpoint = vpn.example.com:51820\nAllowedIPs = 198.51.100.0/24"),
			want: []Warning{
				{Tunnel: "t", Line: 4, Msg: "DNS server 192.0.2.53 ignored: outside AllowedIPs"},
				{Tunnel: "t", Msg: "no reachable DNS server: upstream host names cannot be resolved"},
			},
		},
		{
			name:  "DNS of a family without tunnel address",
			input: conf("PrivateKey = "+testPrivA+"\nAddress = 192.0.2.2/32\nDNS = 2001:db8::53", "PublicKey = "+testPubSrv+"\nEndpoint = vpn.example.com:51820\nAllowedIPs = 0.0.0.0/0, ::/0"),
			want: []Warning{
				{Tunnel: "t", Line: 4, Msg: "DNS server 2001:db8::53 ignored: tunnel has no IPv6 address"},
				{Tunnel: "t", Msg: "no reachable DNS server: upstream host names cannot be resolved"},
			},
		},
		{
			name:  "IPv4 DNS in an IPv6-only tunnel",
			input: conf("PrivateKey = "+testPrivA+"\nAddress = 2001:db8::2/128\nDNS = 192.0.2.53", "PublicKey = "+testPubSrv+"\nEndpoint = vpn.example.com:51820\nAllowedIPs = 0.0.0.0/0, ::/0"),
			want: []Warning{
				{Tunnel: "t", Line: 4, Msg: "DNS server 192.0.2.53 ignored: tunnel has no IPv4 address"},
				{Tunnel: "t", Msg: "no reachable DNS server: upstream host names cannot be resolved"},
			},
		},
		{
			name:  "one reachable DNS server left",
			input: conf("PrivateKey = "+testPrivA+"\nAddress = 192.0.2.2/32\nDNS = 192.0.2.53, 2001:db8::53\nDNS = 198.51.100.53", "PublicKey = "+testPubSrv+"\nEndpoint = vpn.example.com:51820\nAllowedIPs = 192.0.2.0/24"),
			want: []Warning{
				{Tunnel: "t", Line: 4, Msg: "DNS server 2001:db8::53 ignored: tunnel has no IPv6 address"},
				{Tunnel: "t", Line: 5, Msg: "DNS server 198.51.100.53 ignored: outside AllowedIPs"},
			},
		},
		{
			name:  "IPv4-mapped DNS server is checked as IPv4",
			input: conf("PrivateKey = "+testPrivA+"\nAddress = 192.0.2.2/32\nDNS = ::ffff:192.0.2.53", basePeer),
			want:  nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, warns, err := Parse("t", strings.NewReader(tt.input))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if !reflect.DeepEqual(warns, tt.want) {
				t.Errorf("warnings = %v, want %v", warns, tt.want)
			}
			for _, w := range warns {
				assertNoSecrets(t, "warning", w.String())
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantMsg string
		line    int
	}{
		{"empty file", "", "missing [Interface]", 0},
		{"no interface", "[Peer]\n" + basePeer, "missing [Interface]", 0},
		{"no peer", "[Interface]\n" + baseIface, "missing [Peer]", 0},
		{"two interfaces", conf(baseIface, basePeer) + "[Interface]\n", "more than one [Interface]", 9},
		{"two peers", conf(baseIface, basePeer) + "[Peer]\n" + basePeer, "more than one [Peer]", 9},
		{"PreUp", conf(baseIface+"\nPreUp = true", basePeer), "PreUp not supported", 5},
		{"PostUp", conf(baseIface+"\nPostUp = true", basePeer), "PostUp not supported", 5},
		{"PreDown", conf(baseIface+"\nPreDown = true", basePeer), "PreDown not supported", 5},
		{"PostDown", conf(baseIface+"\npostdown = true", basePeer), "PostDown not supported", 5},
		{"unknown interface key", conf(baseIface+"\nFwMark = 51820", basePeer), "unknown key FwMark in [Interface]", 5},
		{"interface key in peer", conf(baseIface, basePeer+"\nAddress = 192.0.2.9/32"), "unknown key Address in [Peer]", 9},
		{"peer key in interface", conf(baseIface+"\nEndpoint = vpn.example.com:51820", basePeer), "unknown key Endpoint in [Interface]", 5},
		{"unknown section", conf(baseIface, basePeer) + "[Extra]\n", "unknown section [Extra]", 9},
		{"unterminated section", "[Interface\n", "invalid section header", 1},
		{"key before section", "PrivateKey = " + testPrivA + "\n" + conf(baseIface, basePeer), "key outside of a section", 1},
		{"line without equals", conf(baseIface+"\njunk", basePeer), "expected key = value", 5},
		{"empty key", conf(baseIface+"\n= 1", basePeer), "expected key = value", 5},
		{"empty value", conf(baseIface+"\nMTU =", basePeer), "empty value for MTU", 5},
		{"empty list value", conf(baseIface+"\nAddress = ,", basePeer), "empty value for Address", 5},
		{"duplicate PrivateKey", conf(baseIface+"\nPrivateKey = "+testPrivB, basePeer), "duplicate PrivateKey", 5},
		{"duplicate MTU", conf(baseIface+"\nMTU = 1400\nMTU = 1400", basePeer), "duplicate MTU", 6},
		{"duplicate PublicKey", conf(baseIface, basePeer+"\nPublicKey = "+testPubSrv), "duplicate PublicKey", 9},
		{"duplicate PresharedKey", conf(baseIface, basePeer+"\nPresharedKey = "+testPSK+"\nPresharedKey = "+testPSK), "duplicate PresharedKey", 10},
		{"duplicate Endpoint", conf(baseIface, basePeer+"\nEndpoint = vpn.example.com:51820"), "duplicate Endpoint", 9},
		{"duplicate PersistentKeepalive", conf(baseIface, basePeer+"\nPersistentKeepalive = 25\nPersistentKeepalive = 25"), "duplicate PersistentKeepalive", 10},
		{"missing PrivateKey", conf("Address = 192.0.2.2/32\nDNS = 192.0.2.53", basePeer), "missing PrivateKey", 0},
		{"missing Address", conf("PrivateKey = "+testPrivA+"\nDNS = 192.0.2.53", basePeer), "missing Address", 0},
		{"missing PublicKey", conf(baseIface, "Endpoint = vpn.example.com:51820\nAllowedIPs = 0.0.0.0/0"), "missing PublicKey", 0},
		{"missing Endpoint", conf(baseIface, "PublicKey = "+testPubSrv+"\nAllowedIPs = 0.0.0.0/0"), "missing Endpoint", 0},
		{"missing AllowedIPs", conf(baseIface, "PublicKey = "+testPubSrv+"\nEndpoint = vpn.example.com:51820"), "missing AllowedIPs", 0},
		{"private key not base64", conf("PrivateKey = not*base64\nAddress = 192.0.2.2/32", basePeer), "invalid PrivateKey", 2},
		{"private key too short", conf("PrivateKey = "+testPrivA[:40]+"=\nAddress = 192.0.2.2/32", basePeer), "invalid PrivateKey", 2},
		{"private key with suffix", conf("PrivateKey = "+testPrivA+"AAAA\nAddress = 192.0.2.2/32", basePeer), "invalid PrivateKey", 2},
		{"private key zero", conf("PrivateKey = "+zeroKeyB64+"\nAddress = 192.0.2.2/32", basePeer), "invalid PrivateKey", 2},
		{"public key zero", conf(baseIface, "PublicKey = "+zeroKeyB64+"\nEndpoint = vpn.example.com:51820\nAllowedIPs = 0.0.0.0/0"), "invalid PublicKey", 6},
		{"public key invalid", conf(baseIface, "PublicKey = "+testPubSrv[:20]+"\nEndpoint = vpn.example.com:51820\nAllowedIPs = 0.0.0.0/0"), "invalid PublicKey", 6},
		{"preshared key invalid", conf(baseIface, basePeer+"\nPresharedKey = "+testPSK[:20]), "invalid PresharedKey", 9},
		{"address invalid", conf("PrivateKey = "+testPrivA+"\nAddress = 192.0.2.300/32", basePeer), "invalid Address", 3},
		{"address with zone", conf("PrivateKey = "+testPrivA+"\nAddress = fe80::1%eth0", basePeer), "invalid Address", 3},
		{"DNS with zone", conf(baseIface+"\nDNS = fe80::53%eth0", basePeer), "invalid DNS", 5},
		{"MTU not a number", conf(baseIface+"\nMTU = big", basePeer), "invalid MTU", 5},
		{"MTU too small", conf(baseIface+"\nMTU = 1279", basePeer), "invalid MTU", 5},
		{"MTU too large", conf(baseIface+"\nMTU = 65536", basePeer), "invalid MTU", 5},
		{"endpoint without port", conf(baseIface, "PublicKey = "+testPubSrv+"\nEndpoint = vpn.example.com\nAllowedIPs = 0.0.0.0/0"), "invalid Endpoint", 7},
		{"endpoint port zero", conf(baseIface, "PublicKey = "+testPubSrv+"\nEndpoint = vpn.example.com:0\nAllowedIPs = 0.0.0.0/0"), "invalid Endpoint", 7},
		{"endpoint port too large", conf(baseIface, "PublicKey = "+testPubSrv+"\nEndpoint = vpn.example.com:65536\nAllowedIPs = 0.0.0.0/0"), "invalid Endpoint", 7},
		{"endpoint IPv6 without brackets", conf(baseIface, "PublicKey = "+testPubSrv+"\nEndpoint = 2001:db8::1:51820\nAllowedIPs = 0.0.0.0/0"), "invalid Endpoint", 7},
		{"endpoint bad host", conf(baseIface, "PublicKey = "+testPubSrv+"\nEndpoint = vpn_ex@mple.com:51820\nAllowedIPs = 0.0.0.0/0"), "invalid Endpoint", 7},
		{"endpoint empty host", conf(baseIface, "PublicKey = "+testPubSrv+"\nEndpoint = :51820\nAllowedIPs = 0.0.0.0/0"), "invalid Endpoint", 7},
		{"endpoint with zone", conf(baseIface, "PublicKey = "+testPubSrv+"\nEndpoint = [fe80::1%eth0]:51820\nAllowedIPs = 0.0.0.0/0"), "invalid Endpoint", 7},
		{"keepalive invalid", conf(baseIface, basePeer+"\nPersistentKeepalive = soon"), "invalid PersistentKeepalive", 9},
		{"keepalive too large", conf(baseIface, basePeer+"\nPersistentKeepalive = 65536"), "invalid PersistentKeepalive", 9},
		{"allowed IPs invalid", conf(baseIface, "PublicKey = "+testPubSrv+"\nEndpoint = vpn.example.com:51820\nAllowedIPs = 0.0.0.0/33"), "invalid AllowedIPs", 8},
		{"file too large", conf(baseIface, basePeer) + strings.Repeat("#", maxFileSize), "file larger than 65536 bytes", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tun, _, err := Parse("t", strings.NewReader(tt.input))
			if err == nil {
				t.Fatalf("Parse succeeded (%+v), want error %q", tun, tt.wantMsg)
			}
			if tun != nil {
				t.Errorf("tunnel = %+v, want nil on error", tun)
			}
			var pe *Error
			if !errors.As(err, &pe) {
				t.Fatalf("error %v is %T, want *Error", err, err)
			}
			if pe.Tunnel != "t" || pe.Line != tt.line || pe.Msg != tt.wantMsg {
				t.Errorf("error = %+v, want tunnel t, line %d, msg %q", *pe, tt.line, tt.wantMsg)
			}
			assertNoSecrets(t, "error", err.Error())
		})
	}
}

const zeroKeyB64 = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

// TestParseErrorsNeverEchoKeys puts keys where values or names are echoed
// by careless parsers.
func TestParseErrorsNeverEchoKeys(t *testing.T) {
	inputs := []string{
		testPrivA + "\n" + conf(baseIface, basePeer),
		conf(baseIface+"\n"+testPrivB, basePeer),
		conf(baseIface+"\n"+testPrivB+" = x", basePeer),
		conf(baseIface+"\nPrivatKey = "+testPrivB, basePeer),
		conf(baseIface+"\nAddress = "+testPrivB, basePeer),
		conf(baseIface+"\nMTU = "+testPrivB, basePeer),
		conf("PrivateKey = "+testPrivA+"x\nAddress = 192.0.2.2/32", basePeer),
		conf(baseIface, "PublicKey = "+testPubSrv+"\nEndpoint = "+testPSK+"\nAllowedIPs = 0.0.0.0/0"),
		conf(baseIface, basePeer+"\nAllowedIPs = "+testPSK),
		conf(baseIface, basePeer+"\nPersistentKeepalive = "+testPSK),
		"[" + testPrivA + "]\n",
		"[" + testPrivA + "\n",
		conf(baseIface+"\nPostUp = wg set %i private-key "+testPrivB, basePeer),
	}
	for i, in := range inputs {
		_, warns, err := Parse("t", strings.NewReader(in))
		if err == nil {
			t.Errorf("input %d: Parse succeeded, want error", i)
			continue
		}
		assertNoSecrets(t, "error", err.Error())
		for _, w := range warns {
			assertNoSecrets(t, "warning", w.String())
		}
	}
}

func TestParseSearchDomainWarningDoesNotEchoValue(t *testing.T) {
	_, warns, err := Parse("t", strings.NewReader(conf(baseIface+"\nDNS = "+testPrivB, basePeer)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(warns) != 1 {
		t.Fatalf("warnings = %v, want 1", warns)
	}
	assertNoSecrets(t, "warning", warns[0].String())
}

func TestErrorAndWarningFormat(t *testing.T) {
	tests := []struct {
		got, want string
	}{
		{(&Error{Tunnel: "gr", Line: 3, Msg: "invalid MTU"}).Error(), `tunnel "gr": line 3: invalid MTU`},
		{(&Error{Tunnel: "gr", Msg: "missing [Peer]"}).Error(), `tunnel "gr": missing [Peer]`},
		{Warning{Tunnel: "gr", Line: 4, Msg: "Table ignored"}.String(), `tunnel "gr": line 4: Table ignored`},
		{Warning{Tunnel: "gr", Msg: "world-readable"}.String(), `tunnel "gr": world-readable`},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("got %q, want %q", tt.got, tt.want)
		}
	}
}

func TestAllows(t *testing.T) {
	in := conf(baseIface, "PublicKey = "+testPubSrv+"\nEndpoint = vpn.example.com:51820\nAllowedIPs = 198.51.100.0/24, 2001:db8:1::/48")
	tun, _, err := Parse("t", strings.NewReader(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	tests := []struct {
		addr string
		want bool
	}{
		{"198.51.100.7", true},
		{"::ffff:198.51.100.7", true},
		{"203.0.113.1", false},
		{"2001:db8:1::5", true},
		{"2001:db8:2::5", false},
	}
	for _, tt := range tests {
		if got := tun.Allows(netip.MustParseAddr(tt.addr)); got != tt.want {
			t.Errorf("Allows(%s) = %v, want %v", tt.addr, got, tt.want)
		}
	}
	if tun.Allows(netip.Addr{}) {
		t.Error("Allows(invalid) = true, want false")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("disk on fire") }

func TestParseReadError(t *testing.T) {
	_, _, err := Parse("t", errReader{})
	if err == nil || !strings.Contains(err.Error(), "disk on fire") {
		t.Fatalf("err = %v, want read error", err)
	}
}

func TestEqual(t *testing.T) {
	base := conf(baseIface, basePeer)
	tests := []struct {
		name  string
		name2 string
		text  string
		want  bool
	}{
		{"same text", "t", base, true},
		{"comments, case, spacing and CRLF", "t", "# reloaded\r\n[interface]\r\nprivatekey=" + testPrivA + " # key\r\naddress = 192.0.2.2\r\ndns=192.0.2.53\r\n\r\n[PEER]\r\npublickey = " + testPubSrv + "\r\nendpoint = vpn.example.com:51820\r\nallowedips = 0.0.0.0/0\r\n", true},
		{"other name", "u", base, false},
		{"private key", "t", conf("PrivateKey = "+testPrivB+"\nAddress = 192.0.2.2/32\nDNS = 192.0.2.53", basePeer), false},
		{"address", "t", conf("PrivateKey = "+testPrivA+"\nAddress = 192.0.2.3/32\nDNS = 192.0.2.53", basePeer), false},
		{"extra address", "t", conf(baseIface+"\nAddress = 2001:db8::2/128", basePeer), false},
		{"DNS", "t", conf("PrivateKey = "+testPrivA+"\nAddress = 192.0.2.2/32\nDNS = 192.0.2.54", basePeer), false},
		{"MTU", "t", conf(baseIface+"\nMTU = 1280", basePeer), false},
		{"preshared key", "t", conf(baseIface, basePeer+"\nPresharedKey = "+testPSK), false},
		{"endpoint", "t", conf(baseIface, "PublicKey = "+testPubSrv+"\nEndpoint = vpn.example.com:51821\nAllowedIPs = 0.0.0.0/0"), false},
		{"allowed IPs", "t", conf(baseIface, "PublicKey = "+testPubSrv+"\nEndpoint = vpn.example.com:51820\nAllowedIPs = 0.0.0.0/0, ::/0"), false},
		{"keepalive", "t", conf(baseIface, basePeer+"\nPersistentKeepalive = 25"), false},
	}
	a, _, err := Parse("t", strings.NewReader(base))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _, err := Parse(tt.name2, strings.NewReader(tt.text))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := a.Equal(b); got != tt.want {
				t.Errorf("Equal = %v, want %v", got, tt.want)
			}
			if got := b.Equal(a); got != tt.want {
				t.Errorf("reverse Equal = %v, want %v", got, tt.want)
			}
		})
	}
	if a.Equal(nil) {
		t.Error("Equal(nil) = true")
	}
}

func TestTunnelReaches(t *testing.T) {
	tun := &Tunnel{
		Addresses: []netip.Prefix{netip.MustParsePrefix("192.0.2.2/32")},
		Peer:      Peer{AllowedIPs: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("2001:db8::/32")}},
	}
	tests := []struct {
		addr string
		want bool
	}{
		{"192.0.2.53", true},
		{"::ffff:192.0.2.53", true},
		{"198.51.100.1", false}, // outside AllowedIPs
		{"2001:db8::53", false}, // inside AllowedIPs, but no IPv6 address
		{"2001:db9::53", false},
	}
	for _, tt := range tests {
		if got := tun.Reaches(netip.MustParseAddr(tt.addr)); got != tt.want {
			t.Errorf("Reaches(%s) = %v, want %v", tt.addr, got, tt.want)
		}
	}
}
