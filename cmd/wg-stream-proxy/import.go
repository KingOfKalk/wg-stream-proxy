// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/importer"
)

// importM3U converts an M3U file into a channel file (spec 7.2). Flags
// may come before or after the file. The output goes to stdout, or to a
// new file named by --out that is never overwritten.
func (c *cli) importM3U(args []string) int {
	flags := newImportFlags()
	flags.fset.SetOutput(c.stderr)
	files, err := parseInterspersed(flags.fset, args)
	if err != nil {
		return c.parseFailed("import", err)
	}
	opts, out, configDir, matchEPG := flags.opts, flags.out, flags.configDir, flags.matchEPG
	if len(files) != 1 {
		_, _ = fmt.Fprintln(c.stderr, "wg-stream-proxy: import: want exactly one M3U file")
		return 2
	}
	file := files[0]
	data, err := c.readFile(file)
	if err != nil {
		_, _ = fmt.Fprintf(c.stderr, "wg-stream-proxy: import: %v\n", err)
		return 1
	}
	if matchEPG {
		copts, err := config.Resolve(config.Flags{ConfigDir: configDir}, c.env)
		if err != nil {
			c.printErrors(err)
			return 1
		}
		cfg, err := config.Load(copts)
		if err != nil {
			c.printErrors(err)
			return 1
		}
		if opts.EPG, err = c.loadEPG(cfg); err != nil {
			_, _ = fmt.Fprintf(c.stderr, "wg-stream-proxy: import: --match-epg: %v\n", err)
			return 1
		}
	}
	res, err := importer.Import(data, opts)
	for _, w := range res.Warnings {
		_, _ = fmt.Fprintf(c.stderr, "wg-stream-proxy: import: warning: %s:%d: %s\n", file, w.Line, w.Msg)
	}
	if err != nil {
		_, _ = fmt.Fprintf(c.stderr, "wg-stream-proxy: import: %s: %v\n", file, err)
		return 1
	}
	if out == "" {
		_, err = c.stdout.Write(res.TOML)
	} else {
		err = writeNewFile(out, res.TOML)
	}
	if err != nil {
		_, _ = fmt.Fprintf(c.stderr, "wg-stream-proxy: import: %v\n", err)
		return 1
	}
	return 0
}

// parseInterspersed parses flags before and after positional arguments
// and returns the positional ones. Everything after "--" is positional.
func parseInterspersed(fset *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fset.Parse(args); err != nil {
			return nil, err
		}
		rest := fset.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		if n := len(args) - len(rest); n > 0 && args[n-1] == "--" {
			return append(pos, rest...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// writeNewFile creates path, which must not exist, and writes data. A
// partly written file is removed.
func writeNewFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%s already exists, not overwritten", path)
	}
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}
