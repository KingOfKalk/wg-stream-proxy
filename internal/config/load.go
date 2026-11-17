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

// Load reads and validates the configuration (spec 5.1-5.4): config.toml,
// the tunnel file names in the tunnels dir and channels.d/*.toml. Missing
// config.toml, tunnels dir or channels.d count as empty. All errors found
// are returned joined, each prefixed with its file.
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

	var raw rawConfig
	src := filepath.Join(p.ConfigDir, "config.toml")
	data, err := os.ReadFile(src)
	switch {
	case err == nil:
		if derrs := decodeTOML(src, data, &raw); derrs != nil {
			// Report decode errors only; do not validate a partially
			// decoded file.
			errs = append(errs, derrs...)
			raw = rawConfig{}
		}
	case !errors.Is(err, fs.ErrNotExist):
		errs = append(errs, err)
	}
	errs = append(errs, c.applySettings(raw, src, opts.Overrides, routes)...)

	tunnelSet := map[string]bool{}
	for _, t := range tunnels {
		tunnelSet[t.Name] = true
	}
	l := &channelLoader{
		configDir: p.ConfigDir,
		tunnels:   tunnelSet,
		userAgent: c.upstream.UserAgent,
		ids:       map[string]string{},
	}
	files, ferrs := listFiles(filepath.Join(p.ConfigDir, "channels.d"), ".toml")
	errs = append(errs, ferrs...)
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		l.loadFile(f, data)
	}
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
