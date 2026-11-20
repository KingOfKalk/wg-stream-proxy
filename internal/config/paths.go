// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// EnvConfigDir names the config directory (spec 5.1); the container image
// sets it.
const EnvConfigDir = "WG_STREAM_PROXY_CONFIG_DIR"

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
	LookupEnv func(key string) (string, bool)
	DirExists func(path string) bool
}

// OSEnv returns the Env of the running process.
func OSEnv() Env {
	return Env{
		LookupEnv: os.LookupEnv,
		DirExists: func(p string) bool {
			fi, err := os.Stat(p)
			return err == nil && fi.IsDir()
		},
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

// Overrides are settings from flags that beat config.toml (spec 5.1).
// Empty strings mean "not set".
type Overrides struct {
	Listen    string
	LogLevel  string
	LogFormat string
}

// Options are the inputs of Load.
type Options struct {
	Paths     Paths
	Overrides Overrides
}

// Resolve determines directories and overrides from flags and the
// environment (spec 5.1). Empty flag and environment values count as not set.
func Resolve(flags Flags, env Env) (Options, error) {
	dir, err := configDir(flags, env)
	if err != nil {
		return Options{}, err
	}
	return Options{
		Paths: Paths{ConfigDir: dir, TunnelsDir: filepath.Join(dir, "tunnels")},
		Overrides: Overrides{
			Listen:    flags.Listen,
			LogLevel:  flags.LogLevel,
			LogFormat: flags.LogFormat,
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
	return "", fmt.Errorf("config dir: set --config-dir or %s (or create %s)",
		EnvConfigDir, systemConfigDir)
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
