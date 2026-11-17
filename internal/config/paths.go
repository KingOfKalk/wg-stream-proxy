// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// Environment variables read by Resolve (spec 4 and 5.1).
const (
	EnvConfigDir     = "WG_STREAM_PROXY_CONFIG_DIR"
	EnvTunnelsDir    = "WG_STREAM_PROXY_TUNNELS_DIR"
	EnvListen        = "WG_STREAM_PROXY_LISTEN"
	EnvLogLevel      = "WG_STREAM_PROXY_LOG_LEVEL"
	EnvLogFormat     = "WG_STREAM_PROXY_LOG_FORMAT"
	EnvPublicBaseURL = "WG_STREAM_PROXY_PUBLIC_BASE_URL"
)

// systemConfigDir is used if it exists and nothing else is configured.
const systemConfigDir = "/etc/wg-stream-proxy"

// Flags holds the command-line flags that affect the configuration.
// Empty strings mean "not set".
type Flags struct {
	ConfigDir string
	Listen    string
	LogLevel  string
	LogFormat string
}

// Env gives access to the process environment; injected for tests.
type Env struct {
	LookupEnv   func(key string) (string, bool)
	DirExists   func(path string) bool
	UserHomeDir func() (string, error)
}

// OSEnv returns the Env of the running process.
func OSEnv() Env {
	return Env{
		LookupEnv: os.LookupEnv,
		DirExists: func(p string) bool {
			fi, err := os.Stat(p)
			return err == nil && fi.IsDir()
		},
		UserHomeDir: os.UserHomeDir,
	}
}

// get returns the value of an environment variable; empty means unset.
func (e Env) get(key string) string {
	v, _ := e.LookupEnv(key)
	return v
}

// Paths are the resolved configuration directories (spec 5.1).
type Paths struct {
	ConfigDir  string
	TunnelsDir string
}

// Overrides are settings from flags or environment that beat
// config.toml (spec 5.1). Empty strings mean "not set".
type Overrides struct {
	Listen        string
	LogLevel      string
	LogFormat     string
	PublicBaseURL string
}

// Options are the inputs of Load.
type Options struct {
	Paths     Paths
	Overrides Overrides
}

// Resolve determines directories and overrides from flags and environment
// (spec 5.1). Empty flag and environment values count as not set.
func Resolve(flags Flags, env Env) (Options, error) {
	dir, err := configDir(flags, env)
	if err != nil {
		return Options{}, err
	}
	tunnels := env.get(EnvTunnelsDir)
	if tunnels == "" {
		tunnels = filepath.Join(dir, "tunnels")
	}
	return Options{
		Paths: Paths{ConfigDir: dir, TunnelsDir: tunnels},
		Overrides: Overrides{
			Listen:        first(flags.Listen, env.get(EnvListen)),
			LogLevel:      first(flags.LogLevel, env.get(EnvLogLevel)),
			LogFormat:     first(flags.LogFormat, env.get(EnvLogFormat)),
			PublicBaseURL: env.get(EnvPublicBaseURL),
		},
	}, nil
}

func configDir(flags Flags, env Env) (string, error) {
	if d := first(flags.ConfigDir, env.get(EnvConfigDir)); d != "" {
		return d, nil
	}
	if env.DirExists(systemConfigDir) {
		return systemConfigDir, nil
	}
	if x := env.get("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "wg-stream-proxy"), nil
	}
	home, err := env.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config dir: no --config-dir, %s, %s, XDG_CONFIG_HOME or home directory: %w",
			EnvConfigDir, systemConfigDir, err)
	}
	return filepath.Join(home, ".config", "wg-stream-proxy"), nil
}

// first returns the first non-empty value.
func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
