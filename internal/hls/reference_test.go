// SPDX-License-Identifier: AGPL-3.0-or-later

package hls

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"net/url"
	"strings"
)

// The rewriter and resource path builder as they were before the
// performance work of issue #76. The differential tests compare the
// current implementation with them byte for byte.

// referenceRewrite is the former Rewrite with an urlFor that gets the
// resolved URL.
func referenceRewrite(playlist []byte, final *url.URL, urlFor func(abs *url.URL) string) ([]byte, error) {
	if len(playlist) > MaxPlaylistSize {
		return nil, ErrPlaylistTooLarge
	}
	if final == nil || !isHTTPScheme(final.Scheme) || final.Host == "" {
		return nil, ErrInvalidFinalURL
	}
	if bytes.IndexByte(playlist, 0) >= 0 {
		return nil, fmt.Errorf("%w: NUL byte", ErrInvalidPlaylist)
	}
	rw := referenceRewriter{final: final, urlFor: urlFor, out: make([]byte, 0, len(playlist)+len(playlist)/2)}
	first := true
	for rest := playlist; len(rest) > 0; {
		var content, eol []byte
		content, eol, rest = nextLine(rest)
		if first {
			first = false
			if bytes.HasPrefix(content, []byte(utf8BOM)) {
				rw.out = append(rw.out, utf8BOM...)
				content = content[len(utf8BOM):]
			}
		}
		if err := rw.line(content); err != nil {
			return nil, err
		}
		rw.out = append(rw.out, eol...)
	}
	return rw.out, nil
}

type referenceRewriter struct {
	final  *url.URL
	urlFor func(*url.URL) string
	out    []byte
}

func (rw *referenceRewriter) line(content []byte) error {
	trimmed := bytes.TrimLeft(content, lineSpace)
	lead := len(content) - len(trimmed)
	trimmed = bytes.TrimRight(trimmed, lineSpace)
	switch {
	case len(trimmed) == 0:
		rw.out = append(rw.out, content...)
		return nil
	case trimmed[0] == '#':
		if bytes.HasPrefix(trimmed, []byte("#EXT")) {
			return rw.tag(content)
		}
		rw.out = append(rw.out, content...)
		return nil
	}
	rw.out = append(rw.out, content[:lead]...)
	if err := rw.uri(trimmed); err != nil {
		return err
	}
	rw.out = append(rw.out, content[lead+len(trimmed):]...)
	return nil
}

func (rw *referenceRewriter) tag(line []byte) error {
	if bytes.HasPrefix(bytes.TrimLeft(line, lineSpace), []byte("#EXTINF:")) {
		rw.out = append(rw.out, line...)
		return nil
	}
	spans := uriAttrValues(line)
	if strayHTTPURI(line, spans) {
		return fmt.Errorf("%w: URI= with an http(s) URL outside a recognised attribute", ErrInvalidPlaylist)
	}
	done := 0
	for _, v := range spans {
		rw.out = append(rw.out, line[done:v.start]...)
		if err := rw.uri(line[v.start:v.end]); err != nil {
			return err
		}
		done = v.end
	}
	rw.out = append(rw.out, line[done:]...)
	return nil
}

func (rw *referenceRewriter) uri(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	if scheme := uriScheme(raw); scheme != "" && !isHTTPScheme(scheme) {
		rw.out = append(rw.out, raw...)
		return nil
	}
	ref, err := url.Parse(string(raw))
	if err != nil {
		return ErrInvalidURI
	}
	abs := rw.final.ResolveReference(ref)
	if len(abs.String()) > MaxURLLen {
		return ErrURLTooLong
	}
	repl := rw.urlFor(abs)
	if repl == "" || strings.ContainsAny(repl, "\"\r\n") {
		return ErrInvalidResourceURL
	}
	rw.out = append(rw.out, repl...)
	return nil
}

// referencePath is the former Signer.Path.
func referencePath(key []byte, channel, upstream string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(channel + "\n" + upstream))
	return resourcePrefix + channel + "/" + b64.EncodeToString(m.Sum(nil)[:sigLen]) + "/" +
		b64.EncodeToString([]byte(upstream)) + "/" + referenceResourceName(upstream)
}

// referenceResourceName is the former resourceName.
func referenceResourceName(upstream string) string {
	u, err := url.Parse(upstream)
	if err != nil {
		return fallbackName
	}
	seg := u.Path[strings.LastIndexByte(u.Path, '/')+1:]
	name := strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			return r
		}
		return -1
	}, seg)
	if len(name) > maxNameLen {
		ext := ""
		if i := strings.LastIndexByte(name, '.'); i >= 0 && len(name)-i < maxNameLen {
			ext = name[i:]
		}
		name = name[:maxNameLen-len(ext)] + ext
	}
	if strings.Trim(name, ".") == "" {
		return fallbackName
	}
	return name
}
