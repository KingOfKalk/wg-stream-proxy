// SPDX-License-Identifier: AGPL-3.0-or-later

package importer

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
)

// assignIDs sets an explicit id where the default id (slug of the name,
// spec 5.2) is empty or already used by an earlier channel: the slug (or
// "channel") with the first free suffix -2, -3, ...
func assignIDs(chs []channel) {
	used := map[string]bool{}
	for i := range chs {
		slug := config.Slug(chs[i].name)
		base := slug
		if base == "" {
			base = "channel"
		}
		id := base
		for n := 2; used[id]; n++ {
			suffix := fmt.Sprintf("-%d", n)
			id = strings.TrimRight(base[:min(len(base), 48-len(suffix))], "-") + suffix
		}
		used[id] = true
		if id != slug {
			chs[i].id = id
		}
	}
}

// layer holds the values written to [defaults.*] or a channel's tables.
type layer struct {
	tvgID, logo string
	tvgHint     string // comment line, only in channel layers
	headers     map[string]string
	attrs       map[string]string
	extra       map[string]map[string]string
}

func layerOf(ch channel) layer {
	return layer{
		tvgID: ch.tvgID, logo: ch.logo, tvgHint: ch.tvgHint,
		headers: maps.Clone(ch.headers), attrs: maps.Clone(ch.attrs), extra: cloneExtra(ch.extra),
	}
}

func cloneExtra(m map[string]map[string]string) map[string]map[string]string {
	out := make(map[string]map[string]string, len(m))
	for d, kv := range m {
		out[d] = maps.Clone(kv)
	}
	return out
}

// shared moves the values all layers have in common (at least two
// layers) into the returned defaults layer.
func shared(ls []layer) layer {
	def := layer{headers: map[string]string{}, attrs: map[string]string{}, extra: map[string]map[string]string{}}
	if len(ls) < 2 {
		return def
	}
	same := func(get func(layer) (string, bool)) (string, bool) {
		v, ok := get(ls[0])
		if !ok {
			return "", false
		}
		for _, l := range ls[1:] {
			if w, ok := get(l); !ok || w != v {
				return "", false
			}
		}
		return v, true
	}
	if v, ok := same(func(l layer) (string, bool) { return l.tvgID, l.tvgID != "" }); ok {
		def.tvgID = v
		for i := range ls {
			ls[i].tvgID = ""
		}
	}
	if v, ok := same(func(l layer) (string, bool) { return l.logo, l.logo != "" }); ok {
		def.logo = v
		for i := range ls {
			ls[i].logo = ""
		}
	}
	sharedMap := func(get func(layer) map[string]string, set func(k, v string)) {
		for _, k := range slices.Sorted(maps.Keys(get(ls[0]))) {
			v, ok := same(func(l layer) (string, bool) { v, ok := get(l)[k]; return v, ok })
			if !ok {
				continue
			}
			set(k, v)
			for _, l := range ls {
				delete(get(l), k)
			}
		}
	}
	sharedMap(func(l layer) map[string]string { return l.headers }, func(k, v string) { def.headers[k] = v })
	sharedMap(func(l layer) map[string]string { return l.attrs }, func(k, v string) { def.attrs[k] = v })
	for _, d := range slices.Sorted(maps.Keys(ls[0].extra)) {
		sharedMap(func(l layer) map[string]string { return l.extra[d] }, func(k, v string) {
			if def.extra[d] == nil {
				def.extra[d] = map[string]string{}
			}
			def.extra[d][k] = v
		})
	}
	return def
}

// render writes the channel file.
func render(doc document, opts Options) []byte {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	if len(doc.epgURLs) > 0 {
		line("# EPG from the #EXTM3U header; it belongs in config.toml:")
		line("# [epg]")
		for _, u := range doc.epgURLs {
			line("# url = %s", quote(u))
		}
		line("")
	}

	chs := doc.channels
	group := ""
	if len(chs) >= 2 && chs[0].group != "" && !slices.ContainsFunc(chs, func(c channel) bool { return c.group != chs[0].group }) {
		group = chs[0].group
	}
	top := false
	if opts.Tunnel != "" {
		line("tunnel = %s", quote(opts.Tunnel))
		top = true
	}
	if group != "" {
		line("group = %s", quote(group))
		top = true
	}

	ls := make([]layer, len(chs))
	for i, ch := range chs {
		ls[i] = layerOf(ch)
	}
	def := shared(ls)
	writeLayer(&b, "defaults", def, top)

	for i, ch := range chs {
		if b.Len() > 0 {
			line("")
		}
		line("[[channel]]")
		line("name = %s", quote(ch.name))
		if ch.id != "" {
			line("id = %s", quote(ch.id))
		}
		line("url = %s", quote(ch.url))
		if group == "" && ch.group != "" {
			line("group = %s", quote(ch.group))
		}
		if !ch.enabled {
			line("enabled = false")
		}
		writeLayer(&b, "channel", ls[i], true)
	}
	return []byte(b.String())
}

// writeLayer writes the upstream and playlist tables of a layer under
// prefix ("defaults" or "channel"); sep adds a blank line before the
// first table.
func writeLayer(b *strings.Builder, prefix string, l layer, sep bool) {
	table := func(name string) {
		if sep {
			b.WriteString("\n")
		}
		sep = true
		b.WriteString("[" + prefix + "." + name + "]\n")
	}
	if len(l.headers) > 0 {
		table("upstream")
		b.WriteString("headers = " + inline(l.headers) + "\n")
	}
	if l.tvgID != "" || l.tvgHint != "" || l.logo != "" || len(l.attrs) > 0 {
		table("playlist")
		if l.tvgID != "" {
			b.WriteString("tvg_id = " + quote(l.tvgID) + "\n")
		}
		if l.tvgHint != "" {
			b.WriteString("# " + l.tvgHint + "\n")
		}
		if l.logo != "" {
			b.WriteString("logo = " + quote(l.logo) + "\n")
		}
		if len(l.attrs) > 0 {
			b.WriteString("attrs = " + inline(l.attrs) + "\n")
		}
	}
	for _, d := range slices.Sorted(maps.Keys(l.extra)) {
		kv := l.extra[d]
		if len(kv) == 0 {
			continue
		}
		table("playlist.extra." + d)
		for _, k := range slices.Sorted(maps.Keys(kv)) {
			b.WriteString(quote(k) + " = " + quote(kv[k]) + "\n")
		}
	}
}

// inline writes an inline table with sorted keys.
func inline(m map[string]string) string {
	parts := make([]string, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, key(k)+" = "+quote(m[k]))
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

// key returns k as a bare TOML key if possible, else quoted.
func key(k string) string {
	if k == "" || strings.ContainsFunc(k, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_'
	}) {
		return quote(k)
	}
	return k
}

// quote returns s as a TOML basic string. s must be valid UTF-8.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Quote returns s as a TOML basic string, for callers that print TOML
// snippets. s must be valid UTF-8.
func Quote(s string) string { return quote(s) }
