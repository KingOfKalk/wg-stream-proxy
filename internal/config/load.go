// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// globalDefaultsFile is the optional global layer in channels.d (spec
// 5.4). It is not a channel file.
const globalDefaultsFile = "_defaults.toml"

// Load reads and validates the configuration (spec 5.1-5.4): config.toml,
// the tunnel file names in the tunnels dir, the global layer
// channels.d/_defaults.toml and the channel files channels.d/*.toml.
// Missing config.toml, tunnels dir or channels.d count as empty. All
// errors found are returned joined, each prefixed with its file.
func Load(opts Options) (*Config, error) {
	p := opts.Paths
	fi, err := os.Stat(p.ConfigDir)
	if err != nil {
		return nil, fmt.Errorf("config dir: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("config dir %s: not a directory", p.ConfigDir)
	}

	c := &Config{paths: p, byID: map[string]int{}}
	var errs []error

	tunnels, terrs := loadTunnels(p.TunnelsDir)
	errs = append(errs, terrs...)
	c.tunnels = tunnels
	routes := map[string]bool{RouteDirect: true}
	for _, t := range tunnels {
		routes[t.Name] = true
	}

	raw, src, rerrs := readConfigToml(p)
	if len(rerrs) > 0 {
		// Report decode errors only; do not validate a partially
		// decoded file.
		errs = append(errs, rerrs...)
		raw = rawConfig{}
	}
	errs = append(errs, c.applySettings(raw, src, opts.Overrides, routes)...)

	tunnelSet := map[string]bool{}
	for _, t := range tunnels {
		tunnelSet[t.Name] = true
	}
	l := &channelLoader{
		configDir:  p.ConfigDir,
		tunnels:    tunnelSet,
		userAgent:  c.upstream.UserAgent,
		epgSources: map[string]bool{},
		ids:        map[string]string{},
	}
	for _, s := range c.epgSources {
		l.epgSources[s.Name] = true
	}
	files, ferrs := listFiles(filepath.Join(p.ConfigDir, "channels.d"), ".toml")
	errs = append(errs, ferrs...)
	// The global layer applies to every channel file, so it is loaded
	// first; other names starting with "_" are reserved (spec 5.4).
	var channelFiles []string
	for _, f := range files {
		switch base := filepath.Base(f); {
		case base == globalDefaultsFile:
			if data, err := os.ReadFile(f); err != nil {
				l.errs = append(l.errs, err)
			} else {
				l.loadGlobal(f, data)
			}
		case strings.HasPrefix(base, "_"):
			l.fail(f, "reserved file name: channels.d names starting with \"_\" are reserved (only %s is known)", globalDefaultsFile)
		default:
			channelFiles = append(channelFiles, f)
		}
	}
	for _, f := range channelFiles {
		data, err := os.ReadFile(f)
		if err != nil {
			l.errs = append(l.errs, err)
			continue
		}
		l.loadFile(f, data)
	}
	l.checkEPGSources()
	errs = append(errs, l.errs...)

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	c.channels = l.channels
	for i, ch := range c.channels {
		c.byID[ch.id] = i
	}
	return c, nil
}

// loadTunnels returns the tunnel files in dir, sorted by name.
func loadTunnels(dir string) ([]Tunnel, []error) {
	files, errs := listFiles(dir, ".conf")
	var tunnels []Tunnel
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".conf")
		switch {
		case name == RouteDirect:
			errs = append(errs, fmt.Errorf("%s: tunnel name %q is reserved", f, name))
		case !ValidName(name):
			errs = append(errs, fmt.Errorf("%s: invalid tunnel name %q: use [a-z0-9-], 1-%d characters", f, name, maxNameLen))
		default:
			tunnels = append(tunnels, Tunnel{Name: name, Path: f})
		}
	}
	return tunnels, errs
}

// listFiles returns the regular files (symlinks followed) in dir with the
// given suffix, sorted, non-recursively. A missing dir is empty.
func listFiles(dir, suffix string) ([]string, []error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []error{err}
	}
	var files []string
	var errs []error
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), suffix) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		fi, err := os.Stat(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if fi.Mode().IsRegular() {
			files = append(files, p)
		}
	}
	return files, errs
}

// logosDir returns the local logo directory (spec 5.1).
func logosDir(configDir string) string {
	return filepath.Join(configDir, "assets", "logos")
}

// LoadAddresses reads only the listen address of the proxy and of the
// debug listener (spec 5.3, 7.2): config.toml is decoded and its errors
// count, but the tunnel and channel files are neither read nor
// validated, and neither are the settings that address no listener.
// The commands that only talk to a running proxy (healthcheck, tunnel
// list|stop, probe) use it, so they keep working while the proxy serves
// its loaded config and a newer version with a broken channel or tunnel
// file is rejected by the reload (5.6). Overrides (flags and
// environment, Resolve) apply like in Load.
func LoadAddresses(opts Options) (listen, debugListen string, err error) {
	p := opts.Paths
	fi, err := os.Stat(p.ConfigDir)
	if err != nil {
		return "", "", fmt.Errorf("config dir: %w", err)
	}
	if !fi.IsDir() {
		return "", "", fmt.Errorf("config dir %s: not a directory", p.ConfigDir)
	}

	raw, src, rerrs := readConfigToml(p)
	if len(rerrs) > 0 {
		return "", "", errors.Join(rerrs...)
	}

	var errs []error
	fail := func(key, origin, format string, args ...any) {
		if origin == overrideSource {
			key += " " + overrideSource
		} else {
			key = origin + ": " + key
		}
		errs = append(errs, fmt.Errorf("%s: %s", key, fmt.Sprintf(format, args...)))
	}

	listen, origin := defaultListen, src
	switch {
	case opts.Overrides.Listen != "":
		listen, origin = opts.Overrides.Listen, overrideSource
	case raw.Listen != nil:
		listen = *raw.Listen
	}
	if !validListen(listen) {
		fail("listen", origin, "invalid address %q: use [host]:port", listen)
	}

	if v := raw.Debug.Listen; v != nil && *v != "" {
		debugListen = *v
		if !validListen(debugListen) {
			fail("debug.listen", src, "invalid address %q: use [host]:port", debugListen)
		}
	}
	if len(errs) > 0 {
		return "", "", errors.Join(errs...)
	}
	return listen, debugListen, nil
}

// readConfigToml reads and decodes config.toml (spec 5.3): a missing
// file counts as empty, the errors carry file and position. A partially
// decoded raw value must not be validated; the errors say what is wrong.
func readConfigToml(p Paths) (raw rawConfig, src string, errs []error) {
	src = filepath.Join(p.ConfigDir, "config.toml")
	data, err := os.ReadFile(src)
	switch {
	case err == nil:
		if derrs := decodeTOML(src, data, &raw); derrs != nil {
			return rawConfig{}, src, derrs
		}
	case !errors.Is(err, fs.ErrNotExist):
		return rawConfig{}, src, []error{err}
	}
	return raw, src, nil
}
