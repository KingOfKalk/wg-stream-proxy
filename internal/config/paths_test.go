// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"reflect"
	"testing"
)

// fakeEnv builds an Env from a variable map and a set of existing
// directories.
func fakeEnv(vars map[string]string, dirs []string) Env {
	return Env{
		LookupEnv: func(k string) (string, bool) {
			v, ok := vars[k]
			return v, ok
		},
		DirExists: func(p string) bool {
			for _, d := range dirs {
				if d == p {
					return true
				}
			}
			return false
		},
	}
}

func TestResolvePaths(t *testing.T) {
	tests := []struct {
		name    string
		flags   Flags
		vars    map[string]string
		dirs    []string
		want    Paths
		wantErr bool
	}{
		{
			name:  "flag beats env and etc",
			flags: Flags{ConfigDir: "/flag"},
			vars:  map[string]string{"WG_STREAM_PROXY_CONFIG_DIR": "/env"},
			dirs:  []string{"/etc/wg-stream-proxy"},
			want:  Paths{ConfigDir: "/flag", TunnelsDir: "/flag/tunnels"},
		},
		{
			name: "env beats etc",
			vars: map[string]string{"WG_STREAM_PROXY_CONFIG_DIR": "/env"},
			dirs: []string{"/etc/wg-stream-proxy"},
			want: Paths{ConfigDir: "/env", TunnelsDir: "/env/tunnels"},
		},
		{
			name: "etc if it exists",
			dirs: []string{"/etc/wg-stream-proxy"},
			want: Paths{ConfigDir: "/etc/wg-stream-proxy", TunnelsDir: "/etc/wg-stream-proxy/tunnels"},
		},
		{
			name: "empty env value counts as unset",
			vars: map[string]string{"WG_STREAM_PROXY_CONFIG_DIR": ""},
			dirs: []string{"/etc/wg-stream-proxy"},
			want: Paths{ConfigDir: "/etc/wg-stream-proxy", TunnelsDir: "/etc/wg-stream-proxy/tunnels"},
		},
		{
			name:    "no flag, env or etc: no XDG or home fallback",
			vars:    map[string]string{"XDG_CONFIG_HOME": "/xdg", "HOME": "/home/u"},
			wantErr: true,
		},
		{
			name:  "removed tunnels dir variable is ignored",
			flags: Flags{ConfigDir: "/flag"},
			vars:  map[string]string{"WG_STREAM_PROXY_TUNNELS_DIR": "/secrets/wg"},
			want:  Paths{ConfigDir: "/flag", TunnelsDir: "/flag/tunnels"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(tt.flags, fakeEnv(tt.vars, tt.dirs))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Resolve() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if got.Paths != tt.want {
				t.Errorf("Resolve().Paths = %+v, want %+v", got.Paths, tt.want)
			}
		})
	}
}

func TestResolveOverrides(t *testing.T) {
	tests := []struct {
		name  string
		flags Flags
		vars  map[string]string
		want  Overrides
	}{
		{
			name: "none",
			want: Overrides{},
		},
		{
			name: "removed variables are ignored",
			vars: map[string]string{
				"WG_STREAM_PROXY_LISTEN":          ":1",
				"WG_STREAM_PROXY_LOG_LEVEL":       "debug",
				"WG_STREAM_PROXY_LOG_FORMAT":      "json",
				"WG_STREAM_PROXY_PUBLIC_BASE_URL": "http://wsp.lan:2349",
			},
			want: Overrides{},
		},
		{
			name:  "flags",
			flags: Flags{Listen: ":2", LogLevel: "warn", LogFormat: "text"},
			want:  Overrides{Listen: ":2", LogLevel: "warn", LogFormat: "text"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.flags.ConfigDir = "/c"
			got, err := Resolve(tt.flags, fakeEnv(tt.vars, nil))
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if !reflect.DeepEqual(got.Overrides, tt.want) {
				t.Errorf("Resolve().Overrides = %+v, want %+v", got.Overrides, tt.want)
			}
		})
	}
}

func TestOSEnv(t *testing.T) {
	t.Setenv("WG_STREAM_PROXY_TEST_VAR", "x")
	env := OSEnv()
	if v, ok := env.LookupEnv("WG_STREAM_PROXY_TEST_VAR"); !ok || v != "x" {
		t.Errorf("LookupEnv = %q, %v", v, ok)
	}
	dir := t.TempDir()
	if !env.DirExists(dir) {
		t.Errorf("DirExists(%q) = false", dir)
	}
	if env.DirExists(dir + "/missing") {
		t.Errorf("DirExists(missing) = true")
	}
}
