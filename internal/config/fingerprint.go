// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Fingerprint describes the files a load reads (spec 5.6): config.toml,
// channels.d/*.toml (with _defaults.toml) and the tunnels dir's *.conf,
// each by path, size and modification time (symlinks are followed, like
// Load does). Two fingerprints are equal exactly if none of these files
// was added, removed or changed in between, as far as size and mtime
// show it. Other files, like editor swap files, and dotfiles do not count
// (listFiles skips them). Missing files and directories are fine; nothing
// is read but the metadata.
func Fingerprint(p Paths) string {
	var b strings.Builder
	add := func(path string) {
		fi, err := os.Stat(path)
		if err != nil || !fi.Mode().IsRegular() {
			return
		}
		b.WriteString(path)
		b.WriteByte(0)
		b.WriteString(strconv.FormatInt(fi.Size(), 10))
		b.WriteByte(0)
		b.WriteString(strconv.FormatInt(fi.ModTime().UnixNano(), 10))
		b.WriteByte('\n')
	}
	add(filepath.Join(p.ConfigDir, "config.toml"))
	channels, _ := listFiles(filepath.Join(p.ConfigDir, "channels.d"), ".toml")
	for _, f := range channels {
		add(f)
	}
	tunnels, _ := listFiles(p.TunnelsDir, ".conf")
	for _, f := range tunnels {
		add(f)
	}
	return b.String()
}
