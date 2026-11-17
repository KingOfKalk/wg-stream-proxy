// SPDX-License-Identifier: AGPL-3.0-or-later

package wgquick

import (
	"net/netip"
	"strings"
	"testing"
)

func TestUAPIConfigGolden(t *testing.T) {
	tun, _, err := Parse("gr", strings.NewReader(readFixture(t, "full.conf")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got, err := tun.UAPIConfig(netip.MustParseAddr("192.0.2.1"))
	if err != nil {
		t.Fatalf("UAPIConfig: %v", err)
	}
	if want := readFixture(t, "full.uapi"); got != want {
		t.Errorf("UAPIConfig mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestUAPIConfigVariants(t *testing.T) {
	tun, _, err := Parse("min", strings.NewReader(readFixture(t, "minimal.conf")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	tests := []struct {
		name     string
		endpoint netip.Addr
		want     []string
		absent   []string
	}{
		{
			name:     "IPv4 endpoint, no preshared key",
			endpoint: netip.MustParseAddr("192.0.2.1"),
			want:     []string{"endpoint=192.0.2.1:51820\n", "persistent_keepalive_interval=0\n", "allowed_ip=0.0.0.0/0\n"},
			absent:   []string{"preshared_key=", "listen_port="},
		},
		{
			name:     "IPv6 endpoint in brackets",
			endpoint: netip.MustParseAddr("2001:db8::1"),
			want:     []string{"endpoint=[2001:db8::1]:51820\n"},
		},
		{
			name:     "IPv4-mapped endpoint is unmapped",
			endpoint: netip.MustParseAddr("::ffff:192.0.2.1"),
			want:     []string{"endpoint=192.0.2.1:51820\n"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tun.UAPIConfig(tt.endpoint)
			if err != nil {
				t.Fatalf("UAPIConfig: %v", err)
			}
			if !strings.HasPrefix(got, "private_key="+mustKey(testPrivB).hex()+"\nreplace_peers=true\npublic_key=") {
				t.Errorf("UAPIConfig does not start with private key, replace_peers, public key:\n%s", got)
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("UAPIConfig lacks %q:\n%s", w, got)
				}
			}
			for _, a := range tt.absent {
				if strings.Contains(got, a) {
					t.Errorf("UAPIConfig contains %q:\n%s", a, got)
				}
			}
		})
	}
}

func TestUAPIConfigRejectsInvalidEndpoint(t *testing.T) {
	tun, _, err := Parse("min", strings.NewReader(readFixture(t, "minimal.conf")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, ep := range []netip.Addr{{}, netip.MustParseAddr("fe80::1%eth0")} {
		if _, err := tun.UAPIConfig(ep); err == nil {
			t.Errorf("UAPIConfig(%v) succeeded, want error", ep)
		} else {
			assertNoSecrets(t, "error", err.Error())
		}
	}
}
