// SPDX-License-Identifier: AGPL-3.0-or-later

package wgquick

import (
	"fmt"
	"io/fs"
)

// CheckUniquePrivateKeys returns a config error if two tunnels share a
// PrivateKey (spec 5.5). The error names both tunnels, never the key.
func CheckUniquePrivateKeys(tunnels []*Tunnel) error {
	first := make(map[Key]string, len(tunnels))
	for _, t := range tunnels {
		if t == nil {
			continue
		}
		if name, ok := first[t.PrivateKey]; ok {
			return &Error{Tunnel: t.Name, Msg: fmt.Sprintf("same PrivateKey as tunnel %q", name)}
		}
		first[t.PrivateKey] = t.Name
	}
	return nil
}

// ModeWarning returns a warning if a tunnel file with the given mode is
// world-readable (spec 5.5). The caller passes the mode from os.Stat.
func ModeWarning(name string, mode fs.FileMode) (Warning, bool) {
	if mode.Perm()&0o004 == 0 {
		return Warning{}, false
	}
	return Warning{Tunnel: name, Msg: "file is world-readable but contains the private key; use chmod 600"}, true
}
