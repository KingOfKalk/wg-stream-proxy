// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"errors"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"
)

// logoCacheControl lets players cache a logo for an hour; after that
// they revalidate with Last-Modified.
const logoCacheControl = "public, max-age=3600"

// logoTypes maps lower-case file extensions to image media types. It is
// fixed so the result does not depend on the host's mime database; other
// extensions fall back to it and then to application/octet-stream.
var logoTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".svg":  "image/svg+xml",
	".ico":  "image/x-icon",
	".avif": "image/avif",
}

// handleLogo serves a file of the logos directory (spec 5.4, 7.1).
//
// Any plain regular file of the directory is served, not only the names
// the channels use, so a logo added while the proxy runs works at once.
// The file name is one path segment and is opened through an os.Root, so
// neither the name nor a symbolic link can lead out of the directory.
func (s *Server) handleLogo(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if !plainLogoName(name) {
		http.NotFound(w, r)
		return
	}
	root, err := os.OpenRoot(s.active().cfg.LogosDir())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("logos dir", "error", err)
		}
		http.NotFound(w, r)
		return
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(name)
	if err != nil {
		// Missing, a link leaving the directory, a loop: all 404.
		http.NotFound(w, r)
		return
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}

	h := w.Header()
	h.Set("Content-Type", logoType(name))
	h.Set("Cache-Control", logoCacheControl)
	h.Set("X-Content-Type-Options", "nosniff")
	// An SVG opened directly must not run scripts or load anything.
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

// plainLogoName reports whether name is a plain file name: not empty,
// no path separator, no NUL or other control character, not a dot name.
func plainLogoName(name string) bool {
	if name == "" || name[0] == '.' || len(name) > 255 {
		return false
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; c < 0x20 || c == 0x7f || c == '/' || c == '\\' {
			return false
		}
	}
	return true
}

// logoType returns the media type of a logo file name.
func logoType(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if t, ok := logoTypes[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); ext != "" && t != "" {
		return t
	}
	return "application/octet-stream"
}
